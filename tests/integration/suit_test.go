//go:build integration

package integration

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/drivers/postgresql"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestMain owns the database, and the Redis that quota counters live in, for
// the whole package.
//
// It cannot be a Ginkgo BeforeSuite: the end-to-end specs are plain Test
// functions, and Go runs those independently of the Ginkgo suite -- in
// file order, so they would run before any BeforeSuite had opened a
// container. One setup here serves both styles.
func TestMain(m *testing.M) {
	code, err := runSuite(m)
	if err != nil {
		log.Fatalf("integration setup: %v", err)
	}
	os.Exit(code)
}

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Integration Suite")
}

var (
	dsn  string
	pool *pgxpool.Pool

	// redisAddr is host:port of the suite's Redis. Specs that need to take
	// Redis down start their own rather than stopping this one.
	redisAddr string
)

// redisImage is shared by the suite's Redis and any a spec starts itself.
const redisImage = "redis:7-alpine"

// runSuite is separate from TestMain so its defers run: os.Exit skips
// them, and a leaked container outlives the test binary.
func runSuite(m *testing.M) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	if err != nil {
		return 0, fmt.Errorf("start postgres: %w", err)
	}
	defer func() {
		stop, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		_ = container.Terminate(stop)
	}()

	if dsn, err = container.ConnectionString(ctx, "sslmode=disable"); err != nil {
		return 0, fmt.Errorf("connection string: %w", err)
	}

	redisContainer, err := tcredis.Run(ctx, redisImage)
	if err != nil {
		return 0, fmt.Errorf("start redis: %w", err)
	}
	defer func() {
		stop, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		_ = redisContainer.Terminate(stop)
	}()

	if redisAddr, err = redisContainer.Endpoint(ctx, ""); err != nil {
		return 0, fmt.Errorf("redis endpoint: %w", err)
	}

	if pool, err = pgxpool.New(ctx, dsn); err != nil {
		return 0, fmt.Errorf("pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return 0, fmt.Errorf("ping: %w", err)
	}

	if err := migrate(ctx); err != nil {
		return 0, err
	}

	// Partition maintenance is the gateway's job, not the schema's, so
	// the suite does what bootstrap does.
	if err := postgresql.NewPartitions(pool).Ensure(ctx); err != nil {
		return 0, err
	}

	if err := seed(ctx); err != nil {
		return 0, err
	}

	return m.Run(), nil
}

// seed provisions what an admin API would. Nothing is created implicitly
// any more -- not tenants, not keys -- so every tenant the specs send
// traffic for has to exist up front.
func seed(ctx context.Context) error {
	for _, name := range []string{string(domains.DefaultTenant), "acme", "globex"} {
		_, err := pool.Exec(ctx,
			`INSERT INTO tenants (id, external_id, name) VALUES ($1, $2, $2)
			 ON CONFLICT (external_id) DO NOTHING`,
			uuid.New(), name)
		if err != nil {
			return fmt.Errorf("seed tenant %s: %w", name, err)
		}
	}

	// The end-to-end specs go through the real authentication middleware,
	// so they need a real key.
	_, err := pool.Exec(ctx, `
		INSERT INTO api_keys (id, tenant_id, key_hash, key_prefix)
		VALUES ($1, (SELECT id FROM tenants WHERE external_id = 'acme'), $2, $3)
		ON CONFLICT (key_hash) DO NOTHING`,
		uuid.New(), sha256Of(e2eKey), e2eKey[:10])
	if err != nil {
		return fmt.Errorf("seed api key: %w", err)
	}
	return nil
}

// migrate runs the real Atlas migration path rather than a test-only
// substitute, so these specs fail if a migration is malformed or missing
// from atlas.sum.
func migrate(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "atlas", "migrate", "apply",
		"--dir", "file://../../db/migrations",
		"--url", dsn,
	)

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("atlas migrate apply: %w\n%s", err, out)
	}
	return nil
}

func truncate() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := pool.Exec(ctx,
		"TRUNCATE usage_ledger, audit_log, admin_credentials, admin_refresh_tokens, admin_users")
	Expect(err).NotTo(HaveOccurred())
}

func tenantID(external string) uuid.UUID {
	c, cancel := ctx()
	defer cancel()

	var id uuid.UUID
	Expect(pool.QueryRow(c, "SELECT id FROM tenants WHERE external_id = $1", external).
		Scan(&id)).To(Succeed())
	return id
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

func withKey(id uuid.UUID) decisionOpt {
	return func(d *domains.RoutingDecision) { d.KeyID = id }
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

func tenantName(s string) domains.Tenant { return domains.Tenant(s) }

// e2eKey is the credential the end-to-end specs authenticate with,
// provisioned once for the whole suite.
const e2eKey = "gr_live_e2e"

func countRows(query string, args ...any) int {
	c, cancel := ctx()
	defer cancel()

	var n int
	Expect(pool.QueryRow(c, query, args...).Scan(&n)).To(Succeed())
	return n
}
