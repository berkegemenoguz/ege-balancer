package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/health"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
)

// secret is the sticky secret the tests sign with.
var secret = []byte("a secret the tests sign cookies with")

// stickyConfig keeps clients on their backend by cookie, over round robin,
// retrying twice.
func stickyConfig() *config.Config {
	cfg := retryingConfig()
	cfg.Sticky = config.Sticky{Mode: config.StickyCookie, Cookie: config.DefaultStickyCookie}
	return cfg
}

// stickyHandler is a round robin handler over backends with sticky cookies
// signed by secret.
func stickyHandler(cfg *config.Config, backends []*balancer.Backend, checker health.Checker, metrics *observability.Metrics) *Handler {
	return New(cfg, balancer.NewRoundRobin(), backends, checker, metrics, WithStickySecret(secret))
}

// sendWith sends one request carrying the given cookies.
func sendWith(handler http.Handler, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	return send(handler, request)
}

// pinOf returns the sticky cookie an answer set, or nil.
func pinOf(answer *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range answer.Result().Cookies() {
		if cookie.Name == config.DefaultStickyCookie {
			return cookie
		}
	}
	return nil
}

// counted reads how many requests were counted with the sticky result.
func counted(t *testing.T, metrics *observability.Metrics, result string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	prefix := `lb_sticky_requests_total{result="` + result + `"} `
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if count, found := strings.CutPrefix(line, prefix); found {
			return count
		}
	}
	t.Fatalf("no sticky result %q in the metrics", result)
	return ""
}

func TestAClientIsPinnedToTheBackendThatFirstAnsweredIt(t *testing.T) {
	metrics := observability.NewMetrics()
	handler := stickyHandler(stickyConfig(), namedBackends(t, 4), allHealthy, metrics)

	first := sendWith(handler)
	pin := pinOf(first)
	if pin == nil {
		t.Fatal("the first answer set no sticky cookie")
	}
	if !pin.HttpOnly || pin.SameSite != http.SameSiteLaxMode || pin.Path != "/" || pin.Secure || pin.MaxAge != 0 {
		t.Errorf("cookie attributes %+v, want HttpOnly, SameSite=Lax, Path=/, not Secure, no Max-Age", pin)
	}
	if strings.Contains(pin.Value, ":") || strings.Contains(pin.Value, "127.0.0.1") {
		t.Errorf("the cookie %q gives the backend's address away", pin.Value)
	}

	// Round robin alone would move the next requests around the pool.
	for range 8 {
		again := sendWith(handler, pin)
		if again.Body.String() != first.Body.String() {
			t.Fatalf("a pinned client reached backend %s, want %s", again.Body.String(), first.Body.String())
		}
		if pinOf(again) != nil {
			t.Fatal("an answer to a client already pinned there set the cookie again")
		}
	}
	if got := counted(t, metrics, observability.StickyNew); got != "1" {
		t.Errorf("%s requests counted as new, want 1", got)
	}
	if got := counted(t, metrics, observability.StickyPinned); got != "8" {
		t.Errorf("%s requests counted as pinned, want 8", got)
	}
}

func TestTheCookieCarriesTheConfiguredAttributes(t *testing.T) {
	cfg := stickyConfig()
	cfg.Sticky.Cookie = "session_backend"
	cfg.Sticky.MaxAge = config.Duration(time.Hour)
	cfg.Sticky.Secure = true
	handler := stickyHandler(cfg, namedBackends(t, 2), allHealthy, observability.NewMetrics())

	set := sendWith(handler).Header().Get("Set-Cookie")
	for _, want := range []string{"session_backend=", "Max-Age=3600", "Secure", "HttpOnly", "SameSite=Lax"} {
		if !strings.Contains(set, want) {
			t.Errorf("Set-Cookie %q lacks %s", set, want)
		}
	}
}

func TestAClientWhoseBackendLeftThePoolIsPinnedAgain(t *testing.T) {
	backends := namedBackends(t, 3)
	first := stickyHandler(stickyConfig(), backends, allHealthy, observability.NewMetrics())
	answer := sendWith(first)
	pin := pinOf(answer)
	was := answer.Body.String()
	gone, _ := strconv.Atoi(was)

	metrics := observability.NewMetrics()
	handler := stickyHandler(stickyConfig(), backends, newFakeChecker(backends[gone].Addr), metrics)
	moved := sendWith(handler, pin)
	if moved.Body.String() == was {
		t.Fatal("the request reached the backend health checking had taken out")
	}
	newPin := pinOf(moved)
	if newPin == nil || newPin.Value == pin.Value {
		t.Fatalf("the answer set %v, want a cookie for the new backend", newPin)
	}
	if again := sendWith(handler, newPin); again.Body.String() != moved.Body.String() {
		t.Errorf("with its new cookie the client reached %s, want %s", again.Body.String(), moved.Body.String())
	}
	if got := counted(t, metrics, observability.StickyRepinned); got != "1" {
		t.Errorf("%s requests counted as repinned, want 1", got)
	}
}

func TestAFailedAttemptOnThePinnedBackendPinsTheClientWhereItWasServed(t *testing.T) {
	backends := append([]*balancer.Backend{{Addr: unreachable}}, namedBackends(t, 2)...)
	pin := &http.Cookie{Name: config.DefaultStickyCookie, Value: pinToken(secret, unreachable)}
	handler := stickyHandler(stickyConfig(), backends, allHealthy, observability.NewMetrics())

	answer := sendWith(handler, pin)
	if answer.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200 from a retry", answer.Code)
	}
	moved := pinOf(answer)
	served, _ := strconv.Atoi(answer.Body.String())
	if moved == nil || moved.Value != pinToken(secret, backends[served+1].Addr) {
		t.Errorf("the answer set %v, want the cookie of the backend that served it", moved)
	}
}

func TestACookieNamingNoBackendIsIgnored(t *testing.T) {
	backends := namedBackends(t, 3)
	for name, value := range map[string]string{
		"made up":             "not-a-token",
		"signed elsewhere":    pinToken([]byte("some other balancer's secret key"), backends[0].Addr),
		"an address in clear": backends[0].Addr,
	} {
		metrics := observability.NewMetrics()
		handler := stickyHandler(stickyConfig(), backends, allHealthy, metrics)

		answer := sendWith(handler, &http.Cookie{Name: config.DefaultStickyCookie, Value: value})
		if answer.Code != http.StatusOK || pinOf(answer) == nil {
			t.Errorf("%s: answered %d with cookie %v, want 200 and a proper cookie", name, answer.Code, pinOf(answer))
		}
		if got := counted(t, metrics, observability.StickyUnknown); got != "1" {
			t.Errorf("%s: %s requests counted as unknown, want 1", name, got)
		}
	}
}

func TestTheStickyCookieIsTakenOffTheRequestAndNothingElse(t *testing.T) {
	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received = r.Header.Values("Cookie")
	}))
	defer server.Close()
	backend := &balancer.Backend{Addr: strings.TrimPrefix(server.URL, "http://")}

	for _, test := range []struct {
		name string
		cfg  *config.Config
		sent string
		want []string
	}{
		{"sticky on", stickyConfig(), `theme="dark mode"; lb_backend=abc; cart=42`, []string{`theme="dark mode"; cart=42`}},
		{"only the sticky cookie", stickyConfig(), `lb_backend=abc`, nil},
		{"sticky off", retryingConfig(), `theme="dark mode"; lb_backend=abc`, []string{`theme="dark mode"; lb_backend=abc`}},
	} {
		received = nil
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Cookie", test.sent)
		send(New(test.cfg, balancer.NewRoundRobin(), []*balancer.Backend{backend}, allHealthy, testMetrics()), request)

		if strings.Join(received, "|") != strings.Join(test.want, "|") {
			t.Errorf("%s: the backend received cookies %q, want %q", test.name, received, test.want)
		}
	}
}

func TestAPinOutranksTheConsistentHash(t *testing.T) {
	backends := namedBackends(t, 4)
	cfg := hashedConfig(0)
	cfg.Sticky = stickyConfig().Sticky
	session := keyHomedOn(t, backends, backends[0])
	handler := New(cfg, balancer.NewConsistentHash(0), backends, allHealthy, observability.NewMetrics(), WithStickySecret(secret))

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Session", session)
	request.AddCookie(&http.Cookie{Name: config.DefaultStickyCookie, Value: pinToken(secret, backends[3].Addr)})
	if got := send(handler, request).Body.String(); got != "3" {
		t.Errorf("the request went to backend %s, want the pinned 3 rather than its key's 0", got)
	}
}

func TestAFailedAnswerPinsNobody(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unwell", http.StatusInternalServerError)
	}))
	defer failing.Close()
	backend := &balancer.Backend{Addr: strings.TrimPrefix(failing.URL, "http://")}
	cfg := stickyConfig()
	cfg.FailurePolicy, cfg.RetryOn5xx = config.FailFast, true

	answer := sendWith(stickyHandler(cfg, []*balancer.Backend{backend}, allHealthy, observability.NewMetrics()))
	if answer.Code != http.StatusServiceUnavailable || pinOf(answer) != nil {
		t.Errorf("answered %d with cookie %v, want 503 and no cookie", answer.Code, pinOf(answer))
	}
}

func TestWithoutStickySessionsNoCookieIsSetOrCounted(t *testing.T) {
	metrics := observability.NewMetrics()
	handler := New(retryingConfig(), balancer.NewRoundRobin(), namedBackends(t, 2), allHealthy, metrics)

	if answer := sendWith(handler); answer.Header().Get("Set-Cookie") != "" {
		t.Errorf("an answer set %q with sticky sessions off", answer.Header().Get("Set-Cookie"))
	}
	if got := counted(t, metrics, observability.StickyNew); got != "0" {
		t.Errorf("%s requests counted as new with sticky sessions off, want none", got)
	}
}

func TestAPinSurvivesAReload(t *testing.T) {
	backends := namedBackends(t, 3)
	handler := New(stickyConfig(), balancer.NewRoundRobin(), backends, allHealthy, observability.NewMetrics())
	first := sendWith(handler)

	// No secret was given: the handler made its own, and keeps it.
	handler.Reload(stickyConfig(), balancer.NewRoundRobin(), backends)
	again := sendWith(handler, pinOf(first))
	if again.Body.String() != first.Body.String() || pinOf(again) != nil {
		t.Errorf("after a reload the client reached %s with cookie %v, want %s and no new cookie",
			again.Body.String(), pinOf(again), first.Body.String())
	}
}

func TestPinTokensDependOnTheSecretAndTheAddress(t *testing.T) {
	a := pinToken(secret, "backend-1:5678")
	if a != pinToken(secret, "backend-1:5678") {
		t.Error("the same secret and address gave two tokens")
	}
	if a == pinToken(secret, "backend-2:5678") {
		t.Error("two backends share a token")
	}
	if a == pinToken([]byte("another secret, as long as the first"), "backend-1:5678") {
		t.Error("two secrets give the same token")
	}
	if len(a) != 22 {
		t.Errorf("token %q is %d characters, want 22 for 128 bits", a, len(a))
	}
}
