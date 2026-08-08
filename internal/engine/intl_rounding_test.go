package engine

import (
	"math"
	"strconv"
	"testing"
)

// The nine rounding modes, on the four inputs that tell them apart: a value
// below the half, exactly on it with an even digit before, exactly on it with
// an odd digit before, and the same three negated. Written out rather than
// derived, because a derivation would share whatever the implementation
// believes and check nothing.
func TestRoundingModes(t *testing.T) {
	cases := []struct {
		in    string
		neg   bool
		modes map[string]string
	}{
		{"1.5", false, map[string]string{
			"ceil": "2", "floor": "1", "expand": "2", "trunc": "1",
			"halfCeil": "2", "halfFloor": "1", "halfExpand": "2",
			"halfTrunc": "1", "halfEven": "2",
		}},
		{"2.5", false, map[string]string{
			"ceil": "3", "floor": "2", "expand": "3", "trunc": "2",
			"halfCeil": "3", "halfFloor": "2", "halfExpand": "3",
			"halfTrunc": "2", "halfEven": "2",
		}},
		{"1.4", false, map[string]string{
			"ceil": "2", "floor": "1", "expand": "2", "trunc": "1",
			"halfCeil": "1", "halfFloor": "1", "halfExpand": "1",
			"halfTrunc": "1", "halfEven": "1",
		}},
		{"1.5", true, map[string]string{
			"ceil": "1", "floor": "2", "expand": "2", "trunc": "1",
			"halfCeil": "1", "halfFloor": "2", "halfExpand": "2",
			"halfTrunc": "1", "halfEven": "2",
		}},
		{"2.5", true, map[string]string{
			"ceil": "2", "floor": "3", "expand": "3", "trunc": "2",
			"halfCeil": "2", "halfFloor": "3", "halfExpand": "3",
			"halfTrunc": "2", "halfEven": "2",
		}},
	}
	for _, c := range cases {
		intPart, frac := expandDecimal(c.in)
		for _, mode := range roundingModes {
			want, ok := c.modes[mode]
			if !ok {
				t.Fatalf("test data has no expectation for %s", mode)
			}
			gotInt, gotFrac := roundDecimal(intPart, frac, 0, mode, 1, c.neg)
			if gotInt != want || gotFrac != "" {
				sign := ""
				if c.neg {
					sign = "-"
				}
				t.Errorf("round(%s%s, %s) = %s%s.%s, want %s%s",
					sign, c.in, mode, sign, gotInt, gotFrac, sign, want)
			}
		}
	}
}

// A carry that runs off the top has to add a digit rather than wrap: 9.9 to
// zero places is 10, not 0.
func TestRoundingCarriesPastTheTop(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"9.9", "10"}, {"99.5", "100"}, {"0.5", "1"}, {"0.4", "0"},
		{"999999999999999999999.5", "1000000000000000000000"},
	} {
		intPart, frac := expandDecimal(tc.in)
		got, _ := roundDecimal(intPart, frac, 0, "halfExpand", 1, false)
		if got != tc.want {
			t.Errorf("round(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// A rounding increment lands the result on a multiple of itself, at the scale
// the fraction digits set. Checked over every increment the option accepts, on
// a value that is not already a multiple of any of them.
func TestRoundingIncrementLandsOnAMultiple(t *testing.T) {
	intPart, frac := expandDecimal("1.337")
	for _, inc := range validRoundingIncrements {
		const places = 4
		gotInt, gotFrac := roundDecimal(intPart, frac, places, "halfExpand", inc, false)
		scaled, err := strconv.ParseInt(gotInt+gotFrac, 10, 64)
		if err != nil {
			t.Fatalf("increment %d produced %q.%q, which is not a number", inc, gotInt, gotFrac)
		}
		if scaled%int64(inc) != 0 {
			t.Errorf("increment %d: %d is not a multiple of it", inc, scaled)
		}
		// And it is the nearest such multiple: no other multiple is closer.
		want := int64(math.Round(13370/float64(inc))) * int64(inc)
		if scaled != want {
			t.Errorf("increment %d: got %d, want %d", inc, scaled, want)
		}
	}
}
