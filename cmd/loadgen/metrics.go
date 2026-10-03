package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// labelPattern picks the one label value out of a counter's labels: the reason
// of a rejection, the placement of a consistently hashed request, or what a
// sticky cookie did.
var labelPattern = regexp.MustCompile(`(?:reason|placement|result)="([^"]+)"`)

// counters is what the balancer says about its own work at one moment: the
// retries it has sent, the requests it refused itself, by reason, and where
// consistent hashing and sticky sessions placed requests.
type counters map[string]float64

// scrapeCounters reads those counters from a Prometheus endpoint. Taking them
// at the start and the end of the measurement window is what makes them belong
// to the run rather than to everything since the balancer started.
func scrapeCounters(ctx context.Context, client *http.Client, url string) (counters, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, response.Status)
	}
	return parseCounters(response.Body)
}

// parseCounters reads the counters the report quotes out of a metrics
// exposition, under short names.
func parseCounters(body interface{ Read([]byte) (int, error) }) (counters, error) {
	found := counters{}

	lines := bufio.NewScanner(body)
	lines.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for lines.Scan() {
		line := lines.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}

		name, value, ok := splitSample(line)
		if !ok {
			continue
		}

		switch {
		case name == "lb_retries_total":
			found["retries"] += value
		case strings.HasPrefix(name, "lb_rejected_requests_total{"):
			found.add("refused ", name, value)
		case strings.HasPrefix(name, "lb_hash_placements_total{"):
			found.add("placed ", name, value)
		case strings.HasPrefix(name, "lb_sticky_requests_total{"):
			found.add("sticky ", name, value)
		}
	}
	return found, lines.Err()
}

// add counts value under prefix and the label value of name.
func (c counters) add(prefix, name string, value float64) {
	if label := labelPattern.FindStringSubmatch(name); label != nil {
		c[prefix+label[1]] += value
	}
}

// splitSample separates a sample line into its name with labels and its value.
func splitSample(line string) (string, float64, bool) {
	space := strings.LastIndex(line, " ")
	if space < 0 {
		return "", 0, false
	}

	value, err := strconv.ParseFloat(line[space+1:], 64)
	if err != nil {
		return "", 0, false
	}
	return line[:space], value, true
}

// since returns what happened between two snapshots. A counter that only
// appears in the later one started at zero.
func (before counters) since(after counters) map[string]int {
	moved := make(map[string]int)
	for name, value := range after {
		if delta := value - before[name]; delta > 0 {
			moved[name] = int(delta)
		}
	}
	return moved
}
