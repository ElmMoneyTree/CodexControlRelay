package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultFileName = "relay.yaml"

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("duration must be a string such as 15s or 10m")
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Security SecurityConfig `yaml:"security"`
	Logging  LoggingConfig  `yaml:"logging"`

	Path string `yaml:"-"`
}

type ServerConfig struct {
	Listen            string    `yaml:"listen"`
	PublicURL         string    `yaml:"public_url"`
	ReadHeaderTimeout Duration  `yaml:"read_header_timeout"`
	ReadTimeout       Duration  `yaml:"read_timeout"`
	WriteTimeout      Duration  `yaml:"write_timeout"`
	IdleTimeout       Duration  `yaml:"idle_timeout"`
	ShutdownTimeout   Duration  `yaml:"shutdown_timeout"`
	MaxBodyBytes      int64     `yaml:"max_body_bytes"`
	TrustedProxies    []string  `yaml:"trusted_proxies"`
	TLS               TLSConfig `yaml:"tls"`
}

type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type DatabaseConfig struct {
	Path        string   `yaml:"path"`
	BusyTimeout Duration `yaml:"busy_timeout"`
}

type SecurityConfig struct {
	ControlToken      string     `yaml:"control_token"`
	ControlTokenEnv   string     `yaml:"control_token_env"`
	AllowInsecureHTTP bool       `yaml:"allow_insecure_http"`
	AllowRegistration bool       `yaml:"allow_registration"`
	LegacyAPIEnabled  bool       `yaml:"legacy_api_enabled"`
	AccountSessionTTL Duration   `yaml:"account_session_ttl"`
	DeviceClaimTTL    Duration   `yaml:"device_claim_ttl"`
	PairingCodeTTL    Duration   `yaml:"pairing_code_ttl"`
	OfflineAfter      Duration   `yaml:"offline_after"`
	RateLimits        RateLimits `yaml:"rate_limits"`
	AllowedOrigins    []string   `yaml:"allowed_origins"`
}

type RateLimits struct {
	Enrollment     RateLimit `yaml:"enrollment"`
	Authentication RateLimit `yaml:"authentication"`
	Claims         RateLimit `yaml:"claims"`
	Control        RateLimit `yaml:"control"`
	Agent          RateLimit `yaml:"agent"`
}

type RateLimit struct {
	Requests int      `yaml:"requests"`
	Window   Duration `yaml:"window"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

func Defaults() Config {
	return Config{
		Server: ServerConfig{
			Listen:            "127.0.0.1:8787",
			ReadHeaderTimeout: duration(5 * time.Second),
			ReadTimeout:       duration(15 * time.Second),
			WriteTimeout:      duration(30 * time.Second),
			IdleTimeout:       duration(60 * time.Second),
			ShutdownTimeout:   duration(15 * time.Second),
			MaxBodyBytes:      1_000_000,
		},
		Database: DatabaseConfig{
			Path:        "data/relay.sqlite",
			BusyTimeout: duration(5 * time.Second),
		},
		Security: SecurityConfig{
			ControlTokenEnv:   "CODEX_RELAY_CONTROL_TOKEN",
			LegacyAPIEnabled:  true,
			AccountSessionTTL: duration(30 * 24 * time.Hour),
			DeviceClaimTTL:    duration(5 * time.Minute),
			PairingCodeTTL:    duration(10 * time.Minute),
			OfflineAfter:      duration(15 * time.Second),
			RateLimits: RateLimits{
				Enrollment:     RateLimit{Requests: 10, Window: duration(time.Minute)},
				Authentication: RateLimit{Requests: 20, Window: duration(time.Minute)},
				Claims:         RateLimit{Requests: 60, Window: duration(time.Minute)},
				Control:        RateLimit{Requests: 300, Window: duration(time.Minute)},
				Agent:          RateLimit{Requests: 1200, Window: duration(time.Minute)},
			},
		},
		Logging: LoggingConfig{Level: "info", Format: "json"},
	}
}

func ResolvePath(explicit string) (string, error) {
	if explicit == "" {
		explicit = strings.TrimSpace(os.Getenv("CODEX_RELAY_CONFIG"))
	}
	if explicit == "" {
		explicit = DefaultFileName
	}
	absolute, err := filepath.Abs(explicit)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func Load(path string) (Config, error) {
	resolved, err := ResolvePath(path)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", resolved, err)
	}

	cfg := Defaults()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", resolved, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", resolved, err)
		}
		return Config{}, fmt.Errorf("parse config %s: multiple YAML documents are not supported", resolved)
	}
	cfg.Path = resolved
	applyEnvironment(&cfg)
	resolveRelativePaths(&cfg, filepath.Dir(resolved))
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %s: %w", resolved, err)
	}
	if err := cfg.validateFiles(); err != nil {
		return Config{}, fmt.Errorf("validate config %s: %w", resolved, err)
	}
	return cfg, nil
}

func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		return fmt.Errorf("server.listen must be host:port: %w", err)
	}
	if parsed, err := strconv.ParseUint(port, 10, 16); err != nil || parsed == 0 {
		return errors.New("server.listen port must be between 1 and 65535")
	}
	if c.Server.MaxBodyBytes < 1024 || c.Server.MaxBodyBytes > 100_000_000 {
		return errors.New("server.max_body_bytes must be between 1024 and 100000000")
	}
	if c.Server.ReadHeaderTimeout.Duration <= 0 || c.Server.ReadTimeout.Duration <= 0 || c.Server.WriteTimeout.Duration <= 0 || c.Server.IdleTimeout.Duration <= 0 || c.Server.ShutdownTimeout.Duration <= 0 {
		return errors.New("server timeouts must be positive")
	}
	if (c.Server.TLS.CertFile == "") != (c.Server.TLS.KeyFile == "") {
		return errors.New("server.tls.cert_file and key_file must be set together")
	}
	if c.Server.TLS.CertFile == "" && !c.Security.AllowInsecureHTTP && !isLoopbackHost(host) {
		return errors.New("plain HTTP may only listen on loopback; configure TLS or set security.allow_insecure_http only behind a trusted reverse proxy")
	}
	if c.Server.PublicURL != "" {
		parsed, err := url.Parse(c.Server.PublicURL)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("server.public_url must be an HTTP(S) origin")
		}
		if parsed.Scheme != "https" && !(parsed.Scheme == "http" && c.Security.AllowInsecureHTTP) {
			return errors.New("server.public_url must use HTTPS")
		}
	}
	if c.Database.Path == "" {
		return errors.New("database.path is required")
	}
	if c.Database.BusyTimeout.Duration <= 0 {
		return errors.New("database.busy_timeout must be positive")
	}
	if len(c.Security.ControlToken) < 32 {
		return errors.New("control token must contain at least 32 characters; set security.control_token or its configured environment variable")
	}
	if c.Security.AccountSessionTTL.Duration <= 0 || c.Security.DeviceClaimTTL.Duration <= 0 || c.Security.PairingCodeTTL.Duration <= 0 || c.Security.OfflineAfter.Duration <= 0 {
		return errors.New("security session, claim, pairing, and offline durations must be positive")
	}
	for name, limit := range map[string]RateLimit{
		"enrollment":     c.Security.RateLimits.Enrollment,
		"authentication": c.Security.RateLimits.Authentication,
		"claims":         c.Security.RateLimits.Claims,
		"control":        c.Security.RateLimits.Control,
		"agent":          c.Security.RateLimits.Agent,
	} {
		if limit.Requests <= 0 || limit.Window.Duration <= 0 {
			return fmt.Errorf("security.rate_limits.%s requests and window must be positive", name)
		}
	}
	for _, proxy := range c.Server.TrustedProxies {
		if net.ParseIP(proxy) == nil {
			if _, _, err := net.ParseCIDR(proxy); err != nil {
				return fmt.Errorf("invalid trusted proxy %q", proxy)
			}
		}
	}
	for _, origin := range c.Security.AllowedOrigins {
		if origin == "*" {
			return errors.New("security.allowed_origins does not accept wildcard origins")
		}
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("invalid allowed origin %q", origin)
		}
	}
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		return errors.New("logging.level must be debug, info, warn, or error")
	}
	switch strings.ToLower(c.Logging.Format) {
	case "json", "text":
	default:
		return errors.New("logging.format must be json or text")
	}
	return nil
}

func (c Config) validateFiles() error {
	files := []struct{ name, path string }{
		{"server.tls.cert_file", c.Server.TLS.CertFile},
		{"server.tls.key_file", c.Server.TLS.KeyFile},
	}
	for _, file := range files {
		if file.path == "" {
			continue
		}
		info, err := os.Stat(file.path)
		if err != nil {
			return fmt.Errorf("%s is not readable: %w", file.name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s must be a regular file", file.name)
		}
	}
	return nil
}

func applyEnvironment(c *Config) {
	if value := firstEnvironment("CODEX_RELAY_LISTEN"); value != "" {
		c.Server.Listen = value
	} else {
		host := firstEnvironment("CODEX_RELAY_HOST", "HOST")
		port := firstEnvironment("CODEX_RELAY_PORT", "PORT")
		if host != "" || port != "" {
			currentHost, currentPort, err := net.SplitHostPort(c.Server.Listen)
			if err == nil {
				if host == "" {
					host = currentHost
				}
				if port == "" {
					port = currentPort
				}
				c.Server.Listen = net.JoinHostPort(host, port)
			}
		}
	}
	if value := firstEnvironment("CODEX_RELAY_PUBLIC_URL"); value != "" {
		c.Server.PublicURL = value
	}
	if value := firstEnvironment("CODEX_RELAY_DB_PATH", "RELAY_DB_PATH"); value != "" {
		c.Database.Path = value
	}
	if value := firstEnvironment("CODEX_RELAY_TLS_CERT_FILE"); value != "" {
		c.Server.TLS.CertFile = value
	}
	if value := firstEnvironment("CODEX_RELAY_TLS_KEY_FILE"); value != "" {
		c.Server.TLS.KeyFile = value
	}
	if value := strings.TrimSpace(os.Getenv(c.Security.ControlTokenEnv)); value != "" {
		c.Security.ControlToken = value
	} else if value := firstEnvironment("CONTROL_TOKEN"); value != "" {
		c.Security.ControlToken = value
	}
}

func resolveRelativePaths(c *Config, base string) {
	c.Database.Path = relativeTo(base, c.Database.Path)
	c.Server.TLS.CertFile = relativeTo(base, c.Server.TLS.CertFile)
	c.Server.TLS.KeyFile = relativeTo(base, c.Server.TLS.KeyFile)
}

func relativeTo(base, value string) string {
	if value == "" || value == ":memory:" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Clean(filepath.Join(base, value))
}

func firstEnvironment(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func duration(value time.Duration) Duration { return Duration{Duration: value} }
