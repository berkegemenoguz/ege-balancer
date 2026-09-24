package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
)

func TestEachFailureIsCountedWithItsReason(t *testing.T) {
	var received atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer slow.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unwell", http.StatusInternalServerError)
	}))
	defer failing.Close()

	tests := []struct {
		reason  string
		backend *balancer.Backend
	}{
		{reasonConnect, &balancer.Backend{Addr: unreachable}},
		{reasonTimeout, &balancer.Backend{Addr: strings.TrimPrefix(slow.URL, "http://")}},
		{reasonReset, silentBackend(t, &received)},
		{reasonCutOff, abortingBackend(t)},
		{reason5xx, &balancer.Backend{Addr: strings.TrimPrefix(failing.URL, "http://")}},
	}
	for _, test := range tests {
		t.Run(test.reason, func(t *testing.T) {
			cfg := testConfig()
			cfg.Timeouts.ResponseTimeout = config.Duration(50 * time.Millisecond)
			cfg.RetryOn5xx = true
			metrics := observability.NewMetrics()

			_ = fetch(t, New(cfg, balancer.NewRoundRobin(), []*balancer.Backend{test.backend}, allHealthy, metrics))

			recorder := httptest.NewRecorder()
			metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			want := `lb_backend_failures_total{backend="` + test.backend.Addr + `",reason="` + test.reason + `"} 1`
			if body := recorder.Body.String(); !strings.Contains(body, want) {
				t.Errorf("metrics do not contain %q; failures counted:\n%s", want, countedFailures(body))
			}
		})
	}
}

// countedFailures keeps the failure counters of a scrape that are above zero.
func countedFailures(body string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "lb_backend_failures_total{") && !strings.HasSuffix(line, " 0") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestEveryBackendsFailureCountersStartAtZero(t *testing.T) {
	metrics := observability.NewMetrics()
	handler := New(testConfig(), balancer.NewRoundRobin(), []*balancer.Backend{{Addr: "backend-1:5678"}}, allHealthy, metrics)
	// A backend added by a reload is prepared as well.
	handler.Reload(testConfig(), balancer.NewRoundRobin(), []*balancer.Backend{{Addr: "backend-1:5678"}, {Addr: "backend-2:5678"}})

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, backend := range []string{"backend-1:5678", "backend-2:5678"} {
		for _, reason := range reasons {
			want := `lb_backend_failures_total{backend="` + backend + `",reason="` + reason + `"} 0`
			if !strings.Contains(recorder.Body.String(), want) {
				t.Errorf("metrics do not contain %q before any failure", want)
			}
		}
	}
}

func TestFailureReasonLooksThroughWrapping(t *testing.T) {
	read := func(err error) error { return &net.OpError{Op: "read", Net: "tcp", Err: err} }

	tests := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("attempt: %w", errRetryable5xx), reason5xx},
		{&net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, reasonConnect},
		{read(os.ErrDeadlineExceeded), reasonTimeout},
		{read(os.NewSyscallError("read", syscall.ECONNRESET)), reasonReset},
		{read(os.NewSyscallError("write", syscall.EPIPE)), reasonReset},
		{fmt.Errorf("reading the answer: %w", io.ErrUnexpectedEOF), reasonReset},
		{io.EOF, reasonReset},
		{errors.New("something nobody expected"), reasonOther},
	}
	for _, test := range tests {
		if got := failureReason(test.err); got != test.want {
			t.Errorf("failureReason(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}
