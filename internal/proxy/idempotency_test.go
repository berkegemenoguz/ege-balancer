package proxy

import (
	"io"
	"net"
	"net/http"
	"testing"
)

func TestIdempotentMethods(t *testing.T) {
	tests := map[string]bool{
		http.MethodGet:     true,
		http.MethodHead:    true,
		http.MethodPut:     true,
		http.MethodDelete:  true,
		http.MethodOptions: true,
		http.MethodTrace:   true,
		http.MethodPost:    false,
		http.MethodPatch:   false,
		http.MethodConnect: false,
		"WHAT":             false,
	}

	for method, want := range tests {
		if got := idempotent(method); got != want {
			t.Errorf("idempotent(%q) = %v, want %v", method, got, want)
		}
	}
}

func TestRetryableDependsOnTheMethodAndTheFailure(t *testing.T) {
	// A real failed dial, so the test reads the error the transport reports
	// rather than one built by hand.
	_, dialErr := net.Dial("tcp", unreachable)
	if dialErr == nil {
		t.Fatalf("dialling %s succeeded, want a connection error", unreachable)
	}

	tests := []struct {
		name   string
		method string
		err    error
		want   bool
	}{
		{"idempotent, connection broke", http.MethodGet, io.ErrUnexpectedEOF, true},
		{"idempotent, no connection", http.MethodGet, dialErr, true},
		{"not idempotent, no connection", http.MethodPost, dialErr, true},
		{"not idempotent, connection broke", http.MethodPost, io.ErrUnexpectedEOF, false},
		{"not idempotent, backend answered 5xx", http.MethodPost, errRetryable5xx, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := retryable(test.method, test.err); got != test.want {
				t.Errorf("retryable(%q, %v) = %v, want %v", test.method, test.err, got, test.want)
			}
		})
	}
}
