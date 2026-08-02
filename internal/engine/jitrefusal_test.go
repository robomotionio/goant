package engine

import "testing"

// TestJITRefusalWeightsChargeTheHotFunction is the diagnostic's own gate.
//
// The number it exists to produce is "how much of the running program would
// this template reach", and the way to get that wrong is to charge a refusal to
// the function that declared it rather than the one that ran. So this runs two
// refused functions a hundredfold apart and requires the weights to say so —
// which a per-function count would, and a per-reason count taken at compile time
// would not.
func TestJITRefusalWeightsChargeTheHotFunction(t *testing.T) {
	savedJIT, savedStats, savedTable := jitEnabled, jitStats.enabled, jitRefusals
	jitEnabled, jitStats.enabled = true, true
	jitRefusals.names, jitRefusals.index, jitRefusals.funcs =
		[]string{""}, map[string]uint32{}, nil
	t.Cleanup(func() {
		jitEnabled, jitStats.enabled, jitRefusals = savedJIT, savedStats, savedTable
	})

	const hot, cold = 2000, 20
	_, err := New().RunString("weights.js", `
		function warm(a) { return typeof a; }        // op:TYPEOF
		function chilly(a) { return a instanceof Object; }
		var s = 0;
		for (var i = 0; i < `+itoa(hot)+`; i++) s += warm(i).length;
		for (var i = 0; i < `+itoa(cold)+`; i++) s += chilly(i) ? 1 : 0;
		s;`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	w := JITRefusalWeights()
	if len(w) == 0 {
		t.Fatal("nothing was recorded; either everything compiled or the refusals are not being charged")
	}
	var warm, chilly uint64
	for _, r := range w {
		switch r.Reason {
		case "op:TYPEOF":
			warm = r.Entries
		case "op:INSTANCEOF":
			chilly = r.Entries
		}
	}
	if warm == 0 {
		t.Fatalf("the hot function's refusal was not recorded; reasons seen: %v", w)
	}
	// Not an equality: the loop bodies are functions too, the tier only starts
	// counting once a function has been entered, and a refusal is recorded on
	// the entry that attempts compilation rather than the first.
	if warm < hot/2 {
		t.Errorf("the hot function was charged %d entries out of %d calls", warm, hot)
	}
	if chilly >= warm {
		t.Errorf("the cold function (%d entries) outweighed the hot one (%d)", chilly, warm)
	}
	if w[0].Entries < w[len(w)-1].Entries {
		t.Error("the weights are not ordered heaviest first")
	}
}
