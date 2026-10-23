package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef"

func TestLoadDefaultConfigFromWorkingDirectory(t *testing.T) {
	clearConfigEnvironment(t)
	directory := t.TempDir()
	writeConfig(t, filepath.Join(directory, DefaultFileName), `
server:
  listen: 127.0.0.1:9000
database:
  path: state/relay.sqlite
security:
  control_token: 0123456789abcdef0123456789abcdef
`)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	resolvedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != filepath.Join(resolvedDirectory, DefaultFileName) {
		t.Fatalf("unexpected config path %q", cfg.Path)
	}
	if cfg.Database.Path != filepath.Join(resolvedDirectory, "state", "relay.sqlite") {
		t.Fatalf("relative database path was not resolved from config: %q", cfg.Database.Path)
	}
	if cfg.Server.ReadTimeout.String() != "15s" {
		t.Fatalf("defaults were not retained: %s", cfg.Server.ReadTimeout.String())
	}
}

func TestLoadExplicitPathAndEnvironmentOverrides(t *testing.T) {
	clearConfigEnvironment(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "custom.yaml")
	writeConfig(t, path, `
server:
  listen: 127.0.0.1:9000
database:
  path: original.sqlite
security:
  control_token_env: RELAY_TEST_SECRET
`)
	t.Setenv("RELAY_TEST_SECRET", testToken)
	t.Setenv("CODEX_RELAY_LISTEN", "127.0.0.1:9443")
	t.Setenv("CODEX_RELAY_DB_PATH", filepath.Join(directory, "override.sqlite"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:9443" {
		t.Fatalf("listen override missing: %s", cfg.Server.Listen)
	}
	if cfg.Database.Path != filepath.Join(directory, "override.sqlite") {
		t.Fatalf("database override missing: %s", cfg.Database.Path)
	}
	if cfg.Security.ControlToken != testToken {
		t.Fatal("secret environment override missing")
	}
}

func TestUnknownConfigurationFieldFails(t *testing.T) {
	clearConfigEnvironment(t)
	path := filepath.Join(t.TempDir(), "relay.yaml")
	writeConfig(t, path, "server:\n  listen: 127.0.0.1:8787\n  typo_field: true\nsecurity:\n  control_token: "+testToken+"\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "field typo_field not found") {
		t.Fatalf("expected strict YAML error, got %v", err)
	}
}

func TestPublicPlainHTTPRequiresExplicitProxyMode(t *testing.T) {
	cfg := Defaults()
	cfg.Server.Listen = "0.0.0.0:8787"
	cfg.Security.ControlToken = testToken
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("expected plain HTTP guard, got %v", err)
	}
	cfg.Security.AllowInsecureHTTP = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit reverse proxy mode should validate: %v", err)
	}
}

func TestDirectTLSAllowsPublicListener(t *testing.T) {
	cfg := Defaults()
	cfg.Server.Listen = "0.0.0.0:8787"
	cfg.Server.PublicURL = "https://relay.example.com:8787"
	cfg.Server.TLS.CertFile = "cert.pem"
	cfg.Server.TLS.KeyFile = "key.pem"
	cfg.Security.ControlToken = testToken
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsMissingTLSFiles(t *testing.T) {
	clearConfigEnvironment(t)
	path := filepath.Join(t.TempDir(), "relay.yaml")
	writeConfig(t, path, `
server:
  listen: 0.0.0.0:8787
  tls:
    cert_file: missing-cert.pem
    key_file: missing-key.pem
security:
  control_token: 0123456789abcdef0123456789abcdef
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "server.tls.cert_file is not readable") {
		t.Fatalf("expected missing TLS file error, got %v", err)
	}
}

func writeConfig(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}
func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CODEX_RELAY_CONFIG", "CODEX_RELAY_LISTEN", "CODEX_RELAY_HOST", "CODEX_RELAY_PORT", "CODEX_RELAY_PUBLIC_URL", "CODEX_RELAY_DB_PATH", "RELAY_DB_PATH", "CODEX_RELAY_TLS_CERT_FILE", "CODEX_RELAY_TLS_KEY_FILE", "CODEX_RELAY_CONTROL_TOKEN", "CONTROL_TOKEN", "HOST", "PORT"} {
		t.Setenv(name, "")
	}
}
