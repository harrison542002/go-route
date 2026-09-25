//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	goredis "github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/adapters/repositories/redisquota"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/drivers/redis"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/quota"
)

// quotaTimeout is generous so a busy CI host cannot turn a slow reply
// into a spurious fail-open, which would show up as a miscount.
const quotaTimeout = 2 * time.Second

var _ = Describe("Quota", func() {
	var (
		tenant   domains.Tenant
		tenantID uuid.UUID
		rdb      *goredis.Client
	)

	BeforeEach(func() {
		tenant, tenantID = newQuotaTenant()

		rdb = redis.New(redis.Config{Addr: redisAddr, Timeout: quotaTimeout})
		DeferCleanup(rdb.Close)
	})

	// replica builds one gateway's worth of enforcement: its own Redis
	// client, its own limits cache, its own flusher. Several of these
	// against one Redis are several replicas.
	replica := func(policy quota.OnUnavailable) *quota.Enforcer {
		client := redis.New(redis.Config{Addr: redisAddr, Timeout: quotaTimeout})
		e := newEnforcer(
			redisquota.New(client, quotaTimeout),
			nanoPerToken{},
			quota.Config{OnUnavailable: policy, FlushInterval: 50 * time.Millisecond},
		)
		DeferCleanup(func() {
			c, cancel := ctx()
			defer cancel()
			_ = e.Close(c)
			_ = client.Close()
		})
		return e
	}

	// resyncing is a replica that raises its counters back to the durable
	// snapshot every interval, rather than once a minute.
	resyncing := func(interval time.Duration) *quota.Enforcer {
		client := redis.New(redis.Config{Addr: redisAddr, Timeout: quotaTimeout})
		e := newEnforcer(
			redisquota.New(client, quotaTimeout),
			nanoPerToken{},
			quota.Config{
				OnUnavailable:  quota.FailClosed,
				FlushInterval:  50 * time.Millisecond,
				ResyncInterval: interval,
			},
		)
		DeferCleanup(func() {
			c, cancel := ctx()
			defer cancel()
			_ = e.Close(c)
			_ = client.Close()
		})
		return e
	}

	// Specs run on the real clock: counter keys expire at their window's
	// end, so a window in the past would be deleted as it was written. A
	// period around now also cannot roll over mid-spec, as a minute can.
	period := func() domains.Window {
		start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
		return domains.Window{Kind: domains.WindowPeriod, Start: start, End: start.Add(2 * time.Hour)}
	}

	request := func(maxOutput int) ports.QuotaRequest {
		return ports.QuotaRequest{
			Tenant: tenant, At: time.Now(), MaxOutputTokens: maxOutput,
			Targets: []string{"fake/m"},
		}
	}

	counter := func(w domains.Window) map[string]string {
		c, cancel := ctx()
		defer cancel()
		vals, err := rdb.HGetAll(c, redisquota.Key(tenantID, w)).Result()
		Expect(err).NotTo(HaveOccurred())
		return vals
	}

	// overwriteCounter puts the counter back to smaller values behind every
	// enforcer's back, which is what a replica promoted without the primary's
	// last acknowledged writes looks like from here.
	overwriteCounter := func(w domains.Window, u domains.QuotaUsage) {
		c, cancel := ctx()
		defer cancel()
		Expect(rdb.HSet(c, redisquota.Key(tenantID, w),
			"requests", u.Requests, "tokens", u.Tokens, "cost_nanos", int64(u.Cost)).Err()).To(Succeed())
	}

	counterTTL := func(w domains.Window) time.Duration {
		c, cancel := ctx()
		defer cancel()
		d, err := rdb.TTL(c, redisquota.Key(tenantID, w)).Result()
		Expect(err).NotTo(HaveOccurred())
		return d
	}

	seed := func(w domains.Window, u domains.QuotaUsage) {
		c, cancel := ctx()
		defer cancel()
		Expect(redisquota.New(rdb, quotaTimeout).
			Seed(c, []domains.WindowUsage{{TenantID: tenantID, Window: w, Usage: u}})).To(Succeed())
	}

	It("never admits more than the limit, however many replicas race for it", func() {
		// The claim the whole design rests on: 150 concurrent requests
		// from three replicas against a limit of 25 admit exactly 25.
		p := period()
		setQuota(tenantID, p, quotaRow{maxCost: 250})

		replicas := []*quota.Enforcer{replica(quota.FailClosed), replica(quota.FailClosed), replica(quota.FailClosed)}

		var admitted, refused atomic.Int64
		var wg sync.WaitGroup
		for i := range 150 {
			wg.Go(func() {
				defer GinkgoRecover()
				res, err := replicas[i%len(replicas)].Reserve(context.Background(), request(10))
				switch {
				case err == nil:
					Expect(res.IsZero()).To(BeFalse())
					admitted.Add(1)
				case errors.Is(err, ports.ErrQuotaExceeded):
					refused.Add(1)
				default:
					Fail("unexpected error: " + err.Error())
				}
			})
		}
		wg.Wait()

		Expect(admitted.Load()).To(Equal(int64(25)))
		Expect(refused.Load()).To(Equal(int64(125)))
		Expect(counter(p)).To(And(
			HaveKeyWithValue("cost_nanos", "250"),
			HaveKeyWithValue("requests", "25")))
	})

	It("reserves against every window or none", func() {
		// The period runs out while the day still has room. A refused
		// request must not leave its reservation on the day, or the day
		// would count requests that never happened.
		p := period()
		setQuota(tenantID, p, quotaRow{maxCost: 10_000})
		setQuota(tenantID, domains.Window{Kind: domains.WindowDay}, quotaRow{maxCost: 1_000_000})

		replicas := []*quota.Enforcer{replica(quota.FailClosed), replica(quota.FailClosed)}

		var admitted atomic.Int64
		var wg sync.WaitGroup
		for i := range 80 {
			wg.Go(func() {
				defer GinkgoRecover()
				// No prompt, 300 output tokens: 300 per request, so 33 fit.
				_, err := replicas[i%len(replicas)].Reserve(context.Background(), request(300))
				if err == nil {
					admitted.Add(1)
				} else {
					Expect(err).To(MatchError(ports.ErrQuotaExceeded))
				}
			})
		}
		wg.Wait()

		Expect(admitted.Load()).To(Equal(int64(33)))
		Expect(counter(p)).To(HaveKeyWithValue("tokens", "9900"))

		day, err := domains.ClockWindow(domains.WindowDay, time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(counter(day)).To(HaveKeyWithValue("requests", "33"))
	})

	It("refuses when the estimate would cross the cap, naming spend and reset", func() {
		p := period()
		setQuota(tenantID, p, quotaRow{maxCost: 500})
		e := replica(quota.FailClosed)

		_, err := e.Reserve(context.Background(), request(400))
		Expect(err).NotTo(HaveOccurred())

		_, err = e.Reserve(context.Background(), request(400))
		var exceeded *ports.QuotaExceededError
		Expect(errors.As(err, &exceeded)).To(BeTrue())
		Expect(exceeded.Breach.Used).To(Equal(domains.USD(400)))
		Expect(exceeded.Breach.Requested).To(Equal(domains.USD(400)))
		Expect(exceeded.Breach.Limit).To(Equal(domains.USD(500)))
		Expect(exceeded.Breach.ResetAt()).To(BeTemporally("==", p.End))
		Expect(exceeded.Error()).To(ContainSubstring("spend per period"))
	})

	It("cannot enforce a request nobody can price, and says so", func() {
		p := period()
		setQuota(tenantID, p, quotaRow{maxCost: 1})

		client := redis.New(redis.Config{Addr: redisAddr, Timeout: quotaTimeout})
		e := newEnforcer(redisquota.New(client, quotaTimeout), nil,
			quota.Config{OnUnavailable: quota.FailClosed})
		DeferCleanup(func() {
			c, cancel := ctx()
			defer cancel()
			_ = e.Close(c)
			_ = client.Close()
		})

		_, err := e.Reserve(context.Background(), request(10))
		Expect(err).To(MatchError(ports.ErrQuotaUnavailable))
		Expect(errors.Is(err, ports.ErrQuotaExceeded)).To(BeFalse())
		Expect(e.Unpriced()).To(Equal(int64(1)))
		Expect(counter(p)).To(BeEmpty(), "an unenforceable request must reserve nothing")
	})

	It("keeps serving past a soft limit, and counts the overage", func() {
		p := period()
		setQuota(tenantID, p, quotaRow{maxCost: 2, onExceed: "allow"})
		e := replica(quota.FailClosed)

		for range 5 {
			_, err := e.Reserve(context.Background(), request(1))
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(counter(p)).To(And(
			HaveKeyWithValue("cost_nanos", "5"),
			HaveKeyWithValue("requests", "5")))
	})

	It("reconciles the reservation to what was actually used, and flushes that", func() {
		p := period()
		setQuota(tenantID, p, quotaRow{maxCost: 1_000_000})
		e := replica(quota.FailClosed)

		res, err := e.Reserve(context.Background(), request(1_000))
		Expect(err).NotTo(HaveOccurred())
		Expect(counter(p)).To(HaveKeyWithValue("tokens", "1000"))

		served := domains.Outcome{
			Status:   domains.StatusOK,
			Attempts: []domains.Attempt{{Target: "fake/m"}},
			Usage:    domains.TokenUsage{Input: 10, Output: 5},
		}
		e.Reconcile(context.Background(), res, served)
		Expect(counter(p)).To(And(
			HaveKeyWithValue("tokens", "15"),
			HaveKeyWithValue("requests", "1")))

		// Exhausted releases everything, the request included.
		res, err = e.Reserve(context.Background(), request(1_000))
		Expect(err).NotTo(HaveOccurred())
		e.Reconcile(context.Background(), res, domains.Outcome{Status: domains.StatusExhausted})
		Expect(counter(p)).To(And(
			HaveKeyWithValue("tokens", "15"),
			HaveKeyWithValue("requests", "1")))

		// Only actual usage reaches Postgres, as a delta.
		Eventually(func() int64 {
			return usageCounter(tenantID, p).Tokens
		}).WithTimeout(5 * time.Second).Should(Equal(int64(15)))
		Expect(usageCounter(tenantID, p).Requests).To(Equal(int64(1)))
	})

	It("rebuilds a lost counter from usage_counters instead of starting from zero", func() {
		p := period()
		setQuota(tenantID, p, quotaRow{maxCost: 10_000})
		addUsageCounter(tenantID, p, domains.QuotaUsage{Requests: 40, Tokens: 9_900, Cost: 9_900})

		// Redis never had this counter, as after a failover or eviction.
		Expect(counter(p)).To(BeEmpty())

		e := replica(quota.FailClosed)
		_, err := e.Reserve(context.Background(), request(300))

		var exceeded *ports.QuotaExceededError
		Expect(errors.As(err, &exceeded)).To(BeTrue(),
			"a fresh budget after Redis lost the counter; err = %v", err)
		Expect(exceeded.Breach.Used).To(Equal(domains.USD(9_900)))
		Expect(counter(p)).To(And(
			HaveKeyWithValue("tokens", "9900"),
			HaveKeyWithValue("requests", "40")))
	})

	It("seeds a lost counter once, however many replicas find it missing at once", func() {
		p := period()
		setQuota(tenantID, p, quotaRow{maxCost: 1_000_000})
		addUsageCounter(tenantID, p, domains.QuotaUsage{Requests: 3, Tokens: 5_000, Cost: 5_000})

		replicas := []*quota.Enforcer{
			replica(quota.FailClosed), replica(quota.FailClosed),
			replica(quota.FailClosed), replica(quota.FailClosed),
		}

		var wg sync.WaitGroup
		for i := range 20 {
			wg.Go(func() {
				defer GinkgoRecover()
				_, err := replicas[i%len(replicas)].Reserve(context.Background(), request(100))
				Expect(err).NotTo(HaveOccurred())
			})
		}
		wg.Wait()

		Expect(counter(p)).To(And(
			HaveKeyWithValue("tokens", "7000"), // 5000 + 20 × 100, history counted once
			HaveKeyWithValue("requests", "23")))
	})

	Describe("seeding", func() {
		It("initialises a counter that is not there, expiry included", func() {
			p := period()
			seed(p, domains.QuotaUsage{Requests: 3, Tokens: 70, Cost: 900})

			Expect(counter(p)).To(And(
				HaveKeyWithValue("requests", "3"),
				HaveKeyWithValue("tokens", "70"),
				HaveKeyWithValue("cost_nanos", "900")))
			Expect(counterTTL(p)).To(BeNumerically(">", 0))
		})

		It("writes an all-zero snapshot too, so the window stops reading as missing", func() {
			p := period()
			seed(p, domains.QuotaUsage{})

			Expect(counter(p)).To(HaveKeyWithValue("requests", "0"))
			Expect(counterTTL(p)).To(BeNumerically(">", 0))
		})

		It("raises a counter that is behind the snapshot, per field, and lowers none", func() {
			p := period()
			overwriteCounter(p, domains.QuotaUsage{Requests: 10, Tokens: 5, Cost: 0})

			// Each dimension can be stale on its own, so each is compared on
			// its own: requests stay, tokens and cost rise.
			seed(p, domains.QuotaUsage{Requests: 4, Tokens: 50, Cost: 7})

			Expect(counter(p)).To(And(
				HaveKeyWithValue("requests", "10"),
				HaveKeyWithValue("tokens", "50"),
				HaveKeyWithValue("cost_nanos", "7")))
			Expect(counterTTL(p)).To(BeNumerically(">", 0))
		})

		It("converges however many replicas seed the same window at once", func() {
			p := period()
			snapshot := domains.QuotaUsage{Requests: 9, Tokens: 4_000, Cost: 1_234}

			var wg sync.WaitGroup
			for range 30 {
				wg.Go(func() {
					defer GinkgoRecover()
					seed(p, snapshot)
				})
			}
			wg.Wait()

			// A maximum, not a sum: history is counted once however many
			// replicas load it.
			Expect(counter(p)).To(And(
				HaveKeyWithValue("requests", "9"),
				HaveKeyWithValue("tokens", "4000"),
				HaveKeyWithValue("cost_nanos", "1234")))
			Expect(counterTTL(p)).To(BeNumerically(">", 0))
		})
	})

	Describe("after a failover that lost acknowledged writes", func() {
		// The key survives, holding smaller values than it had. Nothing
		// fails, nothing is missing, and without a resync the tenant would
		// spend the difference twice before the window ended.
		const resyncInterval = 50 * time.Millisecond

		It("raises the counter back to the durable snapshot", func() {
			p := period()
			setQuota(tenantID, p, quotaRow{maxCost: 10_000})
			addUsageCounter(tenantID, p, domains.QuotaUsage{Requests: 40, Tokens: 9_000, Cost: 9_000})

			e := resyncing(resyncInterval)
			_, err := e.Reserve(context.Background(), request(100))
			Expect(err).NotTo(HaveOccurred())
			Expect(counter(p)).To(HaveKeyWithValue("tokens", "9100"))

			overwriteCounter(p, domains.QuotaUsage{Requests: 5, Tokens: 1_000})

			Eventually(func() map[string]string { return counter(p) }).
				WithTimeout(10 * time.Second).
				Should(And(
					HaveKeyWithValue("tokens", "9000"),
					HaveKeyWithValue("requests", "40")))
		})

		It("refuses the next request instead of handing back the spent budget", func() {
			p := period()
			setQuota(tenantID, p, quotaRow{maxCost: 10_000})
			addUsageCounter(tenantID, p, domains.QuotaUsage{Requests: 40, Tokens: 9_900, Cost: 9_900})

			e := resyncing(resyncInterval)
			_, err := e.Reserve(context.Background(), request(50))
			Expect(err).NotTo(HaveOccurred())

			overwriteCounter(p, domains.QuotaUsage{Requests: 1, Tokens: 100})

			Eventually(func() map[string]string { return counter(p) }).
				WithTimeout(10 * time.Second).
				Should(HaveKeyWithValue("tokens", "9900"))

			_, err = e.Reserve(context.Background(), request(300))
			Expect(err).To(MatchError(ports.ErrQuotaExceeded),
				"a stale counter handed the tenant a budget it had already spent")
		})

		It("does not lower a counter that is ahead of the snapshot", func() {
			p := period()
			setQuota(tenantID, p, quotaRow{maxCost: 1_000_000})
			addUsageCounter(tenantID, p, domains.QuotaUsage{Requests: 1, Tokens: 100, Cost: 100})

			e := resyncing(resyncInterval)
			_, err := e.Reserve(context.Background(), request(5_000))
			Expect(err).NotTo(HaveOccurred())

			// Live usage always runs ahead of the snapshot by up to a flush
			// interval. Resyncing must leave that alone, or every pass would
			// refund the traffic that had not been flushed yet.
			Consistently(func() map[string]string { return counter(p) }).
				WithTimeout(10 * resyncInterval).
				Should(HaveKeyWithValue("tokens", "5100"))
		})
	})

	Describe("when Redis is down", Ordered, func() {
		var down *tcredis.RedisContainer
		var downAddr string

		BeforeAll(func() {
			c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			var err error
			down, err = tcredis.Run(c, redisImage)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				tc, tcancel := ctx()
				defer tcancel()
				_ = down.Terminate(tc)
			})

			downAddr, err = down.Endpoint(c, "")
			Expect(err).NotTo(HaveOccurred())

			stop := 5 * time.Second
			Expect(down.Stop(c, &stop)).To(Succeed())
		})

		// Short on purpose: an outage has to cost a request about this
		// long, not a TCP timeout.
		const outageTimeout = 200 * time.Millisecond

		against := func(policy quota.OnUnavailable) *quota.Enforcer {
			client := redis.New(redis.Config{Addr: downAddr, Timeout: outageTimeout})
			e := newEnforcer(redisquota.New(client, outageTimeout), nanoPerToken{},
				quota.Config{OnUnavailable: policy})
			DeferCleanup(func() {
				c, cancel := ctx()
				defer cancel()
				_ = e.Close(c)
				_ = client.Close()
			})
			return e
		}

		It("fails open: serves, unenforced, and quickly", func() {
			setQuota(tenantID, period(), quotaRow{maxCost: 1})
			e := against(quota.FailOpen)

			for range 3 {
				start := time.Now()
				res, err := e.Reserve(context.Background(), request(10))
				Expect(err).NotTo(HaveOccurred())
				Expect(res.IsZero()).To(BeTrue(), "nothing was reserved, so nothing is reconciled")
				Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
			}

			unavailable, _ := e.Stats()
			Expect(unavailable).To(Equal(int64(3)))
		})

		It("fails closed: refuses as unavailable, never as over quota", func() {
			setQuota(tenantID, period(), quotaRow{maxCost: 1_000})
			e := against(quota.FailClosed)

			start := time.Now()
			_, err := e.Reserve(context.Background(), request(10))
			Expect(err).To(MatchError(ports.ErrQuotaUnavailable))
			Expect(errors.Is(err, ports.ErrQuotaExceeded)).To(BeFalse())
			Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
		})
	})
})

// --- fixtures ----------------------------------------------------------

// newEnforcer wires what bootstrap wires: limits read through the admin
// repository's readers, durable counters through their own.
func newEnforcer(live ports.QuotaCounters, pricing ports.PricingTable, cfg quota.Config) *quota.Enforcer {
	admin := repositories.NewAdminRepo(pool)
	return quota.New(
		admin.Tenants(),
		admin.Quotas(),
		repositories.NewUsageCounterRepo(pool),
		live,
		pricing,
		cfg,
		time.Now,
	)
}

type quotaRow struct {
	maxCost  int64
	onExceed string
}

// nanoPerToken prices every target at one nanodollar a token, so a cap in
// nanodollars reads as a token count and the arithmetic in these specs stays
// legible. Without a price at all a quota cannot be checked.
type nanoPerToken struct{}

func (nanoPerToken) RatesAt(time.Time, string) (domains.Rates, string, error) {
	r := domains.PerMillionTokens(1_000_000)
	return domains.Rates{Input: r, Output: r, CacheRead: r, CacheWrite: r}, "test", nil
}

// newQuotaTenant provisions a tenant of its own for one spec, so live
// counters and cached limits cannot leak between specs.
func newQuotaTenant() (domains.Tenant, uuid.UUID) {
	c, cancel := ctx()
	defer cancel()

	id := uuid.New()
	name := "quota-" + id.String()
	_, err := pool.Exec(c, `INSERT INTO tenants (id, external_id, name) VALUES ($1, $2, $2)`, id, name)
	Expect(err).NotTo(HaveOccurred())
	return domains.Tenant(name), id
}

// setQuota writes a quota row the way the admin API would. Only a
// period window's dates are used; for the others the kind is enough.
func setQuota(tenantID uuid.UUID, w domains.Window, q quotaRow) {
	c, cancel := ctx()
	defer cancel()

	var start, end *time.Time
	if w.Kind == domains.WindowPeriod {
		start, end = &w.Start, &w.End
	}
	action := q.onExceed
	if action == "" {
		action = "block"
	}

	_, err := pool.Exec(c, `
		INSERT INTO quotas (tenant_id, window_kind, period_start, period_end,
		                    max_cost_nanos, on_exceed)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		tenantID, string(w.Kind), start, end, q.maxCost, action)
	Expect(err).NotTo(HaveOccurred())
}

func addUsageCounter(tenantID uuid.UUID, w domains.Window, u domains.QuotaUsage) {
	c, cancel := ctx()
	defer cancel()

	Expect(repositories.NewUsageCounterRepo(pool).AddDeltas(c, []domains.WindowUsage{
		{TenantID: tenantID, Window: w, Usage: u},
	})).To(Succeed())
}

func usageCounter(tenantID uuid.UUID, w domains.Window) domains.QuotaUsage {
	c, cancel := ctx()
	defer cancel()

	u, err := repositories.NewUsageCounterRepo(pool).Get(c, tenantID, w)
	Expect(err).NotTo(HaveOccurred())
	return u
}
