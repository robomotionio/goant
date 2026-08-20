package engine

import "testing"

// Math.random was seeded from a fixed constant, so every Runtime replayed one
// sequence: a host that builds a Runtime per script — which is what a robot
// restart, a cold node, or any run-once flow does — handed out the same "random"
// number every time. The symptom customers saw was uuidv4() returning one UUID
// forever.
func TestRandomDiffersBetweenRuntimes(t *testing.T) {
	const n = 64
	seen := make(map[float64]int, n)
	for i := 0; i < n; i++ {
		seen[New().nextRandom()]++
	}
	if len(seen) != n {
		t.Fatalf("first Math.random() of %d fresh Runtimes gave %d distinct values, want %d", n, len(seen), n)
	}
}

// A realm shares its parent's value pools but not its generator state, so
// $262.createRealm — or any second global on one isolate — does not restart the
// sequence either.
func TestRandomDiffersBetweenRealms(t *testing.T) {
	rt := New()
	a, b := rt.nextRandom(), rt.NewRealm().nextRandom()
	if a == b {
		t.Fatalf("a realm repeated its parent's first Math.random(): %v", a)
	}
}

// The stream itself still has to be a stream: in range, and not stuck.
func TestRandomStaysInRange(t *testing.T) {
	rt := New()
	first := rt.nextRandom()
	same := 0
	for i := 0; i < 10000; i++ {
		x := rt.nextRandom()
		if !(x >= 0 && x < 1) {
			t.Fatalf("Math.random() returned %v, outside [0,1)", x)
		}
		if x == first {
			same++
		}
	}
	if same > 1 {
		t.Fatalf("value %v repeated %d times in 10000 draws", first, same)
	}
}
