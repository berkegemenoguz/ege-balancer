package integration

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// hashed switches cfg to consistent hashing on the X-Session header, with the
// load bounded at balanceFactor per cent.
func hashed(cfg *config.Config, balanceFactor int) *config.Config {
	cfg.Algorithm = config.ConsistentHashing
	cfg.ConsistentHash = config.ConsistentHash{Key: "header:X-Session", BalanceFactor: balanceFactor}
	return cfg
}

// getAs sends one request naming session and returns the backend that answered.
func (b *balancerUnderTest) getAs(t *testing.T, session string) string {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, b.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Session", session)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the body failed: %v", err)
	}
	return strings.TrimSpace(string(body))
}

// placeSessions sends one request for each of n sessions and returns the
// backend that answered each.
func (b *balancerUnderTest) placeSessions(t *testing.T, n int) map[string]string {
	t.Helper()
	placed := make(map[string]string, n)
	for i := range n {
		session := "session-" + strconv.Itoa(i)
		placed[session] = b.getAs(t, session)
	}
	return placed
}

// sendTogetherAs sends n requests naming the same session at once and counts
// the backends that answered them.
func (b *balancerUnderTest) sendTogetherAs(t *testing.T, session string, n int) map[string]int {
	t.Helper()

	var mu sync.Mutex
	answered := map[string]int{}
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			backend := b.getAs(t, session)
			mu.Lock()
			answered[backend]++
			mu.Unlock()
		})
	}
	wg.Wait()
	return answered
}

func TestConsistentHashingKeepsEverySessionOnItsBackend(t *testing.T) {
	backends := newBackends(t, 4)
	under := start(t, hashed(testConfig(backends), 0))

	placed := under.placeSessions(t, 40)
	if again := under.placeSessions(t, 40); len(again) != len(placed) {
		t.Fatal("sessions went missing")
	} else {
		for session, backend := range placed {
			if again[session] != backend {
				t.Errorf("%s went to %s, then %s", session, backend, again[session])
			}
		}
	}

	used := map[string]bool{}
	for _, backend := range placed {
		used[backend] = true
	}
	if len(used) != len(backends) {
		t.Errorf("40 sessions reached %d of %d backends, want every one", len(used), len(backends))
	}
}

func TestABackendThatDiesTakesOnlyItsOwnSessionsWithIt(t *testing.T) {
	backends := newBackends(t, 4)
	under := start(t, hashed(testConfig(backends), 0))
	before := under.placeSessions(t, 40)

	dead := backends[1]
	dead.kill()
	eventually(t, func() bool { return under.status(t).Healthy == 3 }, "health checking takes the dead backend out")

	after := under.placeSessions(t, 40)
	for session, was := range before {
		switch {
		case after[session] == dead.name:
			t.Errorf("%s still reached the dead backend", session)
		case was != dead.name && after[session] != was:
			t.Errorf("%s moved from %s to %s, though its backend is alive", session, was, after[session])
		}
	}
}

func TestReloadSwitchesToConsistentHashingAndItsBound(t *testing.T) {
	backends := newBackends(t, 3)
	under := start(t, pinHealth(testConfig(backends)))

	// Round robin sends one session everywhere.
	spread := map[string]bool{}
	for range 6 {
		spread[under.getAs(t, "session-1")] = true
	}
	if len(spread) != len(backends) {
		t.Fatalf("round robin sent session-1 to %d backends, want all %d", len(spread), len(backends))
	}

	under.reconfigure(t, hashed(pinHealth(testConfig(backends)), 0))
	if got := under.status(t).Algorithm; got != string(config.ConsistentHashing) {
		t.Fatalf("the algorithm is %s after the reload, want %s", got, config.ConsistentHashing)
	}

	// Slow every backend down, so that requests sent together overlap.
	for _, backend := range backends {
		backend.slowDown(200 * time.Millisecond)
	}

	// Without a bound, six requests for one session in flight at once all
	// wait on the same backend.
	if answered := under.sendTogetherAs(t, "session-1", 6); len(answered) != 1 {
		t.Fatalf("without a bound, session-1 was answered by %v, want one backend", answered)
	}

	// Only the bound changes; the reload must still rebuild the strategy.
	under.reconfigure(t, hashed(pinHealth(testConfig(backends)), 100))
	if answered := under.sendTogetherAs(t, "session-1", 6); len(answered) < 2 {
		t.Errorf("with a bound, session-1 was answered by %v, want the overflow spread", answered)
	}
}
