package config

import "fmt"

// RequiresRestart names the settings that differ between the running
// configuration and a newly read one but cannot be applied while serving.
//
// Everything it lists is bound to a socket or to a server that is already
// accepting connections; the caller applies the rest and reports these.
func (c *Config) RequiresRestart(next *Config) []string {
	var fixed []string
	add := func(field string, from, to any) {
		fixed = append(fixed, fmt.Sprintf("%s (%v to %v)", field, from, to))
	}

	if c.ListenAddr != next.ListenAddr {
		add("listen_addr", c.ListenAddr, next.ListenAddr)
	}
	if c.MetricsAddr != next.MetricsAddr {
		add("metrics_addr", c.MetricsAddr, next.MetricsAddr)
	}
	if c.EnablePprof != next.EnablePprof {
		add("enable_pprof", c.EnablePprof, next.EnablePprof)
	}
	if c.Limits.MaxConnections != next.Limits.MaxConnections {
		add("limits.max_connections", c.Limits.MaxConnections, next.Limits.MaxConnections)
	}
	if c.Timeouts != next.Timeouts {
		add("timeouts", c.Timeouts, next.Timeouts)
	}
	return fixed
}
