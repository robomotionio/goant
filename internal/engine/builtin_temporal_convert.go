package engine

// Turning whatever a caller passed into the Temporal value a method needs.
//
// Every Temporal method that takes "a date" takes three things: one of its own
// objects, a bag of fields, or a string. The three are read in three quite
// different orders -- which matters, because reading a field is a getter call a
// test can watch -- so each conversion is written out rather than funnelled
// through one.

import (
	"math/big"
	"sort"
)

// The calendar field keys, in the order the spec reads them, which is
// alphabetical. The bit order is the read order.
const (
	keyDay = 1 << iota
	keyEra
	keyEraYear
	keyHour
	keyMicrosecond
	keyMillisecond
	keyMinute
	keyMonth
	keyMonthCode
	keyNanosecond
	keyOffset
	keySecond
	keyTimeZone
	keyYear
)

var calendarKeyNames = []string{"day", "era", "eraYear", "hour", "microsecond",
	"millisecond", "minute", "month", "monthCode", "nanosecond", "offset",
	"second", "timeZone", "year"}

const keysDate = keyYear | keyMonth | keyMonthCode | keyDay
const keysYearMonth = keyYear | keyMonth | keyMonthCode
const keysMonthDay = keyYear | keyMonth | keyMonthCode | keyDay
const keysTime = keyHour | keyMinute | keySecond | keyMillisecond | keyMicrosecond | keyNanosecond

// calendarDateKeys adds era and eraYear where the calendar counts its years
// from named eras.
func calendarDateKeys(calendar string, base int) int {
	if calendarHasEras(calendar) {
		base |= keyEra | keyEraYear
	}
	return base
}

// temporalFields is what a bag of fields turned out to hold.
type temporalFields struct {
	cal         calFieldSet
	time        isoTimeRec
	hasTime     int // the bits of keysTime that were present
	offset      string
	hasOffset   bool
	timeZone    Value
	hasTimeZone bool
	present     int
}

// readCalendarFields is PrepareCalendarFields: read the wanted keys off the
// object in order, coercing each, and complain if a required one is missing.
func (rt *Runtime) readCalendarFields(item Value, want, required int, requireAny bool) (temporalFields, *ThrowError) {
	var f temporalFields
	for i, name := range calendarKeyNames {
		bit := 1 << i
		if want&bit == 0 {
			continue
		}
		v, e := rt.getField(item, name)
		if e != nil {
			return f, e
		}
		if v.IsUndefined() {
			if required&bit != 0 {
				return f, rt.typeError("missing " + name + " field")
			}
			continue
		}
		f.present |= bit
		switch bit {
		case keyEra:
			s, e := rt.toStringValue(v)
			if e != nil {
				return f, e
			}
			f.cal.era = rt.strGo(s)
			f.cal.has |= fEra
		case keyEraYear:
			n, e := rt.toIntegerWithTruncation(v)
			if e != nil {
				return f, e
			}
			f.cal.eraYear = n
			f.cal.has |= fEraYear
		case keyYear:
			n, e := rt.toIntegerWithTruncation(v)
			if e != nil {
				return f, e
			}
			f.cal.year = n
			f.cal.has |= fYear
		case keyMonth:
			n, e := rt.toPositiveIntegerWithTruncation(v)
			if e != nil {
				return f, e
			}
			f.cal.month = n
			f.cal.has |= fMonth
		case keyMonthCode:
			s, e := rt.toStringValue(v)
			if e != nil {
				return f, e
			}
			f.cal.monthCode = rt.strGo(s)
			f.cal.has |= fMonthCode
		case keyDay:
			n, e := rt.toPositiveIntegerWithTruncation(v)
			if e != nil {
				return f, e
			}
			f.cal.day = n
			f.cal.has |= fDay
		case keyOffset:
			s, e := rt.toStringValue(v)
			if e != nil {
				return f, e
			}
			f.offset, f.hasOffset = rt.strGo(s), true
		case keyTimeZone:
			f.timeZone, f.hasTimeZone = v, true
		default: // the six time fields
			n, e := rt.toIntegerWithTruncation(v)
			if e != nil {
				return f, e
			}
			f.hasTime |= bit
			switch bit {
			case keyHour:
				f.time.hour = n
			case keyMinute:
				f.time.minute = n
			case keySecond:
				f.time.second = n
			case keyMillisecond:
				f.time.ms = n
			case keyMicrosecond:
				f.time.us = n
			case keyNanosecond:
				f.time.ns = n
			}
		}
	}
	if requireAny && f.present == 0 {
		return f, rt.typeError("no recognised fields were given")
	}
	return f, nil
}

// calendarOf reads the calendar property of a bag of fields, which defaults to
// the ISO calendar.
func (rt *Runtime) calendarOfItem(item Value) (string, *ThrowError) {
	v, e := rt.getField(item, "calendar")
	if e != nil {
		return "", e
	}
	if v.IsUndefined() {
		return "iso8601", nil
	}
	return rt.toCalendarID(v)
}

// calendarFromParse is the calendar a string named, defaulting to ISO. A string
// may only name a calendar other than ISO where the caller expects one.
func calendarFromParse(p temporalParse) (string, bool) {
	if p.calendar == "" {
		return "iso8601", true
	}
	return canonicalCalendarID(p.calendar)
}

// throwFor turns the calendar layer's errors into the JavaScript ones.
func (rt *Runtime) throwFor(err error) *ThrowError {
	switch err {
	case errCalendarFields:
		return rt.typeError("the fields do not name a date")
	case errCalendarRange:
		return rt.rangeError("date is outside the representable range")
	}
	return rt.rangeError("date is outside the range of its month")
}

// ---- to a date ----

func (rt *Runtime) toTemporalDate(item, options Value) (isoDateRec, string, *ThrowError) {
	if item.IsObjectType() && rt.objPtr(item) != nil {
		switch rt.temporalKindOf(item) {
		case kindPlainDate:
			o := rt.objPtr(item)
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return isoDateRec{}, "", e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return isoDateRec{}, "", e
			}
			return tDate(o), rt.tCalendar(o), nil
		case kindZonedDateTime:
			o := rt.objPtr(item)
			z, _ := temporalZoneFor(rt.tTimeZone(o))
			dt := z.dateTimeFor(tEpochNs(o))
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return isoDateRec{}, "", e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return isoDateRec{}, "", e
			}
			return dt.date, rt.tCalendar(o), nil
		case kindPlainDateTime:
			o := rt.objPtr(item)
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return isoDateRec{}, "", e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return isoDateRec{}, "", e
			}
			return tDateTimeDate(o), rt.tCalendar(o), nil
		}
		cal, e := rt.calendarOfItem(item)
		if e != nil {
			return isoDateRec{}, "", e
		}
		f, e := rt.readCalendarFields(item, calendarDateKeys(cal, keysDate), 0, true)
		if e != nil {
			return isoDateRec{}, "", e
		}
		opts, e := rt.temporalOptions(options)
		if e != nil {
			return isoDateRec{}, "", e
		}
		overflow, e := rt.getOverflow(opts)
		if e != nil {
			return isoDateRec{}, "", e
		}
		iso, err := calendarDateFromFields(cal, f.cal, overflow)
		if err != nil {
			return isoDateRec{}, "", rt.throwFor(err)
		}
		return iso, cal, nil
	}
	if !item.IsString() {
		return isoDateRec{}, "", rt.typeError("a Temporal.PlainDate, a bag of fields or a string was expected")
	}
	p, ok := parseTemporalDateString(rt.strGo(item))
	if !ok {
		return isoDateRec{}, "", rt.rangeError("cannot parse " + rt.strGo(item) + " as a date")
	}
	if p.z {
		return isoDateRec{}, "", rt.rangeError("a date string may not carry a UTC designator")
	}
	cal, ok := calendarFromParse(p)
	if !ok {
		return isoDateRec{}, "", rt.rangeError("unknown calendar in " + rt.strGo(item))
	}
	opts, e := rt.temporalOptions(options)
	if e != nil {
		return isoDateRec{}, "", e
	}
	if _, e := rt.getOverflow(opts); e != nil {
		return isoDateRec{}, "", e
	}
	iso := isoDateRec{p.year, p.month, p.day}
	if !isoDateWithinLimits(iso) {
		return iso, "", rt.rangeError("date is outside the representable range")
	}
	return iso, cal, nil
}

// ---- to a date-time ----

func (rt *Runtime) toTemporalDateTime(item, options Value) (isoDateTimeRec, string, *ThrowError) {
	var zero isoDateTimeRec
	if item.IsObjectType() && rt.objPtr(item) != nil {
		o := rt.objPtr(item)
		switch rt.temporalKindOf(item) {
		case kindPlainDateTime:
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return zero, "", e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return zero, "", e
			}
			return tDateTime(o), rt.tCalendar(o), nil
		case kindZonedDateTime:
			z, _ := temporalZoneFor(rt.tTimeZone(o))
			dt := z.dateTimeFor(tEpochNs(o))
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return zero, "", e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return zero, "", e
			}
			return dt, rt.tCalendar(o), nil
		case kindPlainDate:
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return zero, "", e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return zero, "", e
			}
			return isoDateTimeRec{tDate(o), midnightTime()}, rt.tCalendar(o), nil
		}
		cal, e := rt.calendarOfItem(item)
		if e != nil {
			return zero, "", e
		}
		f, e := rt.readCalendarFields(item, calendarDateKeys(cal, keysDate)|keysTime, 0, true)
		if e != nil {
			return zero, "", e
		}
		opts, e := rt.temporalOptions(options)
		if e != nil {
			return zero, "", e
		}
		overflow, e := rt.getOverflow(opts)
		if e != nil {
			return zero, "", e
		}
		iso, err := calendarDateFromFields(cal, f.cal, overflow)
		if err != nil {
			return zero, "", rt.throwFor(err)
		}
		t, e := rt.regulateFieldsTime(f, overflow)
		if e != nil {
			return zero, "", e
		}
		return isoDateTimeRec{iso, t}, cal, nil
	}
	if !item.IsString() {
		return zero, "", rt.typeError("a Temporal.PlainDateTime, a bag of fields or a string was expected")
	}
	p, ok := parseTemporalDateString(rt.strGo(item))
	if !ok {
		return zero, "", rt.rangeError("cannot parse " + rt.strGo(item) + " as a date-time")
	}
	if p.z {
		return zero, "", rt.rangeError("a date-time string may not carry a UTC designator")
	}
	cal, ok := calendarFromParse(p)
	if !ok {
		return zero, "", rt.rangeError("unknown calendar in " + rt.strGo(item))
	}
	opts, e := rt.temporalOptions(options)
	if e != nil {
		return zero, "", e
	}
	if _, e := rt.getOverflow(opts); e != nil {
		return zero, "", e
	}
	dt := isoDateTimeRec{isoDateRec{p.year, p.month, p.day}, p.time}
	if dt.time.second == 60 {
		dt.time.second = 59
	}
	if !isoDateTimeWithinLimits(dt) {
		return dt, "", rt.rangeError("date-time is outside the representable range")
	}
	return dt, cal, nil
}

// regulateFieldsTime turns the time fields of a bag into a time, filling in
// what was left out with zero and applying the overflow rule to the rest.
func (rt *Runtime) regulateFieldsTime(f temporalFields, overflow string) (isoTimeRec, *ThrowError) {
	t := f.time
	if overflow == "reject" {
		if !isValidTime(t.hour, t.minute, t.second, t.ms, t.us, t.ns) {
			return t, rt.rangeError("time is out of range")
		}
		return t, nil
	}
	return constrainTime(t.hour, t.minute, t.second, t.ms, t.us, t.ns), nil
}

// ---- to a time ----

func (rt *Runtime) toTemporalTime(item, options Value) (isoTimeRec, *ThrowError) {
	var zero isoTimeRec
	if item.IsObjectType() && rt.objPtr(item) != nil {
		o := rt.objPtr(item)
		switch rt.temporalKindOf(item) {
		case kindPlainTime:
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return zero, e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return zero, e
			}
			return tTime(o), nil
		case kindPlainDateTime:
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return zero, e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return zero, e
			}
			return tDateTimeTime(o), nil
		case kindZonedDateTime:
			z, _ := temporalZoneFor(rt.tTimeZone(o))
			dt := z.dateTimeFor(tEpochNs(o))
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return zero, e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return zero, e
			}
			return dt.time, nil
		}
		f, e := rt.readCalendarFields(item, keysTime, 0, true)
		if e != nil {
			return zero, e
		}
		opts, e := rt.temporalOptions(options)
		if e != nil {
			return zero, e
		}
		overflow, e := rt.getOverflow(opts)
		if e != nil {
			return zero, e
		}
		return rt.regulateFieldsTime(f, overflow)
	}
	if !item.IsString() {
		return zero, rt.typeError("a Temporal.PlainTime, a bag of fields or a string was expected")
	}
	p, ok := parseTemporalTimeString(rt.strGo(item))
	if !ok || !p.hasTime {
		return zero, rt.rangeError("cannot parse " + rt.strGo(item) + " as a time")
	}
	if p.calendar != "" && p.calendar != "iso8601" {
		return zero, rt.rangeError("a time string may not name a calendar")
	}
	opts, e := rt.temporalOptions(options)
	if e != nil {
		return zero, e
	}
	if _, e := rt.getOverflow(opts); e != nil {
		return zero, e
	}
	t := p.time
	if t.second == 60 {
		t.second = 59
	}
	return t, nil
}

// ---- to an instant ----

func (rt *Runtime) toTemporalInstant(item Value) (*big.Int, *ThrowError) {
	if item.IsObjectType() && rt.objPtr(item) != nil {
		switch rt.temporalKindOf(item) {
		case kindInstant, kindZonedDateTime:
			return tEpochNs(rt.objPtr(item)), nil
		}
		p, e := rt.toPrimitive(item, "string")
		if e != nil {
			return nil, e
		}
		item = p
	}
	if !item.IsString() {
		return nil, rt.typeError("a Temporal.Instant or a string was expected")
	}
	s := rt.strGo(item)
	p, ok := parseTemporalInstantString(s)
	if !ok {
		return nil, rt.rangeError("cannot parse " + s + " as an instant")
	}
	t := p.time
	if t.second == 60 {
		t.second = 59
	}
	var offset int64
	if p.hasOffset {
		offset = p.offsetNs
	}
	ns := isoDateTimeToEpochNanoseconds(isoDateTimeRec{isoDateRec{p.year, p.month, p.day}, t}, offset)
	if !epochNsWithinLimits(ns) {
		return nil, rt.rangeError("instant is outside the representable range")
	}
	return ns, nil
}

// ---- to a zoned date-time ----

func (rt *Runtime) toTemporalZonedDateTime(item, options Value) (*big.Int, string, string, *ThrowError) {
	if item.IsObjectType() && rt.objPtr(item) != nil {
		o := rt.objPtr(item)
		if rt.temporalKindOf(item) == kindZonedDateTime {
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return nil, "", "", e
			}
			if _, e := rt.getDisambiguation(opts); e != nil {
				return nil, "", "", e
			}
			if _, e := rt.getOffsetOption(opts, "reject"); e != nil {
				return nil, "", "", e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return nil, "", "", e
			}
			return tEpochNs(o), rt.tTimeZone(o), rt.tCalendar(o), nil
		}
		cal, e := rt.calendarOfItem(item)
		if e != nil {
			return nil, "", "", e
		}
		f, e := rt.readCalendarFields(item,
			calendarDateKeys(cal, keysDate)|keysTime|keyOffset|keyTimeZone,
			keyTimeZone, false)
		if e != nil {
			return nil, "", "", e
		}
		tz, e := rt.toTimeZoneID(f.timeZone)
		if e != nil {
			return nil, "", "", e
		}
		var offsetNs int64
		hasOffset := false
		if f.hasOffset {
			ns, ok := parseDateTimeUTCOffset(f.offset)
			if !ok {
				return nil, "", "", rt.rangeError("invalid offset: " + f.offset)
			}
			offsetNs, hasOffset = ns, true
		}
		opts, e := rt.temporalOptions(options)
		if e != nil {
			return nil, "", "", e
		}
		disambiguation, e := rt.getDisambiguation(opts)
		if e != nil {
			return nil, "", "", e
		}
		offsetOpt, e := rt.getOffsetOption(opts, "reject")
		if e != nil {
			return nil, "", "", e
		}
		overflow, e := rt.getOverflow(opts)
		if e != nil {
			return nil, "", "", e
		}
		iso, err := calendarDateFromFields(cal, f.cal, overflow)
		if err != nil {
			return nil, "", "", rt.throwFor(err)
		}
		t, e := rt.regulateFieldsTime(f, overflow)
		if e != nil {
			return nil, "", "", e
		}
		ns, e := rt.interpretISODateTimeOffset(isoDateTimeRec{iso, t}, hasOffset, offsetNs,
			tz, disambiguation, offsetOpt, true)
		if e != nil {
			return nil, "", "", e
		}
		return ns, tz, cal, nil
	}
	if !item.IsString() {
		return nil, "", "", rt.typeError("a Temporal.ZonedDateTime, a bag of fields or a string was expected")
	}
	s := rt.strGo(item)
	p, ok := parseTemporalZonedDateTimeString(s)
	if !ok {
		return nil, "", "", rt.rangeError("cannot parse " + s + " as a zoned date-time")
	}
	cal, ok := calendarFromParse(p)
	if !ok {
		return nil, "", "", rt.rangeError("unknown calendar in " + s)
	}
	z, ok := temporalZoneFor(p.tzName)
	if !ok {
		return nil, "", "", rt.rangeError("unknown time zone in " + s)
	}
	opts, e := rt.temporalOptions(options)
	if e != nil {
		return nil, "", "", e
	}
	disambiguation, e := rt.getDisambiguation(opts)
	if e != nil {
		return nil, "", "", e
	}
	offsetOpt, e := rt.getOffsetOption(opts, "reject")
	if e != nil {
		return nil, "", "", e
	}
	if _, e := rt.getOverflow(opts); e != nil {
		return nil, "", "", e
	}
	t := p.time
	if t.second == 60 {
		t.second = 59
	}
	dt := isoDateTimeRec{isoDateRec{p.year, p.month, p.day}, t}
	if p.z {
		// A Z says the offset exactly; there is nothing to match against.
		offsetOpt = "use"
	}
	ns, e := rt.interpretISODateTimeOffset(dt, p.hasOffset || p.z, p.offsetNs, z.id,
		disambiguation, offsetOpt, !p.z)
	if e != nil {
		return nil, "", "", e
	}
	return ns, z.id, cal, nil
}

// parseDateTimeUTCOffset reads an offset string as a count of nanoseconds.
func parseDateTimeUTCOffset(s string) (int64, bool) {
	p := &tparse{s: s}
	ns, _, ok := p.utcOffset(true)
	if !ok || !p.eof() {
		return 0, false
	}
	return ns, true
}

// interpretISODateTimeOffset is the rule for a local time that came with an
// offset: whether to believe the offset, the zone, or neither.
func (rt *Runtime) interpretISODateTimeOffset(dt isoDateTimeRec, hasOffset bool,
	offsetNs int64, tz, disambiguation, offsetOpt string, matchMinutes bool) (*big.Int, *ThrowError) {
	z, ok := temporalZoneFor(tz)
	if !ok {
		return nil, rt.rangeError("unknown time zone: " + tz)
	}
	if !hasOffset || offsetOpt == "ignore" {
		ns, ok := z.disambiguate(dt, disambiguation)
		if !ok {
			return nil, rt.rangeError("this local time does not exist in " + tz)
		}
		return ns, nil
	}
	if offsetOpt == "use" {
		ns := isoDateTimeToEpochNanoseconds(dt, offsetNs)
		if !epochNsWithinLimits(ns) {
			return nil, rt.rangeError("instant is outside the representable range")
		}
		return ns, nil
	}
	// "prefer" and "reject" both look for an instant whose offset matches.
	for _, cand := range z.possibleInstants(dt) {
		candOffset := z.offsetNs(cand)
		if candOffset == offsetNs {
			return cand, nil
		}
		if matchMinutes {
			// An offset written to the minute matches a zone that keeps
			// seconds, which the older zones do.
			if roundToMinute(candOffset) == offsetNs {
				return cand, nil
			}
		}
	}
	if offsetOpt == "reject" {
		return nil, rt.rangeError("the offset " + formatOffsetNanoseconds(offsetNs) +
			" is not one " + tz + " was ever at")
	}
	ns, ok2 := z.disambiguate(dt, disambiguation)
	if !ok2 {
		return nil, rt.rangeError("this local time does not exist in " + tz)
	}
	return ns, nil
}

func roundToMinute(ns int64) int64 {
	return roundNumberToIncrement(bigInt(ns), bigInt(nsPerMinute), "halfExpand").Int64()
}

// ---- to a duration ----

func (rt *Runtime) toTemporalDuration(item Value) (durationRec, *ThrowError) {
	var zero durationRec
	if rt.temporalKindOf(item) == kindDuration {
		return tDuration(rt.objPtr(item)), nil
	}
	if item.IsObjectType() && rt.objPtr(item) != nil {
		return rt.durationFromFields(item)
	}
	if !item.IsString() {
		return zero, rt.typeError("a Temporal.Duration, a bag of fields or a string was expected")
	}
	f, ok := parseTemporalDurationString(rt.strGo(item))
	if !ok {
		return zero, rt.rangeError("cannot parse " + rt.strGo(item) + " as a duration")
	}
	if !isValidDuration(f) {
		return zero, rt.rangeError("duration is out of range")
	}
	return durationFromFields(f), nil
}

// durationFields are read in alphabetical order too, and at least one of them
// has to be there.
var durationFieldOrder = []struct {
	name string
	at   int
}{
	{"days", 3}, {"hours", 4}, {"microseconds", 8}, {"milliseconds", 7},
	{"minutes", 5}, {"months", 1}, {"nanoseconds", 9}, {"seconds", 6},
	{"weeks", 2}, {"years", 0},
}

func (rt *Runtime) durationFromFields(item Value) (durationRec, *ThrowError) {
	var f [10]float64
	any := false
	for _, fd := range durationFieldOrder {
		v, e := rt.getField(item, fd.name)
		if e != nil {
			return durationRec{}, e
		}
		if v.IsUndefined() {
			continue
		}
		any = true
		n, e := rt.toNumber(v)
		if e != nil {
			return durationRec{}, e
		}
		if !isIntegralNumber(n) {
			return durationRec{}, rt.rangeError(fd.name + " must be a whole number")
		}
		f[fd.at] = n
	}
	if !any {
		return durationRec{}, rt.typeError("no duration fields were given")
	}
	if !isValidDuration(f) {
		return durationRec{}, rt.rangeError("duration is out of range")
	}
	return durationFromFields(f), nil
}

// ---- relativeTo ----

// relativeToRec is what "relativeTo" resolved to: nothing, a plain date, or a
// zoned date-time. Years, months and weeks cannot be measured without one.
type relativeToRec struct {
	kind     temporalKind
	date     isoDateRec
	calendar string
	epochNs  *big.Int
	timeZone string
}

func (r relativeToRec) none() bool { return r.kind == kindNone }

func (rt *Runtime) getRelativeTo(opts Value) (relativeToRec, *ThrowError) {
	var r relativeToRec
	v, e := rt.getField(opts, "relativeTo")
	if e != nil {
		return r, e
	}
	if v.IsUndefined() {
		return r, nil
	}
	if v.IsObjectType() && rt.objPtr(v) != nil {
		o := rt.objPtr(v)
		switch rt.temporalKindOf(v) {
		case kindZonedDateTime:
			return relativeToRec{kind: kindZonedDateTime, epochNs: tEpochNs(o),
				timeZone: rt.tTimeZone(o), calendar: rt.tCalendar(o)}, nil
		case kindPlainDate:
			return relativeToRec{kind: kindPlainDate, date: tDate(o),
				calendar: rt.tCalendar(o)}, nil
		case kindPlainDateTime:
			return relativeToRec{kind: kindPlainDate, date: tDateTimeDate(o),
				calendar: rt.tCalendar(o)}, nil
		}
		cal, e := rt.calendarOfItem(v)
		if e != nil {
			return r, e
		}
		f, e := rt.readCalendarFields(v,
			calendarDateKeys(cal, keysDate)|keysTime|keyOffset|keyTimeZone, 0, false)
		if e != nil {
			return r, e
		}
		iso, err := calendarDateFromFields(cal, f.cal, "constrain")
		if err != nil {
			return r, rt.throwFor(err)
		}
		t, e := rt.regulateFieldsTime(f, "constrain")
		if e != nil {
			return r, e
		}
		if !f.hasTimeZone {
			return relativeToRec{kind: kindPlainDate, date: iso, calendar: cal}, nil
		}
		tz, e := rt.toTimeZoneID(f.timeZone)
		if e != nil {
			return r, e
		}
		var offsetNs int64
		if f.hasOffset {
			ns, ok := parseDateTimeUTCOffset(f.offset)
			if !ok {
				return r, rt.rangeError("invalid offset: " + f.offset)
			}
			offsetNs = ns
		}
		ns, e := rt.interpretISODateTimeOffset(isoDateTimeRec{iso, t}, f.hasOffset,
			offsetNs, tz, "compatible", "reject", true)
		if e != nil {
			return r, e
		}
		return relativeToRec{kind: kindZonedDateTime, epochNs: ns, timeZone: tz,
			calendar: cal}, nil
	}
	if !v.IsString() {
		return r, rt.typeError("relativeTo must be a date or a string")
	}
	s := rt.strGo(v)
	p, ok := parseISODateTime(s)
	if !ok || !isValidISODate(p.year, p.month, p.day) {
		return r, rt.rangeError("cannot parse " + s + " as a date")
	}
	cal, ok := calendarFromParse(p)
	if !ok {
		return r, rt.rangeError("unknown calendar in " + s)
	}
	iso := isoDateRec{p.year, p.month, p.day}
	if !p.hasTZ {
		if p.z {
			return r, rt.rangeError("relativeTo may not carry a UTC designator without a zone")
		}
		return relativeToRec{kind: kindPlainDate, date: iso, calendar: cal}, nil
	}
	z, ok := temporalZoneFor(p.tzName)
	if !ok {
		return r, rt.rangeError("unknown time zone in " + s)
	}
	t := p.time
	if t.second == 60 {
		t.second = 59
	}
	offsetOpt := "reject"
	if p.z {
		offsetOpt = "use"
	}
	ns, e := rt.interpretISODateTimeOffset(isoDateTimeRec{iso, t}, p.hasOffset || p.z,
		p.offsetNs, z.id, "compatible", offsetOpt, !p.z)
	if e != nil {
		return r, e
	}
	return relativeToRec{kind: kindZonedDateTime, epochNs: ns, timeZone: z.id,
		calendar: cal}, nil
}

// sortStrings keeps the option lists in the order the messages read best.
func sortStrings(ss []string) []string { sort.Strings(ss); return ss }
