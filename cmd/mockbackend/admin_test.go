package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseInjection(t *testing.T) {
	tests := []struct {
		form  string
		mode  mode
		want  fault
		lasts time.Duration
	}{
		{"mode=hang", modeHang, fault{rate: 1}, 30 * time.Second},
		{"mode=hang&rate=0.25&for=10s", modeHang, fault{rate: 0.25}, 10 * time.Second},
		{"mode=drip", modeDrip, fault{rate: 1, over: 2 * time.Second}, 30 * time.Second},
		{"mode=drip&over=5s", modeDrip, fault{rate: 1, over: 5 * time.Second}, 30 * time.Second},
		{"mode=slow", modeSlow, fault{factor: 4}, 30 * time.Second},
		{"mode=slow&factor=3", modeSlow, fault{factor: 3}, 30 * time.Second},
		{"mode=freeze&for=2s", modeFreeze, fault{}, 2 * time.Second},
	}
	for _, test := range tests {
		form, _ := url.ParseQuery(test.form)
		m, ft, lasts, err := parseInjection(form)
		if err != nil {
			t.Errorf("%s: unexpected error %v", test.form, err)
			continue
		}
		if m != test.mode || ft != test.want || lasts != test.lasts {
			t.Errorf("%s = %q %+v %s, want %q %+v %s", test.form, m, ft, lasts, test.mode, test.want, test.lasts)
		}
	}
}

func TestParseInjectionRefusesWhatItCannotDo(t *testing.T) {
	for _, form := range []string{
		"", "mode=explode", "mode=hang&rate=2", "mode=hang&rate=0", "mode=hang&for=-1s",
		"mode=hang&for=1h", "mode=hang&for=soon", "mode=slow&factor=0.5", "mode=drip&over=0s",
	} {
		values, _ := url.ParseQuery(form)
		if _, _, _, err := parseInjection(values); err == nil {
			t.Errorf("%q was accepted, want it refused", form)
		}
	}
}

// admin sends one request to the admin handler and decodes the state it
// answers with.
func admin(t *testing.T, s *server, method, path, form string) (int, state) {
	t.Helper()

	request := httptest.NewRequest(method, path, strings.NewReader(form))
	if form != "" {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	recorder := httptest.NewRecorder()
	s.adminHandler().ServeHTTP(recorder, request)

	var current state
	if recorder.Code == http.StatusOK {
		if err := json.NewDecoder(recorder.Body).Decode(&current); err != nil {
			t.Fatalf("decoding the state failed: %v", err)
		}
	}
	return recorder.Code, current
}

func TestTheAdminPortInjectsListsAndClearsFaults(t *testing.T) {
	s := newServer(profile{name: "backend-3"}, 1)

	code, current := admin(t, s, http.MethodPost, "/faults", "mode=hang&rate=0.5&for=30s")
	if code != http.StatusOK || len(current.Injected) != 1 || current.Injected[0].Mode != modeHang {
		t.Fatalf("POST answered %d with %+v, want the hang in force", code, current.Injected)
	}

	if _, current = admin(t, s, http.MethodGet, "/faults", ""); len(current.Injected) != 1 {
		t.Errorf("GET listed %+v, want the hang", current.Injected)
	}
	if current.Backend != "backend-3" {
		t.Errorf("backend = %q, want backend-3", current.Backend)
	}

	if _, current = admin(t, s, http.MethodDelete, "/faults", ""); len(current.Injected) != 0 {
		t.Errorf("DELETE left %+v, want nothing", current.Injected)
	}
}

func TestClearingFaultsThawsAFrozenBackend(t *testing.T) {
	s := newServer(profile{name: "backend-3"}, 1)

	admin(t, s, http.MethodPost, "/faults", "mode=freeze&for=1m")
	if s.clock.stall() <= 0 {
		t.Fatal("the backend is running, want it frozen")
	}
	admin(t, s, http.MethodDelete, "/faults", "")
	if s.clock.stall() != 0 {
		t.Error("the backend is still frozen after the faults were cleared")
	}
}

func TestTheAdminPortClearsTheCache(t *testing.T) {
	s := newServer(profile{name: "backend-3", cacheSize: 10}, 1)
	s.cache.seen("session-1")

	if _, current := admin(t, s, http.MethodPost, "/cache/clear", ""); current.CacheKeys != 0 {
		t.Errorf("cache keys = %d after clearing, want 0", current.CacheKeys)
	}
}

func TestTheAdminPortRefusesAnUnknownMode(t *testing.T) {
	if code, _ := admin(t, newServer(profile{name: "backend-3"}, 1), http.MethodPost, "/faults", "mode=explode"); code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", code, http.StatusBadRequest)
	}
}

func TestTheTrafficPortHasNoAdminEndpoints(t *testing.T) {
	s := newServer(profile{name: "backend-3"}, 1)

	// Anything that reaches the traffic port could have come through the
	// balancer, so /faults there must be an ordinary request.
	status, backend, body := get(t, s, "/faults")
	if status != http.StatusOK || backend != "backend-3" || body != "backend-3\n" {
		t.Errorf("/faults on the traffic port answered %d %q %q, want the ordinary answer", status, backend, body)
	}
	if len(s.faults.active()) != 0 {
		t.Error("a request on the traffic port injected a fault")
	}
}
