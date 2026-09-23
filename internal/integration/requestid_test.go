package integration

import (
	"io"
	"net/http"
	"testing"
)

// requestIDOf sends one request and returns the identifier the balancer
// answered with.
func (b *balancerUnderTest) requestIDOf(t *testing.T) string {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, b.url, nil)
	if err != nil {
		t.Fatalf("building the request failed: %v", err)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)

	return response.Header.Get("X-Request-Id")
}

// A client's own identifier, and one that is unusable, are covered against the
// proxy's handler chain in internal/proxy; this checks the assembled balancer.
func TestEveryAnswerCarriesARequestID(t *testing.T) {
	under := start(t, testConfig(newBackends(t, 2)))

	first := under.requestIDOf(t)
	second := under.requestIDOf(t)

	if first == "" || second == "" {
		t.Fatalf("identifiers were %q and %q, want one on every answer", first, second)
	}
	if first == second {
		t.Errorf("both answers carried %q, want an identifier per request", first)
	}
}
