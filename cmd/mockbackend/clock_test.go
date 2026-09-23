package main

import (
	"context"
	"math"
	"testing"
	"time"
)

// fakeTime is a clock the test moves by hand.
type fakeTime struct{ at time.Time }

func (f *fakeTime) now() time.Time          { return f.at }
func (f *fakeTime) advance(d time.Duration) { f.at = f.at.Add(d) }

func newFakeTime() *fakeTime { return &fakeTime{at: time.Unix(1_000_000, 0)} }

func TestColdStartSlowsDownAndRecovers(t *testing.T) {
	now := newFakeTime()
	c := newClock(profile{coldStart: 30 * time.Second, coldFactor: 4}, now.now)

	for _, step := range []struct {
		after time.Duration
		want  float64
	}{
		{0, 4},
		{15 * time.Second, 2.5},
		{15 * time.Second, 1},
		{time.Hour, 1},
	} {
		now.advance(step.after)
		if got := c.factor(); math.Abs(got-step.want) > 1e-9 {
			t.Errorf("factor %s after start = %v, want %v", now.at.Sub(time.Unix(1_000_000, 0)), got, step.want)
		}
	}
}

func TestWithoutAColdStartTheBackendRunsAtFullSpeed(t *testing.T) {
	now := newFakeTime()
	for _, p := range []profile{{}, {coldStart: time.Minute, coldFactor: 1}, {coldFactor: 4}} {
		if got := newClock(p, now.now).factor(); got != 1 {
			t.Errorf("factor for %+v = %v, want 1", p, got)
		}
	}
}

func TestPausesComeAtTheEndOfEachPeriod(t *testing.T) {
	now := newFakeTime()
	c := newClock(profile{pauseEvery: 15 * time.Second, pause: 150 * time.Millisecond}, now.now)

	for _, step := range []struct {
		after time.Duration
		want  time.Duration
	}{
		{0, 0},
		{14800 * time.Millisecond, 0},
		{100 * time.Millisecond, 100 * time.Millisecond},
		{100 * time.Millisecond, 0},
		{14850 * time.Millisecond, 150 * time.Millisecond},
	} {
		now.advance(step.after)
		if got := c.stall(); got != step.want {
			t.Errorf("stall at %s = %s, want %s", now.at.Sub(time.Unix(1_000_000, 0)), got, step.want)
		}
	}
}

func TestAFreezeLastsUntilItEndsOrIsThawed(t *testing.T) {
	now := newFakeTime()
	c := newClock(profile{}, now.now)

	c.freeze(5 * time.Second)
	if got := c.stall(); got != 5*time.Second {
		t.Errorf("stall = %s, want 5s", got)
	}

	now.advance(2 * time.Second)
	c.freeze(time.Second) // shorter than what is left: no effect
	if got := c.stall(); got != 3*time.Second {
		t.Errorf("stall = %s, want the 3s left of the first freeze", got)
	}

	c.thaw()
	if got := c.stall(); got != 0 {
		t.Errorf("stall = %s after thaw, want 0", got)
	}
}

func TestWaitReturnsWhenTheFreezeEnds(t *testing.T) {
	c := newClock(profile{}, time.Now)
	c.freeze(40 * time.Millisecond)

	started := time.Now()
	if !c.wait(context.Background()) {
		t.Fatal("wait gave up, want it to outlast the freeze")
	}
	if waited := time.Since(started); waited < 40*time.Millisecond {
		t.Errorf("waited %s, want at least the 40ms freeze", waited)
	}
}

func TestWaitGivesUpWithTheContext(t *testing.T) {
	c := newClock(profile{}, time.Now)
	c.freeze(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if c.wait(ctx) {
		t.Error("wait reported the process running during an hour's freeze")
	}
}

func TestAPhaseShiftsWhenThePausesCome(t *testing.T) {
	now := newFakeTime()
	c := newClock(profile{pauseEvery: 15 * time.Second, pause: 150 * time.Millisecond}, now.now)
	c.phase = 5 * time.Second

	// Shifted by five seconds, the first pause comes at ten seconds in.
	now.advance(9900 * time.Millisecond)
	if got := c.stall(); got != 100*time.Millisecond {
		t.Errorf("stall at 9.9s = %s, want 100ms with the phase shifted by 5s", got)
	}
}

func TestBackendsWithDifferentSeedsPauseAtDifferentTimes(t *testing.T) {
	p := profile{name: "backend-1", pauseEvery: 15 * time.Second, pause: 150 * time.Millisecond}
	if newServer(p, 1).clock.phase == newServer(p, 2).clock.phase {
		t.Error("two backends with different seeds pause in step, want their phases apart")
	}
}
