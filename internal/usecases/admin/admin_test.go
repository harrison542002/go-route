package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/ports/mocks"
)

var testNow = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// txRepos are the per-aggregate mocks an AdminTx hands out.
type txRepos struct {
	tenants *mocks.MockTenantRepository
	keys    *mocks.MockAPIKeyRepository
	quotas  *mocks.MockQuotaRepository
	audit   *mocks.MockAuditRepository
}

func newTxRepos(ctrl *gomock.Controller) (txRepos, *mocks.MockAdminTx) {
	r := txRepos{
		tenants: mocks.NewMockTenantRepository(ctrl),
		keys:    mocks.NewMockAPIKeyRepository(ctrl),
		quotas:  mocks.NewMockQuotaRepository(ctrl),
		audit:   mocks.NewMockAuditRepository(ctrl),
	}
	tx := mocks.NewMockAdminTx(ctrl)
	tx.EXPECT().Tenants().Return(r.tenants).AnyTimes()
	tx.EXPECT().APIKeys().Return(r.keys).AnyTimes()
	tx.EXPECT().Quotas().Return(r.quotas).AnyTimes()
	tx.EXPECT().Audit().Return(r.audit).AnyTimes()
	return r, tx
}

// fixture wires a Service to a mocked repository whose InTx runs its callback
// against the same aggregate mocks its readers hand out, so a test states an
// expectation once whether the call is made in a transaction or not.
type fixture struct {
	svc  *Service
	repo *mocks.MockAdminRepository
	txRepos
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctrl := gomock.NewController(t)

	repos, tx := newTxRepos(ctrl)
	repo := mocks.NewMockAdminRepository(ctrl)
	repo.EXPECT().Tenants().Return(repos.tenants).AnyTimes()
	repo.EXPECT().APIKeys().Return(repos.keys).AnyTimes()
	repo.EXPECT().Quotas().Return(repos.quotas).AnyTimes()
	repo.EXPECT().InTx(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, fn func(context.Context, ports.AdminTx) error) error { return fn(ctx, tx) },
	).AnyTimes()

	svc := New(repo, func() time.Time { return testNow }, bytes.NewReader(make([]byte, 1024)))
	return fixture{svc: svc, repo: repo, txRepos: repos}
}

func ptr[T any](v T) *T { return &v }

var actor = ports.Write{Actor: "admin:test"}

func acme() domains.TenantAccount {
	return domains.TenantAccount{
		ID:         uuid.MustParse("0192a000-0000-7000-8000-000000000001"),
		ExternalID: "acme",
		Name:       "Acme",
		Metadata:   []byte(`{"plan": "pro"}`),
	}
}

// expectTenant makes ref resolve to t by external id and lock.
func (f fixture) expectTenant(t domains.TenantAccount) {
	f.tenants.EXPECT().GetByExternalID(gomock.Any(), string(t.ExternalID)).Return(t, nil).AnyTimes()
	f.tenants.EXPECT().Lock(gomock.Any(), t.ID).Return(t, nil).AnyTimes()
}

// --- tenants -----------------------------------------------------------

func TestCreateTenantRejectsBadInputBeforeTouchingTheStore(t *testing.T) {
	tests := []struct {
		name string
		in   ports.NewTenant
	}{
		{"blank external id", ports.NewTenant{ExternalID: "  ", Name: "Acme"}},
		{"padded external id", ports.NewTenant{ExternalID: " acme", Name: "Acme"}},
		{"overlong external id", ports.NewTenant{ExternalID: strings.Repeat("a", domains.MaxTenantLen+1), Name: "Acme"}},
		{"blank name", ports.NewTenant{ExternalID: "acme", Name: ""}},
		{"array metadata", ports.NewTenant{ExternalID: "acme", Name: "Acme", Metadata: []byte(`[1]`)}},
		{"null metadata", ports.NewTenant{ExternalID: "acme", Name: "Acme", Metadata: []byte(`null`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t) // the mocked writer fails the test on any call

			_, err := f.svc.CreateTenant(context.Background(), actor, tt.in)
			if !errors.Is(err, ports.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestCreateTenantAuditsWhatItCreated(t *testing.T) {
	f := newFixture(t)

	var created domains.TenantAccount
	f.tenants.EXPECT().Create(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, t domains.TenantAccount) (domains.TenantAccount, bool, error) {
			created = t
			return t, true, nil
		})
	f.audit.EXPECT().Write(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, e ports.AuditEntry) error {
			if e.Actor != "admin:test" || e.Action != "tenant.create" {
				t.Errorf("audit = %s by %s", e.Action, e.Actor)
			}
			if e.TenantID == nil || *e.TenantID != created.ID || !e.At.Equal(testNow) {
				t.Errorf("audit row not attributed to the new tenant at now: %+v", e)
			}
			if !strings.Contains(e.Detail, `"external_id":"acme"`) {
				t.Errorf("detail = %s", e.Detail)
			}
			return nil
		})

	res, err := f.svc.CreateTenant(context.Background(), actor, ports.NewTenant{ExternalID: "acme", Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || string(res.Tenant.Metadata) != "{}" {
		t.Errorf("got %+v, want created with {} metadata", res)
	}
	if created.ID.Version() != 7 {
		t.Errorf("tenant id is UUIDv%d, want v7", created.ID.Version())
	}
}

func TestCreateTenantReturnsTheExistingTenantForTheSameBody(t *testing.T) {
	f := newFixture(t)
	existing := acme()
	existing.Metadata = []byte(`{"plan": "pro", "seats": 1000}`)
	f.tenants.EXPECT().Create(gomock.Any(), gomock.Any()).Return(existing, false, nil)

	res, err := f.svc.CreateTenant(context.Background(), actor, ports.NewTenant{
		ExternalID: "acme", Name: "Acme", Metadata: []byte(`{"seats":1e3,"plan":"pro"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || res.Tenant.ID != existing.ID {
		t.Errorf("got %+v, want the existing tenant, not created", res)
	}
}

func TestCreateTenantConflictsOnADifferentBody(t *testing.T) {
	f := newFixture(t)
	f.tenants.EXPECT().Create(gomock.Any(), gomock.Any()).Return(acme(), false, nil)

	_, err := f.svc.CreateTenant(context.Background(), actor, ports.NewTenant{ExternalID: "acme", Name: "Someone else"})
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestDisableTenantIsANoOpWhenAlreadyDisabled(t *testing.T) {
	f := newFixture(t)
	disabled := acme()
	disabled.DisabledAt = ptr(testNow)
	f.expectTenant(disabled)

	got, err := f.svc.DisableTenant(context.Background(), actor, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled() {
		t.Error("want the disabled tenant back")
	}
}

func TestUpdateTenantSkipsAnUnchangedPatch(t *testing.T) {
	f := newFixture(t)
	f.expectTenant(acme())

	_, err := f.svc.UpdateTenant(context.Background(), actor, "acme", ports.TenantPatch{
		Name: ptr("Acme"), Metadata: []byte(`{"plan":"pro"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpdateTenantNeedsSomethingToUpdate(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.UpdateTenant(context.Background(), actor, "acme", ports.TenantPatch{})
	if !errors.Is(err, ports.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestTenantRefFallsBackFromUUIDToExternalID(t *testing.T) {
	f := newFixture(t)
	ref := "0192a000-0000-7000-8000-00000000abcd"
	want := acme()
	want.ExternalID = domains.Tenant(ref)

	f.tenants.EXPECT().Get(gomock.Any(), uuid.MustParse(ref)).Return(domains.TenantAccount{}, ports.ErrNotFound)
	f.tenants.EXPECT().GetByExternalID(gomock.Any(), ref).Return(want, nil)

	got, err := f.svc.GetTenant(context.Background(), ref)
	if err != nil || got.ID != want.ID {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestUnknownTenantIsNotFound(t *testing.T) {
	f := newFixture(t)
	f.tenants.EXPECT().GetByExternalID(gomock.Any(), "nobody").Return(domains.TenantAccount{}, ports.ErrNotFound)

	_, err := f.svc.GetTenant(context.Background(), "nobody")
	if !errors.Is(err, ports.ErrNotFound) || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("err = %v, want ErrNotFound naming the tenant", err)
	}
}

func TestListTenantsBoundsTheLimit(t *testing.T) {
	f := newFixture(t)
	f.tenants.EXPECT().List(gomock.Any(), ports.TenantFilter{Limit: ports.DefaultTenantLimit}).Return(nil, nil)

	if _, err := f.svc.ListTenants(context.Background(), ports.TenantFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ListTenants(context.Background(), ports.TenantFilter{Limit: ports.MaxTenantLimit + 1}); !errors.Is(err, ports.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// --- keys --------------------------------------------------------------

func TestCreateAPIKeyStoresOnlyTheHash(t *testing.T) {
	f := newFixture(t)
	f.expectTenant(acme())

	var storedHash []byte
	var stored domains.APIKey
	f.keys.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, k domains.APIKey, hash []byte) (domains.APIKey, error) {
			stored, storedHash = k, hash
			return k, nil
		})
	f.audit.EXPECT().Write(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, e ports.AuditEntry) error {
		if e.Action != "key.create" || e.KeyID == nil || strings.Contains(e.Detail, KeyPrefix+"aaaaaaaaaa") {
			t.Errorf("audit %+v must name the key without its secret", e)
		}
		return nil
	})

	issued, err := f.svc.CreateAPIKey(context.Background(), actor, "acme", ports.NewAPIKey{ModelAllowlist: []string{"chat"}})
	if err != nil {
		t.Fatal(err)
	}

	wantSecret := KeyPrefix + strings.Repeat("a", 52)
	if issued.Secret != wantSecret {
		t.Errorf("secret = %q, want %q", issued.Secret, wantSecret)
	}
	sum := sha256.Sum256([]byte(issued.Secret))
	if !bytes.Equal(storedHash, sum[:]) {
		t.Error("the stored hash is not the SHA-256 of the secret")
	}
	if stored.Prefix != KeyPrefix+"aaaaaa" || stored.TenantID != acme().ID {
		t.Errorf("stored %+v", stored)
	}
	if len(stored.ModelAllowlist) != 1 || stored.ModelAllowlist[0] != "chat" {
		t.Errorf("allowlist = %v", stored.ModelAllowlist)
	}
}

func TestRepeatedCreateAPIKeyMintsASecondKey(t *testing.T) {
	f := newFixture(t)
	f.svc = New(f.repo, func() time.Time { return testNow }, rand.Reader)
	f.expectTenant(acme())

	var minted []domains.APIKey
	f.keys.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, k domains.APIKey, _ []byte) (domains.APIKey, error) {
			minted = append(minted, k)
			return k, nil
		}).Times(2)
	f.audit.EXPECT().Write(gomock.Any(), gomock.Any()).Return(nil).Times(2)

	first, err := f.svc.CreateAPIKey(context.Background(), actor, "acme", ports.NewAPIKey{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.CreateAPIKey(context.Background(), actor, "acme", ports.NewAPIKey{})
	if err != nil {
		t.Fatal(err)
	}

	if first.Key.ID == second.Key.ID || first.Secret == second.Secret {
		t.Errorf("the second call reused the first key: %+v", first.Key)
	}
	if len(minted) != 2 {
		t.Errorf("stored %d keys, want 2", len(minted))
	}
}

func TestCreateAPIKeyRefusesADisabledTenant(t *testing.T) {
	f := newFixture(t)
	disabled := acme()
	disabled.DisabledAt = ptr(testNow)
	f.expectTenant(disabled)

	_, err := f.svc.CreateAPIKey(context.Background(), actor, "acme", ports.NewAPIKey{})
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestAllowlistValidation(t *testing.T) {
	for name, list := range map[string][]string{
		"blank":     {"chat", " "},
		"duplicate": {"chat", "chat"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.svc.CreateAPIKey(context.Background(), actor, "acme", ports.NewAPIKey{ModelAllowlist: list})
			if !errors.Is(err, ports.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func key(revoked bool, allowlist []string) domains.APIKey {
	k := domains.APIKey{
		ID:             uuid.MustParse("0192a000-0000-7000-8000-0000000000aa"),
		TenantID:       acme().ID,
		Prefix:         "gr_live_abcdef",
		ModelAllowlist: allowlist,
	}
	if revoked {
		k.RevokedAt = ptr(testNow)
	}
	return k
}

func TestRevokeAPIKeyIsANoOpWhenAlreadyRevoked(t *testing.T) {
	f := newFixture(t)
	k := key(true, nil)
	f.keys.EXPECT().Get(gomock.Any(), k.ID).Return(k, nil).Times(2)
	f.tenants.EXPECT().Lock(gomock.Any(), k.TenantID).Return(acme(), nil)

	got, err := f.svc.RevokeAPIKey(context.Background(), actor, k.ID)
	if err != nil || !got.Revoked() {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestRevokeUnknownAPIKeyIsNotFound(t *testing.T) {
	f := newFixture(t)
	id := uuid.New()
	f.keys.EXPECT().Get(gomock.Any(), id).Return(domains.APIKey{}, ports.ErrNotFound)

	_, err := f.svc.RevokeAPIKey(context.Background(), actor, id)
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateAPIKeyTellsNilFromEmpty(t *testing.T) {
	f := newFixture(t)
	k := key(false, nil)
	f.keys.EXPECT().Get(gomock.Any(), k.ID).Return(k, nil).Times(2)
	f.tenants.EXPECT().Lock(gomock.Any(), k.TenantID).Return(acme(), nil)
	f.keys.EXPECT().UpdateAllowlist(gomock.Any(), k.ID, []string{}).Return(key(false, []string{}), nil)
	f.audit.EXPECT().Write(gomock.Any(), gomock.Any()).Return(nil)

	got, err := f.svc.UpdateAPIKey(context.Background(), actor, k.ID, ports.APIKeyPatch{ModelAllowlist: []string{}})
	if err != nil || got.ModelAllowlist == nil {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestUpdateRevokedAPIKeyConflicts(t *testing.T) {
	f := newFixture(t)
	k := key(true, nil)
	f.keys.EXPECT().Get(gomock.Any(), k.ID).Return(k, nil).Times(2)
	f.tenants.EXPECT().Lock(gomock.Any(), k.TenantID).Return(acme(), nil)

	_, err := f.svc.UpdateAPIKey(context.Background(), actor, k.ID, ports.APIKeyPatch{ModelAllowlist: []string{"chat"}})
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

// --- quotas ------------------------------------------------------------

func TestReplaceQuotasValidatesTheWholeSetFirst(t *testing.T) {
	start := testNow
	tests := map[string][]domains.Quota{
		"duplicate kind": {
			{WindowKind: domains.WindowDay, MaxRequests: ptr(int64(1))},
			{WindowKind: domains.WindowDay, MaxTokens: ptr(int64(1))},
		},
		"period without end": {
			{WindowKind: domains.WindowPeriod, PeriodStart: &start, MaxRequests: ptr(int64(1))},
		},
		"no limit": {{WindowKind: domains.WindowMinute}},
		"negative": {{WindowKind: domains.WindowMinute, MaxCost: ptr(domains.USD(-5))}},
	}
	for name, set := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.svc.ReplaceQuotas(context.Background(), actor, "acme", set)
			if !errors.Is(err, ports.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestReplaceQuotasDeletesThenWritesInEnumOrder(t *testing.T) {
	f := newFixture(t)
	f.expectTenant(acme())
	id := acme().ID

	var order []domains.WindowKind
	gomock.InOrder(
		f.quotas.EXPECT().List(gomock.Any(), id).Return([]domains.Quota{
			{WindowKind: domains.WindowHour, MaxRequests: ptr(int64(5)), OnExceed: domains.QuotaBlock},
		}, nil),
		f.quotas.EXPECT().DeleteAll(gomock.Any(), id).Return(nil),
		f.quotas.EXPECT().Upsert(gomock.Any(), id, gomock.Any()).DoAndReturn(
			func(_ context.Context, _ uuid.UUID, q domains.Quota) (domains.Quota, error) {
				order = append(order, q.WindowKind)
				return q, nil
			}).Times(2),
		f.audit.EXPECT().Write(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, e ports.AuditEntry) error {
			if e.Action != "quota.replace" || !strings.Contains(e.Detail, `"on_exceed":"block"`) {
				t.Errorf("audit %+v", e)
			}
			return nil
		}),
	)

	got, err := f.svc.ReplaceQuotas(context.Background(), actor, "acme", []domains.Quota{
		{WindowKind: domains.WindowMonth, MaxCost: ptr(domains.USD(1_000_000_000))},
		{WindowKind: domains.WindowMinute, MaxRequests: ptr(int64(60)), OnExceed: domains.QuotaAllow},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != domains.WindowMinute || order[1] != domains.WindowMonth {
		t.Errorf("written in order %v, want minute then month", order)
	}
	if got[1].OnExceed != domains.QuotaBlock {
		t.Errorf("on_exceed defaulted to %q, want block", got[1].OnExceed)
	}
}

func TestReplaceQuotasWithTheSameSetDoesNothing(t *testing.T) {
	f := newFixture(t)
	f.expectTenant(acme())

	current := []domains.Quota{
		{WindowKind: domains.WindowMinute, MaxRequests: ptr(int64(60)), OnExceed: domains.QuotaBlock, UpdatedAt: testNow},
	}
	f.quotas.EXPECT().List(gomock.Any(), acme().ID).Return(current, nil)

	got, err := f.svc.ReplaceQuotas(context.Background(), actor, "acme", []domains.Quota{
		{WindowKind: domains.WindowMinute, MaxRequests: ptr(int64(60))},
	})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestReplaceQuotasWithAnEmptySetRemovesEveryLimit(t *testing.T) {
	f := newFixture(t)
	f.expectTenant(acme())
	f.quotas.EXPECT().List(gomock.Any(), acme().ID).Return([]domains.Quota{
		{WindowKind: domains.WindowDay, MaxRequests: ptr(int64(1)), OnExceed: domains.QuotaBlock},
	}, nil)
	f.quotas.EXPECT().DeleteAll(gomock.Any(), acme().ID).Return(nil)
	f.audit.EXPECT().Write(gomock.Any(), gomock.Any()).Return(nil)

	got, err := f.svc.ReplaceQuotas(context.Background(), actor, "acme", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestDeleteMissingQuotaSucceedsWithoutAudit(t *testing.T) {
	f := newFixture(t)
	f.expectTenant(acme())
	f.quotas.EXPECT().Delete(gomock.Any(), acme().ID, domains.WindowDay).Return(false, nil)

	if err := f.svc.DeleteQuota(context.Background(), actor, "acme", domains.WindowDay); err != nil {
		t.Fatal(err)
	}
}

func TestPutQuotaSkipsAnUnchangedLimit(t *testing.T) {
	f := newFixture(t)
	f.expectTenant(acme())
	f.quotas.EXPECT().Get(gomock.Any(), acme().ID, domains.WindowDay).Return(
		domains.Quota{WindowKind: domains.WindowDay, MaxTokens: ptr(int64(9)), OnExceed: domains.QuotaBlock}, nil)

	_, err := f.svc.PutQuota(context.Background(), actor, "acme",
		domains.Quota{WindowKind: domains.WindowDay, MaxTokens: ptr(int64(9))})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGetQuotaRejectsAnUnknownWindow(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.GetQuota(context.Background(), "acme", "fortnight")
	if !errors.Is(err, ports.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestSameJSON(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{`{"a":1,"b":[1,2]}`, `{"b": [1, 2], "a": 1}`, true},
		{`{"n":1e3}`, `{"n":1000}`, true},
		{`{"n":12345678901234567890}`, `{"n":12345678901234567891}`, false},
		{`{"a":[1,2]}`, `{"a":[2,1]}`, false},
		{`{"a":null}`, `{}`, false},
	}
	for _, tt := range tests {
		if got := sameJSON([]byte(tt.a), []byte(tt.b)); got != tt.want {
			t.Errorf("sameJSON(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// --- context -------------------------------------------------------------

type (
	callerMarker struct{}
	txMarker     struct{}
)

func TestMutationsUseTheTransactionsContext(t *testing.T) {
	t0 := acme()
	disabled := acme()
	disabled.DisabledAt = ptr(testNow)
	k0 := key(false, nil)
	day := domains.Quota{WindowKind: domains.WindowDay, MaxTokens: ptr(int64(9)), OnExceed: domains.QuotaBlock}

	tests := []struct {
		name   string
		expect func(r txRepos, inTx gomock.Matcher)
		run    func(ctx context.Context, s *Service, w ports.Write) error
	}{
		{
			name: "create tenant",
			expect: func(r txRepos, inTx gomock.Matcher) {
				r.tenants.EXPECT().Create(inTx, gomock.Any()).Return(t0, true, nil)
			},
			run: func(ctx context.Context, s *Service, w ports.Write) error {
				_, err := s.CreateTenant(ctx, w, ports.NewTenant{ExternalID: "acme", Name: "Acme"})
				return err
			},
		},
		{
			name: "update tenant",
			expect: func(r txRepos, inTx gomock.Matcher) {
				r.tenants.EXPECT().GetByExternalID(inTx, "acme").Return(t0, nil)
				r.tenants.EXPECT().Lock(inTx, t0.ID).Return(t0, nil)
				r.tenants.EXPECT().Update(inTx, t0.ID, gomock.Any()).Return(t0, nil)
			},
			run: func(ctx context.Context, s *Service, w ports.Write) error {
				_, err := s.UpdateTenant(ctx, w, "acme", ports.TenantPatch{Name: ptr("Acme Ltd")})
				return err
			},
		},
		{
			name: "disable tenant",
			expect: func(r txRepos, inTx gomock.Matcher) {
				r.tenants.EXPECT().GetByExternalID(inTx, "acme").Return(t0, nil)
				r.tenants.EXPECT().Lock(inTx, t0.ID).Return(t0, nil)
				r.tenants.EXPECT().Disable(inTx, t0.ID).Return(disabled, nil)
			},
			run: func(ctx context.Context, s *Service, w ports.Write) error {
				_, err := s.DisableTenant(ctx, w, "acme")
				return err
			},
		},
		{
			name: "enable tenant",
			expect: func(r txRepos, inTx gomock.Matcher) {
				r.tenants.EXPECT().GetByExternalID(inTx, "acme").Return(disabled, nil)
				r.tenants.EXPECT().Lock(inTx, t0.ID).Return(disabled, nil)
				r.tenants.EXPECT().Enable(inTx, t0.ID).Return(t0, nil)
			},
			run: func(ctx context.Context, s *Service, w ports.Write) error {
				_, err := s.EnableTenant(ctx, w, "acme")
				return err
			},
		},
		{
			name: "create api key",
			expect: func(r txRepos, inTx gomock.Matcher) {
				r.tenants.EXPECT().GetByExternalID(inTx, "acme").Return(t0, nil)
				r.tenants.EXPECT().Lock(inTx, t0.ID).Return(t0, nil)
				r.keys.EXPECT().Create(inTx, gomock.Any(), gomock.Any()).Return(k0, nil)
			},
			run: func(ctx context.Context, s *Service, w ports.Write) error {
				_, err := s.CreateAPIKey(ctx, w, "acme", ports.NewAPIKey{})
				return err
			},
		},
		{
			name: "update api key",
			expect: func(r txRepos, inTx gomock.Matcher) {
				r.keys.EXPECT().Get(inTx, k0.ID).Return(k0, nil).Times(2)
				r.tenants.EXPECT().Lock(inTx, t0.ID).Return(t0, nil)
				r.keys.EXPECT().UpdateAllowlist(inTx, k0.ID, []string{"chat"}).Return(k0, nil)
			},
			run: func(ctx context.Context, s *Service, w ports.Write) error {
				_, err := s.UpdateAPIKey(ctx, w, k0.ID, ports.APIKeyPatch{ModelAllowlist: []string{"chat"}})
				return err
			},
		},
		{
			name: "revoke api key",
			expect: func(r txRepos, inTx gomock.Matcher) {
				r.keys.EXPECT().Get(inTx, k0.ID).Return(k0, nil).Times(2)
				r.tenants.EXPECT().Lock(inTx, t0.ID).Return(t0, nil)
				r.keys.EXPECT().Revoke(inTx, k0.ID).Return(key(true, nil), nil)
			},
			run: func(ctx context.Context, s *Service, w ports.Write) error {
				_, err := s.RevokeAPIKey(ctx, w, k0.ID)
				return err
			},
		},
		{
			name: "replace quotas",
			expect: func(r txRepos, inTx gomock.Matcher) {
				r.tenants.EXPECT().GetByExternalID(inTx, "acme").Return(t0, nil)
				r.tenants.EXPECT().Lock(inTx, t0.ID).Return(t0, nil)
				r.quotas.EXPECT().List(inTx, t0.ID).Return(nil, nil)
				r.quotas.EXPECT().DeleteAll(inTx, t0.ID).Return(nil)
				r.quotas.EXPECT().Upsert(inTx, t0.ID, gomock.Any()).Return(day, nil)
			},
			run: func(ctx context.Context, s *Service, w ports.Write) error {
				_, err := s.ReplaceQuotas(ctx, w, "acme", []domains.Quota{day})
				return err
			},
		},
		{
			name: "put quota",
			expect: func(r txRepos, inTx gomock.Matcher) {
				r.tenants.EXPECT().GetByExternalID(inTx, "acme").Return(t0, nil)
				r.tenants.EXPECT().Lock(inTx, t0.ID).Return(t0, nil)
				r.quotas.EXPECT().Get(inTx, t0.ID, domains.WindowDay).Return(domains.Quota{}, ports.ErrNotFound)
				r.quotas.EXPECT().Upsert(inTx, t0.ID, gomock.Any()).Return(day, nil)
			},
			run: func(ctx context.Context, s *Service, w ports.Write) error {
				_, err := s.PutQuota(ctx, w, "acme", day)
				return err
			},
		},
		{
			name: "delete quota",
			expect: func(r txRepos, inTx gomock.Matcher) {
				r.tenants.EXPECT().GetByExternalID(inTx, "acme").Return(t0, nil)
				r.tenants.EXPECT().Lock(inTx, t0.ID).Return(t0, nil)
				r.quotas.EXPECT().Delete(inTx, t0.ID, domains.WindowDay).Return(true, nil)
			},
			run: func(ctx context.Context, s *Service, w ports.Write) error {
				return s.DeleteQuota(ctx, w, "acme", domains.WindowDay)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			repos, tx := newTxRepos(ctrl)
			repo := mocks.NewMockAdminRepository(ctrl)
			svc := New(repo, func() time.Time { return testNow }, nil)

			fromCaller := gomock.Cond(func(ctx context.Context) bool { return ctx.Value(callerMarker{}) == true })
			repo.EXPECT().InTx(fromCaller, gomock.Any()).DoAndReturn(
				func(ctx context.Context, fn func(context.Context, ports.AdminTx) error) error {
					return fn(context.WithValue(ctx, txMarker{}, true), tx)
				})

			inTx := gomock.Cond(func(ctx context.Context) bool { return ctx.Value(txMarker{}) == true })
			tt.expect(repos, inTx)
			repos.audit.EXPECT().Write(inTx, gomock.Any()).Return(nil)

			ctx := context.WithValue(context.Background(), callerMarker{}, true)
			if err := tt.run(ctx, svc, actor); err != nil {
				t.Fatal(err)
			}
		})
	}
}
