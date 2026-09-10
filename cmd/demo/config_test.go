package main

import (
	"strings"
	"testing"
)

const sample = `listen_addr: ":8080"
algorithm: round_robin              # round_robin | least_connections
backends:
  - addr: "backend-1:5678"
    weight: 1
  - addr: "backend-2:5678"
    weight: 3
limits:
  rate_limit_per_ip: 100            # requests per second
`

func TestSetScalarKeepsTheTrailingComment(t *testing.T) {
	changed, err := setScalar([]byte(sample), "algorithm", "weighted_round_robin")
	if err != nil {
		t.Fatalf("setScalar returned an unexpected error: %v", err)
	}

	line := lineWith(t, string(changed), "algorithm:")
	if !strings.Contains(line, "algorithm: weighted_round_robin") {
		t.Errorf("line = %q, want the new value", line)
	}
	if !strings.Contains(line, "# round_robin | least_connections") {
		t.Errorf("line = %q, want the comment kept", line)
	}
}

func TestSetScalarKeepsIndentation(t *testing.T) {
	changed, err := setScalar([]byte(sample), "rate_limit_per_ip", "5")
	if err != nil {
		t.Fatalf("setScalar returned an unexpected error: %v", err)
	}

	line := lineWith(t, string(changed), "rate_limit_per_ip:")
	if !strings.HasPrefix(line, "  rate_limit_per_ip: 5") {
		t.Errorf("line = %q, want the nested key to keep its indentation", line)
	}
	if !strings.Contains(line, "# requests per second") {
		t.Errorf("line = %q, want the comment kept", line)
	}
}

func TestSetScalarRejectsWhatItCannotFind(t *testing.T) {
	if _, err := setScalar([]byte(sample), "algoritm", "round_robin"); err == nil {
		t.Error("setScalar accepted a key that is not in the file")
	}
}

func TestSetBackendWeight(t *testing.T) {
	changed, err := setBackendWeight([]byte(sample), "backend-2:5678", 5)
	if err != nil {
		t.Fatalf("setBackendWeight returned an unexpected error: %v", err)
	}

	lines := strings.Split(string(changed), "\n")
	for i, line := range lines {
		if !strings.Contains(line, "backend-2:5678") {
			continue
		}
		if got := strings.TrimSpace(lines[i+1]); got != "weight: 5" {
			t.Errorf("weight line = %q, want %q", got, "weight: 5")
		}
	}

	// The other backend must be left alone.
	if !strings.Contains(string(changed), "- addr: \"backend-1:5678\"\n    weight: 1") {
		t.Error("the weight of another backend was changed")
	}
}

func TestSetBackendWeightRejectsAnUnknownBackend(t *testing.T) {
	if _, err := setBackendWeight([]byte(sample), "nowhere:1234", 2); err == nil {
		t.Error("setBackendWeight accepted a backend that is not configured")
	}
}

// lineWith returns the single line containing the given prefix.
func lineWith(t *testing.T, content, key string) string {
	t.Helper()

	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), key) {
			return line
		}
	}
	t.Fatalf("no line contains %q", key)
	return ""
}

func TestNaturalLessOrdersByTrailingNumber(t *testing.T) {
	names := []string{"backend-10", "backend-2", "backend-1", "other-3"}
	sortNames(names)

	want := []string{"backend-1", "backend-2", "backend-10", "other-3"}
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("order = %v, want %v", names, want)
		}
	}
}

// sortNames sorts in place the way the distribution output does.
func sortNames(names []string) {
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && naturalLess(names[j], names[j-1]); j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
}

func TestSetScalarRoundTripsToTheSameBytes(t *testing.T) {
	lowered, err := setScalar([]byte(sample), "rate_limit_per_ip", "5")
	if err != nil {
		t.Fatalf("setScalar returned an unexpected error: %v", err)
	}

	restored, err := setScalar(lowered, "rate_limit_per_ip", "100")
	if err != nil {
		t.Fatalf("setScalar returned an unexpected error: %v", err)
	}

	if string(restored) != sample {
		t.Errorf("restoring the value left the file changed:\n%q\nwant\n%q",
			lineWith(t, string(restored), "rate_limit_per_ip:"),
			lineWith(t, sample, "rate_limit_per_ip:"))
	}
}
