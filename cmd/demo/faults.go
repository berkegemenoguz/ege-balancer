package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxFaultDuration mirrors the limit the backends' admin port enforces, so the
// console can refuse a longer one before sending it.
const maxFaultDuration = 10 * time.Minute

// faultKind is one misbehaviour the console offers.
type faultKind struct {
	mode     string
	describe string
	// strength is what to ask for: "rate" for a share of requests, "factor"
	// for how many times slower, and nothing for freeze.
	strength string
	// doing names the backend's state in the prompt.
	doing string
	// expect tells whoever is watching what to look for.
	expect string
}

// faultKinds are the misbehaviours offered, in menu order.
var faultKinds = []faultKind{
	{"hang", "hang without answering", "rate", "hanging",
		"the balancer abandons each hung request after response_timeout (3s in the example configuration) and retries a GET elsewhere; a POST is refused as not_retryable, since the backend may have carried it out"},
	{"slow", "answer several times slower", "factor", "slow",
		"least connections moves traffic away from it; round robin keeps sending its share"},
	{"error", "answer 500", "rate", "failing",
		"the 500s reach the client unless retry_on_5xx is set; the Resilience dashboard shows them"},
	{"reset", "drop the connection half way through the answer", "rate", "dropping",
		"nothing can retry an answer already on its way: the client sees its connection close, and the balancer counts the failure against the backend"},
	{"drip", "drip the answer out over two seconds", "rate", "dripping",
		"answers arrive whole but late; watch p99 on the Overview dashboard"},
	{"freeze", "freeze entirely, health check included", "", "frozen",
		"its health checks time out as well, so it leaves the pool after three failed checks"},
}

// adminURL is the admin port of a mock backend on this machine. The compose
// file publishes backend-N's admin port at firstPort+N-1, on the loopback
// interface only.
func adminURL(service string, firstPort int) (string, error) {
	number, found := strings.CutPrefix(service, "backend-")
	n, err := strconv.Atoi(number)
	if !found || err != nil || n < 1 {
		return "", fmt.Errorf("%q is not a mock backend", service)
	}
	return "http://127.0.0.1:" + strconv.Itoa(firstPort+n-1), nil
}

// faultForm is what the admin port is sent to start a fault.
func faultForm(kind faultKind, strength string, lasts time.Duration) url.Values {
	form := url.Values{"mode": {kind.mode}, "for": {lasts.String()}}
	switch kind.strength {
	case "rate":
		form.Set("rate", strength)
	case "factor":
		form.Set("factor", strength)
	}
	return form
}

// injectFault asks one backend to misbehave.
func (e *environment) injectFault(ctx context.Context, con *console, service string, form url.Values) error {
	base, err := adminURL(service, e.adminPort)
	if err != nil {
		return err
	}
	con.echo(fmt.Sprintf("curl -s -X POST %s/faults -d '%s'", base, form.Encode()))
	return e.admin(ctx, http.MethodPost, base+"/faults", form)
}

// clearFaults removes the injected faults of every backend. A backend that is
// stopped has none to remove, so a failure to reach one is not reported.
func (e *environment) clearFaults(ctx context.Context, con *console, services []string) {
	if len(services) == 0 {
		return
	}
	con.echo(fmt.Sprintf("for port in $(seq %d %d); do curl -s -X DELETE 127.0.0.1:$port/faults >/dev/null; done",
		e.adminPort, e.adminPort+len(services)-1))

	for _, service := range services {
		if base, err := adminURL(service, e.adminPort); err == nil {
			_ = e.admin(ctx, http.MethodDelete, base+"/faults", nil)
		}
	}
}

// reportedFault is a fault a backend says is in force.
type reportedFault struct {
	Mode      string  `json:"mode"`
	Rate      float64 `json:"rate"`
	Factor    float64 `json:"factor"`
	Remaining string  `json:"remaining"`
}

// String renders the fault for the status list.
func (f reportedFault) String() string {
	switch {
	case f.Factor > 0:
		return fmt.Sprintf("%s ×%g, %s left", f.Mode, f.Factor, f.Remaining)
	case f.Rate > 0:
		return fmt.Sprintf("%s on %g%% of requests, %s left", f.Mode, 100*f.Rate, f.Remaining)
	}
	return fmt.Sprintf("%s, %s left", f.Mode, f.Remaining)
}

// injectedFaults reads the faults one backend reports in force.
func (e *environment) injectedFaults(ctx context.Context, service string) ([]reportedFault, error) {
	base, err := adminURL(service, e.adminPort)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/faults", nil)
	if err != nil {
		return nil, err
	}
	response, err := e.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	var state struct {
		Injected []reportedFault `json:"injected"`
	}
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		return nil, err
	}
	return state.Injected, nil
}

// admin sends one request to an admin port and reports any answer but 200,
// with the reason the backend gave.
func (e *environment) admin(ctx context.Context, method, target string, form url.Values) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	response, err := e.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		reason, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("%s answered %s: %s", target, response.Status, strings.TrimSpace(string(reason)))
	}
	return nil
}

// faultBook remembers the faults this console started and when each ends, so
// that the prompt can show them without asking every backend each time.
type faultBook struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]time.Time // "backend-3 hanging" to when it ends
}

// newFaultBook returns an empty book reading the time from now.
func newFaultBook(now func() time.Time) *faultBook {
	return &faultBook{now: now, entries: map[string]time.Time{}}
}

// add records that service is doing something for the given time.
func (b *faultBook) add(service, doing string, lasts time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries[service+" "+doing] = b.now().Add(lasts)
}

// clear forgets every fault.
func (b *faultBook) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.entries)
}

// any reports whether a fault the console started may still be in force.
func (b *faultBook) any() bool {
	return b.summary() != ""
}

// summary lists the faults still in force, the soonest to end first, as the
// prompt shows them: "backend-7 slow 8s, backend-3 hanging 22s".
func (b *faultBook) summary() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	type left struct {
		what string
		for_ time.Duration
	}
	now := b.now()
	var running []left
	for what, until := range b.entries {
		if remaining := until.Sub(now); remaining > 0 {
			running = append(running, left{what, remaining.Round(time.Second)})
		} else {
			delete(b.entries, what)
		}
	}
	slices.SortFunc(running, func(a, b left) int { return int(a.for_ - b.for_) })

	parts := make([]string, 0, len(running))
	for _, r := range running {
		parts = append(parts, r.what+" "+r.for_.String())
	}
	return strings.Join(parts, ", ")
}
