package main

import (
	"net/http"
	"testing"
)

func TestBackendOfPrefersTheHeader(t *testing.T) {
	header := http.Header{"X-Backend": []string{"backend-10"}}
	if got := backendOf(header, []byte("backend-10\n0123456789abcdef\n")); got != "backend-10" {
		t.Errorf("backendOf = %q, want the name from the header", got)
	}
}

func TestBackendOfFallsBackToTheFirstLine(t *testing.T) {
	for body, want := range map[string]string{
		"backend-3\n":          "backend-3",
		"backend-3\npadding\n": "backend-3",
		"backend-3":            "backend-3",
		"":                     "",
	} {
		if got := backendOf(http.Header{}, []byte(body)); got != want {
			t.Errorf("backendOf(%q) = %q, want %q", body, got, want)
		}
	}
}
