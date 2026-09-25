package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
sink: {dsn: "postgres://localhost/goroute"}
providers:
  openai: {type: oaicompat, base_url: "https://api.openai.com/v1"}
targets:
  openai/m: {provider: openai, model: m}
models:
  chat: [openai/m]
`

func load(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestAdminBlockIsNoLongerAConfigKey(t *testing.T) {
	_, err := load(t, minimal+`admin: {listen: "127.0.0.1:4001"}`+"\n")
	if err == nil || !strings.Contains(err.Error(), "field admin not found") {
		t.Fatalf("err = %v, want it to reject the admin key", err)
	}
}

// Leaving redis out is a supported deployment: quotas go unenforced, so the
// block must not become required by having defaults applied to it.
func TestQuotaDefaults(t *testing.T) {
	cfg, err := load(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.Enabled() {
		t.Error("redis reported as configured with no addr")
	}
	if cfg.Redis.Timeout != 100*time.Millisecond || cfg.Redis.OnUnavailable != "open" {
		t.Errorf("redis = %+v", cfg.Redis)
	}
	want := Quota{
		DefaultMaxOutputTokens: 4096,
		LimitsTTL:              10 * time.Second,
		FlushInterval:          time.Second,
		FlushBuffer:            8192,
		ResyncInterval:         time.Minute,
	}
	if cfg.Quota != want {
		t.Errorf("quota = %+v, want %+v", cfg.Quota, want)
	}
}

func TestQuotaValidation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "on_unavailable must be one of the two policies",
			yaml: `redis: {addr: "localhost:6379", on_unavailable: maybe}
`,
			want: "on_unavailable must be open or closed",
		},
		{
			name: "a negative timeout would disable the bound it exists to impose",
			yaml: `redis: {addr: "localhost:6379", timeout: -1s}
`,
			want: "redis: timeout must not be negative",
		},
		{
			name: "a negative flush buffer",
			yaml: `quota: {flush_buffer: -1}
`,
			want: "quota: flush_buffer must not be negative",
		},
		{
			name: "a negative resync interval",
			yaml: `quota: {resync_interval: -1s}
`,
			want: "quota: resync_interval must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, minimal+tt.yaml)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestQuotaBlockIsRead(t *testing.T) {
	cfg, err := load(t, minimal+`
redis: {addr: "localhost:6379", password: s3cret, db: 2, timeout: 50ms, on_unavailable: closed}
quota: {default_max_output_tokens: 256, limits_ttl: 1m, flush_interval: 2s, flush_buffer: 64, resync_interval: 30s}
`)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Redis.Enabled() || cfg.Redis.DB != 2 || cfg.Redis.Timeout != 50*time.Millisecond ||
		cfg.Redis.OnUnavailable != "closed" || cfg.Redis.Password != "s3cret" {
		t.Errorf("redis = %+v", cfg.Redis)
	}
	if cfg.Quota.DefaultMaxOutputTokens != 256 || cfg.Quota.LimitsTTL != time.Minute ||
		cfg.Quota.FlushInterval != 2*time.Second || cfg.Quota.FlushBuffer != 64 ||
		cfg.Quota.ResyncInterval != 30*time.Second {
		t.Errorf("quota = %+v", cfg.Quota)
	}
}
