package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeSucceedsOnlyOnOK(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"ok", http.StatusOK, false},
		{"unavailable", http.StatusServiceUnavailable, true},
		{"not found", http.StatusNotFound, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			}))
			defer server.Close()

			if err := probe(server.URL); (err != nil) != test.wantErr {
				t.Errorf("probe of a %d answer returned %v, want an error: %v", test.status, err, test.wantErr)
			}
		})
	}
}

func TestProbeFailsWhenNothingAnswers(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()

	if err := probe(url); err == nil {
		t.Error("probe of a closed port succeeded, want an error")
	}
}
