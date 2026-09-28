package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/health"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
)

// hashedConfig places requests by their X-Session header, retrying twice, with
// the load bounded at balanceFactor per cent.
func hashedConfig(balanceFactor int) *config.Config {
	cfg := retryingConfig()
	cfg.Algorithm = config.ConsistentHashing
	cfg.ConsistentHash = config.ConsistentHash{Key: "header:X-Session", BalanceFactor: balanceFactor}
	return cfg
}

// namedBackends starts n echo backends, each answering with its index.
func namedBackends(t *testing.T, n int) []*balancer.Backend {
	t.Helper()
	backends := make([]*balancer.Backend, 0, n)
	for i := range n {
		var served int
		backends = append(backends, echoBackend(t, strconv.Itoa(i), &served))
	}
	return backends
}

// hashedHandler is a consistent hashing handler over backends.
func hashedHandler(cfg *config.Config, backends []*balancer.Backend, checker health.Checker, metrics *observability.Metrics) *Handler {
	return New(cfg, balancer.NewConsistentHash(cfg.ConsistentHash.BalanceFactor), backends, checker, metrics)
}

// sendAs sends one request naming session, or none when session is empty, and
// returns the answer.
func sendAs(handler http.Handler, session string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if session != "" {
		request.Header.Set("X-Session", session)
	}
	return send(handler, request)
}

// keyHomedOn returns a session key whose home among backends is want.
func keyHomedOn(t *testing.T, backends []*balancer.Backend, want *balancer.Backend) string {
	t.Helper()
	strategy := balancer.NewConsistentHash(0)
	for i := range 1000 {
		if key := "session-" + strconv.Itoa(i); strategy.Home(key, backends) == want {
			return key
		}
	}
	t.Fatalf("no key belongs to %s", want.Addr)
	return ""
}

// placed reads how many requests were counted with placement.
func placed(t *testing.T, metrics *observability.Metrics, placement string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	prefix := `lb_hash_placements_total{placement="` + placement + `"} `
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if count, found := strings.CutPrefix(line, prefix); found {
			return count
		}
	}
	t.Fatalf("no placement %q in the metrics", placement)
	return ""
}

func TestRequestsWithTheSameKeyReachTheSameBackend(t *testing.T) {
	metrics := observability.NewMetrics()
	handler := hashedHandler(hashedConfig(0), namedBackends(t, 5), allHealthy, metrics)

	reached := map[string]bool{}
	for i := range 20 {
		session := "session-" + strconv.Itoa(i)
		first := sendAs(handler, session).Body.String()
		for range 5 {
			if again := sendAs(handler, session).Body.String(); again != first {
				t.Fatalf("%s reached backend %s, then %s", session, first, again)
			}
		}
		reached[first] = true
	}

	if len(reached) < 3 {
		t.Errorf("20 keys reached only %d of 5 backends, want them spread", len(reached))
	}
	if got := placed(t, metrics, observability.PlacementHome); got != "120" {
		t.Errorf("%s requests counted as placed at home, want all 120", got)
	}
}

func TestAFailedAttemptIsRetriedOnTheKeysSecondChoice(t *testing.T) {
	backends := append(namedBackends(t, 4), &balancer.Backend{Addr: unreachable})
	home := backends[4]
	session := keyHomedOn(t, backends, home)
	second := balancer.NewConsistentHash(0).Home(session, backends[:4])

	metrics := observability.NewMetrics()
	handler := hashedHandler(hashedConfig(0), backends, allHealthy, metrics)

	for range 3 {
		answer := sendAs(handler, session)
		if answer.Code != http.StatusOK || answer.Body.String() != servedBy(backends, second) {
			t.Fatalf("answered %d by %q, want 200 from the key's second choice %q",
				answer.Code, answer.Body.String(), servedBy(backends, second))
		}
	}
	// Health checking has not taken the home out, so each first attempt still
	// went home: the retry is what moved the request, and a retry is not a
	// placement of its own.
	if got := placed(t, metrics, observability.PlacementHome); got != "3" {
		t.Errorf("%s requests placed at home, want 3", got)
	}
	if got := placed(t, metrics, observability.PlacementUnavailable); got != "0" {
		t.Errorf("%s requests counted as unavailable, want none: only first attempts are placements", got)
	}
}

// servedBy is the answer of the echo backend among backends.
func servedBy(backends []*balancer.Backend, backend *balancer.Backend) string {
	for i, candidate := range backends {
		if candidate == backend {
			return strconv.Itoa(i)
		}
	}
	return ""
}

func TestAKeyWhoseHomeIsOutOfThePoolIsCountedAsUnavailable(t *testing.T) {
	backends := namedBackends(t, 4)
	home := backends[1]
	session := keyHomedOn(t, backends, home)

	metrics := observability.NewMetrics()
	handler := hashedHandler(hashedConfig(0), backends, newFakeChecker(home.Addr), metrics)

	if got := sendAs(handler, session).Body.String(); got == servedBy(backends, home) {
		t.Fatal("the request reached the backend health checking had taken out")
	}
	if got := placed(t, metrics, observability.PlacementUnavailable); got != "1" {
		t.Errorf("%s requests counted as unavailable, want 1", got)
	}
}

func TestAKeyWhoseHomeIsOverItsBoundIsCountedAsOverloaded(t *testing.T) {
	backends := namedBackends(t, 4)
	home := backends[2]
	session := keyHomedOn(t, backends, home)
	for range 10 {
		home.Acquire()
	}
	t.Cleanup(func() {
		for range 10 {
			home.Release()
		}
	})

	metrics := observability.NewMetrics()
	handler := hashedHandler(hashedConfig(125), backends, allHealthy, metrics)

	if got := sendAs(handler, session).Body.String(); got == servedBy(backends, home) {
		t.Fatal("the request reached a home backend already over its bound")
	}
	if got := placed(t, metrics, observability.PlacementOverloaded); got != "1" {
		t.Errorf("%s requests counted as overloaded, want 1", got)
	}
}

func TestARequestWithoutAKeyIsPlacedByLoad(t *testing.T) {
	metrics := observability.NewMetrics()
	handler := hashedHandler(hashedConfig(0), namedBackends(t, 3), allHealthy, metrics)

	if answer := sendAs(handler, ""); answer.Code != http.StatusOK {
		t.Fatalf("a request without a key was answered %d, want 200", answer.Code)
	}
	if got := placed(t, metrics, observability.PlacementKeyless); got != "1" {
		t.Errorf("%s requests counted as keyless, want 1", got)
	}
}

func TestOtherStrategiesCountNoPlacement(t *testing.T) {
	metrics := observability.NewMetrics()
	handler := New(retryingConfig(), balancer.NewRoundRobin(), namedBackends(t, 3), allHealthy, metrics)
	sendAs(handler, "session-1")

	for _, placement := range []string{observability.PlacementHome, observability.PlacementKeyless} {
		if got := placed(t, metrics, placement); got != "0" {
			t.Errorf("round robin counted %s requests as %s, want none", got, placement)
		}
	}
}

func TestTheKeyIsReadFromTheConfiguredPlace(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "203.0.113.7:51234"
	request.Header.Set("X-Session", "session-1")

	for key, want := range map[string]string{
		"header:X-Session": "session-1",
		"header:X-Tenant":  "",
		"client_ip":        "203.0.113.7",
		"":                 "",
	} {
		if got := keyFor(config.ConsistentHash{Key: key})(request); got != want {
			t.Errorf("key %q read %q, want %q", key, got, want)
		}
	}
}
