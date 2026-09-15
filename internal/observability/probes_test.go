package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
)

// probe requests path from the admin endpoints and returns the status and body.
func probe(t *testing.T, handler http.Handler, path string) (int, string) {
	t.Helper()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Code, strings.TrimSpace(recorder.Body.String())
}

func TestReadinessFollowsThePoolAndShutdown(t *testing.T) {
	backends := []*balancer.Backend{
		balancer.NewBackend("backend-1:5678", 1),
		balancer.NewBackend("backend-2:5678", 1),
	}

	tests := []struct {
		name      string
		unhealthy map[string]bool
		draining  bool
		wantCode  int
		wantBody  string
	}{
		{"every backend healthy", nil, false, http.StatusOK, "ok"},
		{"one backend healthy", map[string]bool{"backend-1:5678": true}, false, http.StatusOK, "ok"},
		{
			"no backend healthy",
			map[string]bool{"backend-1:5678": true, "backend-2:5678": true}, false,
			http.StatusServiceUnavailable, "no healthy backend",
		},
		{"shutting down", nil, true, http.StatusServiceUnavailable, "shutting down"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := NewPool("round_robin", backends, stubChecker{unhealthy: test.unhealthy})
			probes := NewProbes(pool)
			if test.draining {
				probes.Drain()
			}

			code, body := probe(t, Endpoints(NewMetrics(), pool, probes, false), "/readyz")
			if code != test.wantCode || body != test.wantBody {
				t.Errorf("/readyz answered %d %q, want %d %q", code, body, test.wantCode, test.wantBody)
			}
		})
	}
}

func TestLivenessIgnoresThePoolAndShutdown(t *testing.T) {
	backends := []*balancer.Backend{balancer.NewBackend("backend-1:5678", 1)}
	pool := NewPool("round_robin", backends, stubChecker{unhealthy: map[string]bool{
		"backend-1:5678": true,
	}})
	probes := NewProbes(pool)
	probes.Drain()

	code, body := probe(t, Endpoints(NewMetrics(), pool, probes, false), "/healthz")
	if code != http.StatusOK || body != "ok" {
		t.Errorf("/healthz answered %d %q, want %d %q", code, body, http.StatusOK, "ok")
	}
}
