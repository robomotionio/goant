package engine

// Intl.DateTimeFormat's options and component formatting.
//
// The constructor read nothing but `timeZone`: every component option --
// weekday, era, year, month, day, hour, minute, second, timeZoneName -- and
// both style options were accepted and dropped, so `{month: "long"}` got the
// locale's default numeric pattern and resolvedOptions() reported no
// components at all.
//
// The patterns assembled here are English's, as in ListFormat and
// DurationFormat: CLDR carries a skeleton-to-pattern table per locale, and
// generating it is what A4 in docs/intl402-and-temporal.md is about. What is
// locale-independent -- which options exist, which combinations are legal,
// which components a bare `new Intl.DateTimeFormat()` implies, and the shape of
// formatToParts -- is here and is most of what the suite checks.

import (
	"strconv"
	"strings"
	"time"
)

// dtComponents is every component option, in the order the specification reads
// them, which is observable through getters on the options bag.
var dtComponents = []string{
	"weekday", "era", "year", "month", "day", "dayPeriod",
	"hour", "minute", "second", "fractionalSecondDigits", "timeZoneName",
}

// dtComponentValues is what each component accepts. fractionalSecondDigits is
// a number and is handled apart.
var dtComponentValues = map[string][]string{
	"weekday":      {"narrow", "short", "long"},
	"era":          {"narrow", "short", "long"},
	"year":         {"2-digit", "numeric"},
	"month":        {"2-digit", "numeric", "narrow", "short", "long"},
	"day":          {"2-digit", "numeric"},
	"dayPeriod":    {"narrow", "short", "long"},
	"hour":         {"2-digit", "numeric"},
	"minute":       {"2-digit", "numeric"},
	"second":       {"2-digit", "numeric"},
	"timeZoneName": {"short", "long", "shortOffset", "longOffset", "shortGeneric", "longGeneric"},
}

type dateTimeOptions struct {
	tag        string
	numbering  string
	timeZone   string
	calendar   string
	hourCycle  string
	hour12Set  bool
	dateStyle  string
	timeStyle  string
	comps      map[string]string // component -> value, absent when not asked for
	fracDigits int               // 0 when not asked for
}

func (d dateTimeOptions) String() string {
	fields := []string{d.tag, d.numbering, d.timeZone, d.calendar, d.hourCycle,
		boolKeyword(d.hour12Set), d.dateStyle, d.timeStyle, strconv.Itoa(d.fracDigits)}
	for _, c := range dtComponents {
		fields = append(fields, d.comps[c])
	}
	return strings.Join(fields, "\t")
}

func parseDateTimeOptions(s string) dateTimeOptions {
	f := strings.Split(s, "\t")
	if len(f) != 9+len(dtComponents) {
		return dateTimeOptions{tag: defaultLocale, numbering: "latn", timeZone: localZoneID(),
			calendar: "gregory", comps: map[string]string{}}
	}
	fd, _ := strconv.Atoi(f[8])
	d := dateTimeOptions{tag: f[0], numbering: f[1], timeZone: f[2], calendar: f[3], hourCycle: f[4],
		hour12Set: f[5] == "true", dateStyle: f[6], timeStyle: f[7], fracDigits: fd,
		comps: map[string]string{}}
	for i, c := range dtComponents {
		if v := f[9+i]; v != "" {
			d.comps[c] = v
		}
	}
	return d
}

func (rt *Runtime) requireDateTimeFormat(this Value) (dateTimeOptions, *ThrowError) {
	this = rt.unwrapLegacyIntl(this)
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlDateTimeOpts); v.IsString() {
			return parseDateTimeOptions(rt.strGo(v)), nil
		}
	}
	return dateTimeOptions{}, rt.typeError("not an Intl.DateTimeFormat")
}

// initDateTimeOptions is CreateDateTimeFormat's option half.
func (rt *Runtime) initDateTimeOptions(options Value, requested []string) (dateTimeOptions, *ThrowError) {
	d := dateTimeOptions{tag: defaultLocale, numbering: "latn", calendar: "gregory", comps: map[string]string{}}
	if len(requested) > 0 {
		d.tag = requested[0]
		if t, ok := parseLangTag(requested[0]); ok {
			if v, has := t.uKeyword("ca"); has && v != "" {
				d.calendar = v
			}
			if v, has := t.uKeyword("hc"); has && tagContains([]string{"h11", "h12", "h23", "h24"}, v) {
				d.hourCycle = v
			}
		}
	}
	if options.IsNull() {
		return d, rt.typeError("Options must be an object")
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return d, e
	}
	cal, ok, e := rt.intlStringOption(options, "calendar", nil)
	if e != nil {
		return d, e
	}
	if ok {
		if !isUnicodeType(cal) {
			return d, rt.rangeError("Invalid calendar: " + cal)
		}
		d.calendar = asciiLower(cal)
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
	h12, e := rt.intlBoolOption(options, "hour12")
	if e != nil {
		return d, e
	}
	hc, ok, e := rt.intlStringOption(options, "hourCycle", []string{"h11", "h12", "h23", "h24"})
	if e != nil {
		return d, e
	}
	if ok {
		d.hourCycle = hc
	}
	if h12 != nil {
		// hour12 wins over hourCycle and over the tag's -u-hc, and picks the
		// cycle the locale would use for that half of the choice.
		d.hour12Set = true
		li, _ := lookupLocale(d.tag)
		if *h12 {
			d.hourCycle = "h12"
			if li.hourCycle == "h11" {
				d.hourCycle = "h11"
			}
		} else {
			d.hourCycle = "h23"
			if li.hourCycle == "h24" {
				d.hourCycle = "h24"
			}
		}
	}
	id, e := rt.optionTimeZone(options)
	if e != nil {
		return d, e
	}
	d.timeZone = id

	for _, c := range dtComponents {
		if c == "fractionalSecondDigits" {
			n, present, e := rt.intlNumberOption(options, c, 1, 3, 0)
			if e != nil {
				return d, e
			}
			if present {
				d.fracDigits = n
				d.comps[c] = strconv.Itoa(n)
			}
			continue
		}
		v, present, e := rt.intlStringOption(options, c, dtComponentValues[c])
		if e != nil {
			return d, e
		}
		if present {
			d.comps[c] = v
		}
	}
	if _, _, e := rt.intlStringOption(options, "formatMatcher", []string{"basic", "best fit"}); e != nil {
		return d, e
	}
	dateStyle, hasDate, e := rt.intlStringOption(options, "dateStyle",
		[]string{"full", "long", "medium", "short"})
	if e != nil {
		return d, e
	}
	timeStyle, hasTime, e := rt.intlStringOption(options, "timeStyle",
		[]string{"full", "long", "medium", "short"})
	if e != nil {
		return d, e
	}
	if hasDate || hasTime {
		// A style names a whole pattern, so naming components as well is a
		// contradiction rather than a refinement.
		if len(d.comps) > 0 {
			return d, rt.typeError("dateStyle and timeStyle cannot be mixed with component options")
		}
		d.dateStyle, d.timeStyle = dateStyle, timeStyle
		return d, nil
	}
	if len(d.comps) == 0 {
		// ToDateTimeOptions' defaults: a formatter that was asked for nothing
		// formats the date.
		d.comps["year"] = "numeric"
		d.comps["month"] = "numeric"
		d.comps["day"] = "numeric"
	}
	return d, nil
}

// resolvedHourCycle is the cycle this formatter uses, falling back to the
// locale's when neither the tag nor the options named one.
func (d dateTimeOptions) resolvedHourCycle() string {
	if d.hourCycle != "" {
		return d.hourCycle
	}
	li, _ := lookupLocale(d.tag)
	return li.hourCycle
}

var enMonthsLong = []string{"January", "February", "March", "April", "May", "June",
	"July", "August", "September", "October", "November", "December"}
var enDaysLong = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday",
	"Friday", "Saturday"}

// dtFieldText renders one component of an instant.
func (d dateTimeOptions) dtFieldText(comp, style string, t time.Time) string {
	two := func(n int) string {
		if n < 10 {
			return "0" + strconv.Itoa(n)
		}
		return strconv.Itoa(n)
	}
	switch comp {
	case "weekday":
		name := enDaysLong[int(t.Weekday())]
		switch style {
		case "short":
			return name[:3]
		case "narrow":
			return name[:1]
		}
		return name
	case "era":
		switch style {
		case "long":
			return "Anno Domini"
		case "narrow":
			return "A"
		}
		return "AD"
	case "year":
		y := t.Year()
		if style == "2-digit" {
			return two(((y % 100) + 100) % 100)
		}
		return strconv.Itoa(y)
	case "month":
		m := int(t.Month())
		switch style {
		case "2-digit":
			return two(m)
		case "numeric":
			return strconv.Itoa(m)
		case "short":
			return enMonthsLong[m-1][:3]
		case "narrow":
			return enMonthsLong[m-1][:1]
		}
		return enMonthsLong[m-1]
	case "day":
		if style == "2-digit" {
			return two(t.Day())
		}
		return strconv.Itoa(t.Day())
	case "dayPeriod":
		// AM/PM is what follows an hour on a 12-hour clock, and it is a
		// different field from the dayPeriod component even though both end up
		// in a part of that name.
		if style == "ampm" {
			if t.Hour() < 12 {
				return "AM"
			}
			return "PM"
		}
		// The standalone dayPeriod component is not AM/PM: CLDR names the
		// parts of the day, and English has six of them plus the two exact
		// moments. AM/PM is what goes after an hour, and that is emitted
		// separately.
		// Noon is a moment with a name of its own; midnight is not one in
		// English -- CLDR calls 00:00 "at night", and only the `b` skeleton,
		// which this option is not, ever says "midnight".
		h, m, sec := t.Hour(), t.Minute(), t.Second()
		if h == 12 && m == 0 && sec == 0 && t.Nanosecond() == 0 {
			if style == "narrow" {
				return "n"
			}
			return "noon"
		}
		switch {
		case h < 6:
			return "at night"
		case h < 12:
			return "in the morning"
		case h < 18:
			return "in the afternoon"
		case h < 21:
			return "in the evening"
		}
		return "at night"
	case "hour":
		h := t.Hour()
		switch d.resolvedHourCycle() {
		case "h12":
			h = h % 12
			if h == 0 {
				h = 12
			}
		case "h11":
			h = h % 12
		case "h24":
			if h == 0 {
				h = 24
			}
		}
		if style == "2-digit" {
			return two(h)
		}
		return strconv.Itoa(h)
	case "minute":
		// Minutes and seconds are two digits wherever an hour or a minute
		// precedes them: "2:03", not "2:3". Only a lone minute or second field
		// is written the way it was asked for.
		if style == "numeric" && !d.paddedTimeField("minute") {
			return strconv.Itoa(t.Minute())
		}
		return two(t.Minute())
	case "second":
		if style == "numeric" && !d.paddedTimeField("second") {
			return strconv.Itoa(t.Second())
		}
		return two(t.Second())
	case "fractionalSecondDigits":
		n, _ := strconv.Atoi(style)
		ms := t.Nanosecond() / 1e6
		s := two(ms/10) + strconv.Itoa(ms%10)
		if n > len(s) {
			n = len(s)
		}
		return s[:n]
	case "timeZoneName":
		switch style {
		case "long", "longGeneric":
			return zoneDisplayNameIn(t)
		case "shortOffset", "longOffset":
			return gmtOffsetName(t)
		}
		abbr, _ := t.Zone()
		return abbr
	}
	return ""
}

// paddedTimeField reports whether a field is preceded by another time field,
// in which case it is written to two digits whatever its option said.
func (d dateTimeOptions) paddedTimeField(comp string) bool {
	n := 0
	for _, c := range []string{"hour", "minute", "second"} {
		if _, ok := d.comps[c]; ok {
			n++
		}
	}
	if n < 2 {
		// A field on its own is written the way it was asked for; the padding
		// is what keeps a clock reading aligned, and one field is not one.
		return false
	}
	for _, c := range []string{"hour", "minute", "second"} {
		if _, ok := d.comps[c]; !ok {
			continue
		}
		// The first time field is written as asked only when it is the hour:
		// English writes "2:03" for an hour and a minute and "02:03" for a
		// minute and a second, because the minute is not the leading field of
		// a clock reading.
		if c == comp {
			return c != "hour"
		}
		return true
	}
	return false
}

// zoneDisplayNameIn is zoneDisplayName for an instant already in the zone being
// displayed, rather than for the host's.
func zoneDisplayNameIn(t time.Time) string {
	if pair, ok := zoneDisplayNames[t.Location().String()]; ok {
		if t.IsDST() {
			return pair[1]
		}
		return pair[0]
	}
	return gmtOffsetName(t)
}

// dateTimeParts assembles the formatted spans. The order is English's: the
// weekday, then the date read largest-unit-last where the month is a word and
// month-first where it is a number, then the time.
func (d dateTimeOptions) dateTimeParts(t time.Time) []numberPart {
	var out []numberPart
	lit := func(s string) {
		if s != "" {
			out = append(out, numberPart{"literal", s})
		}
	}
	field := func(comp string) bool {
		style, ok := d.comps[comp]
		if !ok {
			return false
		}
		out = append(out, numberPart{comp, mapDigits(d.dtFieldText(comp, style, t), d.numbering)})
		return true
	}

	if d.dateStyle != "" || d.timeStyle != "" {
		return d.styleParts(t)
	}

	wroteDate := false
	if field("weekday") {
		wroteDate = true
	}
	numericMonth := d.comps["month"] == "numeric" || d.comps["month"] == "2-digit"
	dateFields := []string{"month", "day", "year"}
	if numericMonth {
		// 1/15/2026: slashes, no spaces, no comma after the weekday.
		if wroteDate {
			lit(" ")
		}
		first := true
		for _, c := range dateFields {
			if _, ok := d.comps[c]; !ok {
				continue
			}
			if !first {
				lit("/")
			}
			field(c)
			first = false
			wroteDate = true
		}
	} else {
		if wroteDate {
			lit(", ")
		}
		wroteMonth := field("month")
		if _, ok := d.comps["day"]; ok {
			if wroteMonth {
				lit(" ")
			}
			field("day")
			wroteMonth = true
		}
		if _, ok := d.comps["year"]; ok {
			if wroteMonth {
				lit(", ")
			}
			field("year")
		}
		if wroteMonth || d.comps["year"] != "" {
			wroteDate = true
		}
	}
	if _, ok := d.comps["era"]; ok {
		if wroteDate {
			lit(" ")
		}
		field("era")
		wroteDate = true
	}

	hasTime := false
	for _, c := range []string{"hour", "minute", "second"} {
		if _, ok := d.comps[c]; ok {
			hasTime = true
		}
	}
	_, hasHour := d.comps["hour"]
	if hasTime {
		if wroteDate {
			lit(", ")
		}
		wroteAny := false
		for _, c := range []string{"hour", "minute", "second"} {
			if _, ok := d.comps[c]; !ok {
				continue
			}
			if wroteAny {
				lit(":")
			}
			field(c)
			wroteAny = true
		}
		if _, ok := d.comps["fractionalSecondDigits"]; ok {
			lit(".")
			out = append(out, numberPart{"fractionalSecond",
				d.dtFieldText("fractionalSecondDigits", d.comps["fractionalSecondDigits"], t)})
		}
		// A day period that was asked for by name replaces AM/PM rather than
		// joining it: "12 at night", not "12 AM at night". And AM/PM itself
		// says which half of the day the HOUR is in, so a pattern with no hour
		// has nothing for it to qualify.
		if style, named := d.comps["dayPeriod"]; named && hasHour {
			lit(" ")
			out = append(out, numberPart{"dayPeriod", d.dtFieldText("dayPeriod", style, t)})
		} else if hc := d.resolvedHourCycle(); hasHour && (hc == "h11" || hc == "h12") {
			lit(" ")
			out = append(out, numberPart{"dayPeriod", d.dtFieldText("dayPeriod", "ampm", t)})
		}
	} else if _, ok := d.comps["dayPeriod"]; ok {
		if wroteDate {
			lit(", ")
		}
		field("dayPeriod")
	}

	if _, ok := d.comps["timeZoneName"]; ok {
		lit(" ")
		field("timeZoneName")
	}
	return out
}

// styleParts renders dateStyle/timeStyle, which name whole patterns rather than
// components. They are expressed as component sets so there is one renderer.
func (d dateTimeOptions) styleParts(t time.Time) []numberPart {
	sub := dateTimeOptions{tag: d.tag, timeZone: d.timeZone, calendar: d.calendar,
		hourCycle: d.hourCycle, hour12Set: d.hour12Set, comps: map[string]string{}}
	switch d.dateStyle {
	case "full":
		sub.comps["weekday"], sub.comps["month"] = "long", "long"
		sub.comps["day"], sub.comps["year"] = "numeric", "numeric"
	case "long":
		sub.comps["month"], sub.comps["day"], sub.comps["year"] = "long", "numeric", "numeric"
	case "medium":
		sub.comps["month"], sub.comps["day"], sub.comps["year"] = "short", "numeric", "numeric"
	case "short":
		sub.comps["month"], sub.comps["day"], sub.comps["year"] = "numeric", "numeric", "2-digit"
	}
	switch d.timeStyle {
	case "full", "long":
		sub.comps["hour"], sub.comps["minute"], sub.comps["second"] = "numeric", "2-digit", "2-digit"
		sub.comps["timeZoneName"] = "long"
		if d.timeStyle == "long" {
			sub.comps["timeZoneName"] = "short"
		}
	case "medium":
		sub.comps["hour"], sub.comps["minute"], sub.comps["second"] = "numeric", "2-digit", "2-digit"
	case "short":
		sub.comps["hour"], sub.comps["minute"] = "numeric", "2-digit"
	}
	return sub.dateTimeParts(t)
}
