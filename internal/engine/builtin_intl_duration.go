package engine

// Intl.DurationFormat.
//
// Ten units, each with its own style and display option, defaulting off a
// base style, with one further rule that a numeric unit forces the units after
// it to be numeric too -- that is what makes "1 hr, 2:03" impossible and
// "1:02:03" the digital form. The option plumbing IS the specification here;
// the English unit names below are the only data, and they are written out
// because there are thirty of them.

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// durationUnits is the order the units are read, formatted and reported in.
var durationUnits = []string{
	"years", "months", "weeks", "days", "hours", "minutes", "seconds",
	"milliseconds", "microseconds", "nanoseconds",
}

// subSecondUnits are the three that fractionalDigits can absorb into seconds.
var subSecondUnits = []string{"milliseconds", "microseconds", "nanoseconds"}

// durationUnitNames is CLDR's en unit patterns, singular and plural, per style.
var durationUnitNames = map[string][3][2]string{
	//                     long                     short             narrow
	"years":        {{"year", "years"}, {"yr", "yrs"}, {"y", "y"}},
	"months":       {{"month", "months"}, {"mth", "mths"}, {"m", "m"}},
	"weeks":        {{"week", "weeks"}, {"wk", "wks"}, {"w", "w"}},
	"days":         {{"day", "days"}, {"day", "days"}, {"d", "d"}},
	"hours":        {{"hour", "hours"}, {"hr", "hrs"}, {"h", "h"}},
	"minutes":      {{"minute", "minutes"}, {"min", "min"}, {"m", "m"}},
	"seconds":      {{"second", "seconds"}, {"sec", "sec"}, {"s", "s"}},
	"milliseconds": {{"millisecond", "milliseconds"}, {"ms", "ms"}, {"ms", "ms"}},
	"microseconds": {{"microsecond", "microseconds"}, {"μs", "μs"}, {"μs", "μs"}},
	"nanoseconds":  {{"nanosecond", "nanoseconds"}, {"ns", "ns"}, {"ns", "ns"}},
}

type durationOptions struct {
	tag        string
	numbering  string
	style      string   // "long", "short", "narrow", "digital"
	unitStyle  []string // per durationUnits
	display    []string // per durationUnits: "auto" or "always"
	fracDigits int      // -1 when the option was not given
}

func (d durationOptions) String() string {
	return strings.Join(append([]string{d.tag, d.numbering, d.style, strconv.Itoa(d.fracDigits)},
		append(d.unitStyle, d.display...)...), "\t")
}

func parseDurationOptions(s string) durationOptions {
	f := strings.Split(s, "\t")
	n := len(durationUnits)
	if len(f) != 4+2*n {
		return durationOptions{tag: defaultLocale, numbering: "latn", style: "short", fracDigits: -1}
	}
	fd, _ := strconv.Atoi(f[3])
	return durationOptions{tag: f[0], numbering: f[1], style: f[2], fracDigits: fd,
		unitStyle: f[4 : 4+n], display: f[4+n:]}
}

// isNumericStyle says whether a unit style spells the value as digits rather
// than as a word, which is what makes the units after it numeric too.
func isNumericStyle(s string) bool { return s == "numeric" || s == "2-digit" }

func (rt *Runtime) requireDurationFormat(this Value) (durationOptions, *ThrowError) {
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlDurationOpts); v.IsString() {
			return parseDurationOptions(rt.strGo(v)), nil
		}
	}
	return durationOptions{}, rt.typeError("not an Intl.DurationFormat")
}

// initDurationOptions is GetDurationUnitOptions, ten times over.
func (rt *Runtime) initDurationOptions(options Value, requested []string) (durationOptions, *ThrowError) {
	d := durationOptions{tag: defaultLocale, numbering: "latn", style: "short", fracDigits: -1}
	if len(requested) > 0 {
		d.tag = lookupMatcher(requested)
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return d, e
	}
	ns, hasNS, e := rt.intlStringOption(options, "numberingSystem", nil)
	if e != nil {
		return d, e
	}
	if hasNS {
		if !isUnicodeType(ns) {
			return d, rt.rangeError("Invalid numberingSystem: " + ns)
		}
		ns = asciiLower(ns)
	} else {
		ns = ""
	}
	d.tag, d.numbering = resolveNumberingSystem(d.tag, ns)
	style, ok, e := rt.intlStringOption(options, "style",
		[]string{"long", "short", "narrow", "digital"})
	if e != nil {
		return d, e
	}
	if ok {
		d.style = style
	}

	base := d.style
	prevNumeric := false
	for _, unit := range durationUnits {
		// Which spellings this unit accepts, and which one the digital style
		// gives it. Only hours, minutes and seconds can be written as digits
		// and only they can be "2-digit"; the sub-second units can be numeric,
		// which for them means being someone else's fraction.
		allowed := []string{"long", "short", "narrow"}
		digitalBase := "short"
		switch unit {
		case "hours", "minutes", "seconds":
			allowed = append(allowed, "numeric", "2-digit")
			digitalBase = "numeric"
		case "milliseconds", "microseconds", "nanoseconds":
			allowed = append(allowed, "numeric")
			digitalBase = "numeric"
		}
		got, present, e := rt.intlStringOption(options, unit, allowed)
		if e != nil {
			return d, e
		}
		// Once a unit is written as digits every later one must be too: a
		// duration cannot be "1:02, 3 seconds", so asking for a word after a
		// number is a contradiction rather than a preference.
		if present && prevNumeric && !isNumericStyle(got) {
			return d, rt.rangeError("Invalid " + unit + " style after a numeric unit")
		}
		// A unit the caller named is written whether or not it is zero -- they
		// asked for it. Only a unit that was defaulted into existence is
		// dropped when it has nothing to say.
		displayDefault := "always"
		unitStyle := got
		if !present {
			switch {
			case base == "digital":
				if unit != "hours" && unit != "minutes" && unit != "seconds" {
					displayDefault = "auto"
				}
				unitStyle = digitalBase
			case prevNumeric:
				if unit != "minutes" && unit != "seconds" {
					displayDefault = "auto"
				}
				unitStyle = "numeric"
			default:
				displayDefault = "auto"
				unitStyle = base
			}
		}
		if unitStyle == "numeric" {
			if tagContains(subSecondUnits, unit) {
				// Numeric here means "part of the seconds", and a fraction
				// nobody asked for is not written on its own.
				displayDefault = "auto"
			} else if prevNumeric && (unit == "minutes" || unit == "seconds") {
				// Second and later in a colon run: "1:02:03", not "1:2:3".
				unitStyle = "2-digit"
			}
		}
		disp, dpresent, e := rt.intlStringOption(options, unit+"Display", []string{"auto", "always"})
		if e != nil {
			return d, e
		}
		if !dpresent {
			disp = displayDefault
		}
		if isNumericStyle(unitStyle) {
			prevNumeric = true
		}
		d.unitStyle = append(d.unitStyle, unitStyle)
		d.display = append(d.display, disp)
	}

	fd, present, e := rt.intlNumberOption(options, "fractionalDigits", 0, 9, 0)
	if e != nil {
		return d, e
	}
	if present {
		d.fracDigits = fd
	}
	return d, nil
}

// durationRecord reads the ten fields off the argument. Every present field
// must be an integral Number, they must all share a sign, and a field that is
// not there is zero.
func (rt *Runtime) durationRecord(v Value) ([10]float64, *ThrowError) {
	var out [10]float64
	if v.IsString() {
		// A duration written down. A string that is not one is a RangeError:
		// it is a duration that could not be read, not a value of the wrong
		// kind.
		rec, ok := parseISODuration(rt.strGo(v))
		if !ok {
			return out, rt.rangeError("Invalid duration string: " + rt.strGo(v))
		}
		return rec, nil
	}
	if !v.IsObjectType() {
		return out, rt.typeError("Duration must be an object or a string")
	}
	if rt.temporalKindOf(v) == kindDuration {
		// A real Temporal.Duration carries its fields in slots, and they are
		// read from there: the getters on the prototype are the script's to
		// replace and nothing here goes through them.
		return tDuration(rt.objPtr(v)).fields(), nil
	}
	any := false
	sign := 0
	for i, unit := range durationUnits {
		f, e := rt.getField(v, unit)
		if e != nil {
			return out, e
		}
		if f.IsUndefined() {
			continue
		}
		n, e := rt.toNumber(f)
		if e != nil {
			return out, e
		}
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
			return out, rt.rangeError("Duration field " + unit + " must be an integer")
		}
		if n != 0 {
			s := 1
			if n < 0 {
				s = -1
			}
			if sign != 0 && s != sign {
				return out, rt.rangeError("Duration fields must all have the same sign")
			}
			sign = s
		}
		out[i] = n
		any = true
	}
	if !any {
		return out, rt.typeError("Duration must have at least one field")
	}
	if !validDuration(out) {
		return out, rt.rangeError("Duration is out of range")
	}
	return out, nil
}

// validDuration is Temporal's IsValidDuration. The calendar fields are bounded
// individually because a year is not a fixed number of seconds; the rest have
// to add up to something a Number can still count exactly, which is why the
// day limit is 104,249,991,374 -- one day more and days*86400 passes 2^53.
func validDuration(rec [10]float64) bool {
	for _, v := range rec[:3] { // years, months, weeks
		if math.Abs(v) >= 1<<32 {
			return false
		}
	}
	// The sum is a mathematical value, not a float: the largest valid duration
	// plus one nanosecond has to come out over the limit, and in float64 that
	// addition is a no-op. So it is done in exact integer nanoseconds.
	total := new(big.Int)
	scale := []int64{0, 0, 0, 86400e9, 3600e9, 60e9, 1e9, 1e6, 1e3, 1}
	term := new(big.Int)
	for i := 3; i < 10; i++ {
		big.NewFloat(rec[i]).Int(term)
		total.Add(total, term.Mul(term, big.NewInt(scale[i])))
	}
	limit := new(big.Int).Mul(big.NewInt(1<<53), big.NewInt(1e9))
	return total.Abs(total).Cmp(limit) < 0
}

// durationParts renders the duration as spans, each carrying the unit it
// belongs to. The worded units go through NumberFormat's own unit style rather
// than being assembled here -- that is what the specification says to do, and
// it also means the two cannot disagree about how "7 hours" is written.
func (rt *Runtime) durationParts(d durationOptions, li localeInfo, rec [10]float64) []relPart {
	var groups [][]relPart
	var numericRun []relPart
	flushNumeric := func() {
		if len(numericRun) > 0 {
			groups = append(groups, numericRun)
			numericRun = nil
		}
	}
	neg := false
	for _, v := range rec {
		if v < 0 {
			neg = true
			break
		}
	}
	// The sign belongs to the first unit that gets written, as a minusSign
	// part inside its number -- not as a literal in front of the whole thing.
	// "-1 hr, 30 min" is one negative duration, not a minus and a duration.
	signPending := neg
	for i, unit := range durationUnits {
		v := math.Abs(rec[i])
		style := d.unitStyle[i]
		exact := ""
		done := false
		// Seconds, milliseconds and microseconds each absorb everything below
		// them when the unit below is written as digits: those are not more
		// colon-separated fields, they are this one's fraction. "3 sec,
		// 444.055006 ms" is one millisecond count, not three.
		if i >= 6 && i <= 8 && d.unitStyle[i+1] == "numeric" {
			exact = fractionalDuration(rec, i)
			v = math.Abs(parseFloatDefault(exact))
			done = true
		}
		// A zero minute is written when it sits between something already
		// written and a second that will be: "1:00:03" needs it. Nothing else
		// is written for the sake of the colons.
		displayRequired := false
		if unit == "minutes" && len(numericRun) > 0 {
			displayRequired = d.display[6] == "always" ||
				rec[6] != 0 || rec[7] != 0 || rec[8] != 0 || rec[9] != 0
		}
		if v == 0 && d.display[i] == "auto" && !displayRequired {
			if done {
				break
			}
			continue
		}
		singular := strings.TrimSuffix(unit, "s")
		signed := v
		if signPending {
			// The sign belongs to the first unit written even when its value
			// is zero, which a negative zero is how a formatter is told.
			signed = -v
			if exact != "" && !strings.HasPrefix(exact, "-") {
				exact = "-" + exact
			}
			signPending = false
		} else if exact != "" {
			exact = strings.TrimPrefix(exact, "-")
		}
		n := defaultNumberOptions()
		n.tag, n.numbering = d.tag, d.numbering
		if done {
			// fractionalDigits says how many places to show; without it, as
			// many as are there and no more. Truncated, never rounded up: a
			// duration that reads longer than it was would be a lie about
			// time.
			n.digits.minFrac, n.digits.maxFrac = 0, 9
			if d.fracDigits >= 0 {
				n.digits.minFrac, n.digits.maxFrac = d.fracDigits, d.fracDigits
			}
			n.roundingMode = "trunc"
		}
		if isNumericStyle(style) {
			n.useGrouping = ""
			if style == "2-digit" {
				n.digits.minInt = 2
			}
			if len(numericRun) > 0 {
				numericRun = append(numericRun, relPart{numberPart{"literal", ":"}, ""})
			}
			for _, p := range numberPartsOf(n, li, signed, exact) {
				numericRun = append(numericRun, relPart{p, singular})
			}
			if done {
				break
			}
			continue
		}
		flushNumeric()
		n.style = "unit"
		n.unit = singular
		n.unitDisplay = style
		var group []relPart
		for _, p := range numberPartsOf(n, li, signed, exact) {
			group = append(group, relPart{p, singular})
		}
		groups = append(groups, group)
		if done {
			break
		}
	}
	flushNumeric()

	l := listOptions{tag: d.tag, kind: "unit", style: "short"}
	if d.style == "long" {
		l.style = "long"
	}
	if d.style == "narrow" {
		l.style = "narrow"
	}
	return joinDurationGroups(l, groups)
}

// joinDurationGroups puts the list patterns' literals between the per-unit
// runs. It is listParts over groups of spans rather than over strings.
func joinDurationGroups(l listOptions, groups [][]relPart) []relPart {
	if len(groups) == 0 {
		return nil
	}
	// The literals are whatever the list patterns put between two elements, so
	// they are read off a formatted pair rather than spelled out again here.
	sep := func(pattern string) string {
		_, rest, _ := strings.Cut(pattern, "{0}")
		between, _, _ := strings.Cut(rest, "{1}")
		return between
	}
	two, start, middle, end := l.patterns()
	out := append([]relPart{}, groups[0]...)
	for i := 1; i < len(groups); i++ {
		p := middle
		switch {
		case len(groups) == 2:
			p = two
		case i == 1:
			p = start
		case i == len(groups)-1:
			p = end
		}
		if s := sep(p); s != "" {
			out = append(out, relPart{numberPart{"literal", s}, ""})
		}
		out = append(out, groups[i]...)
	}
	return out
}

// sign is the multiplier that carries v's sign onto another magnitude.
func sign(v float64) float64 {
	if math.Signbit(v) {
		return -1
	}
	return 1
}

// fractionalDuration is the value of the unit at index i with every smaller
// unit folded into its fraction, as an exact decimal string. Exact because a
// double cannot hold it: ten million seconds and one nanosecond is seventeen
// significant digits, and rounding them is the difference between
// 10000000.000000001 and 10000000.000000002.
func fractionalDuration(rec [10]float64, i int) string {
	// Everything from i down, counted in the smallest unit present.
	scale := []int{9, 6, 3}[i-6]
	ns := new(big.Int)
	for j, mul := range map[int]int64{6: 1e9, 7: 1e6, 8: 1e3, 9: 1} {
		if j < i {
			continue
		}
		// Through big.Float, because a duration field may be any integral
		// double and int64 stops at 2^63.
		term, _ := new(big.Float).SetFloat64(rec[j]).Int(nil)
		ns.Add(ns, term.Mul(term, big.NewInt(mul)))
	}
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	q, r := new(big.Int).QuoRem(ns, unit, new(big.Int))
	neg := q.Sign() < 0 || r.Sign() < 0
	r.Abs(r)
	out := q.Abs(q).String() + "." + fmt.Sprintf("%0*s", scale, r.String())
	if neg {
		out = "-" + out
	}
	return out
}

// parseFloatDefault is the decimal read back as a double, for the sign and
// zero tests that do not need every digit.
func parseFloatDefault(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}
