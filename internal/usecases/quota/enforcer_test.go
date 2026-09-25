package quota

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/ports/mocks"
)

var (
	testNow  = time.Date(2026, 9, 21, 12, 30, 15, 0, time.UTC)
	tenantID = uuid.MustParse("0192f7a0-0000-7000-8000-000000000001")
)

// stubPrices prices by target name.
type stubPrices map[string]domains.Rates

func (s stubPrices) RatesAt(_ time.Time, target string) (domains.Rates, string, error) {
	r, ok := s[target]
	if !ok {
		return domains.Rates{}, "", ports.ErrNoPricing
	}
	return r, "2026-09-01", nil
}

func perMillion(in, out float64) domains.Rates {
	return domains.Rates{
		Input:  domains.PerMillionTokens(domains.FromDollars(in)),
		Output: domains.PerMillionTokens(domains.FromDollars(out)),
	}
}

// freePrices prices the ladder at nothing. A quota is a spend cap, so a
// request with no price at all is never reserved; a free target is a price
// and costs zero, which leaves every other figure under test alone.
func freePrices() stubPrices {
	return stubPrices{"cheap": {}, "dear": {}}
}

type fixture struct {
	usage    *mocks.MockUsageCounterRepository
	counters *mocks.MockQuotaCounters
	enforcer *Enforcer
	now      time.Time
}

// admin wires the readers the enforcer borrows from the admin side, answering
// for one tenant named acme.
func admin(ctrl *gomock.Controller, quotas []domains.Quota) (ports.TenantReader, ports.QuotaReader) {
	tenants := mocks.NewMockTenantReader(ctrl)
	tenants.EXPECT().GetByExternalID(gomock.Any(), "acme").
		Return(domains.TenantAccount{ID: tenantID, ExternalID: "acme"}, nil).AnyTimes()

	rows := mocks.NewMockQuotaReader(ctrl)
	rows.EXPECT().List(gomock.Any(), tenantID).Return(quotas, nil).AnyTimes()

	return tenants, rows
}

func newFixture(t *testing.T, cfg Config, prices ports.PricingTable, quotas ...domains.Quota) *fixture {
	t.Helper()
	ctrl := gomock.NewController(t)

	f := &fixture{
		usage:    mocks.NewMockUsageCounterRepository(ctrl),
		counters: mocks.NewMockQuotaCounters(ctrl),
		now:      testNow,
	}

	tenants, rows := admin(ctrl, quotas)
	f.enforcer = New(tenants, rows, f.usage, f.counters, prices, cfg, func() time.Time { return f.now })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = f.enforcer.Close(ctx)
	})
	return f
}

func request() ports.QuotaRequest {
	return ports.QuotaRequest{
		Tenant:          "acme",
		At:              testNow,
		Prompt:          domains.PromptSize{TextBytes: 400, Messages: 1}, // 104 tokens
		MaxOutputTokens: 100,
		Targets:         []string{"cheap", "dear"},
	}
}

func minuteQuota() domains.Quota {
	return domains.Quota{WindowKind: domains.WindowMinute, MaxCost: domains.FromDollars(1)}
}

func TestReserve_TenantWithoutQuotasTouchesNoCounter(t *testing.T) {
	f := newFixture(t, Config{}, nil)
	// No EXPECT on counters: any call fails the test.

	res, err := f.enforcer.Reserve(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsZero() {
		t.Errorf("reservation = %+v, want none", res)
	}
}

func TestReserve_ReservesTheWorstCaseAgainstEveryWindow(t *testing.T) {
	day := domains.Quota{WindowKind: domains.WindowDay, MaxCost: domains.FromDollars(100)}
	f := newFixture(t, Config{}, freePrices(), minuteQuota(), day)

	var gotLimits []domains.WindowLimit
	var gotAmount domains.QuotaUsage
	f.counters.EXPECT().Reserve(gomock.Any(), tenantID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, l []domains.WindowLimit, a domains.QuotaUsage) (ports.CounterReservation, error) {
			gotLimits, gotAmount = l, a
			return ports.CounterReservation{}, nil
		})

	res, err := f.enforcer.Reserve(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}

	want := domains.QuotaUsage{Requests: 1, Tokens: 104 + 100}
	if gotAmount != want || res.Reserved != want {
		t.Errorf("amount = %+v, reserved = %+v, want %+v", gotAmount, res.Reserved, want)
	}
	if len(gotLimits) != 2 || len(res.Windows) != 2 {
		t.Fatalf("limits = %d, windows = %d, want 2 each", len(gotLimits), len(res.Windows))
	}
	if res.Windows[0].Kind != domains.WindowMinute || res.Windows[1].Kind != domains.WindowDay {
		t.Errorf("windows = %+v", res.Windows)
	}
	if res.TenantID != tenantID || !res.At.Equal(testNow) {
		t.Errorf("tenant/at = %v/%v", res.TenantID, res.At)
	}
}

func TestReserve_DefaultCeilingFillsAnUnsetMaxTokens(t *testing.T) {
	f := newFixture(t, Config{DefaultMaxOutput: 777}, freePrices(), minuteQuota())

	f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(),
		domains.QuotaUsage{Requests: 1, Tokens: 104 + 777}).Return(ports.CounterReservation{}, nil)

	req := request()
	req.MaxOutputTokens = 0
	if _, err := f.enforcer.Reserve(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

// Any target on the ladder might serve, so the estimate is priced at the
// dearest one that has a price.
func TestReserve_PricesTheEstimateAtTheDearestTarget(t *testing.T) {
	prices := stubPrices{"cheap": perMillion(1, 1), "dear": perMillion(10, 100)}
	f := newFixture(t, Config{}, prices, minuteQuota())

	wantCost := perMillion(10, 100).Cost(domains.TokenUsage{Input: 104, Output: 100})
	f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(),
		domains.QuotaUsage{Requests: 1, Tokens: 204, Cost: wantCost}).Return(ports.CounterReservation{}, nil)

	if _, err := f.enforcer.Reserve(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
}

// A quota is money, so a request nobody can put a number on is a quota that
// cannot be checked, and takes the same path as an unreachable counter store.
func TestReserve_UnpriceableRequestFollowsTheOutagePolicy(t *testing.T) {
	tests := []struct {
		name    string
		policy  OnUnavailable
		prices  ports.PricingTable
		wantErr error
	}{
		{name: "no price table at all, open", policy: FailOpen, prices: nil},
		{name: "no price table at all, closed", policy: FailClosed, prices: nil,
			wantErr: ports.ErrQuotaUnavailable},
		{name: "no rates for any target on the ladder", policy: FailClosed,
			prices: stubPrices{"elsewhere": perMillion(1, 1)}, wantErr: ports.ErrQuotaUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Config{OnUnavailable: tt.policy}, tt.prices, minuteQuota())
			// No EXPECT on counters: nothing may be reserved.

			res, err := f.enforcer.Reserve(context.Background(), request())
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if !res.IsZero() {
				t.Error("an unenforced request must carry no reservation to reconcile")
			}
			if n := f.enforcer.Unpriced(); n != 1 {
				t.Errorf("Unpriced = %d, want 1; an unpriceable request has to be visible", n)
			}
		})
	}
}

// A free target is a price. Its zero cost is enforceable against a cap, and
// must not be mistaken for a missing one.
func TestReserve_AFreeTargetIsAPrice(t *testing.T) {
	f := newFixture(t, Config{}, stubPrices{"cheap": {}}, minuteQuota())

	f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(),
		domains.QuotaUsage{Requests: 1, Tokens: 204, Cost: 0}).Return(ports.CounterReservation{}, nil)

	if _, err := f.enforcer.Reserve(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if n := f.enforcer.Unpriced(); n != 0 {
		t.Errorf("Unpriced = %d, want 0", n)
	}
}

func TestReserve_BlockedIsAQuotaExceededError(t *testing.T) {
	f := newFixture(t, Config{}, freePrices(), minuteQuota())

	w, _ := domains.ClockWindow(domains.WindowMinute, testNow)
	b := domains.QuotaBreach{
		Window: w, Used: domains.FromDollars(0.99), Requested: domains.FromDollars(0.02),
		Limit: domains.FromDollars(1),
	}
	f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ports.CounterReservation{Blocked: &b}, nil)

	_, err := f.enforcer.Reserve(context.Background(), request())

	var exceeded *ports.QuotaExceededError
	if !errors.As(err, &exceeded) || !errors.Is(err, ports.ErrQuotaExceeded) {
		t.Fatalf("err = %v, want a QuotaExceededError", err)
	}
	if exceeded.Breach != b {
		t.Errorf("breach = %+v, want %+v", exceeded.Breach, b)
	}
}

// A counter Redis does not have may be one it lost. It is rebuilt from
// Postgres before anything is reserved against it, never assumed zero.
func TestReserve_RehydratesMissingWindowsThenRetries(t *testing.T) {
	f := newFixture(t, Config{}, freePrices(), minuteQuota())
	w, _ := domains.ClockWindow(domains.WindowMinute, testNow)
	snapshot := domains.QuotaUsage{Requests: 7, Tokens: 1234, Cost: 99}

	gomock.InOrder(
		f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(ports.CounterReservation{Missing: []domains.Window{w}}, nil),
		f.usage.EXPECT().Get(gomock.Any(), tenantID, w).Return(snapshot, nil),
		f.counters.EXPECT().Seed(gomock.Any(),
			[]domains.WindowUsage{{TenantID: tenantID, Window: w, Usage: snapshot}}).Return(nil),
		f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(ports.CounterReservation{}, nil),
	)

	res, err := f.enforcer.Reserve(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if res.IsZero() {
		t.Error("nothing reserved after rehydration")
	}
}

func TestReserve_CountersThatNeverStickAreAnOutage(t *testing.T) {
	f := newFixture(t, Config{OnUnavailable: FailClosed}, freePrices(), minuteQuota())
	w, _ := domains.ClockWindow(domains.WindowMinute, testNow)

	f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ports.CounterReservation{Missing: []domains.Window{w}}, nil).Times(maxSeedRounds)
	f.usage.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(domains.QuotaUsage{}, nil).Times(maxSeedRounds)
	f.counters.EXPECT().Seed(gomock.Any(), gomock.Any()).Return(nil).Times(maxSeedRounds)

	if _, err := f.enforcer.Reserve(context.Background(), request()); !errors.Is(err, ports.ErrQuotaUnavailable) {
		t.Fatalf("err = %v, want ErrQuotaUnavailable", err)
	}
}

func TestReserve_OutagePolicy(t *testing.T) {
	down := errors.New("dial tcp 10.0.0.5:6379: connect: connection refused")

	tests := []struct {
		name    string
		policy  OnUnavailable
		wantErr error
	}{
		{name: "open serves unenforced", policy: FailOpen, wantErr: nil},
		{name: "closed refuses", policy: FailClosed, wantErr: ports.ErrQuotaUnavailable},
		{name: "unset means open", policy: "", wantErr: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Config{OnUnavailable: tt.policy}, freePrices(), minuteQuota())
			f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
				Return(ports.CounterReservation{}, down).Times(2)

			for range 2 {
				res, err := f.enforcer.Reserve(context.Background(), request())
				if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if !res.IsZero() {
					t.Error("an unenforced request must carry no reservation to reconcile")
				}
			}
			if n, _ := f.enforcer.Stats(); n != 2 {
				t.Errorf("unavailable = %d, want 2", n)
			}
		})
	}
}

func TestReserve_RehydrationFailureIsAnOutage(t *testing.T) {
	f := newFixture(t, Config{OnUnavailable: FailClosed}, freePrices(), minuteQuota())
	w, _ := domains.ClockWindow(domains.WindowMinute, testNow)

	f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ports.CounterReservation{Missing: []domains.Window{w}}, nil)
	f.usage.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(domains.QuotaUsage{}, errors.New("postgres down"))

	if _, err := f.enforcer.Reserve(context.Background(), request()); !errors.Is(err, ports.ErrQuotaUnavailable) {
		t.Fatalf("err = %v; seeding zero instead would hand the tenant a fresh budget", err)
	}
}

func TestReserve_CachesLimitsAndServesStaleOnRefreshFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	counters := mocks.NewMockQuotaCounters(ctrl)
	now := testNow

	tenants := mocks.NewMockTenantReader(ctrl)
	tenants.EXPECT().GetByExternalID(gomock.Any(), "acme").
		Return(domains.TenantAccount{ID: tenantID, ExternalID: "acme"}, nil).AnyTimes()

	rows := mocks.NewMockQuotaReader(ctrl)
	gomock.InOrder(
		rows.EXPECT().List(gomock.Any(), tenantID).Return([]domains.Quota{minuteQuota()}, nil),
		rows.EXPECT().List(gomock.Any(), tenantID).Return(nil, errors.New("postgres down")),
	)

	e := New(
		tenants, rows, mocks.NewMockUsageCounterRepository(ctrl),
		counters, freePrices(), Config{LimitsTTL: 10 * time.Second}, func() time.Time { return now })
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ports.CounterReservation{}, nil).Times(3)

	for range 2 { // one load, one cache hit
		if res, err := e.Reserve(context.Background(), request()); err != nil || res.IsZero() {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
	}

	now = now.Add(11 * time.Second) // expired; the refresh fails
	if res, err := e.Reserve(context.Background(), request()); err != nil || res.IsZero() {
		t.Fatalf("res = %+v, err = %v; old limits beat no limits", res, err)
	}
}

// --- reconcile -------------------------------------------------------

func reservation() ports.Reservation {
	w, _ := domains.ClockWindow(domains.WindowMinute, testNow)
	d, _ := domains.ClockWindow(domains.WindowDay, testNow)
	return ports.Reservation{
		TenantID: tenantID,
		At:       testNow,
		Windows:  []domains.Window{w, d},
		Reserved: domains.QuotaUsage{Requests: 1, Tokens: 1000, Cost: 5000},
	}
}

func served(usage domains.TokenUsage) domains.Outcome {
	return domains.Outcome{
		Status:   domains.StatusOK,
		Attempts: []domains.Attempt{{Target: "dear"}},
		Usage:    usage,
	}
}

// deltas captures what reaches Postgres.
type deltas struct {
	mu  sync.Mutex
	got []domains.WindowUsage
}

func (d *deltas) record(_ context.Context, batch []domains.WindowUsage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, batch...)
	return nil
}

func (d *deltas) all() []domains.WindowUsage {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]domains.WindowUsage(nil), d.got...)
}

func TestReconcile(t *testing.T) {
	prices := stubPrices{"dear": perMillion(10, 100)}
	usage := domains.TokenUsage{Input: 100, Output: 50, CacheRead: 10}
	actualCost := prices["dear"].Cost(usage)

	tests := []struct {
		name       string
		outcome    domains.Outcome
		wantAdjust *domains.QuotaUsage // nil: no Adjust call
		wantDelta  *domains.QuotaUsage // nil: nothing flushed
	}{
		{
			name:       "served: counters move to what was actually used",
			outcome:    served(usage),
			wantAdjust: &domains.QuotaUsage{Requests: 0, Tokens: 160 - 1000, Cost: actualCost - 5000},
			wantDelta:  &domains.QuotaUsage{Requests: 1, Tokens: 160, Cost: actualCost},
		},
		{
			name:       "exhausted: nothing was served, so everything is released",
			outcome:    domains.Outcome{Status: domains.StatusExhausted, Attempts: []domains.Attempt{{Target: "dear", Failure: &domains.AttemptFailure{}}}},
			wantAdjust: &domains.QuotaUsage{Requests: -1, Tokens: -1000, Cost: -5000},
		},
		{
			// A zero outcome is what a panic mid-dispatch leaves.
			name:       "no outcome at all releases too",
			outcome:    domains.Outcome{},
			wantAdjust: &domains.QuotaUsage{Requests: -1, Tokens: -1000, Cost: -5000},
		},
		{
			name: "served without usage keeps the worst case",
			outcome: domains.Outcome{
				Status: domains.StatusClientDisconnect, Attempts: []domains.Attempt{{Target: "dear"}},
			},
			wantDelta: &domains.QuotaUsage{Requests: 1, Tokens: 1000, Cost: 5000},
		},
		{
			name: "truncated with partial usage reconciles to it",
			outcome: domains.Outcome{
				Status: domains.StatusTruncated, Attempts: []domains.Attempt{{Target: "dear"}},
				Usage: domains.TokenUsage{Input: 100, Output: 3},
			},
			wantAdjust: &domains.QuotaUsage{Tokens: 103 - 1000,
				Cost: prices["dear"].Cost(domains.TokenUsage{Input: 100, Output: 3}) - 5000},
			wantDelta: &domains.QuotaUsage{Requests: 1, Tokens: 103,
				Cost: prices["dear"].Cost(domains.TokenUsage{Input: 100, Output: 3})},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Config{}, prices)
			sink := &deltas{}
			f.usage.EXPECT().AddDeltas(gomock.Any(), gomock.Any()).DoAndReturn(sink.record).AnyTimes()

			res := reservation()
			if tt.wantAdjust != nil {
				f.counters.EXPECT().Adjust(gomock.Any(), tenantID, res.Windows, *tt.wantAdjust).Return(nil)
			}

			f.enforcer.Reconcile(context.Background(), res, tt.outcome)
			if err := f.enforcer.Close(context.Background()); err != nil {
				t.Fatal(err)
			}

			got := sink.all()
			if tt.wantDelta == nil {
				if len(got) != 0 {
					t.Errorf("flushed %+v, want nothing", got)
				}
				return
			}
			if len(got) != len(res.Windows) {
				t.Fatalf("flushed %d deltas, want one per window", len(got))
			}
			for _, d := range got {
				if d.Usage != *tt.wantDelta || d.TenantID != tenantID {
					t.Errorf("delta = %+v, want %+v", d, *tt.wantDelta)
				}
			}
		})
	}
}

// The request context is cancelled by the time most requests reconcile.
// The correction has to go through regardless.
func TestReconcile_SurvivesACancelledRequestContext(t *testing.T) {
	f := newFixture(t, Config{}, nil)
	f.usage.EXPECT().AddDeltas(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f.counters.EXPECT().Adjust(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ uuid.UUID, _ []domains.Window, _ domains.QuotaUsage) error {
			if err := c.Err(); err != nil {
				t.Errorf("adjust ran with a dead context: %v", err)
			}
			if _, ok := c.Deadline(); !ok {
				t.Error("adjust has no deadline; a hung Redis would pile up goroutines")
			}
			return nil
		})

	f.enforcer.Reconcile(ctx, reservation(), served(domains.TokenUsage{Input: 1}))
}

func TestReconcile_NothingReservedIsANoOp(t *testing.T) {
	f := newFixture(t, Config{}, nil)
	f.enforcer.Reconcile(context.Background(), ports.Reservation{}, served(domains.TokenUsage{Input: 5}))
}

func TestReconcile_AdjustFailureStillFlushesTheTruth(t *testing.T) {
	f := newFixture(t, Config{}, nil)
	sink := &deltas{}
	f.usage.EXPECT().AddDeltas(gomock.Any(), gomock.Any()).DoAndReturn(sink.record).AnyTimes()
	f.counters.EXPECT().Adjust(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(errors.New("redis down"))

	f.enforcer.Reconcile(context.Background(), reservation(), served(domains.TokenUsage{Input: 40}))
	_ = f.enforcer.Close(context.Background())

	if got := sink.all(); len(got) != 2 || got[0].Usage.Tokens != 40 {
		t.Errorf("flushed %+v, want the actual usage per window", got)
	}
}

// A failover that drops acknowledged writes fails nothing: the counter is
// still there, only smaller. Recovery is the one signal the request path does
// get, so it must not be spent waiting out an interval.
func TestResync_RecoveryFromUnavailabilityTriggersOne(t *testing.T) {
	f := newFixture(t, Config{ResyncInterval: time.Hour}, freePrices(), minuteQuota())
	w, _ := domains.ClockWindow(domains.WindowMinute, testNow)
	snapshot := domains.QuotaUsage{Requests: 12, Tokens: 3400}

	gomock.InOrder(
		f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(ports.CounterReservation{}, errors.New("redis down")),
		f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(ports.CounterReservation{}, nil),
	)
	f.usage.EXPECT().Get(gomock.Any(), tenantID, w).Return(snapshot, nil).AnyTimes()

	seeded := make(chan []domains.WindowUsage, 1)
	f.counters.EXPECT().Seed(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, s []domains.WindowUsage) error {
			select {
			case seeded <- s:
			default:
			}
			return nil
		}).AnyTimes()

	for range 2 {
		if _, err := f.enforcer.Reserve(context.Background(), request()); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case got := <-seeded:
		if len(got) != 1 || got[0].Window != w || got[0].Usage != snapshot {
			t.Errorf("seeded %+v, want the snapshot for the live minute", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not trigger a resync")
	}
}

func TestResync_PeriodicPassCoversCachedTenantsAndStopsOnClose(t *testing.T) {
	f := newFixture(t, Config{ResyncInterval: 5 * time.Millisecond}, freePrices(), minuteQuota())
	w, _ := domains.ClockWindow(domains.WindowMinute, testNow)
	snapshot := domains.QuotaUsage{Requests: 2, Tokens: 500}

	f.counters.EXPECT().Reserve(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(ports.CounterReservation{}, nil).AnyTimes()
	f.usage.EXPECT().Get(gomock.Any(), tenantID, w).Return(snapshot, nil).AnyTimes()

	var seeds atomic.Int64
	f.counters.EXPECT().Seed(gomock.Any(),
		[]domains.WindowUsage{{TenantID: tenantID, Window: w, Usage: snapshot}}).DoAndReturn(
		func(context.Context, []domains.WindowUsage) error {
			seeds.Add(1)
			return nil
		}).AnyTimes()

	// Nothing is worth resyncing until a tenant has been served.
	if _, err := f.enforcer.Reserve(context.Background(), request()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for seeds.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if seeds.Load() < 2 {
		t.Fatalf("seeded %d times, want the loop to keep raising cached tenants", seeds.Load())
	}

	if err := f.enforcer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopped := seeds.Load()
	time.Sleep(100 * time.Millisecond)
	if n := seeds.Load(); n != stopped {
		t.Errorf("%d resyncs after Close; the loop outlived shutdown", n-stopped)
	}
	if f.enforcer.Resyncs() < 2 {
		t.Errorf("Resyncs = %d", f.enforcer.Resyncs())
	}
}
