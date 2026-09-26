package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const dsn = "postgres://localhost/goroute"

const minimal = `
sink: {dsn: "` + dsn + `"}
` + routing

const routing = `
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

// withSink builds a config whose sink block carries extra keys beside the dsn.
func withSink(extra string) string {
	return "sink:\n  dsn: \"" + dsn + "\"" + extra + "\n" + routing
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

// The sink block is all optional: a config that names only a dsn must still
// load, because the sink package owns the defaults.
func TestSinkDefaultsAreLeftToTheSink(t *testing.T) {
	cfg, err := load(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sink != (Sink{DSN: dsn}) {
		t.Errorf("sink = %+v, want every spool key left at its zero value", cfg.Sink)
	}
}

func TestSinkBlockIsRead(t *testing.T) {
	cfg, err := load(t, withSink(`
  spool_dir: /var/lib/go-route/spool
  segment_max_bytes: 1048576
  sync: always
  sync_interval: 50ms
  max_spool_bytes: 2097152
  buffer_size: 128
  batch_size: 25
  flush_interval: 2s`))
	if err != nil {
		t.Fatal(err)
	}
	want := Sink{
		DSN:             dsn,
		SpoolDir:        "/var/lib/go-route/spool",
		SegmentMaxBytes: 1 << 20,
		Sync:            "always",
		SyncInterval:    50 * time.Millisecond,
		MaxSpoolBytes:   2 << 20,
		BufferSize:      128,
		BatchSize:       25,
		FlushInterval:   2 * time.Second,
	}
	if cfg.Sink != want {
		t.Errorf("sink = %+v, want %+v", cfg.Sink, want)
	}
}

func TestSinkValidation(t *testing.T) {
	tests := []struct {
		name string
		sink string
		want string
	}{
		{
			name: "an unknown sync mode",
			sink: "  sync: sometimes",
			want: "sink: sync must be interval or always",
		},
		{
			name: "a negative segment size",
			sink: "  segment_max_bytes: -1",
			want: "sink: segment_max_bytes must not be negative",
		},
		{
			name: "a negative sync interval",
			sink: "  sync_interval: -1s",
			want: "sink: sync_interval must not be negative",
		},
		{
			name: "an alarm smaller than one segment would never switch off",
			sink: "  segment_max_bytes: 1048576\n  max_spool_bytes: 1024",
			want: "sink: max_spool_bytes must be at least segment_max_bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, withSink("\n"+tt.sink))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
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
