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
}

type Sink struct {
	Type          string        `yaml:"type"`
	BufferSize    int           `yaml:"buffer_size"`
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

	if cfg.Sink.Type == "" {
		cfg.Sink.Type = "log"
	}

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

	switch c.Sink.Type {
	case "log", "memory", "none":
	default:
		errs = append(errs, fmt.Sprintf("sink: unknown type %q (log, memory, none)", c.Sink.Type))
	}

	errs = append(errs, c.validatePricing()...)

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("config: invalid:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
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
