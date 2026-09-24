package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// readMetrics scrapes the admin port the way Prometheus does, with Prometheus's
// own parser, and returns every sample keyed by its name and label, and every
// family's type.
func readMetrics(t *testing.T, s *server) (values map[string]float64, types map[string]string) {
	t.Helper()

	recorder := httptest.NewRecorder()
	s.adminHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/metrics answered %d", recorder.Code)
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(recorder.Body)
	if err != nil {
		t.Fatalf("Prometheus cannot read the metrics: %v", err)
	}

	values, types = map[string]float64{}, map[string]string{}
	for name, family := range families {
		types[name] = strings.ToLower(family.GetType().String())
		for _, metric := range family.GetMetric() {
			key := name
			for _, label := range metric.GetLabel() {
				key += "{" + label.GetName() + `="` + label.GetValue() + `"}`
			}
			values[key] = metric.GetCounter().GetValue() + metric.GetGauge().GetValue()
		}
	}
	return values, types
}

func TestPrometheusCanReadEveryMetricFromTheStart(t *testing.T) {
	s := newServer(profile{name: "backend-1", capacity: 8, cacheSize: 10}, 1)
	values, types := readMetrics(t, s)

	for name, kind := range map[string]string{
		"mock_requests_total": "counter", "mock_cache_requests_total": "counter",
		"mock_faults_injected_total": "counter", "mock_fault_in_force": "gauge",
		"mock_cache_keys": "gauge", "mock_busy_workers": "gauge", "mock_capacity": "gauge",
		"mock_waiting": "gauge", "mock_cold_factor": "gauge",
	} {
		if types[name] != kind {
			t.Errorf("%s is a %q, want a %s", name, types[name], kind)
		}
	}

	// Every labelled series is there at zero before anything has happened, so
	// that the first event shows as a change.
	for _, key := range []string{
		`mock_requests_total{outcome="answered"}`, `mock_requests_total{outcome="abandoned"}`,
		`mock_requests_total{outcome="overloaded"}`, `mock_cache_requests_total{result="miss"}`,
		`mock_faults_injected_total{mode="freeze"}`, `mock_fault_in_force{mode="hang"}`,
	} {
		if value, present := values[key]; !present || value != 0 {
			t.Errorf("%s is %v (present: %v), want 0 from the start", key, value, present)
		}
	}
	if got := len(values); got != 7+2+6+6+5 {
		t.Errorf("%d series, want 26", got)
	}
	if values["mock_capacity"] != 8 || values["mock_cold_factor"] != 1 {
		t.Errorf("capacity %v and cold factor %v, want 8 and 1", values["mock_capacity"], values["mock_cold_factor"])
	}
}

func TestMetricsCountWhatTheBackendDidWithEachRequest(t *testing.T) {
	s := newServer(profile{name: "backend-1", cacheSize: 10}, 1)

	get(t, s, "/")
	_, _ = admin(t, s, http.MethodPost, "/faults", "mode=error")
	get(t, s, "/")
	sendAs(s, "session-1")
	sendAs(s, "session-1")

	values, _ := readMetrics(t, s)
	for key, want := range map[string]float64{
		`mock_requests_total{outcome="answered"}`:   1,
		`mock_requests_total{outcome="error"}`:      3,
		`mock_cache_requests_total{result="miss"}`:  1,
		`mock_cache_requests_total{result="hit"}`:   1,
		`mock_cache_keys`:                           1,
		`mock_faults_injected_total{mode="error"}`:  1,
		`mock_fault_in_force{mode="error"}`:         1,
		`mock_requests_total{outcome="hang"}`:       0,
		`mock_requests_total{outcome="abandoned"}`:  0,
		`mock_requests_total{outcome="overloaded"}`: 0,
		`mock_faults_injected_total{mode="freeze"}`: 0,
		`mock_fault_in_force{mode="freeze"}`:        0,
	} {
		if values[key] != want {
			t.Errorf("%s = %v, want %v", key, values[key], want)
		}
	}
}

func TestAClearedFaultIsNoLongerInForceButStaysCounted(t *testing.T) {
	s := newServer(profile{name: "backend-1"}, 1)

	_, _ = admin(t, s, http.MethodPost, "/faults", "mode=slow&for=1m")
	_, _ = admin(t, s, http.MethodDelete, "/faults", "")

	values, _ := readMetrics(t, s)
	if values[`mock_fault_in_force{mode="slow"}`] != 0 || values[`mock_faults_injected_total{mode="slow"}`] != 1 {
		t.Errorf("in force %v, injected %v; want 0 and 1",
			values[`mock_fault_in_force{mode="slow"}`], values[`mock_faults_injected_total{mode="slow"}`])
	}
}

func TestAClientThatLeavesFirstIsCountedAsAbandoned(t *testing.T) {
	s := newServer(profile{name: "backend-1", latency: time.Second}, 1)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	s.handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil))

	values, _ := readMetrics(t, s)
	if values[`mock_requests_total{outcome="abandoned"}`] != 1 || values[`mock_requests_total{outcome="answered"}`] != 0 {
		t.Errorf("abandoned %v, answered %v; want 1 and 0",
			values[`mock_requests_total{outcome="abandoned"}`], values[`mock_requests_total{outcome="answered"}`])
	}
}

func TestABusyBackendShowsItsWorkersAndCountsWhatItRefuses(t *testing.T) {
	s := newServer(profile{name: "backend-1", capacity: 1}, 1)
	_, _ = admin(t, s, http.MethodPost, "/faults", "mode=hang")

	ctx, cancel := context.WithCancel(t.Context())
	held := make(chan struct{})
	go func() {
		defer close(held)
		s.handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil))
	}()
	eventually(t, func() bool { return s.busy.Load() == 1 }, "the hanging request holds the only worker")

	if status, _, _ := get(t, s, "/"); status != http.StatusServiceUnavailable {
		t.Fatalf("a second request was answered %d, want 503 with the only worker held", status)
	}
	values, _ := readMetrics(t, s)
	if values["mock_busy_workers"] != 1 || values[`mock_requests_total{outcome="overloaded"}`] != 1 {
		t.Errorf("busy %v, overloaded %v; want 1 and 1",
			values["mock_busy_workers"], values[`mock_requests_total{outcome="overloaded"}`])
	}

	cancel()
	<-held
	values, _ = readMetrics(t, s)
	if values["mock_busy_workers"] != 0 || values[`mock_requests_total{outcome="hang"}`] != 1 {
		t.Errorf("busy %v, hang %v once the client gave up; want 0 and 1",
			values["mock_busy_workers"], values[`mock_requests_total{outcome="hang"}`])
	}
}

func TestADroppedAnswerIsCountedOnTheWayOut(t *testing.T) {
	s := newServer(profile{name: "backend-1", bodySize: 1024}, 1)
	_, _ = admin(t, s, http.MethodPost, "/faults", "mode=reset")

	if response, err := http.Get(serveOverHTTP(t, s)); err == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}

	eventually(t, func() bool {
		values, _ := readMetrics(t, s)
		return values[`mock_requests_total{outcome="reset"}`] == 1
	}, "the dropped answer is counted as a reset")
}

func TestTheMetricsAnswerWhileTheBackendIsFrozen(t *testing.T) {
	s := newServer(profile{name: "backend-1"}, 1)
	_, _ = admin(t, s, http.MethodPost, "/faults", "mode=freeze&for=1m")

	read := make(chan map[string]float64, 1)
	go func() {
		values, _ := readMetrics(t, s)
		read <- values
	}()
	select {
	case values := <-read:
		if values[`mock_fault_in_force{mode="freeze"}`] != 1 {
			t.Errorf("the freeze is not shown in force")
		}
	case <-time.After(time.Second):
		t.Fatal("the metrics waited for the freeze to end")
	}
}

func TestAColdBackendShowsItsFactor(t *testing.T) {
	s := newServer(profile{name: "backend-1", coldStart: time.Minute, coldFactor: 4}, 1)

	values, _ := readMetrics(t, s)
	if factor := values["mock_cold_factor"]; factor <= 3.9 || factor > 4 {
		t.Errorf("cold factor %v just after starting, want close to 4", factor)
	}
}
