package engine

import (
	"os"
	"testing"
)

// TestJITRefusalWeightsChargeTheHotFunction is the diagnostic's own gate.
//
// The number it exists to produce is "how much of the running program would
// this template reach", and there are two ways to get it wrong. Charging a
// refusal to the function that declared it rather than the one that ran is the
// first. Counting frame entries rather than work is the second, and it is the
// one that took longest to see: DeltaBlue runs 72% of its frame entries as
// machine code and that code is 11% of its CPU time, because a function called
// once and looping for a second costs an entry count nothing.
//
// So this runs two refused functions a hundredfold apart and requires the
// weights to say so.
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
		function warm(a) { return function () { return a; }; }   // op:CLOSURE
		function chilly(a) { try { return a; } catch (e) { return 0; } }
		var s = 0;
		for (var i = 0; i < `+itoa(hot)+`; i++) s += warm(i)();
		for (var i = 0; i < `+itoa(cold)+`; i++) s += chilly(i);
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
		case "op:CLOSURE":
			warm = r.Insns
		case "op:TRY_PUSH":
			chilly = r.Insns
		}
	}
	if warm == 0 {
		t.Fatalf("the hot function's refusal was not recorded; reasons seen: %v", w)
	}
	// Not an equality: the loop bodies are functions too, the tier only starts
	// counting once a function has been entered, and a refusal is recorded on
	// the entry that attempts compilation rather than the first.
	if warm < hot {
		t.Errorf("the hot function was charged %d instructions over %d calls", warm, hot)
	}
	if chilly >= warm {
		t.Errorf("the cold function (%d entries) outweighed the hot one (%d)", chilly, warm)
	}
	if w[0].Insns < w[len(w)-1].Insns {
		t.Error("the weights are not ordered heaviest first")
	}
}

// TestEnvOnHonoursItsValue is the regression test for a measurement bug rather
// than a code one.
//
// `GOANT_JIT=0` used to mean the tier was ON, because the check was for the
// variable's presence and not its value. Every A/B run of the tier against the
// interpreter therefore compared the tier against itself, and the conclusion
// drawn from all of them — "Octane is unchanged with the tier on" — was drawn
// from two identical arms.
func TestEnvOnHonoursItsValue(t *testing.T) {
	for _, tc := range []struct {
		v  string
		on bool
	}{
		{"", false}, {"0", false}, {"false", false}, {"FALSE", false},
		{"no", false}, {"off", false}, {" 0 ", false},
		{"1", true}, {"true", true}, {"yes", true}, {"on", true}, {"x", true},
	} {
		t.Setenv("GOANT_TEST_ENV_ON", tc.v)
		if got := envOn("GOANT_TEST_ENV_ON"); got != tc.on {
			t.Errorf("envOn(%q) = %v, want %v", tc.v, got, tc.on)
		}
	}
	t.Setenv("GOANT_TEST_ENV_ON", "")
	os.Unsetenv("GOANT_TEST_ENV_ON")
	if envOn("GOANT_TEST_ENV_ON") {
		t.Error("an absent variable read as on")
	}
}
