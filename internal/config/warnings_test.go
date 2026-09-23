package config

import (
	"strings"
	"testing"
	"time"
)

func TestAResponseTimeoutAsLongAsTheWriteTimeoutIsWarnedAbout(t *testing.T) {
	for _, test := range []struct {
		response, write time.Duration
		warned          bool
	}{
		{10 * time.Second, 10 * time.Second, true},
		{15 * time.Second, 10 * time.Second, true},
		{3 * time.Second, 10 * time.Second, false},
		{3 * time.Second, 0, false},
	} {
		cfg := &Config{Timeouts: Timeouts{ResponseTimeout: Duration(test.response), WriteTimeout: Duration(test.write)}}
		warnings := cfg.Warnings()

		if got := len(warnings) == 1 && strings.Contains(warnings[0], "response_timeout"); got != test.warned {
			t.Errorf("response %s, write %s: warnings %q, want a warning: %v", test.response, test.write, warnings, test.warned)
		}
	}
}
