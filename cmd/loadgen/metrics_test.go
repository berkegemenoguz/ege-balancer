package main

import (
	"strings"
	"testing"
)

const exposition = `# HELP lb_retries_total Retries sent to another backend.
# TYPE lb_retries_total counter
lb_retries_total 37
lb_rejected_requests_total{reason="no_healthy_backend"} 84
lb_rejected_requests_total{reason="retry_budget_exhausted"} 75
lb_requests_total{backend="backend-1:5678",status="200"} 5
lb_backend_healthy{backend="backend-1:5678"} 1
not a sample at all
`

func TestParseCountersReadsRetriesAndRefusals(t *testing.T) {
	got, err := parseCounters(strings.NewReader(exposition))
	if err != nil {
		t.Fatalf("parsing failed: %v", err)
	}

	want := map[string]float64{
		"retries":                        37,
		"refused no_healthy_backend":     84,
		"refused retry_budget_exhausted": 75,
	}
	if len(got) != len(want) {
		t.Errorf("read %v, want only the retries and the refusals", got)
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %v, want %v", name, got[name], value)
		}
	}
}

func TestCountersSinceReportsOnlyWhatMoved(t *testing.T) {
	before := counters{"retries": 10, "refused rate_limited": 4}
	after := counters{"retries": 37, "refused rate_limited": 4, "refused no_healthy_backend": 5}

	got := before.since(after)

	want := map[string]int{"retries": 27, "refused no_healthy_backend": 5}
	if len(got) != len(want) {
		t.Errorf("since = %v, want %v", got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s moved by %d, want %d", name, got[name], value)
		}
	}
}

func TestSplitSampleRejectsWhatIsNotASample(t *testing.T) {
	if _, _, ok := splitSample("lb_retries_total not_a_number"); ok {
		t.Error("a line without a numeric value was read as a sample")
	}
	if _, _, ok := splitSample("nospace"); ok {
		t.Error("a line without a value was read as a sample")
	}

	name, value, ok := splitSample(`lb_rejected_requests_total{reason="x"} 12`)
	if !ok || name != `lb_rejected_requests_total{reason="x"}` || value != 12 {
		t.Errorf("splitSample gave %q %v %v, want the name and 12", name, value, ok)
	}
}
