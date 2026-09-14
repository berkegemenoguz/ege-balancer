package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSamplerMatchesTheMedianAndTheTail(t *testing.T) {
	const samples = 100000

	s := newSampler(profile{latency: 20 * time.Millisecond, p99: 200 * time.Millisecond}, 1)
	drawn := make([]time.Duration, samples)
	for i := range drawn {
		drawn[i] = s.latency()
	}
	slices.Sort(drawn)

	within := func(name string, got, want time.Duration, tolerance float64) {
		t.Helper()
		if diff := float64(got-want) / float64(want); diff < -tolerance || diff > tolerance {
			t.Errorf("%s = %s, want within %.0f%% of %s", name, got, tolerance*100, want)
		}
	}
	within("median", drawn[samples/2], 20*time.Millisecond, 0.05)
	within("p99", drawn[samples*99/100], 200*time.Millisecond, 0.10)
}

func TestSamplerIsConstantWithoutATail(t *testing.T) {
	s := newSampler(profile{latency: 15 * time.Millisecond}, 1)
	for range 100 {
		if got := s.latency(); got != 15*time.Millisecond {
			t.Fatalf("latency = %s, want exactly 15ms without a p99", got)
		}
	}
}

func TestSamplerRepeatsWithTheSameSeed(t *testing.T) {
	p := profile{latency: 20 * time.Millisecond, p99: 200 * time.Millisecond}
	first, second := newSampler(p, 42), newSampler(p, 42)
	for i := range 1000 {
		if a, b := first.latency(), second.latency(); a != b {
			t.Fatalf("draw %d differs between samplers with the same seed: %s and %s", i, a, b)
		}
	}
}

func TestSamplerFailsAtTheConfiguredRate(t *testing.T) {
	const draws = 100000

	s := newSampler(profile{}, 1)
	failures := 0
	for range draws {
		if s.fails(0.1) {
			failures++
		}
	}
	if share := float64(failures) / draws; share < 0.095 || share > 0.105 {
		t.Errorf("failed %.3f of draws, want about 0.1", share)
	}
	if s.fails(0) {
		t.Error("failed with a rate of zero")
	}
}

func TestParseSize(t *testing.T) {
	for input, want := range map[string]int{
		"0":      0,
		"512":    512,
		"512B":   512,
		"4KiB":   4096,
		"256KiB": 256 << 10,
		"1MiB":   1 << 20,
		" 2 MiB": 2 << 20,
	} {
		if got, err := parseSize(input); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v, want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "4KB", "-1", "lots"} {
		if _, err := parseSize(input); err == nil {
			t.Errorf("parseSize(%q) succeeded, want an error", input)
		}
	}
}

func TestProfileValidation(t *testing.T) {
	valid := profile{name: "backend-1", latency: 10 * time.Millisecond, p99: 50 * time.Millisecond,
		capacity: 4, queue: 8, bodySize: 1024, errorRate: 0.01}
	if err := valid.validate(); err != nil {
		t.Fatalf("a valid profile was rejected: %v", err)
	}

	tests := []struct {
		name    string
		change  func(*profile)
		wantErr string
	}{
		{"no name", func(p *profile) { p.name = "" }, "name is required"},
		{"tail below the median", func(p *profile) { p.p99 = time.Millisecond }, "must not be below latency"},
		{"tail without a median", func(p *profile) { p.latency = 0 }, "needs a median latency"},
		{"negative capacity", func(p *profile) { p.capacity = -1 }, "capacity must not be negative"},
		{"queue without capacity", func(p *profile) { p.capacity = 0 }, "queue needs a capacity"},
		{"error rate above one", func(p *profile) { p.errorRate = 1.5 }, "between 0 and 1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := valid
			test.change(&p)
			err := p.validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, test.wantErr)
			}
		})
	}
}
