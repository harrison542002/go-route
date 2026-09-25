package quota

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

// OnUnavailable is what happens to a request when its quotas cannot be checked.
type OnUnavailable string

const (
	// FailOpen serves the request unenforced. Availability wins; a Redis
	// outage costs quota accuracy for its duration, not traffic.
	FailOpen OnUnavailable = "open"

	// FailClosed rejects with 503. Budget wins; a Redis outage is an outage of
	// the gateway.
	FailClosed OnUnavailable = "closed"
)

const (
	// maxSeedRounds bounds how often one reservation will seed missing counters
	// and try again.
	maxSeedRounds = 3

	// storeTimeout bounds the Postgres reads on the request path: loading
	// limits and rehydrating a counter.
	storeTimeout = 2 * time.Second

	// reconcileTimeout bounds the correction after a request. It runs detached
	// from the request, so without a bound a hung Redis would pile up goroutines.
	reconcileTimeout = 2 * time.Second

	// resyncTimeout bounds one whole resync pass, so a slow Redis cannot leave
	// the loop wedged and stop every later pass.
	resyncTimeout = 30 * time.Second
)

type Config struct {
	OnUnavailable    OnUnavailable
	DefaultMaxOutput int
	LimitsTTL        time.Duration
	FlushInterval    time.Duration
	FlushBuffer      int
	ResyncInterval   time.Duration
}

var _ ports.QuotaEnforcer = (*Enforcer)(nil)

type Enforcer struct {
	tenants  ports.TenantReader
	quotas   ports.QuotaReader
	durable  ports.UsageCounterRepository
	counters ports.QuotaCounters
	pricing  ports.PricingTable
	cfg      Config
	now      func() time.Time

	flusher *Flusher

	mu     sync.Mutex
	limits map[domains.Tenant]tenantLimits

	wake       chan struct{}
	stop       chan struct{}
	resyncDone chan struct{}
	stopOnce   sync.Once

	unavailable atomic.Int64
	unpriced    atomic.Int64
	degraded    atomic.Bool
	resyncs     atomic.Int64
}

// tenantLimits is a tenant's quota rows with the id its counters are keyed by,
// and when they were read.
type tenantLimits struct {
	tenantID uuid.UUID
	quotas   []domains.Quota
	fetched  time.Time
}

func New(
	tenants ports.TenantReader,
	quotas ports.QuotaReader,
	durable ports.UsageCounterRepository,
	counters ports.QuotaCounters,
	pricing ports.PricingTable,
	cfg Config,
	now func() time.Time,
) *Enforcer {
	if now == nil {
		now = time.Now
	}
	if cfg.OnUnavailable == "" {
		cfg.OnUnavailable = FailOpen
	}
	if cfg.DefaultMaxOutput <= 0 {
		cfg.DefaultMaxOutput = 4096
	}
	if cfg.LimitsTTL <= 0 {
		cfg.LimitsTTL = 10 * time.Second
	}
	if cfg.ResyncInterval <= 0 {
		cfg.ResyncInterval = time.Minute
	}

	e := &Enforcer{
		tenants:    tenants,
		quotas:     quotas,
		durable:    durable,
		counters:   counters,
		pricing:    pricing,
		cfg:        cfg,
		now:        now,
		flusher:    NewFlusher(durable, cfg.FlushInterval, cfg.FlushBuffer),
		limits:     make(map[domains.Tenant]tenantLimits),
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
		resyncDone: make(chan struct{}),
	}
	go e.resyncLoop()
	return e
}

func (e *Enforcer) Reserve(ctx context.Context, req ports.QuotaRequest) (ports.Reservation, error) {
	tl, err := e.tenantLimits(ctx, req.Tenant)
	if err != nil {
		return e.failed(err)
	}

	limits := domains.ApplicableLimits(tl.quotas, req.At)
	if len(limits) == 0 {
		return ports.Reservation{}, nil
	}

	estimate := domains.EstimateTokens(req.Prompt, req.MaxOutputTokens, req.Choices, e.cfg.DefaultMaxOutput)
	cost, priced := e.priceEstimate(req, estimate)
	if !priced {
		return e.unpriceable(req)
	}

	amount := domains.QuotaUsage{
		Requests: 1,
		Tokens:   int64(estimate.Total()),
		Cost:     cost,
	}

	for range maxSeedRounds {
		got, err := e.counters.Reserve(ctx, tl.tenantID, limits, amount)
		if err != nil {
			return e.failed(err)
		}

		if len(got.Missing) > 0 {
			if err := e.rehydrate(ctx, tl.tenantID, got.Missing); err != nil {
				return e.failed(err)
			}
			continue
		}

		e.recovered()

		if got.Blocked != nil {
			return ports.Reservation{}, &ports.QuotaExceededError{Breach: *got.Blocked}
		}

		for _, b := range got.Soft {
			slog.Warn("soft quota exceeded; serving anyway",
				"tenant", req.Tenant, "window", b.Window.Kind,
				"used_nanos", int64(b.Used), "requested_nanos", int64(b.Requested),
				"limit_nanos", int64(b.Limit))
		}

		windows := make([]domains.Window, len(limits))
		for i, l := range limits {
			windows[i] = l.Window
		}
		return ports.Reservation{
			TenantID: tl.tenantID,
			At:       req.At,
			Windows:  windows,
			Reserved: amount,
		}, nil
	}

	return e.failed(errors.New("quota: counters kept disappearing while being seeded"))
}

func (e *Enforcer) rehydrate(ctx context.Context, tenantID uuid.UUID, windows []domains.Window) error {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()

	seeds := make([]domains.WindowUsage, 0, len(windows))
	for _, w := range windows {
		u, err := e.durable.Get(ctx, tenantID, w)
		if err != nil {
			return fmt.Errorf("quota: rehydrate %s window: %w", w.Kind, err)
		}
		seeds = append(seeds, domains.WindowUsage{TenantID: tenantID, Window: w, Usage: u})
	}

	return e.counters.Seed(ctx, seeds)
}

func (e *Enforcer) resyncLoop() {
	defer close(e.resyncDone)

	ticker := time.NewTicker(e.cfg.ResyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.resync()
		case <-e.wake:
			e.resync()
		}
	}
}

func (e *Enforcer) resync() {
	e.mu.Lock()
	tenants := make([]tenantLimits, 0, len(e.limits))
	for _, c := range e.limits {
		tenants = append(tenants, c)
	}
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), resyncTimeout)
	defer cancel()

	now := e.now()
	var failures int
	var lastErr error

	for _, tl := range tenants {
		limits := domains.ApplicableLimits(tl.quotas, now)
		if len(limits) == 0 {
			continue
		}

		windows := make([]domains.Window, len(limits))
		for i, l := range limits {
			windows[i] = l.Window
		}
		if err := e.rehydrate(ctx, tl.tenantID, windows); err != nil {
			failures++
			lastErr = err
		}
	}

	e.resyncs.Add(1)
	if lastErr != nil {
		slog.Warn("quota counter resync incomplete; stale counters may persist until the next pass",
			"tenants", len(tenants), "failed", failures, "err", lastErr)
	}
}

func (e *Enforcer) requestResync() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Enforcer) Reconcile(ctx context.Context, res ports.Reservation, outcome domains.Outcome) {
	if res.IsZero() {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconcileTimeout)
	defer cancel()

	actual := e.actualUsage(res, outcome)

	if delta := actual.Sub(res.Reserved); !delta.IsZero() {
		if err := e.counters.Adjust(ctx, res.TenantID, res.Windows, delta); err != nil {
			slog.Error("quota reconcile failed; counter keeps the reservation",
				"tenant_id", res.TenantID, "err", err)
		}
	}

	if actual.IsZero() {
		return
	}
	for _, w := range res.Windows {
		e.flusher.Add(domains.WindowUsage{TenantID: res.TenantID, Window: w, Usage: actual})
	}
}

func (e *Enforcer) actualUsage(res ports.Reservation, outcome domains.Outcome) domains.QuotaUsage {
	chosen := outcome.ChosenTarget()
	if outcome.Status == domains.StatusExhausted || chosen == "" {
		return domains.QuotaUsage{}
	}

	if outcome.Usage.Total() == 0 {
		return res.Reserved
	}

	actual := domains.QuotaUsage{Requests: 1, Tokens: int64(outcome.Usage.Total())}
	if e.pricing != nil {
		if rates, _, err := e.pricing.RatesAt(res.At, chosen); err == nil {
			actual.Cost = rates.Cost(outcome.Usage)
		}
	}
	return actual
}

// priceEstimate costs the estimate at the dearest target the ladder may pick.
// It is false when no target could be priced at all: a free target prices to
// zero and is still a price.
func (e *Enforcer) priceEstimate(req ports.QuotaRequest, estimate domains.TokenUsage) (domains.USD, bool) {
	if e.pricing == nil {
		return 0, false
	}

	var dearest domains.USD
	var priced bool
	for _, t := range req.Targets {
		rates, _, err := e.pricing.RatesAt(req.At, t)
		if err != nil {
			continue
		}
		priced = true
		if c := rates.Cost(estimate); c > dearest {
			dearest = c
		}
	}
	return dearest, priced
}

// unpriceable applies the outage policy to a request nobody can put a number
// on. Reserving it at zero would slip it under every cap and leave no trace,
// so an unpriceable request is treated as exactly what it is: a quota that
// cannot be checked, the same case as an unreachable counter store.
func (e *Enforcer) unpriceable(req ports.QuotaRequest) (ports.Reservation, error) {
	n := e.unpriced.Add(1)

	if n == 1 || n%1000 == 0 {
		slog.Error("QUOTAS UNENFORCEABLE: this request's targets have no price",
			"tenant", req.Tenant, "targets", req.Targets,
			"on_unavailable", e.cfg.OnUnavailable, "unpriced_total", n)
	}

	if e.cfg.OnUnavailable == FailClosed {
		return ports.Reservation{}, fmt.Errorf("%w: no price for targets %v", ports.ErrQuotaUnavailable, req.Targets)
	}
	return ports.Reservation{}, nil
}

// tenantLimits reads through a short-lived cache. Quotas change rarely and are
// read on every request, so a round trip per request would buy freshness nobody
// needs. A failed refresh serves the stale entry: old limits beat no limits.
func (e *Enforcer) tenantLimits(ctx context.Context, tenant domains.Tenant) (tenantLimits, error) {
	now := e.now()

	e.mu.Lock()
	cached, ok := e.limits[tenant]
	e.mu.Unlock()

	if ok && now.Sub(cached.fetched) < e.cfg.LimitsTTL {
		return cached, nil
	}

	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()

	fresh, err := e.loadLimits(ctx, tenant)
	if err != nil {
		if ok {
			slog.Warn("could not refresh quotas; enforcing the cached set",
				"tenant", tenant, "err", err)
			return cached, nil
		}
		return tenantLimits{}, err
	}
	fresh.fetched = now

	e.mu.Lock()
	e.limits[tenant] = fresh
	e.mu.Unlock()
	return fresh, nil
}

func (e *Enforcer) loadLimits(ctx context.Context, tenant domains.Tenant) (tenantLimits, error) {
	account, err := e.tenants.GetByExternalID(ctx, string(tenant))
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return tenantLimits{}, nil
		}
		return tenantLimits{}, fmt.Errorf("quota: resolve tenant %s: %w", tenant, err)
	}

	quotas, err := e.quotas.List(ctx, account.ID)
	if err != nil {
		return tenantLimits{}, fmt.Errorf("quota: list quotas %s: %w", tenant, err)
	}
	return tenantLimits{tenantID: account.ID, quotas: quotas}, nil
}

// failed applies the configured outage policy.
func (e *Enforcer) failed(err error) (ports.Reservation, error) {
	n := e.unavailable.Add(1)

	// Log the transition, then sparsely: during an outage this fires per
	// request, and a log flood makes the incident harder to read.
	if e.degraded.CompareAndSwap(false, true) || n%1000 == 0 {
		slog.Error("QUOTAS UNENFORCEABLE: counter store unavailable",
			"on_unavailable", e.cfg.OnUnavailable, "unavailable_total", n, "err", err)
	}

	if e.cfg.OnUnavailable == FailClosed {
		return ports.Reservation{}, fmt.Errorf("%w: %v", ports.ErrQuotaUnavailable, err)
	}
	return ports.Reservation{}, nil
}

// recovered notes that Redis is answering again and asks for a resync at once.
// Whatever made it unreachable may also have promoted a replica whose counters
// are behind, and waiting a whole interval to find out would be a whole
// interval of budget handed back.
func (e *Enforcer) recovered() {
	if e.degraded.CompareAndSwap(true, false) {
		slog.Info("quota enforcement recovered", "unavailable_total", e.unavailable.Load())
		e.requestResync()
	}
}

// Stats reports counters for metrics and tests.
func (e *Enforcer) Stats() (unavailable, deltasDropped int64) {
	return e.unavailable.Load(), e.flusher.Dropped()
}

// Resyncs is the number of resync passes run, periodic and on recovery alike.
func (e *Enforcer) Resyncs() int64 {
	return e.resyncs.Load()
}

// Unpriced is the number of requests that could not be costed, and so could
// not be held to any quota.
func (e *Enforcer) Unpriced() int64 {
	return e.unpriced.Load()
}

// Close stops the resync and flushes the durable deltas still buffered.
func (e *Enforcer) Close(ctx context.Context) error {
	e.stopOnce.Do(func() { close(e.stop) })

	select {
	case <-e.resyncDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	return e.flusher.Close(ctx)
}
