package main

import (
	"strings"
	"testing"
	"time"
)

// environment returns a getenv over a fixed set of variables.
func environment(variables map[string]string) func(string) string {
	return func(key string) string { return variables[key] }
}

func TestParseFlagsReadsEveryFlag(t *testing.T) {
	p, seed, listen, err := parseFlags([]string{
		"-name=backend-3", "-listen=:9000", "-latency=20ms", "-latency-p99=150ms",
		"-capacity=32", "-queue=64", "-body-size=16KiB", "-error-rate=0.01", "-seed=7",
	}, environment(nil))
	if err != nil {
		t.Fatalf("parseFlags returned an unexpected error: %v", err)
	}

	want := profile{name: "backend-3", latency: 20 * time.Millisecond, p99: 150 * time.Millisecond,
		capacity: 32, queue: 64, bodySize: 16 << 10, errorRate: 0.01}
	if p != want {
		t.Errorf("profile = %+v, want %+v", p, want)
	}
	if seed != 7 || listen != ":9000" {
		t.Errorf("seed %d, listen %q, want 7 and :9000", seed, listen)
	}
}

func TestParseFlagsFallsBackToTheEnvironment(t *testing.T) {
	p, _, listen, err := parseFlags([]string{"-capacity=8"}, environment(map[string]string{
		"MOCK_NAME":        "backend-4",
		"MOCK_LATENCY_P99": "80ms",
		"MOCK_LATENCY":     "15ms",
		"MOCK_CAPACITY":    "64",
	}))
	if err != nil {
		t.Fatalf("parseFlags returned an unexpected error: %v", err)
	}

	if p.name != "backend-4" || p.latency != 15*time.Millisecond || p.p99 != 80*time.Millisecond {
		t.Errorf("profile = %+v, want the name and latencies from the environment", p)
	}
	if p.capacity != 8 {
		t.Errorf("capacity = %d, want the flag to win over MOCK_CAPACITY", p.capacity)
	}
	if listen != ":5678" {
		t.Errorf("listen = %q, want the default :5678", listen)
	}
}

func TestParseFlagsPicksASeedWhenNoneIsGiven(t *testing.T) {
	_, seed, _, err := parseFlags([]string{"-name=backend-1"}, environment(nil))
	if err != nil {
		t.Fatalf("parseFlags returned an unexpected error: %v", err)
	}
	if seed == 0 {
		t.Error("seed = 0, want one picked at random")
	}
}

func TestParseFlagsReportsEveryProblem(t *testing.T) {
	_, _, _, err := parseFlags([]string{
		"-name=backend-1", "-latency=soon", "-capacity=many", "-body-size=huge",
	}, environment(nil))
	if err == nil {
		t.Fatal("parseFlags accepted invalid values")
	}
	for _, want := range []string{"latency", "capacity", "body-size"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %s", err, want)
		}
	}
}

func TestParseFlagsValidatesTheProfile(t *testing.T) {
	if _, _, _, err := parseFlags(nil, environment(nil)); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error = %v, want the missing name reported", err)
	}
}
