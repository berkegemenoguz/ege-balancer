package config

import (
	"fmt"
	"time"
)

// Warnings reports settings that are valid but work against each other, for
// the caller to log. They are not errors: a configuration that loaded before
// keeps loading, and the operator is told what it costs.
func (c *Config) Warnings() []string {
	var warnings []string

	response, write := time.Duration(c.Timeouts.ResponseTimeout), time.Duration(c.Timeouts.WriteTimeout)
	if write > 0 && response >= write {
		warnings = append(warnings, fmt.Sprintf(
			"timeouts.response_timeout (%s) is not shorter than timeouts.write_timeout (%s): "+
				"by the time a stalled backend is abandoned, there is no time left to answer the client from another",
			response, write))
	}
	return warnings
}
