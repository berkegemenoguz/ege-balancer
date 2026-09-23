package main

import (
	"testing"
	"time"
)

func TestPercentileOfASortedRun(t *testing.T) {
	sorted := make([]time.Duration, 0, 100)
	for i := 1; i <= 100; i++ {
		sorted = append(sorted, time.Duration(i)*time.Millisecond)
	}

	tests := []struct {
		quantile float64
		want     time.Duration
	}{
		{0, time.Millisecond},
		{0.5, 51 * time.Millisecond},
		{0.95, 96 * time.Millisecond},
		{0.99, 100 * time.Millisecond},
		{1, 100 * time.Millisecond},
	}

	for _, test := range tests {
		if got := percentile(sorted, test.quantile); got != test.want {
			t.Errorf("percentile(%.2f) = %s, want %s", test.quantile, got, test.want)
		}
	}
}

func TestPercentileOfNothing(t *testing.T) {
	if got := percentile(nil, 0.95); got != 0 {
		t.Errorf("percentile of an empty run = %s, want 0", got)
	}
}

func TestSummariseCountsWhatTheRunSaw(t *testing.T) {
	first, second := newSamples(), newSamples()
	first.record(10*time.Millisecond, 200, "backend-1", 1<<20)
	first.record(30*time.Millisecond, 200, "backend-2", 1<<20)
	second.record(20*time.Millisecond, 503, "backend-9", 0)
	second.recordFailure("timeout")

	first.merge(second)
	got := summarise(first, 4, 2*time.Second)

	if got.Requests != 3 {
		t.Errorf("requests = %d, want 3", got.Requests)
	}
	if got.Failures != 1 || got.FailureKind["timeout"] != 1 {
		t.Errorf("failures = %d %v, want one timeout", got.Failures, got.FailureKind)
	}
	if got.Throughput != 1.5 {
		t.Errorf("throughput = %.2f, want 1.5 per second", got.Throughput)
	}
	if got.MegabytesPS != 1 {
		t.Errorf("throughput = %.2f MB/s, want 1", got.MegabytesPS)
	}
	if got.Statuses[200] != 2 || got.Statuses[503] != 1 {
		t.Errorf("statuses = %v, want two 200s and one 503", got.Statuses)
	}
	if got.Backends["backend-1"] != 1 || got.Backends["backend-9"] != 1 {
		t.Errorf("backends = %v, want one request each", got.Backends)
	}
	if got.Connections != 4 || got.Duration != "2s" {
		t.Errorf("run was %d connections for %s, want 4 for 2s", got.Connections, got.Duration)
	}
}

func TestSummariseOfAnEmptyRun(t *testing.T) {
	got := summarise(newSamples(), 1, time.Second)

	if got.Requests != 0 || got.Throughput != 0 {
		t.Errorf("empty run reported %d requests at %.2f/s, want none", got.Requests, got.Throughput)
	}
}

func TestSummariseCountsTheCacheAnswers(t *testing.T) {
	all := newSamples()
	for _, answer := range []string{"hit", "hit", "hit", "miss", ""} {
		all.record(time.Millisecond, 200, "backend-1", 0)
		all.recordCache(answer)
	}

	got := summarise(all, 1, time.Second)
	if got.Cache == nil || got.Cache.Hits != 3 || got.Cache.Misses != 1 || got.Cache.HitRate != 0.75 {
		t.Errorf("cache = %+v, want 3 hits, 1 miss and a 75%% hit rate", got.Cache)
	}
}

func TestWithoutSessionsThereIsNoCacheToReport(t *testing.T) {
	all := newSamples()
	all.record(time.Millisecond, 200, "backend-1", 0)

	if got := summarise(all, 1, time.Second); got.Cache != nil {
		t.Errorf("cache = %+v, want none reported when no answer mentioned it", got.Cache)
	}
}
