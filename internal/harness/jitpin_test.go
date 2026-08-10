package harness

import (
	"os"
	"slices"
	"testing"
)

// The tier a harness measured has been wrong twice in this repo's history, both
// times because something decided it by silence. PinJIT exists to make silence
// impossible, so these assert the silence case hardest.
func TestPinJIT(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  bool
		val  string
		want []string
	}{
		{"unset pins the tier off", false, "", []string{"TZ=UTC", "GOANT_JIT=0"}},
		{"an explicit on is left alone", true, "1", []string{"TZ=UTC"}},
		{"an explicit off is left alone", true, "0", []string{"TZ=UTC"}},
		// Set-but-empty is off to the engine (jit_tier.go treats "" as off) and
		// present to LookupEnv. Adding GOANT_JIT=0 here would agree with it, but
		// by re-deciding rather than deferring -- and a second opinion about what
		// the value means is the bug this package exists to avoid.
		{"set to empty is still the engine's call", true, "", []string{"TZ=UTC"}},
		{"a spelling of no is the engine's call", true, "off", []string{"TZ=UTC"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("GOANT_JIT", tc.val)
			} else {
				unsetJIT(t)
			}
			got := PinJIT([]string{"TZ=UTC"})
			if !slices.Equal(got, tc.want) {
				t.Errorf("PinJIT() = %q, want %q", got, tc.want)
			}
		})
	}
}

// unsetJIT removes GOANT_JIT for the duration of the test. t.Setenv registers
// the restore; there is no t.Unsetenv, so set it and then clear it, which keeps
// the same cleanup.
func unsetJIT(t *testing.T) {
	t.Helper()
	t.Setenv("GOANT_JIT", "")
	if err := os.Unsetenv("GOANT_JIT"); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
}
