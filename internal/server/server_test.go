package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// testConfig listens on a port picked by the operating system, so that tests
// never collide with a busy port.
func testConfig() *config.Config {
	return &config.Config{
		ListenAddr: "127.0.0.1:0",
		Timeouts: config.Timeouts{
			ReadTimeout:  config.Duration(5 * time.Second),
			WriteTimeout: config.Duration(5 * time.Second),
			IdleTimeout:  config.Duration(5 * time.Second),
		},
	}
}

// start runs a server with the given handler and returns its base URL together
// with the channel carrying the result of Run.
func start(t *testing.T, ctx context.Context, handler http.Handler) (string, <-chan error) {
	t.Helper()

	srv, err := New(testConfig(), handler)
	if err != nil {
		t.Fatalf("New returned an unexpected error: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	return "http://" + srv.Addr(), done
}

func TestServesRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	url, done := start(t, ctx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "served")
	}))

	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "served" {
		t.Errorf("body = %q, want %q", body, "served")
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v, want a clean shutdown", err)
	}
}

func TestShutdownWaitsForInFlightRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	entered := make(chan struct{})
	url, done := start(t, ctx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		// Long enough that the shutdown below starts while this is running.
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "finished")
	}))

	type result struct {
		body string
		err  error
	}
	responses := make(chan result, 1)
	go func() {
		response, err := http.Get(url)
		if err != nil {
			responses <- result{err: err}
			return
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		responses <- result{body: string(body), err: err}
	}()

	<-entered
	cancel()

	got := <-responses
	if got.err != nil {
		t.Fatalf("in-flight request failed during shutdown: %v", got.err)
	}
	if got.body != "finished" {
		t.Errorf("body = %q, want the handler to run to completion", got.body)
	}
	if err := <-done; err != nil {
		t.Errorf("Run returned %v, want a clean shutdown", err)
	}
}

func TestShutdownStopsAcceptingRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	url, done := start(t, ctx, http.NotFoundHandler())

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want a clean shutdown", err)
	}

	if _, err := http.Get(url); err == nil {
		t.Error("the server still accepts requests after shutdown")
	}
}

func TestNewRejectsUnavailableAddress(t *testing.T) {
	cfg := testConfig()
	srv, err := New(cfg, http.NotFoundHandler())
	if err != nil {
		t.Fatalf("New returned an unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = srv.listener.Close() })

	taken := testConfig()
	taken.ListenAddr = srv.Addr()
	if _, err := New(taken, http.NotFoundHandler()); err == nil {
		t.Errorf("New accepted the address %s that is already in use", taken.ListenAddr)
	}
}

func TestNewRejectsMalformedAddress(t *testing.T) {
	cfg := testConfig()
	cfg.ListenAddr = "127.0.0.1:not-a-port"

	if _, err := New(cfg, http.NotFoundHandler()); err == nil {
		t.Error("New accepted a malformed address")
	} else if want := fmt.Sprintf("listen on %s", cfg.ListenAddr); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to mention %q", err, want)
	}
}
