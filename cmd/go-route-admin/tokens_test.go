package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

var credentialID = uuid.MustParse("0192a000-0000-7000-8000-0000000000c1")

func TestPrintIssuedCredential(t *testing.T) {
	var out bytes.Buffer
	secret := "gr_admin_" + strings.Repeat("a", 52)

	printIssuedCredential(&out, ports.IssuedAdminCredential{
		Credential: domains.AdminCredential{ID: credentialID, Name: "billing-sync", Prefix: secret[:15]},
		Secret:     secret,
	})

	got := out.String()
	for _, want := range []string{
		secret,
		"billing-sync",
		credentialID.String(),
		"shown once and cannot be shown again",
		"Authorization: Bearer",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not mention %q:\n%s", want, got)
		}
	}
	if strings.Count(got, secret) != 1 {
		t.Errorf("the secret appears %d times", strings.Count(got, secret))
	}
}

func TestPrintCredentials(t *testing.T) {
	used := time.Date(2026, 9, 22, 14, 7, 0, 0, time.UTC)
	revoked := time.Date(2026, 9, 22, 15, 0, 0, 0, time.UTC)

	var out bytes.Buffer
	printCredentials(&out, []domains.AdminCredential{
		{
			Name: "billing-sync", Prefix: "gr_admin_4k2xq7", Role: domains.RoleAdmin,
			CreatedAt: time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC), LastUsedAt: &used,
		},
		{
			Name: "retired", Prefix: "gr_admin_zzzzzz", Role: domains.RoleAdmin,
			CreatedAt: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC), RevokedAt: &revoked,
		},
	})

	got := out.String()
	for _, want := range []string{
		"NAME", "billing-sync", "gr_admin_4k2xq7…", "2026-09-22", "2026-09-22 14:07", "retired",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not mention %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "—") != 4 {
		t.Errorf("unset timestamps are not blank:\n%s", got)
	}
}

func TestPrintCredentialsWhenThereAreNone(t *testing.T) {
	var out bytes.Buffer
	printCredentials(&out, nil)

	if !strings.Contains(out.String(), "create-token") {
		t.Errorf("an empty listing must say how to mint one: %s", out.String())
	}
}

func TestRevokedLine(t *testing.T) {
	line := revokedLine(domains.AdminCredential{Name: "billing-sync", Prefix: "gr_admin_4k2xq7"})
	if !strings.Contains(line, "revoked billing-sync") || !strings.Contains(line, "—") {
		t.Errorf("line = %q", line)
	}
}

func TestOSUserActor(t *testing.T) {
	for in, want := range map[string]string{
		"alice":             "cli:alice",
		"CORP\\alice":       "cli:CORP_alice",
		"alice@example.com": "cli:alice@example.com",
		"a b":               "cli:a_b",
		"":                  "cli",
	} {
		if got := osUserActor(in); got != want {
			t.Errorf("osUserActor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCredentialSpec(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		role      string
		expiresIn string
		wantRole  domains.Role
		wantExp   *time.Time
		wantErr   string
	}{
		{name: "the default role", role: "admin", wantRole: domains.RoleAdmin},
		{name: "readonly", role: "readonly", wantRole: domains.RoleReadonly},

		{name: "unknown role", role: "superuser", wantErr: "--role"},
		{name: "cased role", role: "Admin", wantErr: "--role"},
		{name: "empty role", role: "", wantErr: "--role"},

		{
			name: "ninety days", role: "readonly", expiresIn: "90d",
			wantRole: domains.RoleReadonly, wantExp: ptrTime(now.Add(90 * 24 * time.Hour)),
		},
		{name: "thirty days", role: "admin", expiresIn: "30d", wantRole: domains.RoleAdmin, wantExp: ptrTime(now.Add(30 * 24 * time.Hour))},
		{name: "hours", role: "admin", expiresIn: "24h", wantRole: domains.RoleAdmin, wantExp: ptrTime(now.Add(24 * time.Hour))},
		{name: "minutes", role: "admin", expiresIn: "90m", wantRole: domains.RoleAdmin, wantExp: ptrTime(now.Add(90 * time.Minute))},

		{name: "no expiry", role: "admin", expiresIn: "", wantRole: domains.RoleAdmin},

		{name: "garbage expiry", role: "admin", expiresIn: "ninety days", wantErr: "--expires-in"},
		{name: "bare number", role: "admin", expiresIn: "90", wantErr: "--expires-in"},
		{name: "a date", role: "admin", expiresIn: "2026-12-01", wantErr: "--expires-in"},
		{name: "negative", role: "admin", expiresIn: "-5m", wantErr: "--expires-in"},
		{name: "zero", role: "admin", expiresIn: "0s", wantErr: "--expires-in"},
		{name: "zero days", role: "admin", expiresIn: "0d", wantErr: "--expires-in"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := credentialSpec("billing-sync", tt.role, tt.expiresIn, now)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one mentioning %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if spec.Name != "billing-sync" || spec.Role != tt.wantRole {
				t.Errorf("spec = %+v", spec)
			}
			switch {
			case tt.wantExp == nil && spec.ExpiresAt != nil:
				t.Errorf("expires at %v, want never", spec.ExpiresAt)
			case tt.wantExp != nil && (spec.ExpiresAt == nil || !spec.ExpiresAt.Equal(*tt.wantExp)):
				t.Errorf("expires at %v, want %v", spec.ExpiresAt, tt.wantExp)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestCreateTokenDefaultsToAdmin(t *testing.T) {
	flag := createTokenCmd().Flags().Lookup("role")
	if flag == nil || flag.DefValue != string(domains.RoleAdmin) {
		t.Fatalf("--role default = %v", flag)
	}
	if expires := createTokenCmd().Flags().Lookup("expires-in"); expires == nil || expires.DefValue != "" {
		t.Fatalf("--expires-in default = %v", expires)
	}
}

func TestPrintIssuedCredentialShowsRoleAndExpiry(t *testing.T) {
	expires := time.Date(2026, 12, 22, 9, 0, 0, 0, time.UTC)

	var out bytes.Buffer
	printIssuedCredential(&out, ports.IssuedAdminCredential{
		Credential: domains.AdminCredential{
			ID: credentialID, Name: "dashboard", Prefix: "gr_admin_zzzzzz",
			Role: domains.RoleReadonly, ExpiresAt: &expires,
		},
		Secret: "gr_admin_secret",
	})
	for _, want := range []string{"role readonly", "403", "expires 2026-12-22 09:00 UTC"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not mention %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	printIssuedCredential(&out, ports.IssuedAdminCredential{
		Credential: domains.AdminCredential{ID: credentialID, Name: "billing-sync", Role: domains.RoleAdmin},
		Secret:     "gr_admin_secret",
	})
	for _, want := range []string{"role admin", "expires never"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not mention %q:\n%s", want, out.String())
		}
	}
}

func TestPrintCredentialsShowsRoleAndDistinguishesExpiredFromRevoked(t *testing.T) {
	past := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	created := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	var out bytes.Buffer
	printCredentials(&out, []domains.AdminCredential{
		{Name: "billing-sync", Prefix: "gr_admin_4k2xq7", Role: domains.RoleAdmin, CreatedAt: created},
		{Name: "ran-out", Prefix: "gr_admin_aaaaaa", Role: domains.RoleReadonly, CreatedAt: created, ExpiresAt: &past},
		{Name: "still-good", Prefix: "gr_admin_bbbbbb", Role: domains.RoleReadonly, CreatedAt: created, ExpiresAt: &future},
		{Name: "taken-away", Prefix: "gr_admin_cccccc", Role: domains.RoleAdmin, CreatedAt: created, RevokedAt: &past},
	})

	got := out.String()
	for _, want := range []string{
		"ROLE", "EXPIRES", "REVOKED",
		"admin", "readonly",
		"2026-09-01 00:00 (expired)",
		"2099-01-01 00:00",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not mention %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "(expired)") != 1 {
		t.Errorf("only the credential that ran out is expired:\n%s", got)
	}

	revoked := lineWith(t, got, "taken-away")
	if strings.Contains(revoked, "(expired)") || !strings.Contains(revoked, "2026-09-01 00:00") {
		t.Errorf("revoked line = %q", revoked)
	}
}

func lineWith(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, name) {
			return line
		}
	}
	t.Fatalf("no line mentions %q in:\n%s", name, out)
	return ""
}
