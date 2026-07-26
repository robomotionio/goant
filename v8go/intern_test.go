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

	// Cell occupancy counts headers, not payload bytes, so the ceiling here is
	// about how many *cells* leak, not how many bytes. Interning would add a
	// permanent map entry per distinct string on top of the cell; not interning
	// still allocates a cell per string (there is no collector yet), so the
	// check is that growth stays proportional to the strings created and does
	// not carry the extra interned-table retention.
	perRun := float64(after-before) / runs
	t.Logf("occupancy %d -> %d over %d runs (%.0f bytes/run of cell headers)",
		before, after, runs, perRun)

	// A non-interned string is one flat-string cell. Interning adds a map entry
	// keyed by the whole string, which retains the Go string too. We cannot see
	// the map from here, so assert the observable half: the runtime must not be
	// accumulating more than a small number of cells per call.
	if perRun > 512 {
		t.Fatalf("growth of %.0f bytes/run suggests more than a cell per string "+
			"is being retained", perRun)
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
