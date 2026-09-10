package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// testConfig probes often enough that a test never waits noticeably.
func testConfig() config.HealthCheck {
	return config.HealthCheck{
		Path:               "/healthz",
		Interval:           config.Duration(5 * time.Millisecond),
		Timeout:            config.Duration(time.Second),
		HealthyThreshold:   2,
		UnhealthyThreshold: 3,
	}
}

// eventually waits up to a second for condition to hold.
func eventually(t *testing.T, condition func() bool, describe string) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", describe)
}

func TestUnknownBackendIsTreatedAsHealthy(t *testing.T) {
	if !New(testConfig()).IsHealthy("backend-1:5678") {
		t.Error("an unregistered backend must be allowed to serve")
	}
}

func TestPassiveCheckRemovesAndRestoresBackend(t *testing.T) {
	cfg := testConfig()
	checker := New(cfg)
	// A cancelled context stops the active probes immediately, leaving the
	// passive path alone under test.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	checker.Start(ctx, []*balancer.Backend{{Addr: "backend-1:5678"}})

	for range cfg.UnhealthyThreshold - 1 {
		checker.ReportFailure("backend-1:5678")
	}
	if !checker.IsHealthy("backend-1:5678") {
		t.Fatalf("backend left the pool before %d consecutive failures", cfg.UnhealthyThreshold)
	}

	checker.ReportFailure("backend-1:5678")
	if checker.IsHealthy("backend-1:5678") {
		t.Fatalf("backend stayed in the pool after %d consecutive failures", cfg.UnhealthyThreshold)
	}

	for range cfg.HealthyThreshold - 1 {
		checker.ReportSuccess("backend-1:5678")
	}
	if checker.IsHealthy("backend-1:5678") {
		t.Fatalf("backend rejoined before %d consecutive successes", cfg.HealthyThreshold)
	}

	checker.ReportSuccess("backend-1:5678")
	if !checker.IsHealthy("backend-1:5678") {
		t.Fatalf("backend did not rejoin after %d consecutive successes", cfg.HealthyThreshold)
	}
}

func TestSingleSuccessResetsTheFailureStreak(t *testing.T) {
	cfg := testConfig()
	checker := New(cfg)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	checker.Start(ctx, []*balancer.Backend{{Addr: "backend-1:5678"}})

	checker.ReportFailure("backend-1:5678")
	checker.ReportFailure("backend-1:5678")
	checker.ReportSuccess("backend-1:5678")
	checker.ReportFailure("backend-1:5678")
	checker.ReportFailure("backend-1:5678")

	if !checker.IsHealthy("backend-1:5678") {
		t.Error("the failure streak was not reset by the success in between")
	}
}

func TestReportsForUnknownBackendAreIgnored(t *testing.T) {
	checker := New(testConfig())

	for range 10 {
		checker.ReportFailure("nowhere:1234")
	}
	if !checker.IsHealthy("nowhere:1234") {
		t.Error("reports for an unregistered backend must not change anything")
	}
}

func TestActiveCheckFollowsTheBackend(t *testing.T) {
	var probes atomic.Int64
	var serving atomic.Bool
	serving.Store(true)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probe requested %q, want %q", r.URL.Path, "/healthz")
		}
		probes.Add(1)
		if !serving.Load() {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer backend.Close()

	addr := strings.TrimPrefix(backend.URL, "http://")
	checker := New(testConfig())
	checker.Start(t.Context(), []*balancer.Backend{{Addr: addr}})

	eventually(t, func() bool { return probes.Load() > 0 }, "the first probe has run")
	if !checker.IsHealthy(addr) {
		t.Fatal("a backend answering 200 was taken out of the pool")
	}

	serving.Store(false)
	eventually(t, func() bool { return !checker.IsHealthy(addr) }, "the failing backend is taken out")

	serving.Store(true)
	eventually(t, func() bool { return checker.IsHealthy(addr) }, "the recovered backend rejoins")
}

func TestActiveCheckMarksUnreachableBackendUnhealthy(t *testing.T) {
	// Port 1 on the loopback interface is not served by anything.
	checker := New(testConfig())
	checker.Start(t.Context(), []*balancer.Backend{{Addr: "127.0.0.1:1"}})

	eventually(t, func() bool { return !checker.IsHealthy("127.0.0.1:1") },
		"the unreachable backend is taken out")
}

func TestProbingStopsWhenContextIsCancelled(t *testing.T) {
	var probes atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
	}))
	defer backend.Close()

	ctx, cancel := context.WithCancel(t.Context())
	checker := New(testConfig())
	checker.Start(ctx, []*balancer.Backend{{Addr: strings.TrimPrefix(backend.URL, "http://")}})

	eventually(t, func() bool { return probes.Load() > 0 }, "probing has started")
	cancel()

	// Allow the in-flight probe to finish, then confirm no further ones arrive.
	time.Sleep(50 * time.Millisecond)
	stopped := probes.Load()
	time.Sleep(50 * time.Millisecond)

	if got := probes.Load(); got != stopped {
		t.Errorf("probes continued after cancellation: %d then %d", stopped, got)
	}
}

// TestCheckerIsConcurrencySafe exercises the read and write paths together, the
// way the proxy does under load.
func TestCheckerIsConcurrencySafe(t *testing.T) {
	const goroutines = 50

	checker := New(testConfig())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	checker.Start(ctx, []*balancer.Backend{{Addr: "backend-1:5678"}})

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if i%2 == 0 {
					checker.ReportFailure("backend-1:5678")
				} else {
					checker.ReportSuccess("backend-1:5678")
				}
				checker.IsHealthy("backend-1:5678")
			}
		}()
	}
	wg.Wait()
}

func TestReloadKeepsWhatIsKnownAboutSurvivingBackends(t *testing.T) {
	cfg := testConfig()
	checker := New(cfg)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // no active probing; the passive path is what is under test

	const (
		staying = "backend-1:5678"
		leaving = "backend-2:5678"
		joining = "backend-3:5678"
	)

	checker.Start(ctx, []*balancer.Backend{{Addr: staying}, {Addr: leaving}})
	for range cfg.UnhealthyThreshold {
		checker.ReportFailure(staying)
	}
	if checker.IsHealthy(staying) {
		t.Fatal("the backend did not leave the pool")
	}

	checker.Reload(ctx, cfg, []*balancer.Backend{{Addr: staying}, {Addr: joining}})

	if checker.IsHealthy(staying) {
		t.Error("a reload handed a known-unhealthy backend a clean slate")
	}
	if !checker.IsHealthy(joining) {
		t.Error("a backend added by a reload did not start healthy")
	}

	// The removed backend is forgotten, so reports about it change nothing.
	for range cfg.UnhealthyThreshold {
		checker.ReportFailure(leaving)
	}
	if !checker.IsHealthy(leaving) {
		t.Error("a backend dropped by a reload is still being tracked")
	}
}

func TestReloadAppliesNewThresholds(t *testing.T) {
	cfg := testConfig()
	checker := New(cfg)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	checker.Start(ctx, []*balancer.Backend{{Addr: "backend-1:5678"}})

	stricter := cfg
	stricter.UnhealthyThreshold = 1
	checker.Reload(ctx, stricter, []*balancer.Backend{{Addr: "backend-1:5678"}})

	checker.ReportFailure("backend-1:5678")
	if checker.IsHealthy("backend-1:5678") {
		t.Error("the reloaded threshold of one failure was not applied")
	}
}
