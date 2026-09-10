package observability

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// stubChecker reports the health the test asks for.
type stubChecker struct{ unhealthy map[string]bool }

func (s stubChecker) Start(context.Context, []*balancer.Backend)                      {}
func (s stubChecker) Reload(context.Context, config.HealthCheck, []*balancer.Backend) {}
func (s stubChecker) IsHealthy(addr string) bool                                      { return !s.unhealthy[addr] }
func (s stubChecker) ReportSuccess(string)                                            {}
func (s stubChecker) ReportFailure(string)                                            {}

// scrape renders the metrics endpoint as text.
func scrape(t *testing.T, handler http.Handler) string {
	t.Helper()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("scrape returned %d, want %d", recorder.Code, http.StatusOK)
	}
	return recorder.Body.String()
}

func TestMetricsReportServedRequests(t *testing.T) {
	metrics := NewMetrics()
	metrics.ObserveRequest("backend-1:5678", "200", 12*time.Millisecond)
	metrics.ObserveRequest("backend-1:5678", "200", 30*time.Millisecond)
	metrics.ObserveRequest("backend-2:5678", "500", time.Second)

	body := scrape(t, metrics.Handler())
	for _, want := range []string{
		`lb_requests_total{backend="backend-1:5678",status="200"} 2`,
		`lb_requests_total{backend="backend-2:5678",status="500"} 1`,
		`lb_request_duration_seconds_count{backend="backend-1:5678"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics do not contain %q", want)
		}
	}
}

func TestMetricsReportFailuresAndRejections(t *testing.T) {
	metrics := NewMetrics()
	metrics.ObserveBackendFailure("backend-1:5678")
	metrics.ObserveRejection("rate_limited")
	metrics.ObserveRejection("rate_limited")

	body := scrape(t, metrics.Handler())
	for _, want := range []string{
		`lb_backend_failures_total{backend="backend-1:5678"} 1`,
		`lb_rejected_requests_total{reason="rate_limited"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics do not contain %q", want)
		}
	}
}

func TestPoolReportsLiveStateAtScrapeTime(t *testing.T) {
	backends := []*balancer.Backend{
		{Addr: "backend-1:5678", Weight: 1},
		{Addr: "backend-2:5678", Weight: 3},
	}
	backends[0].Acquire()
	backends[0].Acquire()

	metrics := NewMetrics()
	pool := NewPool("round_robin", backends, stubChecker{unhealthy: map[string]bool{
		"backend-2:5678": true,
	}})
	metrics.Register(pool)

	body := scrape(t, metrics.Handler())
	for _, want := range []string{
		`lb_backend_active_connections{backend="backend-1:5678"} 2`,
		`lb_backend_active_connections{backend="backend-2:5678"} 0`,
		`lb_backend_healthy{backend="backend-1:5678"} 1`,
		`lb_backend_healthy{backend="backend-2:5678"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics do not contain %q", want)
		}
	}

	// The gauge must follow the pool rather than report a stale copy.
	backends[0].Release()
	if body := scrape(t, metrics.Handler()); !strings.Contains(body,
		`lb_backend_active_connections{backend="backend-1:5678"} 1`) {
		t.Error("the active connection gauge did not follow the pool")
	}
}

func TestStatusReportsThePool(t *testing.T) {
	backends := []*balancer.Backend{
		{Addr: "backend-1:5678", Weight: 1},
		{Addr: "backend-2:5678", Weight: 3},
	}
	backends[1].Acquire()

	pool := NewPool("weighted_round_robin", backends, stubChecker{unhealthy: map[string]bool{
		"backend-1:5678": true,
	}})

	recorder := httptest.NewRecorder()
	Endpoints(NewMetrics(), pool, false).ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/status", nil))

	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}

	var got status
	if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil {
		t.Fatalf("decode status: %v", err)
	}

	if got.Algorithm != "weighted_round_robin" {
		t.Errorf("algorithm = %q, want the configured one", got.Algorithm)
	}
	if got.Total != 2 || got.Healthy != 1 {
		t.Errorf("healthy %d of %d, want 1 of 2", got.Healthy, got.Total)
	}
	if got.Backends[0].Healthy {
		t.Error("the unhealthy backend is reported as healthy")
	}
	if got.Backends[1].ActiveConnections != 1 {
		t.Errorf("active connections = %d, want 1", got.Backends[1].ActiveConnections)
	}
	if got.Backends[1].Weight != 3 {
		t.Errorf("weight = %d, want the configured 3", got.Backends[1].Weight)
	}
}

func TestLoggerHonoursFormatAndLevel(t *testing.T) {
	tests := []struct {
		name     string
		logging  config.Logging
		wantJSON bool
	}{
		{"json", config.Logging{Level: config.LevelInfo, Format: config.FormatJSON}, true},
		{"text", config.Logging{Level: config.LevelInfo, Format: config.FormatText}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger := NewLogger(test.logging)
			if logger == nil {
				t.Fatal("NewLogger returned nil")
			}
			if slog.Default() != logger {
				t.Error("the logger was not installed as the default")
			}
		})
	}
}

func TestLogLevelMapping(t *testing.T) {
	tests := map[config.LogLevel]slog.Level{
		config.LevelDebug:   slog.LevelDebug,
		config.LevelInfo:    slog.LevelInfo,
		config.LevelWarn:    slog.LevelWarn,
		config.LevelError:   slog.LevelError,
		config.LogLevel(""): slog.LevelInfo,
	}

	for level, want := range tests {
		if got := levelOf(level); got != want {
			t.Errorf("levelOf(%q) = %v, want %v", level, got, want)
		}
	}
}

func TestPprofIsServedOnlyWhenEnabled(t *testing.T) {
	pool := NewPool("round_robin", nil, stubChecker{})

	for _, test := range []struct {
		enabled bool
		want    int
	}{
		{false, http.StatusNotFound},
		{true, http.StatusOK},
	} {
		recorder := httptest.NewRecorder()
		Endpoints(NewMetrics(), pool, test.enabled).ServeHTTP(recorder,
			httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))

		if recorder.Code != test.want {
			t.Errorf("with pprof enabled=%v the endpoint answered %d, want %d",
				test.enabled, recorder.Code, test.want)
		}
	}
}

func TestPoolReloadFollowsTheNewBackends(t *testing.T) {
	first := []*balancer.Backend{{Addr: "backend-1:5678", Weight: 1}}
	pool := NewPool("round_robin", first, stubChecker{})

	metrics := NewMetrics()
	metrics.Register(pool)

	pool.Reload("least_connections", []*balancer.Backend{{Addr: "backend-2:5678", Weight: 2}})

	body := scrape(t, metrics.Handler())
	if strings.Contains(body, `backend="backend-1:5678"`) {
		t.Error("a backend removed by a reload is still reported")
	}
	if !strings.Contains(body, `lb_backend_healthy{backend="backend-2:5678"} 1`) {
		t.Error("the backend added by a reload is not reported")
	}

	recorder := httptest.NewRecorder()
	Endpoints(metrics, pool, false).ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/status", nil))

	var reported status
	if err := json.NewDecoder(recorder.Body).Decode(&reported); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if reported.Algorithm != "least_connections" {
		t.Errorf("algorithm = %q, want the reloaded one", reported.Algorithm)
	}
	if reported.Reloads != 1 {
		t.Errorf("reloads = %d, want 1", reported.Reloads)
	}
}

func TestReloadOutcomesAreCounted(t *testing.T) {
	metrics := NewMetrics()
	metrics.ObserveReload("applied")
	metrics.ObserveReload("rejected")
	metrics.ObserveReload("rejected")

	body := scrape(t, metrics.Handler())
	for _, want := range []string{
		`lb_config_reloads_total{result="applied"} 1`,
		`lb_config_reloads_total{result="rejected"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics do not contain %q", want)
		}
	}
}
