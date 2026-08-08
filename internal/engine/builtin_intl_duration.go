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
	"math"
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
	style      string   // "long", "short", "narrow", "digital"
	unitStyle  []string // per durationUnits
	display    []string // per durationUnits: "auto" or "always"
	fracDigits int      // -1 when the option was not given
}

func (d durationOptions) String() string {
	return strings.Join(append([]string{d.tag, d.style, strconv.Itoa(d.fracDigits)},
		append(d.unitStyle, d.display...)...), "\t")
}

func parseDurationOptions(s string) durationOptions {
	f := strings.Split(s, "\t")
	n := len(durationUnits)
	if len(f) != 3+2*n {
		return durationOptions{tag: defaultLocale, style: "short", fracDigits: -1}
	}
	fd, _ := strconv.Atoi(f[2])
	return durationOptions{tag: f[0], style: f[1], fracDigits: fd,
		unitStyle: f[3 : 3+n], display: f[3+n:]}
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
	d := durationOptions{tag: defaultLocale, style: "short", fracDigits: -1}
	if len(requested) > 0 {
		if t, ok := parseLangTag(requested[0]); ok {
			d.tag = t.languageID()
		} else {
			d.tag = requested[0]
		}
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return d, e
	}
	if _, _, e := rt.intlStringOption(options, "numberingSystem", nil); e != nil {
		return d, e
	}
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
		// Which spellings this unit accepts. Only hours, minutes and seconds
		// can be written as digits, and only they can be "2-digit".
		allowed := []string{"long", "short", "narrow"}
		digital := ""
		switch unit {
		case "hours":
			allowed = append(allowed, "numeric", "2-digit")
			digital = "numeric"
		case "minutes", "seconds":
			allowed = append(allowed, "numeric", "2-digit")
			digital = "2-digit"
		case "milliseconds", "microseconds", "nanoseconds":
			allowed = append(allowed, "numeric")
			digital = "numeric"
		}
		got, present, e := rt.intlStringOption(options, unit, allowed)
		if e != nil {
			return d, e
		}
		displayDefault := "auto"
		unitStyle := got
		if !present {
			if base == "digital" {
				if digital != "" && (unit == "hours" || unit == "minutes" || unit == "seconds") {
					unitStyle = digital
					if unit != "seconds" {
						displayDefault = "always"
					}
					if unit == "hours" {
						displayDefault = "always"
					}
				} else {
					unitStyle = "short"
				}
			} else {
				unitStyle = base
			}
			// A numeric unit forces every later one to be numeric: a duration
			// cannot be "1 hr, 2:03". The sub-second units follow that rule but
			// keep display "auto", because they are written as a fraction of
			// the seconds rather than as another colon-separated field.
			if prevNumeric && digital != "" {
				unitStyle = digital
				if !tagContains(subSecondUnits, unit) {
					displayDefault = "always"
				}
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
	if !v.IsObjectType() {
		return out, rt.typeError("Duration must be an object")
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
	return out, nil
}

// durationParts renders the duration as spans. Numeric units are joined by
// colons into one run; worded units become their own list elements, which the
// list patterns then join.
func (rt *Runtime) durationParts(d durationOptions, li localeInfo, rec [10]float64) []numberPart {
	var elements []string
	var numericRun []string
	flushNumeric := func() {
		if len(numericRun) > 0 {
			elements = append(elements, strings.Join(numericRun, ":"))
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
	for i, unit := range durationUnits {
		v := math.Abs(rec[i])
		style := d.unitStyle[i]
		if v == 0 && d.display[i] == "auto" {
			// A zero unit is written only to keep a colon run contiguous --
			// "1:00:03" needs its minutes. The sub-second units are never part
			// of that run: they belong after the decimal point, not after
			// another colon.
			if !isNumericStyle(style) || len(numericRun) == 0 || tagContains(subSecondUnits, unit) {
				continue
			}
		}
		if isNumericStyle(style) {
			s := strconv.FormatFloat(v, 'f', -1, 64)
			if style == "2-digit" && len(s) < 2 {
				s = "0" + s
			}
			numericRun = append(numericRun, s)
			continue
		}
		flushNumeric()
		names, ok := durationUnitNames[unit]
		if !ok {
			continue
		}
		idx := 0
		switch d.style {
		case "short", "digital":
			idx = 1
		case "narrow":
			idx = 2
		}
		switch style {
		case "long":
			idx = 0
		case "short":
			idx = 1
		case "narrow":
			idx = 2
		}
		word := names[idx][0]
		if v != 1 {
			word = names[idx][1]
		}
		num := li.formatNumber(v)
		if idx == 2 {
			elements = append(elements, num+word)
		} else {
			elements = append(elements, num+" "+word)
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
	parts := l.listParts(elements)
	if neg && len(parts) > 0 {
		parts = append([]numberPart{{"literal", li.minus}}, parts...)
	}
	return parts
}
