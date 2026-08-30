//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/harrison542002/go-route/internal/adapters/outbound/sink"
	"github.com/harrison542002/go-route/internal/core/domains"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Integration Suite")
}

var (
	dsn  string
	pool *pgxpool.Pool
)

var _ = BeforeSuite(func() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("goroute"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	Expect(err).NotTo(HaveOccurred(), "start postgres container")

	DeferCleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = container.Terminate(ctx)
	})

	dsn, err = container.ConnectionString(ctx, "sslmode=disable")
	Expect(err).NotTo(HaveOccurred())

	pool, err = pgxpool.New(ctx, dsn)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(pool.Close)

	Expect(pool.Ping(ctx)).To(Succeed())

	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	w, err := sink.NewPostgresWriter(c, dsn)
	Expect(err).NotTo(HaveOccurred())
	Expect(w.Close()).To(Succeed())
})

func truncate() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := pool.Exec(ctx, "TRUNCATE decisions")
	Expect(err).NotTo(HaveOccurred())
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

var testTime = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

type decisionOpt func(*domains.RoutingDecision)

func newDecision(opts ...decisionOpt) domains.RoutingDecision {
	d := domains.RoutingDecision{
		ID:         domains.NewDecisionID(),
		OccurredAt: testTime,
		Tenant:     domains.DefaultTenant,
		Request: domains.RequestSummary{
			RequestedModel: "chat",
			Stream:         true,
			WantsUsage:     false,
			Metadata:       map[string]string{"feature": "auto-tag", "team": "platform"},
		},
		Ladder: domains.Ladder{
			Targets: []domains.TargetRef{
				{Name: "openai/gpt-5-mini", Provider: "openai", UpstreamModel: "gpt-5-mini"},
			},
			Reason: domains.Reason{Kind: domains.ReasonModelAlias, ModelAlias: "chat"},
		},
		Outcome: domains.Outcome{
			Status:   domains.StatusOK,
			Attempts: []domains.Attempt{{Target: "openai/gpt-5-mini", StartedAt: testTime, DurationMs: 412}},
			Usage: domains.TokenUsage{
				Input: 80, Output: 50, CacheRead: 20, Reasoning: 10,
			},
			TTFTMs:  412,
			TotalMs: 1893,
		},
		Cost: &domains.CostBreakdown{
			Actual:            domains.USD(120_500),
			PriceTableVersion: "2026-08-01",
			Counterfactuals: []domains.Counterfactual{
				{Target: "openai/gpt-5", Cost: domains.USD(602_500)},
			},
		},
	}

	for _, opt := range opts {
		opt(&d)
	}
	return d
}

func withTenant(t string) decisionOpt {
	return func(d *domains.RoutingDecision) { d.Tenant = domains.Tenant(t) }
}

func withTime(at time.Time) decisionOpt {
	return func(d *domains.RoutingDecision) { d.OccurredAt = at }
}

func withMetadata(m map[string]string) decisionOpt {
	return func(d *domains.RoutingDecision) { d.Request.Metadata = m }
}

func unpriced() decisionOpt {
	return func(d *domains.RoutingDecision) { d.Cost = nil }
}

func exhausted() decisionOpt {
	return func(d *domains.RoutingDecision) {
		d.Outcome.Status = domains.StatusExhausted
		d.Outcome.Usage = domains.TokenUsage{}
		d.Outcome.TTFTMs = 0
		d.Outcome.Attempts = []domains.Attempt{{
			Target:    "openai/gpt-5-mini",
			StartedAt: testTime,
			Failure: &domains.AttemptFailure{
				Kind: "connect", Message: "connection refused", Retryable: true,
			},
		}}
		d.Cost = nil
	}
}

func countRows(query string, args ...any) int {
	c, cancel := ctx()
	defer cancel()

	var n int
	Expect(pool.QueryRow(c, query, args...).Scan(&n)).To(Succeed())
	return n
}
