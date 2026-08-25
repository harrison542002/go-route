package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen    string              `yaml:"listen"`
	Providers map[string]Provider `yaml:"providers"`
	Targets   map[string]Target   `yaml:"targets"`
	Models    map[string][]string `yaml:"models"`
}

type Provider struct {
	Type                 string            `yaml:"type"`
	BaseURL              string            `yaml:"base_url"`
	APIKey               string            `yaml:"api_key"`
	DisableStreamOptions bool              `yaml:"disable_stream_options"`
	ExtraHeaders         map[string]string `yaml:"extra_headers"`
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
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// expandEnv substitutes ${VAR}. An unset variable is an error rather
// than an empty string: an empty API key would otherwise surface as a
// 401 on the first request instead of a clear failure at boot.
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

// validate catches every referential error at boot. A dangling target
// reference discovered on the first live request is a production
// incident; discovered at startup it is a failed deploy.
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

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("config: invalid:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}
