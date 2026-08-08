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
	// defaulted records that no field was asked for and the defaults were
	// filled in. A Temporal value formatted by such a formatter gets its own
	// kind's defaults instead: a formatter that was told nothing has no reason
	// to insist on a date when it is handed a time.
	defaulted bool
}

func (d dateTimeOptions) String() string {
	fields := []string{d.tag, d.numbering, d.timeZone, d.calendar, d.hourCycle,
		boolKeyword(d.hour12Set), d.dateStyle, d.timeStyle, strconv.Itoa(d.fracDigits),
		boolKeyword(d.defaulted)}
	for _, c := range dtComponents {
		fields = append(fields, d.comps[c])
	}
	return strings.Join(fields, "\t")
}

func parseDateTimeOptions(s string) dateTimeOptions {
	f := strings.Split(s, "\t")
	if len(f) != 10+len(dtComponents) {
		return dateTimeOptions{tag: defaultLocale, numbering: "latn", timeZone: localZoneID(),
			calendar: "gregory", comps: map[string]string{}}
	}
	fd, _ := strconv.Atoi(f[8])
	d := dateTimeOptions{tag: f[0], numbering: f[1], timeZone: f[2], calendar: f[3], hourCycle: f[4],
		hour12Set: f[5] == "true", dateStyle: f[6], timeStyle: f[7], fracDigits: fd,
		defaulted: f[9] == "true", comps: map[string]string{}}
	for i, c := range dtComponents {
		if v := f[10+i]; v != "" {
			d.comps[c] = v
		}
	}
	return d
}

func (rt *Runtime) requireDateTimeFormat(this Value) (dateTimeOptions, *ThrowError) {
	this = rt.unwrapLegacyIntl(this, slotIntlDateTimeOpts)
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlDateTimeOpts); v.IsString() {
			return parseDateTimeOptions(rt.strGo(v)), nil
		}
	}
	return dateTimeOptions{}, rt.typeError("not an Intl.DateTimeFormat")
}

// initDateTimeOptions is CreateDateTimeFormat's option half.
func (rt *Runtime) initDateTimeOptions(options Value, requested []string) (dateTimeOptions, *ThrowError) {
	return rt.initDateTimeOptionsFor(options, requested, "any", "date")
}

// initDateTimeOptionsFor is CreateDateTimeFormat with the `required` and
// `defaults` that Date.prototype.toLocaleDateString and toLocaleTimeString
// pass: a formatter asked for no field of the required kind gets the default
// set for it, which is what makes toLocaleTimeString show a time.
func (rt *Runtime) initDateTimeOptionsFor(options Value, requested []string, required, defaults string) (dateTimeOptions, *ThrowError) {
	d := dateTimeOptions{tag: defaultLocale, numbering: "latn", calendar: "gregory", comps: map[string]string{}}
	tagHC, tagCal := "", ""
	if len(requested) > 0 {
		d.tag = lookupMatcher(requested)
		if t, ok := parseLangTag(d.tag); ok {
			if v, has := t.uKeyword("ca"); has && v != "" {
				// Only a calendar we have counts as what the tag asked for.
				// "en-u-ca-invalid" is a locale with no calendar in it.
				if c, ok := supportedCalendar(v); ok {
					d.calendar, tagCal = c, c
				}
			}
			if v, has := t.uKeyword("hc"); has && tagContains([]string{"h11", "h12", "h23", "h24"}, v) {
				d.hourCycle, tagHC = v, v
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
		// An option naming a calendar this engine does not have is not an
		// error and does not win: what the tag asked for still stands, and
		// only when that is nothing too does it fall back to gregory.
		if c, ok := supportedCalendar(cal); ok {
			d.calendar = c
		}
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
			// Every locale counts a 12-hour clock from one except Japanese,
			// which counts from zero: midnight there is 午前0時, not 午前12時.
			d.hourCycle = "h12"
			if t, ok := parseLangTag(d.tag); ok && t.lang == "ja" {
				d.hourCycle = "h11"
			}
		} else {
			d.hourCycle = "h23"
			if li.hourCycle == "h24" {
				d.hourCycle = "h24"
			}
		}
	}
	// -u-ca survives into the resolved locale when the calendar in use is
	// the one the tag asked for -- an option that changed it takes it out.
	if tagCal != "" && d.calendar == tagCal {
		if t, ok := parseLangTag(d.tag); ok {
			t.setUKeyword("ca", tagCal)
			d.tag = t.String()
		}
	}
	// -u-hc survives into the resolved locale on the same rule as -u-nu: it
	// does when the value in use is the one the tag asked for. An hour12 or
	// hourCycle option that changes it takes it out.
	if tagHC != "" && d.hourCycle == tagHC {
		if t, ok := parseLangTag(d.tag); ok {
			t.setUKeyword("hc", tagHC)
			d.tag = t.String()
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
	// ToDateTimeOptions: a formatter asked for no field of the required kind
	// gets the defaults for it.
	dateFields := []string{"weekday", "year", "month", "day"}
	timeFields := []string{"dayPeriod", "hour", "minute", "second", "fractionalSecondDigits"}
	has := func(fields []string) bool {
		for _, f := range fields {
			if _, ok := d.comps[f]; ok {
				return true
			}
		}
		return false
	}
	needed := false
	switch required {
	case "date":
		needed = !has(dateFields)
	case "time":
		needed = !has(timeFields)
	case "year-month":
		needed = !has([]string{"year", "month"})
	case "month-day":
		needed = !has([]string{"month", "day"})
	default:
		needed = !has(dateFields) && !has(timeFields)
	}
	d.defaulted = needed
	if needed {
		switch defaults {
		case "date", "all":
			d.comps["year"], d.comps["month"], d.comps["day"] = "numeric", "numeric", "numeric"
		case "year-month":
			d.comps["year"], d.comps["month"] = "numeric", "numeric"
		case "month-day":
			d.comps["month"], d.comps["day"] = "numeric", "numeric"
		}
		if defaults == "time" || defaults == "all" {
			d.comps["hour"], d.comps["minute"], d.comps["second"] = "numeric", "numeric", "numeric"
		}
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
		return eraName(d.calendarDate(t).era, style)
	case "year":
		// The era's own year, so 1 BC is year 1 and not year 0 -- unless the
		// calendar is ISO 8601, which has no eras and numbers straight
		// through zero into the negatives.
		cd := d.calendarDate(t)
		y := cd.eraYear
		if d.calendar == "iso8601" {
			y = cd.year
		}
		if style == "2-digit" {
			return two(((y % 100) + 100) % 100)
		}
		return strconv.Itoa(y)
	case "relatedYear":
		cd := d.calendarDate(t)
		return relatedGregorianYear(calendarFor(d.calendar), d.calendar, cd.year)
	case "yearName":
		lang := ""
		if tag, ok := parseLangTag(d.tag); ok {
			lang = tag.lang
		}
		cy := d.calendarDate(t).year
		if c, ok := calendarFor(d.calendar).(lunisolarCalendar); ok {
			cy = c.cyclicYear(cy)
		}
		return sexagenaryName(cy, lang)
	case "month":
		cd := d.calendarDate(t)
		m := cd.month
		// A lunisolar month is written by its NAME rather than its place in
		// the year: in a year with a leap fourth month, the month after it is
		// still the fifth. The leap month itself takes the name of the one
		// before with a marker on it.
		marker := ""
		if d.lunisolar() {
			if n, leap, ok := parseMonthCode(cd.code); ok {
				m = n
				if leap {
					marker = "bis"
				}
			}
		}
		switch style {
		case "2-digit":
			return two(m) + marker
		case "numeric":
			return strconv.Itoa(m) + marker
		}
		name := d.monthName(cd)
		if name == "" {
			return strconv.Itoa(m)
		}
		switch style {
		case "short":
			if len(name) > 3 {
				return name[:3]
			}
			return name
		case "narrow":
			return name[:1]
		}
		return name
	case "day":
		day := d.calendarDate(t).day
		if style == "2-digit" {
			return two(day)
		}
		return strconv.Itoa(day)
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
		// A lunisolar year is not a number, it is a name in a sixty-year
		// cycle, and it is written beside the Gregorian year it began in --
		// "2019(ji-hai)" -- because the name alone does not say which round
		// of the cycle it is.
		if comp == "year" && d.lunisolar() {
			related := numberPart{"relatedYear",
				mapDigits(d.dtFieldText("relatedYear", style, t), d.numbering)}
			name := numberPart{"yearName", d.dtFieldText("yearName", style, t)}
			// Chinese writes the two together and marks them as a year;
			// everything else brackets the name after the number.
			if lang, ok := parseLangTag(d.tag); ok && lang.lang == "zh" {
				out = append(out, related, name, numberPart{"literal", "年"})
				return true
			}
			out = append(out, related, numberPart{"literal", "("}, name,
				numberPart{"literal", ")"})
			return true
		}
		out = append(out, numberPart{comp, mapDigits(d.dtFieldText(comp, style, t), d.numbering)})
		return true
	}

	if d.dateStyle != "" || d.timeStyle != "" {
		return d.styleParts(t)
	}
	if parts, ok := d.patternParts(t); ok {
		return parts
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
			point := "."
			if sep, _, ok := numberingSeparators(d.numbering); ok {
				point = sep
			}
			lit(point)
			out = append(out, numberPart{"fractionalSecond",
				mapDigits(d.dtFieldText("fractionalSecondDigits", d.comps["fractionalSecondDigits"], t), d.numbering)})
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

// patternParts renders the locale's own date and time patterns, and reports
// whether they applied. They apply when the caller asked for exactly what
// toLocaleDateString and toLocaleTimeString ask for by default -- a numeric
// date, a numeric time, or both -- because that is the case the patterns are
// FOR: every locale writes that date its own way, and building it out of
// components would write all of them the American way.
//
// Anything else -- a named month, a weekday, a zone name, an hour cycle the
// caller chose -- is assembled from the components instead.
func (d dateTimeOptions) patternParts(t time.Time) ([]numberPart, bool) {
	if d.dateStyle != "" || d.timeStyle != "" || d.calendar != "gregory" {
		return nil, false
	}
	li, _ := lookupLocale(d.tag)
	if d.hour12Set || (d.hourCycle != "" && d.hourCycle != li.hourCycle) {
		return nil, false
	}
	want := func(names ...string) bool {
		if len(d.comps) != len(names) {
			return false
		}
		for _, n := range names {
			if d.comps[n] != "numeric" {
				return false
			}
		}
		return true
	}
	switch {
	case want("year", "month", "day"):
		return d.expandParts(li, li.date, t), true
	case want("hour", "minute", "second"):
		return d.expandParts(li, li.time, t), true
	case want("year", "month", "day", "hour", "minute", "second"):
		date, clock := d.expandParts(li, li.date, t), d.expandParts(li, li.time, t)
		var out []numberPart
		for _, piece := range splitGlue(li.glue) {
			switch piece {
			case "{d}":
				out = append(out, date...)
			case "{t}":
				out = append(out, clock...)
			default:
				out = append(out, numberPart{"literal", piece})
			}
		}
		return out, true
	}
	return nil, false
}

// splitGlue cuts the date-time glue pattern into its two placeholders and the
// literals around them.
func splitGlue(glue string) []string {
	var out []string
	rest := glue
	for rest != "" {
		i := strings.IndexAny(rest, "{")
		if i < 0 || i+3 > len(rest) {
			out = append(out, rest)
			break
		}
		if i > 0 {
			out = append(out, rest[:i])
		}
		out = append(out, rest[i:i+3])
		rest = rest[i+3:]
	}
	return out
}

// patternFields maps a pattern token to the part type it produces.
var patternFields = map[string]string{
	"Y": "year", "M": "month", "MM": "month", "D": "day", "DD": "day",
	"H": "hour", "HH": "hour", "m": "minute", "mm": "minute",
	"s": "second", "ss": "second", "P": "dayPeriod",
}

// expandParts is localeInfo.expand with the spans kept apart, so that
// formatToParts can say which is which.
func (d dateTimeOptions) expandParts(li localeInfo, pattern string, t time.Time) []numberPart {
	var out []numberPart
	lit := func(s string) {
		if s != "" {
			out = append(out, numberPart{"literal", s})
		}
	}
	for i := 0; i < len(pattern); {
		if pattern[i] != '{' {
			j := strings.IndexByte(pattern[i:], '{')
			if j < 0 {
				lit(pattern[i:])
				break
			}
			lit(pattern[i : i+j])
			i += j
			continue
		}
		j := strings.IndexByte(pattern[i:], '}')
		if j < 0 {
			lit(pattern[i:])
			break
		}
		tok := pattern[i+1 : i+j]
		text := li.token(tok, t)
		if typ, ok := patternFields[tok]; ok && text != "" {
			if typ != "dayPeriod" {
				text = mapDigits(text, d.numbering)
			}
			out = append(out, numberPart{typ, text})
		} else {
			lit(text)
		}
		i += j + 1
	}
	return out
}

// dtSignificance orders the fields from the one that says most about which
// date this is to the one that says least. It decides how much of a range the
// two ends can share.
var dtSignificance = map[string]int{
	"era": 0, "year": 1, "month": 2, "day": 3, "weekday": 4, "dayPeriod": 5,
	"hour": 6, "minute": 7, "second": 8, "fractionalSecond": 9,
}

// rangeParts is the date half of FormatDateTimeRange. Where a range of numbers
// decides what to repeat by what is written AROUND the ends, a range of dates
// decides by which fields differ: "Jan 3 – 5, 2019" says the month and the year
// once because only the day changed.
//
// How much is shared is per-locale data this engine does not carry -- CLDR
// gives an interval pattern per skeleton and per differing field -- so the one
// rule implemented here is the one that holds wherever the month is a NAME.
// A numeric date repeats whole, which is what CLDR says for it too: "1/3/2019 –
// 1/5/2019" is how en writes that, and sharing the slashes would not read.
func (d dateTimeOptions) rangeParts(start, end []numberPart) []sourcedPart {
	if samePartRun(start, end) {
		out := make([]sourcedPart, 0, len(start))
		for _, p := range start {
			out = append(out, sourcedPart{p, "shared"})
		}
		return out
	}
	whole := func() []sourcedPart {
		var out []sourcedPart
		for _, p := range start {
			out = append(out, sourcedPart{p, "startRange"})
		}
		out = append(out, sourcedPart{numberPart{"literal", " – "}, "shared"})
		for _, p := range end {
			out = append(out, sourcedPart{p, "endRange"})
		}
		return out
	}
	named := false
	switch d.comps["month"] {
	case "short", "long", "narrow":
		named = true
	}
	if !named || len(start) != len(end) {
		return whole()
	}
	first, last, greatest := -1, -1, len(dtSignificance)
	for i := range start {
		if start[i] == end[i] {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
		if s, ok := dtSignificance[start[i].typ]; ok && s < greatest {
			greatest = s
		}
	}
	// A range that spans a year or an era is not two points in one of them, so
	// there is nothing outside the difference to say once.
	if first < 0 || greatest <= dtSignificance["year"] {
		return whole()
	}
	var out []sourcedPart
	for _, p := range start[:first] {
		out = append(out, sourcedPart{p, "shared"})
	}
	for _, p := range start[first : last+1] {
		out = append(out, sourcedPart{p, "startRange"})
	}
	out = append(out, sourcedPart{numberPart{"literal", " – "}, "shared"})
	for _, p := range end[first : last+1] {
		out = append(out, sourcedPart{p, "endRange"})
	}
	for _, p := range end[last+1:] {
		out = append(out, sourcedPart{p, "shared"})
	}
	return out
}

// calendarDate is the instant's wall-clock date as the formatter's calendar
// sees it. Everything above the hour goes through here, because a year, a
// month and a day mean different things in each of the sixteen.
func (d dateTimeOptions) calendarDate(t time.Time) calendarDate {
	return calendarFor(d.calendar).dateFromDay(isoDay(t.Year(), int(t.Month()), t.Day()))
}

// monthName is the month's name in the formatter's calendar, or "" where that
// calendar numbers its months. The Gregorian family keeps the English names
// that were here before the other calendars existed.
func (d dateTimeOptions) monthName(cd calendarDate) string {
	switch d.calendar {
	case "gregory", "iso8601", "buddhist", "roc", "japanese":
		if cd.month >= 1 && cd.month <= 12 {
			return enMonthsLong[cd.month-1]
		}
		return ""
	}
	return monthNameFor(d.calendar, calendarFor(d.calendar), cd.year, cd.month)
}

// lunisolar reports whether the formatter's calendar names its years rather
// than numbering them.
func (d dateTimeOptions) lunisolar() bool {
	return d.calendar == "chinese" || d.calendar == "dangi"
}
