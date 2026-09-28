package balancer

import (
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
)

// named is a pool of n backends called backend-1:5678 to backend-n:5678, each
// of weight one.
func named(n int) []*Backend {
	backends := make([]*Backend, 0, n)
	for i := 1; i <= n; i++ {
		backends = append(backends, NewBackend("backend-"+strconv.Itoa(i)+":5678", 1))
	}
	return backends
}

// assign places keys session-0 to session-(n-1) on backends, without a bound.
func assign(t *testing.T, backends []*Backend, n int) map[string]*Backend {
	t.Helper()
	strategy := NewConsistentHash(0)
	placed := make(map[string]*Backend, n)
	for i := range n {
		key := "session-" + strconv.Itoa(i)
		backend, err := strategy.SelectFor(key, backends)
		if err != nil {
			t.Fatalf("SelectFor(%s) failed: %v", key, err)
		}
		placed[key] = backend
	}
	return placed
}

// without returns backends with the given ones left out.
func without(backends []*Backend, left ...*Backend) []*Backend {
	return slices.DeleteFunc(slices.Clone(backends), func(b *Backend) bool {
		return slices.Contains(left, b)
	})
}

// busy gives backend n requests in flight.
func busy(backend *Backend, n int) {
	for range n {
		backend.Acquire()
	}
}

func TestConsistentHashSendsAKeyToTheSameBackendInAnyOrder(t *testing.T) {
	backends := named(10)
	strategy := NewConsistentHash(0)
	home, _ := strategy.SelectFor("session-42", backends)

	shuffled := slices.Clone(backends)
	for range 20 {
		rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got, _ := strategy.SelectFor("session-42", shuffled); got != home {
			t.Fatalf("session-42 went to %s after shuffling the pool, want %s every time", got.Addr, home.Addr)
		}
	}
}

// TestConsistentHashPlacementIsFixedAcrossVersions pins the placement of a few
// keys. A key must reach the same backend after a restart and from every
// balancer in front of the pool, so a change to the hash is a change to where
// every client's data lives; this test makes such a change deliberate.
func TestConsistentHashPlacementIsFixedAcrossVersions(t *testing.T) {
	backends := named(10)
	strategy := NewConsistentHash(0)
	for key, want := range map[string]string{
		"session-0":    "backend-1:5678",
		"session-1":    "backend-4:5678",
		"session-2":    "backend-4:5678",
		"session-3":    "backend-1:5678",
		"session-4":    "backend-3:5678",
		"203.0.113.7":  "backend-10:5678",
		"user@example": "backend-8:5678",
	} {
		if got, _ := strategy.SelectFor(key, backends); got.Addr != want {
			t.Errorf("%s went to %s, want %s", key, got.Addr, want)
		}
	}
}

func TestConsistentHashSpreadsKeysEvenly(t *testing.T) {
	const keys = 100_000
	counts := map[*Backend]int{}
	for _, backend := range assign(t, named(10), keys) {
		counts[backend]++
	}

	for backend, count := range counts {
		// A tenth each, within 5%: the binomial spread at this size is under 1%.
		if count < keys/10*95/100 || count > keys/10*105/100 {
			t.Errorf("%s took %d keys, want %d within 5%%", backend.Addr, count, keys/10)
		}
	}
}

func TestConsistentHashFollowsTheWeights(t *testing.T) {
	const keys = 100_000
	backends := []*Backend{
		NewBackend("backend-1:5678", 1), NewBackend("backend-2:5678", 2),
		NewBackend("backend-3:5678", 3), NewBackend("backend-4:5678", 4),
	}
	counts := map[*Backend]int{}
	for _, backend := range assign(t, backends, keys) {
		counts[backend]++
	}

	for _, backend := range backends {
		want := keys * backend.Weight() / 10
		if got := counts[backend]; got < want-keys/100 || got > want+keys/100 {
			t.Errorf("%s of weight %d took %d keys, want %d within 1%% of all keys",
				backend.Addr, backend.Weight(), got, want)
		}
	}
}

func TestRemovingABackendMovesOnlyItsOwnKeys(t *testing.T) {
	backends := named(10)
	before := assign(t, backends, 20_000)
	leaving := backends[2]
	after := assign(t, without(backends, leaving), 20_000)

	moved := 0
	for key, was := range before {
		switch {
		case was == leaving && after[key] == leaving:
			t.Fatalf("%s still went to %s after it left", key, leaving.Addr)
		case was != leaving && after[key] != was:
			t.Fatalf("%s moved from %s to %s, though its backend stayed", key, was.Addr, after[key].Addr)
		case was == leaving:
			moved++
		}
	}
	if moved == 0 {
		t.Fatal("no key belonged to the backend that left")
	}
}

func TestAddingABackendTakesOnlyTheKeysItNowWins(t *testing.T) {
	const keys = 20_000
	backends := named(10)
	before := assign(t, backends, keys)
	joining := NewBackend("backend-11:5678", 1)
	after := assign(t, append(slices.Clone(backends), joining), keys)

	moved := 0
	for key, was := range before {
		if after[key] == was {
			continue
		}
		if after[key] != joining {
			t.Fatalf("%s moved from %s to %s, not to the backend that joined", key, was.Addr, after[key].Addr)
		}
		moved++
	}
	// An eleventh of the keys, within 1% of all keys.
	if want := keys / 11; moved < want-keys/100 || moved > want+keys/100 {
		t.Errorf("%d keys moved to the new backend, want about %d", moved, want)
	}
}

func TestAKeyKeepsItsOrderWhateverElseLeaves(t *testing.T) {
	backends := named(10)
	strategy := NewConsistentHash(0)
	first := strategy.Home("session-7", backends)
	second := strategy.Home("session-7", without(backends, first))

	// A retry excludes the backend already tried; whichever other backends are
	// unhealthy as well, the key's second choice stays the same.
	for _, gone := range without(backends, first, second) {
		got, _ := strategy.SelectFor("session-7", without(backends, first, gone))
		if got != second {
			t.Errorf("with %s and %s gone, session-7 went to %s, want its second choice %s",
				first.Addr, gone.Addr, got.Addr, second.Addr)
		}
	}
}

func TestTheBoundSendsAKeyOnWhenItsHomeIsFull(t *testing.T) {
	backends := named(4)
	strategy := NewConsistentHash(125)
	home := strategy.Home("session-7", backends)
	second := strategy.Home("session-7", without(backends, home))

	// Six requests in flight and this one make seven, a share of 1.75 per
	// backend; at 125% each backend may serve up to 3.
	busy(home, 3)
	for _, other := range without(backends, home) {
		busy(other, 1)
	}
	if got, _ := strategy.SelectFor("session-7", backends); got != second {
		t.Errorf("with its home full, session-7 went to %s, want its second choice %s", got.Addr, second.Addr)
	}

	// Once the rest of the pool is as busy, the home backend is no longer
	// ahead of its share and keeps the key: nine in flight and this one make
	// ten, a share of 2.5, and a bound of 4.
	for _, other := range without(backends, home) {
		busy(other, 1)
	}
	if got, _ := strategy.SelectFor("session-7", backends); got != home {
		t.Errorf("with room at home, session-7 went to %s, want %s", got.Addr, home.Addr)
	}
}

func TestTheBoundGivesAHeavierBackendMoreRoom(t *testing.T) {
	light, heavy := NewBackend("backend-1:5678", 1), NewBackend("backend-2:5678", 3)
	backends := []*Backend{light, heavy}
	strategy := NewConsistentHash(100)

	// Seven in flight and one more make eight: shares of 2 and 6.
	busy(light, 1)
	busy(heavy, 6)
	for i := range 1000 {
		key := "session-" + strconv.Itoa(i)
		if strategy.Home(key, backends) != heavy {
			continue
		}
		if got, _ := strategy.SelectFor(key, backends); got != light {
			t.Fatalf("%s stayed on the full heavy backend, want it sent to the light one", key)
		}
		return
	}
	t.Fatal("no key belonged to the heavy backend")
}

func TestTheBoundAlwaysFindsABackendWithRoom(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	for trial := range 500 {
		backends := named(1 + random.IntN(12))
		for _, backend := range backends {
			backend.SetWeight(1 + random.IntN(4))
			busy(backend, random.IntN(20))
		}
		strategy := NewConsistentHash(100 + random.IntN(100))

		got, err := strategy.SelectFor("session-"+strconv.Itoa(trial), backends)
		if err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		if !strategy.room(backends)(got) {
			t.Fatalf("trial %d: %s is over its bound, and another backend had room", trial, got.Addr)
		}
	}
}

func TestWithoutABoundTheHomeTakesEverything(t *testing.T) {
	backends := named(4)
	strategy := NewConsistentHash(0)
	home := strategy.Home("session-7", backends)
	busy(home, 1000)

	if got, _ := strategy.SelectFor("session-7", backends); got != home {
		t.Errorf("session-7 went to %s, want its home %s however busy", got.Addr, home.Addr)
	}
}

func TestARequestWithoutAKeyGoesToTheLessBusy(t *testing.T) {
	idle, loaded := NewBackend("backend-1:5678", 1), NewBackend("backend-2:5678", 1)
	busy(loaded, 5)

	// With two backends the power of two choices always compares both.
	if got, _ := NewConsistentHash(0).Select([]*Backend{loaded, idle}); got != idle {
		t.Errorf("a request without a key went to %s, want the idle backend", got.Addr)
	}
}

func TestConsistentHashWithNothingToChooseFrom(t *testing.T) {
	strategy := NewConsistentHash(125)
	if _, err := strategy.SelectFor("session-1", nil); !errors.Is(err, ErrNoBackends) {
		t.Errorf("SelectFor on an empty pool returned %v, want ErrNoBackends", err)
	}
	if home := strategy.Home("session-1", nil); home != nil {
		t.Errorf("Home on an empty pool returned %s, want nil", home.Addr)
	}
}
