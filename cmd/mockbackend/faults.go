package main

import (
	"slices"
	"sync"
	"time"
)

// mode is one way a backend can misbehave.
type mode string

const (
	modeHang   mode = "hang"   // hold the request without ever answering
	modeReset  mode = "reset"  // drop the connection half way through the answer
	modeDrip   mode = "drip"   // spread the answer out over a few seconds
	modeError  mode = "error"  // answer 500
	modeSlow   mode = "slow"   // take factor times as long over every request
	modeFreeze mode = "freeze" // stop the whole process, health check included
)

// perRequest are the modes decided for each request, in the order they take
// their slice of the random draw.
var perRequest = []mode{modeHang, modeReset, modeDrip, modeError}

// fault is one misbehaviour in force.
type fault struct {
	rate   float64       // the share of requests, for the per-request modes
	factor float64       // how much slower, for slow
	over   time.Duration // how long the answer takes to drip out
	until  time.Time     // when an injected fault ends; zero for the profile's
}

// faults holds the rates the profile sets, which last the whole run, and the
// faults injected through the admin port, which take over their mode until
// they expire. An injected fault replaces the profile's rate for its mode
// rather than adding to it, so "hang half the requests" means half.
type faults struct {
	now func() time.Time

	mu       sync.Mutex
	profile  map[mode]fault
	injected map[mode]fault
}

// newFaults returns the faults of a backend with profile p.
func newFaults(p profile, now func() time.Time) *faults {
	f := &faults{now: now, profile: map[mode]fault{}, injected: map[mode]fault{}}
	for m, ft := range map[mode]fault{
		modeHang:  {rate: p.hangRate},
		modeReset: {rate: p.resetRate},
		modeDrip:  {rate: p.dripRate, over: p.dripOver},
		modeError: {rate: p.errorRate},
	} {
		if ft.rate > 0 {
			f.profile[m] = ft
		}
	}
	return f
}

// current is the fault in force for m, if any: the injected one while it
// lasts, the profile's otherwise.
func (f *faults) current(m mode) (fault, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if ft, injected := f.injected[m]; injected {
		if f.now().Before(ft.until) {
			return ft, true
		}
		delete(f.injected, m)
	}
	ft, set := f.profile[m]
	return ft, set
}

// possible reports whether any per-request mode has a rate, so that a backend
// with none spends no random draw deciding.
func (f *faults) possible() bool {
	for _, m := range perRequest {
		if ft, ok := f.current(m); ok && ft.rate > 0 {
			return true
		}
	}
	return false
}

// choose picks the mode of one request for a uniform draw u in [0, 1). Each
// mode in force takes a slice of the interval as wide as its rate, in the
// order of perRequest; the rest of the interval is a normal answer.
func (f *faults) choose(u float64) (mode, fault) {
	edge := 0.0
	for _, m := range perRequest {
		ft, ok := f.current(m)
		if !ok || ft.rate <= 0 {
			continue
		}
		edge += ft.rate
		if u < edge {
			return m, ft
		}
	}
	return "", fault{}
}

// slowdown is the factor the slow mode multiplies service time by, and 1 while
// it is not in force.
func (f *faults) slowdown() float64 {
	if ft, ok := f.current(modeSlow); ok && ft.factor > 1 {
		return ft.factor
	}
	return 1
}

// inject puts ft in force for m until d has passed.
func (f *faults) inject(m mode, ft fault, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ft.until = f.now().Add(d)
	f.injected[m] = ft
}

// clear removes every injected fault; the profile's own rates stay.
func (f *faults) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.injected)
}

// activeFault describes an injected fault for the admin port.
type activeFault struct {
	Mode      mode    `json:"mode"`
	Rate      float64 `json:"rate,omitempty"`
	Factor    float64 `json:"factor,omitempty"`
	Over      string  `json:"over,omitempty"`
	Remaining string  `json:"remaining"`
}

// active lists the injected faults still in force, ordered by mode.
func (f *faults) active() []activeFault {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := f.now()
	listed := []activeFault{}
	for m, ft := range f.injected {
		left := ft.until.Sub(now)
		if left <= 0 {
			continue
		}
		entry := activeFault{Mode: m, Rate: ft.rate, Factor: ft.factor,
			Remaining: left.Round(100 * time.Millisecond).String()}
		if ft.over > 0 {
			entry.Over = ft.over.String()
		}
		listed = append(listed, entry)
	}
	slices.SortFunc(listed, func(a, b activeFault) int {
		switch {
		case a.Mode < b.Mode:
			return -1
		case a.Mode > b.Mode:
			return 1
		}
		return 0
	})
	return listed
}

// profileRates lists the per-request rates the profile sets.
func (f *faults) profileRates() map[mode]float64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	rates := make(map[mode]float64, len(f.profile))
	for m, ft := range f.profile {
		rates[m] = ft.rate
	}
	return rates
}
