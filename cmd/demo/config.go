package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The configuration is edited line by line rather than parsed and re-encoded,
// because re-encoding would strip the comments that document the schema.

// scalarLine matches a "key: value" line, capturing the indentation and key,
// the value, and any trailing comment, so a rewrite keeps both.
var scalarLine = regexp.MustCompile(`^(\s*[a-z_]+:\s*)([^#\n]*?)(\s*)(#.*)?$`)

// setScalar replaces the value of the named key. The key must appear exactly
// once; anything else is a configuration the console does not understand.
func setScalar(content []byte, key, value string) ([]byte, error) {
	lines := strings.Split(string(content), "\n")

	found := -1
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), key+":") {
			continue
		}
		if found >= 0 {
			return nil, fmt.Errorf("key %q appears more than once", key)
		}
		found = i
	}
	if found < 0 {
		return nil, fmt.Errorf("key %q not found", key)
	}

	parts := scalarLine.FindStringSubmatch(lines[found])
	if parts == nil {
		return nil, fmt.Errorf("cannot rewrite line %q", lines[found])
	}

	rewritten := parts[1] + value
	if parts[4] != "" {
		// Keep the comment in the column it was in, whether the new value is
		// longer or shorter, so that setting a value and setting it back leaves
		// the file byte for byte as it was.
		column := len(parts[1]) + len(parts[2]) + len(parts[3])
		padding := column - len(rewritten)
		if padding < 1 {
			padding = 1
		}
		rewritten += strings.Repeat(" ", padding) + parts[4]
	}
	lines[found] = rewritten

	return []byte(strings.Join(lines, "\n")), nil
}

// setBackendWeight sets the weight of the backend at addr. Weights follow the
// address they belong to, so the line to change is the next weight after it.
func setBackendWeight(content []byte, addr string, weight int) ([]byte, error) {
	lines := strings.Split(string(content), "\n")

	for i, line := range lines {
		if !strings.Contains(line, addr) || !strings.Contains(line, "addr:") {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if strings.HasPrefix(trimmed, "weight:") {
				indent := lines[j][:len(lines[j])-len(strings.TrimLeft(lines[j], " "))]
				lines[j] = indent + "weight: " + strconv.Itoa(weight)
				return []byte(strings.Join(lines, "\n")), nil
			}
			if strings.HasPrefix(trimmed, "- addr:") {
				break // the next backend started, so this one has no weight
			}
		}
		return nil, fmt.Errorf("backend %q has no weight to change", addr)
	}
	return nil, fmt.Errorf("backend %q not found", addr)
}
