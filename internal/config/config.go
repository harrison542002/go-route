package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen    string              `yaml:"listen"`
	Providers map[string]Provider `yaml:"providers"`
	Targets   map[string]Target   `yaml:"targets"`
	Models    map[string][]string `yaml:"models"`
	Sink      Sink                `yaml:"sink"`
	Pricing   Pricing             `yaml:"pricing"`
	Redis     Redis               `yaml:"redis"`
	Quota     Quota               `yaml:"quota"`
}

type Redis struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`

	// Timeout bounds every Redis call on the request path, so an outage costs
	// each request this long rather than a TCP timeout.
	Timeout time.Duration `yaml:"timeout"`

	// OnUnavailable is "open" (serve, unenforced) or "closed" (503) for when
	// Redis cannot be reached.
	OnUnavailable string `yaml:"on_unavailable"`
}

// Enabled reports whether quota enforcement is configured.
func (r Redis) Enabled() bool { return r.Addr != "" }

// Quota tunes enforcement. Everything has a default; the block can be left out.
type Quota struct {
	// DefaultMaxOutputTokens is reserved for a request that sets no max_tokens
	// or max_completion_tokens.
	DefaultMaxOutputTokens int `yaml:"default_max_output_tokens"`

	// LimitsTTL is how long a tenant's quota rows are cached, and so how long a
	// changed limit takes to bite.
	LimitsTTL time.Duration `yaml:"limits_ttl"`

	// FlushInterval and FlushBuffer govern how usage counter deltas reach
	// Postgres, the snapshot a lost Redis is rebuilt from.
	FlushInterval time.Duration `yaml:"flush_interval"`
	FlushBuffer   int           `yaml:"flush_buffer"`

	// ResyncInterval is how often live counters are raised back to that
	// snapshot. It cannot be switched off: a failover to a replica missing
	// acknowledged writes lowers a counter silently, and only this raises it
	// back before the window ends.
	ResyncInterval time.Duration `yaml:"resync_interval"`
}

type Sink struct {
	DSN string `yaml:"dsn"`

	SpoolDir        string        `yaml:"spool_dir"`
	SegmentMaxBytes int64         `yaml:"segment_max_bytes"`
	Sync            string        `yaml:"sync"`
	SyncInterval    time.Duration `yaml:"sync_interval"`
	MaxSpoolBytes   int64         `yaml:"max_spool_bytes"`

	// BufferSize is how many records may wait in memory for the spool's file
	// writer before Record blocks.
	BufferSize int `yaml:"buffer_size"`

	// BatchSize and FlushInterval govern the shipper: rows per transaction, and
	// how often a partly filled segment is shipped.
	BatchSize     int           `yaml:"batch_size"`
	FlushInterval time.Duration `yaml:"flush_interval"`
}

type Provider struct {
	Type                 string            `yaml:"type"`
	BaseURL              string            `yaml:"base_url"`
	APIKey               string            `yaml:"api_key"`
	DisableStreamOptions bool              `yaml:"disable_stream_options"`
	ExtraHeaders         map[string]string `yaml:"extra_headers"`
}

type PriceBlock struct {
	EffectiveFrom time.Time             `yaml:"effective_from"`
	Source        string                `yaml:"source,omitempty"`
	Rates         map[string]RateConfig `yaml:"rates"`
}

type RateConfig struct {
	// Free must be flagged explicitly
	Free bool `yaml:"free,omitempty"`

	InputPerMillion      float64 `yaml:"input_per_million"`
	OutputPerMillion     float64 `yaml:"output_per_million"`
	CacheReadPerMillion  float64 `yaml:"cache_read_per_million,omitempty"`
	CacheWritePerMillion float64 `yaml:"cache_write_per_million,omitempty"`

	Note string `yaml:"note,omitempty"`
}

type Pricing struct {
	CompareAgainst []string     `yaml:"compare_against"`
	Table          []PriceBlock `yaml:"table"`
}

type Target struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`

	// Region is recorded on every decision so past traffic can be
	// audited for data residency. Nothing reads it for routing yet;
	// residency rules will. Optional.
	Region string `yaml:"region,omitempty"`
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	expanded, err := expandEnv(raw)
	if err != nil {
		return nil, err
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(expanded)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if cfg.Listen == "" {
		cfg.Listen = ":4000"
	}
	cfg.applyQuotaDefaults()

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func expandEnv(raw []byte) ([]byte, error) {
	var missing []string

	out := envRef.ReplaceAllFunc(raw, func(m []byte) []byte {
		name := string(envRef.FindSubmatch(m)[1])
		val, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return nil
		}
		return []byte(val)
	})

	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("config: unset environment variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func (c *Config) validate() error {
	var errs []string

	if len(c.Providers) == 0 {
		errs = append(errs, "no providers defined")
	}
	if len(c.Models) == 0 {
		errs = append(errs, "no models defined; clients would have nothing to request")
	}

	for name, p := range c.Providers {
		if p.Type != "oaicompat" {
			errs = append(errs, fmt.Sprintf("provider %q: unknown type %q", name, p.Type))
		}
		if p.BaseURL == "" {
			errs = append(errs, fmt.Sprintf("provider %q: base_url is required", name))
		}
	}

	for name, t := range c.Targets {
		if _, ok := c.Providers[t.Provider]; !ok {
			errs = append(errs, fmt.Sprintf("target %q: unknown provider %q", name, t.Provider))
		}
		if t.Model == "" {
			errs = append(errs, fmt.Sprintf("target %q: model is required", name))
		}
	}

	for alias, ladder := range c.Models {
		if len(ladder) == 0 {
			errs = append(errs, fmt.Sprintf("model %q: empty ladder", alias))
		}
		for _, tn := range ladder {
			if _, ok := c.Targets[tn]; !ok {
				errs = append(errs, fmt.Sprintf("model %q: unknown target %q", alias, tn))
			}
		}
	}

	if c.Sink.DSN == "" {
		errs = append(errs, "sink: dsn is required; go-route needs Postgres to authenticate and to record spend")
	}

	errs = append(errs, c.validateSink()...)
	errs = append(errs, c.validatePricing()...)
	errs = append(errs, c.validateQuota()...)

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("config: invalid:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func (c *Config) validateSink() []string {
	var errs []string
	s := c.Sink

	switch s.Sync {
	case "", "interval", "always":
	default:
		errs = append(errs, fmt.Sprintf("sink: sync must be interval or always, not %q", s.Sync))
	}

	for name, v := range map[string]int64{
		"segment_max_bytes": s.SegmentMaxBytes,
		"max_spool_bytes":   s.MaxSpoolBytes,
		"buffer_size":       int64(s.BufferSize),
		"batch_size":        int64(s.BatchSize),
		"sync_interval":     int64(s.SyncInterval),
		"flush_interval":    int64(s.FlushInterval),
	} {
		if v < 0 {
			errs = append(errs, fmt.Sprintf("sink: %s must not be negative", name))
		}
	}

	// A threshold smaller than one segment would sit permanently over the line
	// the moment a single segment filled, and an alarm that is always on is an
	// alarm nobody reads.
	if s.MaxSpoolBytes > 0 && s.SegmentMaxBytes > 0 && s.MaxSpoolBytes < s.SegmentMaxBytes {
		errs = append(errs, "sink: max_spool_bytes must be at least segment_max_bytes")
	}
	return errs
}

func (c *Config) validatePricing() []string {
	var errs []string

	if len(c.Pricing.Table) == 0 {
		return nil
	}

	seen := map[string]bool{}
	for _, b := range c.Pricing.Table {
		if b.EffectiveFrom.IsZero() {
			errs = append(errs, "pricing: a block has no effective_from")
			continue
		}
		key := b.EffectiveFrom.Format("2006-01-02")
		if seen[key] {
			errs = append(errs, fmt.Sprintf("pricing: two blocks share effective_from %s", key))
		}
		seen[key] = true

		for name, rc := range b.Rates {
			if _, ok := c.Targets[name]; !ok {
				errs = append(errs, fmt.Sprintf("pricing %s: unknown target %q", key, name))
			}
			if !rc.Free && rc.InputPerMillion == 0 && rc.OutputPerMillion == 0 {
				errs = append(errs, fmt.Sprintf(
					"pricing %s: target %q has no rates and is not marked free", key, name))
			}
			if err := rc.ToRates().Validate(); err != nil {
				errs = append(errs, fmt.Sprintf("pricing %s: target %q: %v", key, name, err))
			}
		}
	}

	if len(c.Pricing.Table) > 1 {
		latest := c.Pricing.Table[len(c.Pricing.Table)-1]
		for _, b := range c.Pricing.Table[:len(c.Pricing.Table)-1] {
			for name := range b.Rates {
				if _, ok := latest.Rates[name]; !ok {
					errs = append(errs, fmt.Sprintf(
						"pricing: target %q priced in an earlier block but missing from the latest", name))
				}
			}
		}
	}

	return errs
}

func (c *Config) applyQuotaDefaults() {
	if c.Redis.Timeout == 0 {
		c.Redis.Timeout = 100 * time.Millisecond
	}
	if c.Redis.OnUnavailable == "" {
		c.Redis.OnUnavailable = "open"
	}
	if c.Quota.DefaultMaxOutputTokens == 0 {
		c.Quota.DefaultMaxOutputTokens = 4096
	}
	if c.Quota.LimitsTTL == 0 {
		c.Quota.LimitsTTL = 10 * time.Second
	}
	if c.Quota.FlushInterval == 0 {
		c.Quota.FlushInterval = time.Second
	}
	if c.Quota.FlushBuffer == 0 {
		c.Quota.FlushBuffer = 8192
	}
	if c.Quota.ResyncInterval == 0 {
		c.Quota.ResyncInterval = time.Minute
	}
}

func (c *Config) validateQuota() []string {
	var errs []string

	switch c.Redis.OnUnavailable {
	case "open", "closed":
	default:
		errs = append(errs, fmt.Sprintf(
			"redis: on_unavailable must be open or closed, got %q", c.Redis.OnUnavailable))
	}
	if c.Redis.Timeout < 0 {
		errs = append(errs, "redis: timeout must not be negative")
	}
	if c.Redis.DB < 0 {
		errs = append(errs, "redis: db must not be negative")
	}
	if c.Quota.DefaultMaxOutputTokens < 0 {
		errs = append(errs, "quota: default_max_output_tokens must not be negative")
	}
	if c.Quota.LimitsTTL < 0 {
		errs = append(errs, "quota: limits_ttl must not be negative")
	}
	if c.Quota.FlushInterval < 0 {
		errs = append(errs, "quota: flush_interval must not be negative")
	}
	if c.Quota.FlushBuffer < 0 {
		errs = append(errs, "quota: flush_buffer must not be negative")
	}
	if c.Quota.ResyncInterval < 0 {
		errs = append(errs, "quota: resync_interval must be positive")
	}

	return errs
}

func (rc RateConfig) ToRates() domains.Rates {
	if rc.Free {
		return domains.Rates{}
	}
	return domains.Rates{
		Input:      domains.PerMillionTokens(domains.FromDollars(rc.InputPerMillion)),
		Output:     domains.PerMillionTokens(domains.FromDollars(rc.OutputPerMillion)),
		CacheRead:  domains.PerMillionTokens(domains.FromDollars(rc.CacheReadPerMillion)),
		CacheWrite: domains.PerMillionTokens(domains.FromDollars(rc.CacheWritePerMillion)),
	}
}
