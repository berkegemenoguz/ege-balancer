package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdminURLFollowsTheComposeFile(t *testing.T) {
	for service, want := range map[string]string{
		"backend-1":  "http://127.0.0.1:5781",
		"backend-10": "http://127.0.0.1:5790",
	} {
		if got, err := adminURL(service, 5781); err != nil || got != want {
			t.Errorf("adminURL(%q) = %q %v, want %q", service, got, err, want)
		}
	}
	for _, service := range []string{"loadbalancer", "backend-0", "backend-x"} {
		if _, err := adminURL(service, 5781); err == nil {
			t.Errorf("adminURL(%q) succeeded, want it refused", service)
		}
	}
}

func TestFaultFormCarriesTheStrength(t *testing.T) {
	kinds := map[string]faultKind{}
	for _, kind := range faultKinds {
		kinds[kind.mode] = kind
	}

	for _, test := range []struct {
		kind     string
		strength string
		want     string
	}{
		{"hang", "0.5", "for=30s&mode=hang&rate=0.5"},
		{"slow", "3", "factor=3&for=30s&mode=slow"},
		{"freeze", "", "for=30s&mode=freeze"},
	} {
		if got := faultForm(kinds[test.kind], test.strength, 30*time.Second).Encode(); got != test.want {
			t.Errorf("form for %s = %q, want %q", test.kind, got, test.want)
		}
	}
}

func TestFaultBookShowsWhatIsStillInForce(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	book := newFaultBook(func() time.Time { return now })

	book.add("backend-3", "hanging", 30*time.Second)
	book.add("backend-7", "slow", 10*time.Second)
	if got := book.summary(); got != "backend-7 slow 10s, backend-3 hanging 30s" {
		t.Errorf("summary = %q, want the soonest to end first", got)
	}

	now = now.Add(15 * time.Second)
	if got := book.summary(); got != "backend-3 hanging 15s" {
		t.Errorf("summary = %q, want only what has not ended", got)
	}

	book.clear()
	if book.any() {
		t.Errorf("summary = %q after clear, want nothing", book.summary())
	}
}

// adminStub records what reaches it and answers like a backend's admin port.
type adminStub struct {
	mu       sync.Mutex
	requests []string
}

func (a *adminStub) serve(t *testing.T, answer int) *environment {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		a.mu.Lock()
		a.requests = append(a.requests, r.Method+" "+r.URL.Path+" "+r.PostForm.Encode())
		a.mu.Unlock()

		if answer != http.StatusOK {
			http.Error(w, "mode must be one of hang, reset, drip, error, slow or freeze", answer)
			return
		}
		_, _ = io.WriteString(w, `{"injected":[{"mode":"hang","rate":0.5,"remaining":"29.9s"}]}`)
	}))
	t.Cleanup(server.Close)

	address, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(address.Port())
	// backend-1's admin port is the first one, so it lands on the stub.
	return &environment{adminPort: port, client: server.Client()}
}

func TestTheConsoleReachesTheAdminPort(t *testing.T) {
	stub := &adminStub{}
	env := stub.serve(t, http.StatusOK)
	con := &console{out: io.Discard}
	ctx := context.Background()

	form := url.Values{"mode": {"hang"}, "rate": {"0.5"}, "for": {"30s"}}
	if err := env.injectFault(ctx, con, "backend-1", form); err != nil {
		t.Fatalf("injecting failed: %v", err)
	}
	env.clearFaults(ctx, con, []string{"backend-1"})

	faults, err := env.injectedFaults(ctx, "backend-1")
	if err != nil || len(faults) != 1 || faults[0].String() != "hang on 50% of requests, 29.9s left" {
		t.Errorf("faults = %v %v, want the hang the stub reports", faults, err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	want := []string{"POST /faults for=30s&mode=hang&rate=0.5", "DELETE /faults ", "GET /faults "}
	if strings.Join(stub.requests, "|") != strings.Join(want, "|") {
		t.Errorf("the admin port saw %q, want %q", stub.requests, want)
	}
}

func TestTheBackendsReasonIsReported(t *testing.T) {
	env := (&adminStub{}).serve(t, http.StatusBadRequest)

	err := env.injectFault(context.Background(), &console{out: io.Discard}, "backend-1", url.Values{"mode": {"explode"}})
	if err == nil || !strings.Contains(err.Error(), "mode must be one of") {
		t.Errorf("error = %v, want the backend's reason passed on", err)
	}
}
