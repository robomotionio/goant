package engine

// Reading the Temporal string formats: ISO 8601 dates and times as RFC 9557
// extends them, with a bracketed time zone and bracketed annotations on the
// end.
//
// Every Temporal type accepts a string, and every one of them accepts a
// slightly different subset, so the parser is written once over the whole
// grammar and the callers say which shapes they will take.

import (
	"strconv"
	"strings"
)

// temporalParse is everything a Temporal string can say.
type temporalParse struct {
	hasDate    bool
	year       int
	month      int
	day        int
	yearAbsent bool // a month-day string names no year
	dayAbsent  bool // a year-month string names no day

	hasTime bool
	time    isoTimeRec

	z         bool // the string ended in Z
	hasOffset bool
	offsetNs  int64
	offsetStr string

	hasTZ  bool
	tzName string // an IANA name or an offset, out of the [..] annotation

	calendar string
}

type tparse struct {
	s string
	i int
}

func (p *tparse) eof() bool { return p.i >= len(p.s) }

func (p *tparse) peek() byte {
	if p.eof() {
		return 0
	}
	return p.s[p.i]
}

func (p *tparse) at(off int) byte {
	if p.i+off >= len(p.s) {
		return 0
	}
	return p.s[p.i+off]
}

func (p *tparse) accept(b byte) bool {
	if p.peek() == b {
		p.i++
		return true
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// digits reads exactly n digits.
func (p *tparse) digits(n int) (int, bool) {
	if p.i+n > len(p.s) {
		return 0, false
	}
	v := 0
	for k := 0; k < n; k++ {
		b := p.s[p.i+k]
		if !isDigit(b) {
			return 0, false
		}
		v = v*10 + int(b-'0')
	}
	p.i += n
	return v, true
}

// ---- the pieces ----

// signHere reads a sign. The typographic minus is not one: Temporal's grammar
// says ASCII, and the suite checks that "\u2212009999-11-18" is a RangeError
// rather than the year 9999 BC.
func (p *tparse) signHere() (int, bool) {
	switch {
	case p.accept('+'):
		return 1, true
	case p.accept('-'):
		return -1, true
	}
	return 1, false
}

// year reads a four-digit year or a signed six-digit one.
func (p *tparse) year() (int, bool) {
	start := p.i
	if sign, ok := p.signHere(); ok {
		v, good := p.digits(6)
		if !good {
			p.i = start
			return 0, false
		}
		if sign < 0 && v == 0 {
			// -000000 names no year: the grammar rejects it outright.
			p.i = start
			return 0, false
		}
		return sign * v, true
	}
	v, good := p.digits(4)
	if !good {
		p.i = start
		return 0, false
	}
	return v, true
}

// date reads a Date production and reports whether the separators were used.
func (p *tparse) date() (y, m, d int, ok bool) {
	start := p.i
	y, ok = p.year()
	if !ok {
		return
	}
	dash := p.accept('-')
	m, ok = p.digits(2)
	if !ok {
		p.i = start
		return 0, 0, 0, false
	}
	if dash != p.accept('-') {
		p.i = start
		return 0, 0, 0, false
	}
	d, ok = p.digits(2)
	if !ok {
		p.i = start
		return 0, 0, 0, false
	}
	return y, m, d, true
}

// fraction reads a decimal fraction and returns it as nanoseconds.
func (p *tparse) fraction() (int, bool) {
	if p.peek() != '.' && p.peek() != ',' {
		return 0, false
	}
	if !isDigit(p.at(1)) {
		return 0, false
	}
	p.i++
	digits := ""
	for !p.eof() && isDigit(p.peek()) && len(digits) < 9 {
		digits += string(p.peek())
		p.i++
	}
	// Ten or more fractional digits is not a Temporal string.
	if !p.eof() && isDigit(p.peek()) {
		return 0, false
	}
	for len(digits) < 9 {
		digits += "0"
	}
	v, _ := strconv.Atoi(digits)
	return v, true
}

// timeSpec reads a Time production. sep reports whether colons were used and
// n counts how many of hour/minute/second were given, which the callers that
// must resolve an ambiguity with a date want to know.
func (p *tparse) timeSpec() (t isoTimeRec, fields int, sep bool, ok bool) {
	start := p.i
	h, good := p.digits(2)
	if !good {
		return t, 0, false, false
	}
	t.hour = h
	fields = 1
	if p.eof() {
		return t, fields, false, true
	}
	colon := p.accept(':')
	sep = colon
	mi, good := p.digits(2)
	if !good {
		if colon {
			p.i = start
			return t, 0, false, false
		}
		return t, fields, sep, true
	}
	t.minute = mi
	fields = 2
	if colon != p.accept(':') {
		return t, fields, sep, true
	}
	s, good := p.digits(2)
	if !good {
		if colon {
			p.i = start
			return t, 0, false, false
		}
		return t, fields, sep, true
	}
	// A second of 60 is a leap second, which Temporal accepts and clamps.
	t.second = s
	fields = 3
	if ns, has := p.fraction(); has {
		t.ms = ns / nsPerMilli
		t.us = (ns / nsPerMicro) % 1000
		t.ns = ns % 1000
	}
	return t, fields, sep, true
}

// utcOffset reads ±HH, ±HH:MM, ±HH:MM:SS and the fractional form, returning
// the offset in nanoseconds and the text it was written as.
func (p *tparse) utcOffset(subMinute bool) (ns int64, text string, ok bool) {
	start := p.i
	sign, has := p.signHere()
	if !has {
		return 0, "", false
	}
	h, good := p.digits(2)
	if !good || h > 23 {
		p.i = start
		return 0, "", false
	}
	ns = int64(h) * nsPerHour
	if p.eof() {
		return int64(sign) * ns, p.s[start:p.i], true
	}
	colon := p.accept(':')
	mi, good := p.digits(2)
	if !good {
		if colon {
			p.i = start
			return 0, "", false
		}
		return int64(sign) * ns, p.s[start:p.i], true
	}
	if mi > 59 {
		p.i = start
		return 0, "", false
	}
	ns += int64(mi) * nsPerMinute
	if colon != p.accept(':') {
		return int64(sign) * ns, p.s[start:p.i], true
	}
	s, good := p.digits(2)
	if !good {
		if colon {
			p.i = start
			return 0, "", false
		}
		return int64(sign) * ns, p.s[start:p.i], true
	}
	if !subMinute || s > 59 {
		p.i = start
		return 0, "", false
	}
	ns += int64(s) * nsPerSecond
	if f, has := p.fraction(); has {
		ns += int64(f)
	}
	return int64(sign) * ns, p.s[start:p.i], true
}

// ---- the bracketed parts ----

func isTZChar(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || isDigit(b) ||
		b == '.' || b == '_' || b == '-' || b == '+'
}

func isTZLead(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b == '.' || b == '_'
}

// ianaName reads a time zone name: slash-separated components of letters and a
// few punctuation marks, where "." and ".." are not names of their own.
func (p *tparse) ianaName() (string, bool) {
	start := p.i
	for {
		cs := p.i
		if !isTZLead(p.peek()) {
			p.i = start
			return "", false
		}
		p.i++
		for !p.eof() && isTZChar(p.peek()) {
			p.i++
		}
		comp := p.s[cs:p.i]
		if comp == "." || comp == ".." || len(comp) > 14 {
			p.i = start
			return "", false
		}
		if !p.accept('/') {
			break
		}
	}
	return p.s[start:p.i], true
}

// timeZoneAnnotation reads "[Europe/Paris]" or "[+01:00]", with the critical
// flag it may carry.
func (p *tparse) timeZoneAnnotation() (string, bool) {
	if p.peek() != '[' {
		return "", false
	}
	// An annotation with a "key=" in it is not a time zone.
	start := p.i
	p.i++
	p.accept('!')
	if ns, text, ok := p.utcOffset(false); ok {
		_ = ns
		if p.accept(']') {
			return text, true
		}
		p.i = start
		return "", false
	}
	name, ok := p.ianaName()
	if !ok || !p.accept(']') {
		p.i = start
		return "", false
	}
	return name, true
}

func isAnnotationKeyLead(b byte) bool { return b >= 'a' && b <= 'z' || b == '_' }
func isAnnotationKeyChar(b byte) bool {
	return isAnnotationKeyLead(b) || isDigit(b) || b == '-'
}
// annotations reads the trailing [key=value] group. It answers the calendar it
// found, and refuses a string whose critical annotation it does not understand.
func (p *tparse) annotations() (calendar string, ok bool) {
	calendars, criticalCalendar := 0, false
	for p.peek() == '[' {
		start := p.i
		p.i++
		critical := p.accept('!')
		ks := p.i
		if !isAnnotationKeyLead(p.peek()) {
			p.i = start
			return "", false
		}
		p.i++
		for !p.eof() && isAnnotationKeyChar(p.peek()) {
			p.i++
		}
		key := p.s[ks:p.i]
		if !p.accept('=') {
			p.i = start
			return "", false
		}
		vs := p.i
		for {
			cs := p.i
			for !p.eof() && isAlnum(p.peek()) {
				p.i++
			}
			if p.i == cs {
				p.i = start
				return "", false
			}
			if !p.accept('-') {
				break
			}
		}
		value := p.s[vs:p.i]
		if !p.accept(']') {
			p.i = start
			return "", false
		}
		if key == "u-ca" {
			// A second calendar is allowed and ignored, but only where none of
			// them was marked critical: a string that insists on a calendar
			// and then names another one is asking for two things at once,
			// whichever order they were written in.
			calendars++
			criticalCalendar = criticalCalendar || critical
			if calendars == 1 {
				calendar = value
			} else if criticalCalendar {
				return "", false
			}
		} else if critical {
			// Any other annotation the engine does not know, marked critical,
			// says the string means something this engine cannot honour.
			return "", false
		}
	}
	return calendar, true
}

// ---- the whole strings ----

// parseISODateTime reads a date, optionally a time, optionally an offset, and
// the bracketed tail. It is the body of nearly every Temporal string format.
func parseISODateTime(s string) (temporalParse, bool) {
	var r temporalParse
	p := &tparse{s: s}
	y, m, d, ok := p.date()
	if !ok {
		return r, false
	}
	r.hasDate, r.year, r.month, r.day = true, y, m, d
	if b := p.peek(); b == 'T' || b == 't' || b == ' ' {
		p.i++
		t, _, _, good := p.timeSpec()
		if !good {
			return r, false
		}
		r.hasTime, r.time = true, t
		if p.peek() == 'Z' || p.peek() == 'z' {
			p.i++
			r.z = true
		} else if ns, text, has := p.utcOffset(true); has {
			r.hasOffset, r.offsetNs, r.offsetStr = true, ns, text
		}
	}
	if tz, has := p.timeZoneAnnotation(); has {
		r.hasTZ, r.tzName = true, tz
	}
	cal, good := p.annotations()
	if !good || !p.eof() {
		return r, false
	}
	r.calendar = cal
	return r, true
}

// parseTemporalDateString is a date, with anything after it that a date string
// may carry.
func parseTemporalDateString(s string) (temporalParse, bool) {
	r, ok := parseISODateTime(s)
	if !ok {
		return r, false
	}
	// A Z says the reading was taken in UTC, which is a fact about an instant
	// and not about a date. The types that carry no zone reject it rather than
	// quietly dropping it.
	if r.z {
		return r, false
	}
	if !isValidISODate(r.year, r.month, r.day) {
		return r, false
	}
	return r, true
}

// parseTemporalTimeString takes either a time on its own or a whole date-time,
// of which it keeps the time.
func parseTemporalTimeString(s string) (temporalParse, bool) {
	if r, ok := parseISODateTime(s); ok {
		if !r.hasTime || r.z {
			return r, false
		}
		return r, true
	}
	var r temporalParse
	p := &tparse{s: s}
	designated := false
	if b := p.peek(); b == 'T' || b == 't' {
		p.i++
		designated = true
	}
	t, fields, sep, ok := p.timeSpec()
	if !ok {
		return r, false
	}
	// Without the T, a bare HHMM or HHMMSS could as easily have been a date,
	// and Temporal will not guess: it rejects the ones that read both ways.
	if !designated && !sep {
		if fields == 2 && isValidISODate(2000, t.hour, t.minute) {
			return r, false
		}
		if fields == 3 && t.hour >= 1 && t.hour <= 12 {
			// HHMMSS could be MMDD with a two-digit tail only in the
			// four-digit case, which is already covered; six digits are never
			// a date.
			_ = t
		}
	}
	if p.peek() == 'Z' || p.peek() == 'z' {
		// A time with a Z is an instant, not a plain time.
		return r, false
	}
	if ns, text, has := p.utcOffset(true); has {
		r.hasOffset, r.offsetNs, r.offsetStr = true, ns, text
	}
	if tz, has := p.timeZoneAnnotation(); has {
		r.hasTZ, r.tzName = true, tz
	}
	cal, good := p.annotations()
	if !good || !p.eof() {
		return r, false
	}
	r.calendar = cal
	r.hasTime, r.time = true, t
	return r, true
}

// parseTemporalYearMonthString takes "2020-01" as well as a whole date.
func parseTemporalYearMonthString(s string) (temporalParse, bool) {
	var r temporalParse
	p := &tparse{s: s}
	y, ok := p.year()
	if ok {
		dash := p.accept('-')
		if m, good := p.digits(2); good {
			// It is only a year-month if nothing that belongs to a date follows.
			if p.eof() || p.peek() == '[' {
				if tz, has := p.timeZoneAnnotation(); has {
					r.hasTZ, r.tzName = true, tz
				}
				cal, good := p.annotations()
				if good && p.eof() {
					if m < 1 || m > 12 {
						return r, false
					}
					r.hasDate, r.year, r.month, r.day = true, y, m, 1
					r.dayAbsent = true
					r.calendar = cal
					return r, true
				}
			}
			_ = dash
		}
	}
	return parseTemporalDateString(s)
}

// parseTemporalMonthDayString takes "--12-31" and "12-31" as well as a date.
func parseTemporalMonthDayString(s string) (temporalParse, bool) {
	var r temporalParse
	p := &tparse{s: s}
	if strings.HasPrefix(s, "--") {
		p.i += 2
	}
	m, ok := p.digits(2)
	if ok {
		dash := p.accept('-')
		if d, good := p.digits(2); good {
			if p.eof() || p.peek() == '[' {
				if tz, has := p.timeZoneAnnotation(); has {
					r.hasTZ, r.tzName = true, tz
				}
				cal, good := p.annotations()
				if good && p.eof() {
					if m < 1 || m > 12 || d < 1 || d > isoDaysInMonth(1972, m) {
						return r, false
					}
					r.hasDate, r.year, r.month, r.day = true, 1972, m, d
					r.yearAbsent = true
					r.calendar = cal
					return r, true
				}
			}
			_ = dash
		}
	}
	if strings.HasPrefix(s, "--") {
		return r, false
	}
	return parseTemporalDateString(s)
}

// parseTemporalInstantString needs a time and something that says where in the
// world it was: an offset or a Z.
func parseTemporalInstantString(s string) (temporalParse, bool) {
	r, ok := parseISODateTime(s)
	if !ok || !r.hasTime {
		return r, false
	}
	if !r.z && !r.hasOffset {
		return r, false
	}
	if !isValidISODate(r.year, r.month, r.day) {
		return r, false
	}
	return r, true
}

// parseTemporalZonedDateTimeString needs the bracketed zone; an offset alone
// does not say which zone the time is kept in.
func parseTemporalZonedDateTimeString(s string) (temporalParse, bool) {
	r, ok := parseISODateTime(s)
	if !ok || !r.hasTZ {
		return r, false
	}
	if !isValidISODate(r.year, r.month, r.day) {
		return r, false
	}
	return r, true
}

// parseTimeZoneIdentifier reads a zone on its own, which is either a name or an
// offset of whole minutes.
func parseTimeZoneIdentifier(s string) (name string, offsetNs int64, isOffset bool, ok bool) {
	p := &tparse{s: s}
	if ns, _, has := p.utcOffset(true); has && p.eof() {
		return "", ns, true, true
	}
	p.i = 0
	if n, has := p.ianaName(); has && p.eof() {
		return n, 0, false, true
	}
	return "", 0, false, false
}

// ---- durations ----

// parseTemporalDurationString reads "P1Y2M3DT4H5M6.7S". A fraction is allowed
// only on the last time unit present, which is the rule that stops a duration
// from saying the same thing twice.
func parseTemporalDurationString(s string) (fields [10]float64, ok bool) {
	p := &tparse{s: s}
	sign := 1.0
	if sg, has := p.signHere(); has {
		sign = float64(sg)
	}
	if !p.accept('P') && !p.accept('p') {
		return fields, false
	}
	// Y M W D, in that order, each optional.
	dateUnits := []byte{'Y', 'M', 'W', 'D'}
	idx := 0
	any := false
	for idx < 4 && !p.eof() && isDigit(p.peek()) {
		start := p.i
		v := 0.0
		for !p.eof() && isDigit(p.peek()) {
			v = v*10 + float64(p.peek()-'0')
			p.i++
		}
		if p.i-start > 16 {
			// Beyond what a double can hold exactly, the duration is out of
			// range rather than merely long.
			return fields, false
		}
		c := p.peek()
		matched := -1
		for k := idx; k < 4; k++ {
			if c == dateUnits[k] || c == dateUnits[k]+32 {
				matched = k
				break
			}
		}
		if matched < 0 {
			return fields, false
		}
		p.i++
		fields[matched] = v
		idx = matched + 1
		any = true
	}
	if b := p.peek(); b == 'T' || b == 't' {
		p.i++
		timeUnits := []byte{'H', 'M', 'S'}
		tidx := 0
		tany := false
		for tidx < 3 && !p.eof() && isDigit(p.peek()) {
			v := 0.0
			for !p.eof() && isDigit(p.peek()) {
				v = v*10 + float64(p.peek()-'0')
				p.i++
			}
			frac, hasFrac := p.fraction()
			c := p.peek()
			matched := -1
			for k := tidx; k < 3; k++ {
				if c == timeUnits[k] || c == timeUnits[k]+32 {
					matched = k
					break
				}
			}
			if matched < 0 {
				return fields, false
			}
			p.i++
			fields[4+matched] = v
			tidx = matched + 1
			tany = true
			if hasFrac {
				// The fraction spills into the smaller units, and nothing may
				// follow it.
				spill := int64(frac) // nanoseconds of one second
				switch matched {
				case 0:
					spill *= 3600
				case 1:
					spill *= 60
				}
				fields[6] += float64(spill / nsPerSecond)
				rest := spill % nsPerSecond
				fields[7] += float64(rest / nsPerMilli)
				fields[8] += float64((rest / nsPerMicro) % 1000)
				fields[9] += float64(rest % 1000)
				break
			}
		}
		if !tany {
			return fields, false
		}
		any = true
	}
	if !any || !p.eof() {
		return fields, false
	}
	for i := range fields {
		fields[i] *= sign
	}
	return fields, true
}

// formatTemporalDuration writes a duration back out. Zero is "PT0S", because a
// duration must name at least one unit.
func formatTemporalDuration(d durationRec, precision int) string {
	sign := d.sign()
	var b strings.Builder
	if sign < 0 {
		b.WriteByte('-')
	}
	b.WriteByte('P')
	abs := func(f float64) float64 {
		if f < 0 {
			return -f
		}
		return f
	}
	writeUnit := func(v float64, unit byte) {
		if v != 0 {
			b.WriteString(formatIntegralFloat(abs(v)))
			b.WriteByte(unit)
		}
	}
	writeUnit(d.years, 'Y')
	writeUnit(d.months, 'M')
	writeUnit(d.weeks, 'W')
	writeUnit(d.days, 'D')

	// The seconds and everything under them are written as one number.
	secs := abs(d.seconds)
	subNs := int64(abs(d.ms))*nsPerMilli + int64(abs(d.us))*nsPerMicro + int64(abs(d.ns))
	secs += float64(subNs / nsPerSecond)
	rem := subNs % nsPerSecond
	timeAny := d.hours != 0 || d.minutes != 0 || secs != 0 || rem != 0 || precision >= 0
	if timeAny {
		b.WriteByte('T')
		writeUnit(d.hours, 'H')
		writeUnit(d.minutes, 'M')
		if secs != 0 || rem != 0 || precision >= 0 ||
			(d.hours == 0 && d.minutes == 0) {
			b.WriteString(formatIntegralFloat(secs))
			b.WriteString(formatFractionalSeconds(int(rem/nsPerMilli),
				int((rem/nsPerMicro)%1000), int(rem%1000), precision))
			b.WriteByte('S')
		}
	}
	out := b.String()
	if strings.HasSuffix(out, "P") {
		out += "T0S"
	}
	return out
}

// formatIntegralFloat writes a whole number that may be larger than an int64.
func formatIntegralFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
