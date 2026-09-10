package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validConfig is the smallest configuration that passes validation. Tests build
// their fixtures by replacing one line of it, so that each case documents
// exactly the field it is about.
const validConfig = `
listen_addr: ":8080"
algorithm: round_robin
failure_policy: retry_next_backend
retry_on_5xx: false
retry:
  max_retries: 2
backends:
  - addr: "backend-1:5678"
    weight: 1
health_check:
  path: "/healthz"
  interval: 5s
  timeout: 2s
  healthy_threshold: 2
  unhealthy_threshold: 3
timeouts:
  connect_timeout: 2s
  read_timeout: 10s
  write_timeout: 10s
  idle_timeout: 60s
limits:
  max_connections: 10000
  max_request_body_bytes: 10485760
  rate_limit_per_ip: 100
logging:
  level: info
  format: json
`

// writeConfig stores content in a temporary file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lb.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// replace swaps a single line of the valid configuration, identified by its
// unique prefix, for the given replacement.
func replace(t *testing.T, prefix, replacement string) string {
	t.Helper()
	lines := strings.Split(strings.TrimPrefix(validConfig, "\n"), "\n")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			if found {
				t.Fatalf("prefix %q matches more than one line", prefix)
			}
			lines[i], found = replacement, true
		}
	}
	if !found {
		t.Fatalf("prefix %q matches no line", prefix)
	}
	return strings.Join(lines, "\n")
}

func TestLoadValidConfig(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}

	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, ":8080")
	}
	if cfg.Algorithm != RoundRobin {
		t.Errorf("Algorithm = %q, want %q", cfg.Algorithm, RoundRobin)
	}
	if cfg.FailurePolicy != RetryNextBackend {
		t.Errorf("FailurePolicy = %q, want %q", cfg.FailurePolicy, RetryNextBackend)
	}
	if got, want := len(cfg.Backends), 1; got != want {
		t.Fatalf("len(Backends) = %d, want %d", got, want)
	}
	if cfg.Backends[0].Addr != "backend-1:5678" {
		t.Errorf("Backends[0].Addr = %q, want %q", cfg.Backends[0].Addr, "backend-1:5678")
	}
	if got, want := time.Duration(cfg.HealthCheck.Interval), 5*time.Second; got != want {
		t.Errorf("HealthCheck.Interval = %s, want %s", got, want)
	}
	if got, want := cfg.Limits.MaxRequestBodyBytes, int64(10485760); got != want {
		t.Errorf("Limits.MaxRequestBodyBytes = %d, want %d", got, want)
	}
}

func TestLoadExampleConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "configs", "lb.example.yaml"))
	if err != nil {
		t.Fatalf("the shipped example config must stay valid: %v", err)
	}
	if got, want := len(cfg.Backends), 10; got != want {
		t.Errorf("len(Backends) = %d, want %d (one per mock backend)", got, want)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	content := replace(t, "    weight:", "")
	content = strings.ReplaceAll(content, "  level: info\n", "")
	content = strings.ReplaceAll(content, "  format: json\n", "")
	content = strings.ReplaceAll(content, "logging:\n", "")

	cfg, err := Load(writeConfig(t, content))
	if err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}
	if cfg.Backends[0].Weight != 1 {
		t.Errorf("omitted weight = %d, want the default 1", cfg.Backends[0].Weight)
	}
	if cfg.Logging.Level != LevelInfo {
		t.Errorf("omitted logging.level = %q, want the default %q", cfg.Logging.Level, LevelInfo)
	}
	if cfg.Logging.Format != FormatJSON {
		t.Errorf("omitted logging.format = %q, want the default %q", cfg.Logging.Format, FormatJSON)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		line    string
		wantErr string
	}{
		{"empty listen address", "listen_addr:", `listen_addr: ""`, "listen_addr is required"},
		{"listen address without port", "listen_addr:", `listen_addr: "localhost"`, "not a host:port"},
		{"unknown algorithm", "algorithm:", "algorithm: random", `algorithm "random" is unknown`},
		{"unknown failure policy", "failure_policy:", "failure_policy: explode", `failure_policy "explode" is unknown`},
		{"negative retries", "  max_retries:", "  max_retries: -1", "must not be negative"},
		{"retry policy without retries", "  max_retries:", "  max_retries: 0", "must be at least 1 when failure_policy"},
		{"backend without port", "  - addr:", `  - addr: "backend-1"`, "not a host:port"},
		{"negative weight", "    weight:", "    weight: -2", "weight must not be negative"},
		{"health path without slash", "  path:", `  path: "healthz"`, "must start with a slash"},
		{"health timeout above interval", "  timeout:", "  timeout: 9s", "must be shorter than"},
		{"zero healthy threshold", "  healthy_threshold:", "  healthy_threshold: 0", "healthy_threshold must be at least 1"},
		{"zero connect timeout", "  connect_timeout:", "  connect_timeout: 0s", "connect_timeout must be greater than zero"},
		{"zero max connections", "  max_connections:", "  max_connections: 0", "max_connections must be at least 1"},
		{"negative rate limit", "  rate_limit_per_ip:", "  rate_limit_per_ip: -5", "rate_limit_per_ip must not be negative"},
		{"unknown log level", "  level:", "  level: verbose", `logging.level "verbose" is unknown`},
		{"unknown log format", "  format:", "  format: xml", `logging.format "xml" is unknown`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, replace(t, test.prefix, test.line)))
			if err == nil {
				t.Fatal("Load accepted an invalid configuration")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	content := replace(t, "algorithm:", "algorithm: random")
	content = strings.Replace(content, "  max_connections: 10000", "  max_connections: 0", 1)

	_, err := Load(writeConfig(t, content))
	if err == nil {
		t.Fatal("Load accepted an invalid configuration")
	}
	for _, want := range []string{"algorithm", "max_connections"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q as well", err, want)
		}
	}
}

func TestLoadRejectsEmptyBackendList(t *testing.T) {
	content := strings.Replace(validConfig,
		"backends:\n  - addr: \"backend-1:5678\"\n    weight: 1\n", "backends: []\n", 1)

	_, err := Load(writeConfig(t, content))
	if err == nil {
		t.Fatal("Load accepted a configuration without backends")
	}
	if !strings.Contains(err.Error(), "at least one entry") {
		t.Errorf("error = %v, want it to mention the empty backend list", err)
	}
}

func TestLoadRejectsDuplicateBackends(t *testing.T) {
	content := strings.Replace(validConfig,
		"    weight: 1\n", "    weight: 1\n  - addr: \"backend-1:5678\"\n    weight: 2\n", 1)

	_, err := Load(writeConfig(t, content))
	if err == nil {
		t.Fatal("Load accepted a duplicated backend")
	}
	if !strings.Contains(err.Error(), "duplicates") {
		t.Errorf("error = %v, want it to mention the duplicate", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	_, err := Load(writeConfig(t, validConfig+"colour: blue\n"))
	if err == nil {
		t.Fatal("Load accepted an unknown field")
	}
	if !strings.Contains(err.Error(), "colour") {
		t.Errorf("error = %v, want it to name the unknown field", err)
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	_, err := Load(writeConfig(t, "listen_addr: \":8080\"\n  algorithm: round_robin\n"))
	if err == nil {
		t.Fatal("Load accepted malformed YAML")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error = %v, want a parse error", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("Load accepted a missing file")
	}
	if !strings.Contains(err.Error(), "open config") {
		t.Errorf("error = %v, want an open error", err)
	}
}

func TestDurationRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"missing unit", "  interval: 5"},
		{"not a duration", "  interval: soon"},
		{"not a scalar", "  interval: [5s]"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, replace(t, "  interval:", test.line)))
			if err == nil {
				t.Fatal("Load accepted an invalid duration")
			}
		})
	}
}

func TestDurationString(t *testing.T) {
	if got, want := Duration(90*time.Second).String(), "1m30s"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestMetricsAddrDefaultsAndValidates(t *testing.T) {
	content := strings.Replace(validConfig, "listen_addr: \":8080\"\n",
		"listen_addr: \":8080\"\nmetrics_addr: \":9999\"\n", 1)

	cfg, err := Load(writeConfig(t, content))
	if err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}
	if cfg.MetricsAddr != ":9999" {
		t.Errorf("MetricsAddr = %q, want the configured %q", cfg.MetricsAddr, ":9999")
	}

	// Omitting it keeps the observability endpoints on their default port.
	cfg, err = Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}
	if cfg.MetricsAddr != ":8081" {
		t.Errorf("omitted metrics_addr = %q, want the default %q", cfg.MetricsAddr, ":8081")
	}

	broken := strings.Replace(validConfig, "listen_addr: \":8080\"\n",
		"listen_addr: \":8080\"\nmetrics_addr: \"localhost\"\n", 1)
	if _, err := Load(writeConfig(t, broken)); err == nil {
		t.Error("Load accepted a metrics_addr without a port")
	}
}

func TestResponseTimeoutDefaultsToTheReadTimeout(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}
	if cfg.Timeouts.ResponseTimeout != cfg.Timeouts.ReadTimeout {
		t.Errorf("omitted response_timeout = %s, want the read timeout %s",
			cfg.Timeouts.ResponseTimeout, cfg.Timeouts.ReadTimeout)
	}

	explicit := replace(t, "  connect_timeout:", "  connect_timeout: 2s\n  response_timeout: 3s")
	cfg, err = Load(writeConfig(t, explicit))
	if err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}
	if got, want := time.Duration(cfg.Timeouts.ResponseTimeout), 3*time.Second; got != want {
		t.Errorf("response_timeout = %s, want %s", got, want)
	}
}

func TestRequiresRestartNamesUnchangeableSettings(t *testing.T) {
	running, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}

	next, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}
	if fixed := running.RequiresRestart(next); len(fixed) != 0 {
		t.Errorf("an unchanged configuration reported %v as needing a restart", fixed)
	}

	next.ListenAddr = ":9090"
	next.Limits.MaxConnections = 5
	next.Algorithm = LeastConnections // reloadable, must not be listed

	fixed := running.RequiresRestart(next)
	if len(fixed) != 2 {
		t.Fatalf("RequiresRestart = %v, want the two unchangeable settings", fixed)
	}
	for _, want := range []string{"listen_addr", "limits.max_connections"} {
		if !strings.Contains(strings.Join(fixed, " "), want) {
			t.Errorf("RequiresRestart = %v, want it to name %q", fixed, want)
		}
	}
}
