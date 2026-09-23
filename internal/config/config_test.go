package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
