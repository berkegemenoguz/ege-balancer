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
	p, opts, err := parseFlags([]string{
		"-name=backend-3", "-listen=:9000", "-admin=:9001", "-latency=20ms", "-latency-p99=150ms",
		"-capacity=32", "-queue=64", "-body-size=16KiB", "-error-rate=0.01", "-seed=7",
		"-cache-size=5000", "-miss-penalty=40ms", "-hang-rate=0.002", "-reset-rate=0.002",
		"-drip-rate=0.005", "-drip-over=3s", "-cold-start=30s", "-cold-factor=4",
		"-pause-every=15s", "-pause=150ms",
	}, environment(nil))
	if err != nil {
		t.Fatalf("parseFlags returned an unexpected error: %v", err)
	}

	want := profile{name: "backend-3", latency: 20 * time.Millisecond, p99: 150 * time.Millisecond,
		capacity: 32, queue: 64, bodySize: 16 << 10, errorRate: 0.01,
		cacheSize: 5000, missPenalty: 40 * time.Millisecond, hangRate: 0.002, resetRate: 0.002,
		dripRate: 0.005, dripOver: 3 * time.Second, coldStart: 30 * time.Second, coldFactor: 4,
		pauseEvery: 15 * time.Second, pause: 150 * time.Millisecond}
	if p != want {
		t.Errorf("profile = %+v, want %+v", p, want)
	}
	if opts != (options{seed: 7, listen: ":9000", admin: ":9001"}) {
		t.Errorf("options = %+v, want seed 7, listen :9000 and admin :9001", opts)
	}
}

func TestParseFlagsFallsBackToTheEnvironment(t *testing.T) {
	p, opts, err := parseFlags([]string{"-capacity=8"}, environment(map[string]string{
		"MOCK_NAME":        "backend-4",
		"MOCK_LATENCY_P99": "80ms",
		"MOCK_LATENCY":     "15ms",
		"MOCK_CAPACITY":    "64",
		"MOCK_ADMIN":       ":5679",
		"MOCK_CACHE_SIZE":  "100",
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
	if p.cacheSize != 100 || opts.admin != ":5679" {
		t.Errorf("cache size %d and admin %q, want 100 and :5679 from the environment", p.cacheSize, opts.admin)
	}
	if opts.listen != ":5678" {
		t.Errorf("listen = %q, want the default :5678", opts.listen)
	}
}

func TestTheNewBehaviourIsOffUnlessAskedFor(t *testing.T) {
	p, opts, err := parseFlags([]string{"-name=backend-1"}, environment(nil))
	if err != nil {
		t.Fatalf("parseFlags returned an unexpected error: %v", err)
	}

	if p.cacheSize != 0 || p.hangRate != 0 || p.resetRate != 0 || p.dripRate != 0 ||
		p.coldFactor != 1 || p.pauseEvery != 0 || opts.admin != "" {
		t.Errorf("profile = %+v and admin %q, want no cache, no faults, no cold start, no pauses and no admin port",
			p, opts.admin)
	}
}

func TestParseFlagsPicksASeedWhenNoneIsGiven(t *testing.T) {
	_, opts, err := parseFlags([]string{"-name=backend-1"}, environment(nil))
	if err != nil {
		t.Fatalf("parseFlags returned an unexpected error: %v", err)
	}
	if opts.seed == 0 {
		t.Error("seed = 0, want one picked at random")
	}
}

func TestParseFlagsReportsEveryProblem(t *testing.T) {
	_, _, err := parseFlags([]string{
		"-name=backend-1", "-latency=soon", "-capacity=many", "-body-size=huge", "-hang-rate=often",
	}, environment(nil))
	if err == nil {
		t.Fatal("parseFlags accepted invalid values")
	}
	for _, want := range []string{"latency", "capacity", "body-size", "hang-rate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %s", err, want)
		}
	}
}

func TestParseFlagsValidatesTheProfile(t *testing.T) {
	if _, _, err := parseFlags(nil, environment(nil)); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error = %v, want the missing name reported", err)
	}
}
