package engine

import "testing"

// The grammar, checked on the shapes that tell its rules apart. Written out
// rather than derived: a derivation would share whatever the parser believes.
func TestParseISODuration(t *testing.T) {
	// Fields in durationUnits' order: years, months, weeks, days, hours,
	// minutes, seconds, milliseconds, microseconds, nanoseconds.
	for in, want := range map[string][10]float64{
		"P1Y":            {1},
		"P1Y2M3W4D":      {1, 2, 3, 4},
		"PT1H":           {0, 0, 0, 0, 1},
		"PT1H30M":        {0, 0, 0, 0, 1, 30},
		"P1DT2H3M4S":     {0, 0, 0, 1, 2, 3, 4},
		"-P1Y":           {-1},
		"+P1Y":           {1},
		"−P1Y":           {-1}, // U+2212 MINUS SIGN
		"p1y":            {1},  // the letters are case-insensitive
		"PT0S":           {},
		"PT4.5S":         {0, 0, 0, 0, 0, 0, 4, 500},
		"PT4,5S":         {0, 0, 0, 0, 0, 0, 4, 500}, // comma is a decimal point
		"PT1.5H":         {0, 0, 0, 0, 1, 30},
		"PT1.5M":         {0, 0, 0, 0, 0, 1, 30},
		"PT0.000000001S": {0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
		"PT0.001S":       {0, 0, 0, 0, 0, 0, 0, 1},
		"PT0.000001S":    {0, 0, 0, 0, 0, 0, 0, 0, 1},
	} {
		got, ok := parseISODuration(in)
		if !ok {
			t.Errorf("parseISODuration(%q) rejected a valid duration", in)
			continue
		}
		if got != want {
			t.Errorf("parseISODuration(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseISODurationRejects(t *testing.T) {
	for _, in := range []string{
		"", "P", "PT", "1Y", "P1YT", "PT1H1", "P1", "Y1P",
		"P1H",      // an hour is a time component and needs the T
		"PT1Y",     // a year is not
		"P1.5Y",    // a fraction is only allowed on the last TIME component
		"PT1.5H1M", // ...and nothing may follow it
		"P1D1D",    // the same component twice
		"P1M1Y",    // out of order
		"PT.5S",    // a fraction needs a whole part
		"PT1.S",    // and a fractional part
		"P-1Y",     // the sign goes before the P
		"PT1H ",    // trailing rubbish
	} {
		if got, ok := parseISODuration(in); ok {
			t.Errorf("parseISODuration(%q) accepted it as %v", in, got)
		}
	}
}
