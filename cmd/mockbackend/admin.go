package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// defaultFaultDuration is how long an injected fault lasts when the request
	// does not say.
	defaultFaultDuration = 30 * time.Second
	// maxFaultDuration bounds an injected fault, so that one left behind by a
	// demonstration cannot spoil the next measurement by much.
	maxFaultDuration = 10 * time.Minute
	// defaultSlowFactor and defaultDripOver are the strengths of slow and drip
	// when the request does not give them.
	defaultSlowFactor = 4.0
	defaultDripOver   = 2 * time.Second
)

// adminHandler serves the admin port, through which the demo console or a
// person changes how the backend behaves while it runs, and Prometheus reads
// what it has been doing. It is a separate server on a separate port for two
// reasons. The balancer forwards every path on the traffic port, so an
// endpoint there could be reached by any client of the balancer. And nothing
// here waits on the fault layer, so it answers while the backend is hanging or
// frozen — exactly when someone wants to stop it, and when its metrics matter
// most.
func (s *server) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /faults", s.listFaults)
	mux.HandleFunc("POST /faults", s.injectFault)
	mux.HandleFunc("DELETE /faults", s.clearFaults)
	mux.HandleFunc("POST /cache/clear", s.clearCache)
	mux.HandleFunc("GET /metrics", s.metrics)
	return mux
}

// state is what the admin port reports about the backend.
type state struct {
	Backend    string           `json:"backend"`
	Injected   []activeFault    `json:"injected"`
	Profile    map[mode]float64 `json:"profile_rates,omitempty"`
	CacheKeys  int              `json:"cache_keys"`
	CacheSize  int              `json:"cache_size"`
	ColdFactor float64          `json:"cold_factor"`
	Waiting    int64            `json:"waiting"`
}

// state reads the backend's current state.
func (s *server) state() state {
	current := state{
		Backend:    s.profile.name,
		Injected:   s.faults.active(),
		Profile:    s.faults.profileRates(),
		CacheSize:  s.profile.cacheSize,
		ColdFactor: s.clock.factor(),
		Waiting:    s.waiting.Load(),
	}
	if s.cache != nil {
		current.CacheKeys = s.cache.size()
	}
	return current
}

func (s *server) listFaults(w http.ResponseWriter, _ *http.Request) {
	writeState(w, s.state())
}

func (s *server) injectFault(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m, ft, lasts, err := parseInjection(r.Form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if m == modeFreeze {
		s.clock.freeze(lasts)
	}
	s.faults.inject(m, ft, lasts)
	s.counts.injected[m].Add(1)
	slog.Info("fault injected", "backend", s.profile.name, "mode", m,
		"rate", ft.rate, "factor", ft.factor, "over", ft.over, "for", lasts)
	writeState(w, s.state())
}

func (s *server) clearFaults(w http.ResponseWriter, _ *http.Request) {
	s.faults.clear()
	s.clock.thaw()
	slog.Info("faults cleared", "backend", s.profile.name)
	writeState(w, s.state())
}

func (s *server) clearCache(w http.ResponseWriter, _ *http.Request) {
	if s.cache != nil {
		s.cache.clear()
	}
	slog.Info("cache cleared", "backend", s.profile.name)
	writeState(w, s.state())
}

// writeState answers with the backend's state as JSON.
func writeState(w http.ResponseWriter, current state) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(current)
}

// parseInjection reads a fault from the admin port's form: its mode, its
// strength, and how long it lasts. Strengths default to the plainest case —
// every request for the per-request modes, four times slower for slow — so
// that "mode=hang" alone does what it says.
func parseInjection(form url.Values) (mode, fault, time.Duration, error) {
	m := mode(form.Get("mode"))

	lasts := defaultFaultDuration
	if raw := form.Get("for"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return "", fault{}, 0, fmt.Errorf("for %q is not a positive duration", raw)
		}
		lasts = parsed
	}
	if lasts > maxFaultDuration {
		return "", fault{}, 0, fmt.Errorf("for %s is longer than the %s a fault may last", lasts, maxFaultDuration)
	}

	var ft fault
	switch m {
	case modeHang, modeReset, modeDrip, modeError:
		ft.rate = 1
		if raw := form.Get("rate"); raw != "" {
			rate, err := strconv.ParseFloat(raw, 64)
			if err != nil || rate <= 0 || rate > 1 {
				return "", fault{}, 0, fmt.Errorf("rate %q is not a share above 0 and at most 1", raw)
			}
			ft.rate = rate
		}
		if m == modeDrip {
			ft.over = defaultDripOver
			if raw := form.Get("over"); raw != "" {
				over, err := time.ParseDuration(raw)
				if err != nil || over <= 0 {
					return "", fault{}, 0, fmt.Errorf("over %q is not a positive duration", raw)
				}
				ft.over = over
			}
		}
	case modeSlow:
		ft.factor = defaultSlowFactor
		if raw := form.Get("factor"); raw != "" {
			factor, err := strconv.ParseFloat(raw, 64)
			if err != nil || factor <= 1 {
				return "", fault{}, 0, fmt.Errorf("factor %q is not a number above 1", raw)
			}
			ft.factor = factor
		}
	case modeFreeze:
	default:
		return "", fault{}, 0, errors.New("mode must be one of hang, reset, drip, error, slow or freeze")
	}
	return m, ft, lasts, nil
}
