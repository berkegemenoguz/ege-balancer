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

// reasonPattern picks the reason out of a rejection counter's labels.
var reasonPattern = regexp.MustCompile(`reason="([^"]+)"`)

// counters is what the balancer says about its own work at one moment: the
// retries it has sent and the requests it refused itself, by reason.
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

// parseCounters reads the two counters the report quotes out of a metrics
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
			reason := reasonPattern.FindStringSubmatch(name)
			if reason == nil {
				continue
			}
			found["refused "+reason[1]] += value
		}
	}
	return found, lines.Err()
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
