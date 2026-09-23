package domains

import (
	"errors"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestQuotaValidate(t *testing.T) {
	start := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)

	tests := []struct {
		name  string
		quota Quota
		ok    bool
	}{
		{"clock window with one limit", Quota{WindowKind: WindowDay, MaxRequests: ptr(int64(100)), OnExceed: QuotaBlock}, true},
		{"zero is a limit, not unset", Quota{WindowKind: WindowMinute, MaxTokens: ptr(int64(0)), OnExceed: QuotaAllow}, true},
		{"period with dates", Quota{WindowKind: WindowPeriod, PeriodStart: &start, PeriodEnd: &end, MaxCost: ptr(USD(5)), OnExceed: QuotaBlock}, true},

		{"unknown kind", Quota{WindowKind: "week", MaxRequests: ptr(int64(1)), OnExceed: QuotaBlock}, false},
		{"period without dates", Quota{WindowKind: WindowPeriod, MaxRequests: ptr(int64(1)), OnExceed: QuotaBlock}, false},
		{"period with only a start", Quota{WindowKind: WindowPeriod, PeriodStart: &start, MaxRequests: ptr(int64(1)), OnExceed: QuotaBlock}, false},
		{"period ending before it starts", Quota{WindowKind: WindowPeriod, PeriodStart: &end, PeriodEnd: &start, MaxRequests: ptr(int64(1)), OnExceed: QuotaBlock}, false},
		{"period of zero length", Quota{WindowKind: WindowPeriod, PeriodStart: &start, PeriodEnd: &start, MaxRequests: ptr(int64(1)), OnExceed: QuotaBlock}, false},
		{"clock window with dates", Quota{WindowKind: WindowMonth, PeriodStart: &start, PeriodEnd: &end, MaxRequests: ptr(int64(1)), OnExceed: QuotaBlock}, false},
		{"limits nothing", Quota{WindowKind: WindowHour, OnExceed: QuotaBlock}, false},
		{"negative requests", Quota{WindowKind: WindowHour, MaxRequests: ptr(int64(-1)), OnExceed: QuotaBlock}, false},
		{"negative tokens", Quota{WindowKind: WindowHour, MaxTokens: ptr(int64(-1)), OnExceed: QuotaBlock}, false},
		{"negative cost", Quota{WindowKind: WindowHour, MaxCost: ptr(USD(-1)), OnExceed: QuotaBlock}, false},
		{"unknown action", Quota{WindowKind: WindowHour, MaxRequests: ptr(int64(1)), OnExceed: "warn"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.quota.Validate()
			if tt.ok && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidQuota) {
				t.Fatalf("Validate() = %v, want ErrInvalidQuota", err)
			}
		})
	}
}

func TestQuotaSameLimits(t *testing.T) {
	start := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	sameInstant := start.In(time.FixedZone("UTC+7", 7*3600))

	base := Quota{WindowKind: WindowPeriod, PeriodStart: &start, PeriodEnd: ptr(start.Add(time.Hour)),
		MaxRequests: ptr(int64(10)), OnExceed: QuotaBlock, UpdatedAt: start}

	other := base
	other.PeriodStart = &sameInstant
	other.MaxRequests = ptr(int64(10))
	other.UpdatedAt = start.Add(time.Hour)
	if !base.SameLimits(other) {
		t.Error("same instant in another zone, and a different updated_at, should not count as a change")
	}

	other.MaxTokens = ptr(int64(0))
	if base.SameLimits(other) {
		t.Error("an added limit of zero is a change: nil is unlimited, zero is nothing")
	}
}

func TestWindowKindRankFollowsTheEnum(t *testing.T) {
	for i, k := range WindowKinds {
		if k.Rank() != i {
			t.Errorf("%s.Rank() = %d, want %d", k, k.Rank(), i)
		}
	}
	if WindowKind("week").Valid() {
		t.Error("week is not a window kind")
	}
}

func TestAdminCredentialExpired(t *testing.T) {
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		expires *time.Time
		now     time.Time
		want    bool
	}{
		{"never expires", nil, noon, false},
		{"never expires, far future", nil, noon.AddDate(10, 0, 0), false},
		{"an hour to go", &noon, noon.Add(-time.Hour), false},
		{"a nanosecond to go", &noon, noon.Add(-time.Nanosecond), false},
		{"exactly at expiry", &noon, noon, true},
		{"a nanosecond past", &noon, noon.Add(time.Nanosecond), true},
		{"long past", &noon, noon.AddDate(0, 1, 0), true},
		{"same instant elsewhere", &noon, noon.In(time.FixedZone("UTC+9", 9*3600)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := AdminCredential{ExpiresAt: tt.expires}
			if got := c.Expired(tt.now); got != tt.want {
				t.Errorf("Expired(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

func TestAdminCredentialExpiredIsNotRevoked(t *testing.T) {
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	expired := AdminCredential{ExpiresAt: &at}
	if !expired.Expired(at.Add(time.Hour)) || expired.Revoked() {
		t.Error("an expired credential reads as revoked")
	}

	revoked := AdminCredential{RevokedAt: &at}
	if !revoked.Revoked() || revoked.Expired(at.Add(time.Hour)) {
		t.Error("a revoked credential reads as expired")
	}
}

func TestRoleAllows(t *testing.T) {
	tests := []struct {
		role      Role
		read      bool
		write     bool
		validRole bool
	}{
		{RoleAdmin, true, true, true},
		{RoleReadonly, true, false, true},

		{"", false, false, false},
		{"superuser", false, false, false},
		{"Admin", false, false, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			if got := tt.role.Allows(PermissionRead); got != tt.read {
				t.Errorf("Allows(read) = %v, want %v", got, tt.read)
			}
			if got := tt.role.Allows(PermissionWrite); got != tt.write {
				t.Errorf("Allows(write) = %v, want %v", got, tt.write)
			}
			if got := tt.role.Valid(); got != tt.validRole {
				t.Errorf("Valid() = %v, want %v", got, tt.validRole)
			}
			if tt.role != RoleAdmin && tt.role.Allows("delete-everything") {
				t.Error("an unknown permission was allowed")
			}
		})
	}
}
