package main

import (
	"strings"
	"testing"
	"time"

	"github.com/harrison542002/go-route/internal/bootstrap"
)

func TestResolveDSN(t *testing.T) {
	const envDSN = "postgres://env/goroute"

	tests := []struct {
		name string
		flag string
		env  string
		want string
	}{
		{"flag alone", "postgres://flag/goroute", "", "postgres://flag/goroute"},
		{"env alone", "", envDSN, envDSN},
		{"flag over env", "postgres://flag/goroute", envDSN, "postgres://flag/goroute"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withDSN(t, tt.flag, tt.env)
			got, err := resolveDSN()
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("resolveDSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveDSNWithNeither(t *testing.T) {
	withDSN(t, "", "")

	_, err := resolveDSN()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"--dsn", "DATABASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestServeOptions(t *testing.T) {
	tests := []struct {
		name           string
		flag           string
		env            string
		listen         string
		requestTimeout time.Duration
		secret         string
		accessTTL      time.Duration
		wantErr        []string
	}{
		{
			name: "defaults", env: "postgres://env/goroute",
			listen: bootstrap.DefaultAdminListen, requestTimeout: bootstrap.DefaultAdminRequestTimeout,
		},
		{
			name: "no dsn anywhere", listen: bootstrap.DefaultAdminListen,
			requestTimeout: bootstrap.DefaultAdminRequestTimeout,
			wantErr:        []string{"--dsn", "DATABASE_URL"},
		},
		{
			name: "empty listen", env: "postgres://env/goroute",
			requestTimeout: bootstrap.DefaultAdminRequestTimeout,
			wantErr:        []string{"--listen is required"},
		},
		{
			name: "zero timeout", env: "postgres://env/goroute", listen: bootstrap.DefaultAdminListen,
			wantErr: []string{"--request-timeout must be above 0s"},
		},
		{
			name: "negative timeout", env: "postgres://env/goroute", listen: bootstrap.DefaultAdminListen,
			requestTimeout: -time.Second,
			wantErr:        []string{"--request-timeout must be above 0s", "got -1s"},
		},
		{
			name: "beyond the cap", env: "postgres://env/goroute", listen: bootstrap.DefaultAdminListen,
			requestTimeout: 10 * time.Minute,
			wantErr:        []string{"at most 2m0s"},
		},
		{
			name: "no signing secret", env: "postgres://env/goroute", listen: bootstrap.DefaultAdminListen,
			requestTimeout: bootstrap.DefaultAdminRequestTimeout, secret: "-",
			wantErr: []string{"--jwt-secret", "GO_ROUTE_JWT_SECRET"},
		},
		{
			name: "short signing secret", env: "postgres://env/goroute", listen: bootstrap.DefaultAdminListen,
			requestTimeout: bootstrap.DefaultAdminRequestTimeout, secret: "too-short",
			wantErr: []string{"--jwt-secret must be at least 32 bytes"},
		},
		{
			name: "access ttl below the floor", env: "postgres://env/goroute", listen: bootstrap.DefaultAdminListen,
			requestTimeout: bootstrap.DefaultAdminRequestTimeout, accessTTL: time.Second,
			wantErr: []string{"--access-token-ttl must be between"},
		},
		{
			name: "access ttl beyond the cap", env: "postgres://env/goroute", listen: bootstrap.DefaultAdminListen,
			requestTimeout: bootstrap.DefaultAdminRequestTimeout, accessTTL: 48 * time.Hour,
			wantErr: []string{"--access-token-ttl must be between"},
		},
		{
			name: "everything at once", listen: "", requestTimeout: 0, secret: "-",
			wantErr: []string{"--dsn", "--jwt-secret", "--listen is required", "--request-timeout"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withDSN(t, tt.flag, tt.env)

			secret, accessTTL := tt.secret, tt.accessTTL
			switch secret {
			case "":
				secret = testSigningSecret
			case "-":
				secret = ""
			}
			if accessTTL == 0 {
				accessTTL = bootstrap.DefaultAccessTokenTTL
			}

			t.Setenv("GO_ROUTE_JWT_SECRET", "")
			opts, err := serveOptions(tt.listen, tt.requestTimeout, secret, accessTTL)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatal(err)
				}
				if opts.DSN != tt.env || opts.Listen != tt.listen || opts.RequestTimeout != tt.requestTimeout {
					t.Errorf("opts = %+v", opts)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error, got %+v", opts)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestServeFlagDefaults(t *testing.T) {
	cmd := serveCmd()

	if got := cmd.Flag("listen").DefValue; got != "127.0.0.1:4001" {
		t.Errorf("--listen default = %q", got)
	}
	if got := cmd.Flag("request-timeout").DefValue; got != "10s" {
		t.Errorf("--request-timeout default = %q", got)
	}
	if got := cmd.Flag("access-token-ttl").DefValue; got != "15m0s" {
		t.Errorf("--access-token-ttl default = %q", got)
	}
	if got := cmd.Flag("jwt-secret").DefValue; got != "" {
		t.Errorf("--jwt-secret default = %q", got)
	}
}

// testSigningSecret is long enough to pass the floor and is a test fixture,
// never a default: serve refuses to start without one of its own.
const testSigningSecret = "0123456789abcdef0123456789abcdef"

func TestJWTSecretFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("GO_ROUTE_JWT_SECRET", testSigningSecret)

	if got := resolveJWTSecret(""); got != testSigningSecret {
		t.Errorf("resolveJWTSecret = %q", got)
	}
	if got := resolveJWTSecret("from-the-flag"); got != "from-the-flag" {
		t.Errorf("the flag must win, got %q", got)
	}
}

// withDSN sets both sources for one test. dsnFlag is package state, so it is
// restored rather than left behind for the next test.
func withDSN(t *testing.T, flag, env string) {
	t.Helper()

	previous := dsnFlag
	dsnFlag = flag
	t.Cleanup(func() { dsnFlag = previous })

	t.Setenv("DATABASE_URL", env)
}
