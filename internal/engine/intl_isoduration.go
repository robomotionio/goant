package engine

// The ISO 8601 duration grammar, as Temporal restricts it.
//
// `Intl.DurationFormat.prototype.format` takes one of these as well as a
// record, and rejects a malformed one with a RangeError rather than a
// TypeError -- it is a duration that could not be read, not a value of the
// wrong kind. Temporal.Duration parses the same strings, which is why this
// lives on its own rather than inside the formatter.
//
//	[+-] P [nY] [nM] [nW] [nD] [ T [nH] [nM] [nS] ]
//
// with a fraction allowed on the last time component only, and at least one
// component required. A fraction on the hours or the minutes cascades into the
// smaller units, because a duration's fields are integers and "1.5 hours" has
// to be spelled as an hour and thirty minutes somewhere.

import (
	"math"
	"strings"
)

// parseISODuration returns the ten duration fields in durationUnits' order.
func parseISODuration(s string) ([10]float64, bool) {
	var out [10]float64
	if s == "" {
		return out, false
	}
	// The sign may be an ASCII hyphen or a U+2212 MINUS SIGN; the latter is
	// three bytes, so it is cut as a string rather than matched as a byte.
	sign := 1.0
	if v, ok := strings.CutPrefix(s, "−"); ok {
		sign, s = -1, v
	} else {
		switch s[0] {
		case '+':
			s = s[1:]
		case '-':
			sign, s = -1, s[1:]
		}
	}
	if len(s) == 0 || (s[0] != 'P' && s[0] != 'p') {
		return out, false
	}
	s = s[1:]

	date, timePart, hasT := strings.Cut(s, "T")
	if !hasT {
		date, timePart, hasT = strings.Cut(s, "t")
	}
	any := false

	// The date half: whole numbers only, in this order, each optional.
	pos := 0
	for _, spec := range []struct {
		suffix string
		index  int
	}{{"Y", 0}, {"M", 1}, {"W", 2}, {"D", 3}} {
		n, next, ok := readDurationNumber(date, pos, spec.suffix)
		if !ok {
			continue
		}
		if n != math.Trunc(n) {
			return out, false // a fraction is only allowed on the last time field
		}
		out[spec.index] = n
		pos = next
		any = true
	}
	if pos != len(date) {
		return out, false
	}

	if hasT {
		// The time half: the same shape, and the LAST component present may
		// carry a fraction, which then cascades down.
		pos = 0
		timeAny := false
		var frac float64
		var fracIndex = -1
		for _, spec := range []struct {
			suffix string
			index  int
		}{{"H", 4}, {"M", 5}, {"S", 6}} {
			n, next, ok := readDurationNumber(timePart, pos, spec.suffix)
			if !ok {
				continue
			}
			if fracIndex >= 0 {
				return out, false // something followed a fractional component
			}
			whole := math.Trunc(n)
			if n != whole {
				frac, fracIndex = n-whole, spec.index
			}
			out[spec.index] = whole
			pos = next
			timeAny, any = true, true
		}
		if pos != len(timePart) || !timeAny {
			return out, false
		}
		switch fracIndex {
		case 4: // hours: into minutes, then seconds
			m := frac * 60
			out[5] += math.Trunc(m)
			frac, fracIndex = m-math.Trunc(m), 5
			fallthrough
		case 5: // minutes: into seconds
			sec := frac * 60
			out[6] += math.Trunc(sec)
			frac = sec - math.Trunc(sec)
			fallthrough
		case 6: // seconds: into milli, micro, nano
			ns := math.Round(frac * 1e9)
			out[7] += math.Trunc(ns / 1e6)
			ns -= math.Trunc(ns/1e6) * 1e6
			out[8] += math.Trunc(ns / 1e3)
			out[9] += ns - math.Trunc(ns/1e3)*1e3
		}
	}
	if !any {
		return out, false
	}
	for i := range out {
		out[i] *= sign
	}
	return out, true
}

// readDurationNumber reads digits (with an optional fraction) followed by the
// given unit letter, at pos. It reports the value and where it ended, or false
// if this component is not the one at pos.
func readDurationNumber(s string, pos int, suffix string) (float64, int, bool) {
	i := pos
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == pos {
		return 0, pos, false
	}
	end := i
	if end < len(s) && (s[end] == '.' || s[end] == ',') {
		end++
		start := end
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		if end == start || end-start > 9 {
			return 0, pos, false
		}
	}
	if end >= len(s) {
		return 0, pos, false
	}
	if !strings.EqualFold(string(s[end]), suffix) {
		return 0, pos, false
	}
	n := parseDecimalDigits(s[pos:end])
	return n, end + 1, true
}

// parseDecimalDigits reads an unsigned decimal with an optional comma or point
// fraction. The input has already been shaped by readDurationNumber.
func parseDecimalDigits(s string) float64 {
	whole, frac, _ := strings.Cut(strings.Replace(s, ",", ".", 1), ".")
	v := 0.0
	for i := 0; i < len(whole); i++ {
		v = v*10 + float64(whole[i]-'0')
	}
	scale := 1.0
	for i := 0; i < len(frac); i++ {
		scale /= 10
		v += float64(frac[i]-'0') * scale
	}
	return v
}
