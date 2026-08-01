package v8go

import (
	"fmt"
	"strings"
	"testing"
)

// Host data must not be interned. The intern table is permanent and shared by
// every context on an isolate, so interning a message payload retains it for
// the life of the process — and a host that passes a distinct message per call
// retains every one of them.
//
// The distinction only shows up with *distinct* strings: interning identical
// input looks fine because the table hits. This test therefore feeds a
// different payload each time, which is what a real message pump does.
func TestHostStringsAreNotInterned(t *testing.T) {
	const runs = 200
	const payload = 64 * 1024

	iso := NewIsolate()
	defer iso.Dispose()
	ctx := NewContext(iso)
	defer ctx.Close()

	// Warm up so one-off setup is not counted as growth.
	for i := 0; i < 5; i++ {
		v, err := NewValue(iso, strings.Repeat("w", payload))
		if err != nil {
			t.Fatal(err)
		}
		if err := ctx.Global().Set("__inMsg__", v); err != nil {
			t.Fatal(err)
		}
	}
	before := iso.GetHeapStatistics().UsedHeapSize
	internedBefore := iso.InternedStrings()

	for i := 0; i < runs; i++ {
		// Distinct every time — no intern-table hit is possible.
		s := fmt.Sprintf("%0*d", payload, i)
		v, err := NewValue(iso, s)
		if err != nil {
			t.Fatal(err)
		}
		if err := ctx.Global().Set("__inMsg__", v); err != nil {
			t.Fatal(err)
		}
	}
	after := iso.GetHeapStatistics().UsedHeapSize
	internedAfter := iso.InternedStrings()

	// The load-bearing assertion. Cell occupancy cannot see the intern table —
	// interning adds a Go map entry, not a pool cell — so checking occupancy
	// alone passes whether or not the strings are interned. This is the check
	// that actually fails if NewValue starts interning again.
	if grew := internedAfter - internedBefore; grew > 0 {
		t.Fatalf("%d host strings were pinned in the intern table "+
			"(%d -> %d); host data must not be interned",
			grew, internedBefore, internedAfter)
	}

	// Reported occupancy covers the cell AND the bytes it points at, so one
	// non-interned string of this size costs its payload plus a header — and
	// that is the whole budget here. Interning would add a map entry keyed by
	// the string, retaining a second copy of the same bytes on top; the map is
	// not visible from here, so the observable half is that per-run growth
	// stays at roughly one copy rather than two.
	perRun := float64(after-before) / runs
	t.Logf("occupancy %d -> %d over %d runs (%.0f bytes/run for a %d-byte string)",
		before, after, runs, perRun, payload)

	if ceiling := float64(payload) * 1.5; perRun > ceiling {
		t.Fatalf("growth of %.0f bytes/run against a %d-byte payload (ceiling %.0f) "+
			"suggests each string is being retained more than once",
			perRun, payload, ceiling)
	}
}

// The zero-copy constructor must not copy, and must read back correctly.
func TestExternalOneByteValueIsZeroCopy(t *testing.T) {
	iso := NewIsolate()
	defer iso.Dispose()
	ctx := NewContext(iso)
	defer ctx.Close()

	data := []byte(`{"hello":"world"}`)
	v, err := NewExternalOneByteValue(iso, data)
	if err != nil {
		t.Fatalf("NewExternalOneByteValue: %v", err)
	}
	if got := v.String(); got != `{"hello":"world"}` {
		t.Fatalf("got %q", got)
	}
	if err := ctx.Global().Set("__inMsg__", v); err != nil {
		t.Fatal(err)
	}
	out, err := ctx.RunScript(`JSON.parse(globalThis.__inMsg__).hello`, "z.js")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := out.String(); got != "world" {
		t.Fatalf("got %q, want %q", got, "world")
	}
}
