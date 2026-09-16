package integration

import (
	"io"
	"net/http"
	"testing"
)

// requestIDOf sends one request, optionally carrying sent as its identifier,
// and returns the identifier the balancer answered with.
func (b *balancerUnderTest) requestIDOf(t *testing.T, sent string) string {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, b.url, nil)
	if err != nil {
		t.Fatalf("building the request failed: %v", err)
	}
	if sent != "" {
		request.Header.Set("X-Request-Id", sent)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)

	return response.Header.Get("X-Request-Id")
}

func TestEveryAnswerCarriesARequestID(t *testing.T) {
	under := start(t, testConfig(newBackends(t, 2)))

	first := under.requestIDOf(t, "")
	second := under.requestIDOf(t, "")

	if first == "" || second == "" {
		t.Fatalf("identifiers were %q and %q, want one on every answer", first, second)
	}
	if first == second {
		t.Errorf("both answers carried %q, want an identifier per request", first)
	}
}

func TestAClientsRequestIDComesBack(t *testing.T) {
	under := start(t, testConfig(newBackends(t, 2)))

	const sent = "order-7"
	if got := under.requestIDOf(t, sent); got != sent {
		t.Errorf("the answer carried %q, want the client's own %q", got, sent)
	}
}
