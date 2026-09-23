package main

import (
	"testing"
	"time"
)

func TestTheProfilesRatesAreInForceFromTheStart(t *testing.T) {
	f := newFaults(profile{hangRate: 0.1, errorRate: 0.2}, time.Now)

	if ft, ok := f.current(modeHang); !ok || ft.rate != 0.1 {
		t.Errorf("hang = %+v %v, want the profile's 0.1", ft, ok)
	}
	if ft, ok := f.current(modeError); !ok || ft.rate != 0.2 {
		t.Errorf("error = %+v %v, want the profile's 0.2", ft, ok)
	}
	if _, ok := f.current(modeReset); ok {
		t.Error("reset is in force, want nothing the profile did not set")
	}
	if !f.possible() {
		t.Error("possible = false, want a draw to be worth making")
	}
}

func TestWithoutRatesThereIsNothingToDecide(t *testing.T) {
	f := newFaults(profile{}, time.Now)

	if f.possible() {
		t.Error("possible = true for a profile with no rates")
	}
	if m, _ := f.choose(0); m != "" {
		t.Errorf("choose = %q, want a normal answer", m)
	}
}

func TestEachModeTakesASliceOfTheDraw(t *testing.T) {
	f := newFaults(profile{hangRate: 0.1, resetRate: 0.1, dripRate: 0.1, dripOver: time.Second, errorRate: 0.1}, time.Now)

	for draw, want := range map[float64]mode{
		0.05: modeHang, 0.15: modeReset, 0.25: modeDrip, 0.35: modeError, 0.5: "",
	} {
		if got, _ := f.choose(draw); got != want {
			t.Errorf("choose(%v) = %q, want %q", draw, got, want)
		}
	}
}

func TestAnInjectedFaultReplacesTheProfilesRateAndExpires(t *testing.T) {
	now := newFakeTime()
	f := newFaults(profile{hangRate: 0.01}, now.now)

	f.inject(modeHang, fault{rate: 0.5}, 30*time.Second)
	if ft, _ := f.current(modeHang); ft.rate != 0.5 {
		t.Errorf("hang rate = %v while injected, want 0.5 in place of 0.01", ft.rate)
	}

	now.advance(31 * time.Second)
	if ft, _ := f.current(modeHang); ft.rate != 0.01 {
		t.Errorf("hang rate = %v after expiry, want the profile's 0.01 back", ft.rate)
	}
}

func TestClearRemovesOnlyInjectedFaults(t *testing.T) {
	f := newFaults(profile{errorRate: 0.3}, time.Now)
	f.inject(modeHang, fault{rate: 1}, time.Minute)
	f.clear()

	if _, ok := f.current(modeHang); ok {
		t.Error("the injected hang survived clear")
	}
	if ft, ok := f.current(modeError); !ok || ft.rate != 0.3 {
		t.Error("clear removed the profile's own error rate")
	}
}

func TestSlowdownIsOneUnlessInjected(t *testing.T) {
	now := newFakeTime()
	f := newFaults(profile{}, now.now)

	if got := f.slowdown(); got != 1 {
		t.Errorf("slowdown = %v, want 1", got)
	}
	f.inject(modeSlow, fault{factor: 4}, 10*time.Second)
	if got := f.slowdown(); got != 4 {
		t.Errorf("slowdown = %v while injected, want 4", got)
	}
	now.advance(11 * time.Second)
	if got := f.slowdown(); got != 1 {
		t.Errorf("slowdown = %v after expiry, want 1", got)
	}
}

func TestActiveListsWhatIsLeftOfEachInjectedFault(t *testing.T) {
	now := newFakeTime()
	f := newFaults(profile{}, now.now)
	f.inject(modeHang, fault{rate: 0.5}, 30*time.Second)
	f.inject(modeDrip, fault{rate: 1, over: 2 * time.Second}, 5*time.Second)
	now.advance(10 * time.Second)

	listed := f.active()
	if len(listed) != 1 {
		t.Fatalf("active = %+v, want only the hang, the drip having expired", listed)
	}
	if got := listed[0]; got.Mode != modeHang || got.Rate != 0.5 || got.Remaining != "20s" {
		t.Errorf("active = %+v, want hang at 0.5 with 20s left", got)
	}
}
