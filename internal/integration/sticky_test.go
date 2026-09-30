package integration

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/app"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// sticky keeps clients on their backend by cookie, over cfg's algorithm.
func sticky(cfg *config.Config) *config.Config {
	cfg.Sticky = config.Sticky{Mode: config.StickyCookie, Cookie: config.DefaultStickyCookie}
	return cfg
}

// browser is a client that keeps its cookies, as a browser does.
func browser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// visit sends one request as client and returns the backend that answered, and
// the sticky cookie the answer set, if any.
func visit(t *testing.T, client *http.Client, url string) (string, *http.Cookie) {
	t.Helper()
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the body failed: %v", err)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == config.DefaultStickyCookie {
			return strings.TrimSpace(string(body)), cookie
		}
	}
	return strings.TrimSpace(string(body)), nil
}

func TestABrowserStaysOnTheBackendThatFirstAnsweredIt(t *testing.T) {
	backends := newBackends(t, 4)
	under := start(t, pinHealth(testConfig(backends)))
	if got := under.status(t).Sticky; got != string(config.StickyNone) {
		t.Fatalf("/status reports sticky %q before it is turned on, want none", got)
	}

	// Turned on by a reload, as the demo console does it.
	under.reconfigure(t, sticky(pinHealth(testConfig(backends))))
	if got := under.status(t).Sticky; got != string(config.StickyCookie) {
		t.Fatalf("/status reports sticky %q after the reload, want cookie", got)
	}

	reached := map[string]bool{}
	for range 4 {
		client := browser(t)
		first, pin := visit(t, client, under.url)
		if pin == nil {
			t.Fatal("the first answer set no sticky cookie")
		}
		for range 10 {
			if again, _ := visit(t, client, under.url); again != first {
				t.Fatalf("a browser pinned to %s reached %s", first, again)
			}
		}
		reached[first] = true
	}
	// Round robin placed the four browsers' first requests on four backends.
	if len(reached) != len(backends) {
		t.Errorf("four browsers were pinned to %d backends, want 4", len(reached))
	}
}

func TestABrowserWhoseBackendDiesIsPinnedToAnother(t *testing.T) {
	backends := newBackends(t, 3)
	cfg := sticky(testConfig(backends))
	// Retried, the first request after the death is answered by another
	// backend at once, and pins the browser there; under fail_fast it would be
	// answered 503 until health checking took the backend out.
	cfg.FailurePolicy = config.RetryNextBackend
	under := start(t, cfg)

	client := browser(t)
	first, _ := visit(t, client, under.url)
	for _, backend := range backends {
		if backend.name == first {
			backend.kill()
		}
	}

	moved, pin := visit(t, client, under.url)
	if moved == first || pin == nil {
		t.Fatalf("after its backend died the browser reached %s with cookie %v, want another backend and a new cookie", moved, pin)
	}
	for range 5 {
		if again, _ := visit(t, client, under.url); again != moved {
			t.Errorf("the browser, pinned again to %s, reached %s", moved, again)
		}
	}
}

func TestBalancersGivenTheSameSecretHonourEachOthersCookies(t *testing.T) {
	t.Setenv(app.StickySecretEnv, "a secret shared by every balancer of the pool")
	backends := newBackends(t, 4)
	first := start(t, sticky(pinHealth(testConfig(backends))))
	second := start(t, sticky(pinHealth(testConfig(backends))))

	served, pin := visit(t, &http.Client{}, first.url)
	request, err := http.NewRequest(http.MethodGet, second.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(pin)
	for range 4 {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if got := strings.TrimSpace(string(body)); got != served {
			t.Fatalf("the second balancer sent the first one's client to %s, want %s", got, served)
		}
	}
}

func TestAShortStickySecretIsRefused(t *testing.T) {
	t.Setenv(app.StickySecretEnv, "too short")
	path := filepath.Join(t.TempDir(), "lb.yaml")
	writeConfig(t, path, sticky(testConfig(newBackends(t, 1))))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := app.New(cfg, path); err == nil || !strings.Contains(err.Error(), app.StickySecretEnv) {
		t.Errorf("app.New returned %v, want an error naming %s", err, app.StickySecretEnv)
	}
}
