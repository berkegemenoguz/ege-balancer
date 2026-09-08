package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAmbiguouslyFramedRequestsAreRejected(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string][]string
	}{
		{"two content lengths", map[string][]string{"Content-Length": {"7", "42"}}},
		{"length and chunking", map[string][]string{
			"Content-Length": {"7"}, "Transfer-Encoding": {"chunked"},
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var served int
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("payload"))
			for name, values := range test.headers {
				request.Header[name] = values
			}

			response := send(validateRequest(testMetrics(), countingHandler(&served)), request)
			if response.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", response.Code, http.StatusBadRequest)
			}
			if served != 0 {
				t.Error("an ambiguously framed request reached a backend")
			}
		})
	}
}

func TestWellFramedRequestIsForwarded(t *testing.T) {
	var served int
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("payload"))
	request.Header.Set("Content-Length", "7")

	if response := send(validateRequest(testMetrics(), countingHandler(&served)), request); response.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if served != 1 {
		t.Error("a well framed request was not forwarded")
	}
}
