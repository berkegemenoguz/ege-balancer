package main

import (
	"context"
	"sync"
	"time"
)

// clock accounts for the ways time makes a real process slower than its
// profile: a cold start, while its caches and pools fill, and pauses — the
// scheduled ones a stop-the-world garbage collection makes, and freezes
// injected through the admin port.
type clock struct {
	now   func() time.Time
	start time.Time

	coldStart  time.Duration
	coldFactor float64
	pauseEvery time.Duration
	pause      time.Duration
	// phase shifts the pauses within their period. Backends started together
	// would otherwise all pause at the same moment, where the collections of
	// separate processes have nothing to do with each other.
	phase time.Duration

	mu          sync.Mutex
	frozenUntil time.Time
}

// newClock returns the clock of a backend with profile p, started now.
func newClock(p profile, now func() time.Time) *clock {
	return &clock{
		now:        now,
		start:      now(),
		coldStart:  p.coldStart,
		coldFactor: p.coldFactor,
		pauseEvery: p.pauseEvery,
		pause:      p.pause,
	}
}

// factor is how much slower than its profile the backend serves right now:
// coldFactor at start, falling linearly to 1 once coldStart has passed.
func (c *clock) factor() float64 {
	if c.coldFactor <= 1 || c.coldStart <= 0 {
		return 1
	}

	elapsed := c.now().Sub(c.start)
	if elapsed >= c.coldStart {
		return 1
	}
	left := 1 - float64(elapsed)/float64(c.coldStart)
	return 1 + (c.coldFactor-1)*left
}

// stall is how long the process stays stopped from now: the rest of a
// scheduled pause or of a freeze, whichever ends later, and zero while it
// runs. Scheduled pauses come at the end of each period, so a backend does not
// start its life paused.
func (c *clock) stall() time.Duration {
	now := c.now()

	var wait time.Duration
	if c.pause > 0 && c.pauseEvery > c.pause {
		into := (now.Sub(c.start) + c.phase) % c.pauseEvery
		if into >= c.pauseEvery-c.pause {
			wait = c.pauseEvery - into
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if frozen := c.frozenUntil.Sub(now); frozen > wait {
		wait = frozen
	}
	return wait
}

// freeze stops the process for d from now. A freeze already in force that
// lasts longer is not shortened.
func (c *clock) freeze(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until := c.now().Add(d); until.After(c.frozenUntil) {
		c.frozenUntil = until
	}
}

// thaw ends a freeze early.
func (c *clock) thaw() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frozenUntil = time.Time{}
}

// wait blocks while the process is stopped and reports whether it runs again
// before ctx ends. A freeze extended while waiting is waited out as well.
func (c *clock) wait(ctx context.Context) bool {
	for {
		stopped := c.stall()
		if stopped <= 0 {
			return ctx.Err() == nil
		}

		timer := time.NewTimer(stopped)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return false
		}
	}
}
