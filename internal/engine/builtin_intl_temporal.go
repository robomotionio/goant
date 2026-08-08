package engine

// Where Temporal and Intl meet: formatting a Temporal value with
// Intl.DateTimeFormat, and the toLocaleString on each of the eight that does
// it for you.
//
// A Temporal value is not a timestamp, so the formatter has to be told what to
// make of it. A PlainDate has no time, so it is read at noon. A PlainTime has
// no date, so it is read on the first of January 1970. And a formatter asked
// for a field the value does not have is a mistake, not something to fill in:
// a PlainYearMonth formatted with a timeStyle would have to invent an hour.
//
// A plain value is read and shown in UTC, never in the formatter's zone. It
// says a wall reading and nothing else, so moving it into a zone would only
// choose a different reading to show -- and in a zone's spring-forward gap
// there is no reading to choose. That is also why no plain value can carry a
// time-zone name: it does not have one.

import (
	"math/big"
	"time"
)

// temporalFieldSet is the required/defaults pair each type asks
// CreateDateTimeFormat for, and the components it can answer for.
type temporalFieldSet struct {
	required string
	defaults string
	allowed  []string
	styles   bool // may a dateStyle be used?
	timeOK   bool // may a timeStyle be used?
}

var temporalFieldSets = [kindCount]temporalFieldSet{
	kindInstant: {"any", "all", dtComponents, true, true},
	kindPlainDate: {"date", "date",
		[]string{"weekday", "era", "year", "month", "day"}, true, false},
	kindPlainTime: {"time", "time",
		[]string{"dayPeriod", "hour", "minute", "second", "fractionalSecondDigits"},
		false, true},
	kindPlainDateTime: {"any", "all",
		[]string{"weekday", "era", "year", "month", "day", "dayPeriod",
			"hour", "minute", "second", "fractionalSecondDigits"}, true, true},
	kindPlainYearMonth: {"year-month", "year-month",
		[]string{"era", "year", "month"}, true, false},
	kindPlainMonthDay: {"month-day", "month-day",
		[]string{"month", "day"}, true, false},
	kindZonedDateTime: {"any", "all", dtComponents, true, true},
}

// withTemporalDefaults re-fills a formatter that was given no fields at all
// with the ones this kind of value has.
func withTemporalDefaults(d dateTimeOptions, kind temporalKind) dateTimeOptions {
	if !d.defaulted {
		return d
	}
	out := d
	out.comps = map[string]string{}
	for k, v := range d.comps {
		switch k {
		case "year", "month", "day", "hour", "minute", "second":
		default:
			out.comps[k] = v
		}
	}
	set := func(names ...string) {
		for _, n := range names {
			out.comps[n] = "numeric"
		}
	}
	switch temporalFieldSets[kind].defaults {
	case "date":
		set("year", "month", "day")
	case "time":
		set("hour", "minute", "second")
	case "year-month":
		set("year", "month")
	case "month-day":
		set("month", "day")
	case "all":
		set("year", "month", "day", "hour", "minute", "second")
	}
	if kind == kindZonedDateTime {
		// A zoned reading says where it was read, or it says nothing that
		// distinguishes it from a plain one.
		out.comps["timeZoneName"] = "short"
	}
	return out
}

// temporalEffective is the formatter as it applies to one kind of Temporal
// value: its styles written out, its defaults replaced with this kind's, and
// every field this kind has not got dropped. It reports false when nothing is
// left, which is the case a TypeError is for -- a formatter asked only for an
// hour cannot show a year-month at all.
func temporalEffective(d dateTimeOptions, kind temporalKind) (dateTimeOptions, bool) {
	set := temporalFieldSets[kind]
	d = withTemporalDefaults(d, kind)
	if d.dateStyle != "" || d.timeStyle != "" {
		d = d.styleComponents()
	}
	out := d
	out.comps = map[string]string{}
	for c, v := range d.comps {
		if tagContains(set.allowed, c) {
			out.comps[c] = v
		}
	}
	if !tagContains(set.allowed, "fractionalSecondDigits") {
		out.fracDigits = 0
	}
	return out, len(out.comps) > 0
}

// temporalFormatValue is HandleDateTimeValue: the instant a Temporal value
// formats as, the options narrowed to the fields it actually has, and whether
// it is plain -- which is to say read and shown in UTC rather than in the
// formatter's zone.
func (rt *Runtime) temporalFormatValue(v Value, d dateTimeOptions) (float64, dateTimeOptions, bool, *ThrowError) {
	ms, e := rt.temporalFormatEpochMs(v, d)
	if e != nil {
		return 0, d, false, e
	}
	kind := rt.temporalKindOf(v)
	eff, _ := temporalEffective(d, kind)
	return ms, eff, temporalIsPlain(kind), nil
}

// temporalIsPlain reports whether this kind of value carries no zone, and so
// is read as the wall reading it states.
func temporalIsPlain(kind temporalKind) bool {
	switch kind {
	case kindPlainDate, kindPlainTime, kindPlainDateTime,
		kindPlainYearMonth, kindPlainMonthDay:
		return true
	}
	return false
}

// temporalFormatEpochMs is the epoch half of HandleDateTimeValue.
func (rt *Runtime) temporalFormatEpochMs(v Value, d dateTimeOptions) (float64, *ThrowError) {
	kind := rt.temporalKindOf(v)
	o := rt.objPtr(v)
	if kind == kindDuration {
		return 0, rt.typeError("a Temporal.Duration cannot be formatted as a date; use Intl.DurationFormat")
	}
	if kind == kindZonedDateTime {
		return 0, rt.typeError("format() cannot take a Temporal.ZonedDateTime; use its toLocaleString")
	}
	// A value in a calendar the formatter does not use cannot be shown in it.
	switch kind {
	case kindPlainDate, kindPlainDateTime:
		if cal := rt.tCalendar(o); cal != "iso8601" && cal != d.calendar {
			return 0, rt.rangeError("this value is in the " + cal +
				" calendar and the formatter is in the " + d.calendar + " one")
		}
	case kindPlainYearMonth, kindPlainMonthDay:
		// Part of a date only means anything in the calendar it is part of, so
		// here the two must agree outright.
		if cal := rt.tCalendar(o); cal != d.calendar {
			return 0, rt.rangeError("this value is in the " + cal +
				" calendar and the formatter is in the " + d.calendar + " one")
		}
	}
	if _, ok := temporalEffective(d, kind); !ok {
		return 0, rt.typeError("this formatter asks only for fields a Temporal." +
			temporalKindNames[kind] + " does not have")
	}
	if kind == kindInstant {
		ns := tEpochNs(o)
		f, _ := bigRatFloat(ns, nsPerMilli)
		return f, nil
	}
	var local isoDateTimeRec
	switch kind {
	case kindPlainDate, kindPlainYearMonth, kindPlainMonthDay:
		local = isoDateTimeRec{tDate(o), noonTime()}
	case kindPlainDateTime:
		local = tDateTime(o)
	case kindPlainTime:
		local = isoDateTimeRec{isoDateRec{1970, 1, 1}, tTime(o)}
	default:
		return 0, rt.typeError("not a Temporal value")
	}
	f, _ := bigRatFloat(isoDateTimeToEpochNanoseconds(local, 0), nsPerMilli)
	return f, nil
}

// temporalLocaleString is the body of every Temporal toLocaleString: build the
// formatter this kind of value asks for, then format the value with it.
func (rt *Runtime) temporalLocaleString(v Value, kind temporalKind, locales, options Value) (Value, *ThrowError) {
	requested, e := rt.canonicalizeLocaleList(locales)
	if e != nil {
		return mkundef(), e
	}
	set := temporalFieldSets[kind]
	if kind == kindZonedDateTime {
		// The value carries its own zone, so naming another one is a
		// contradiction rather than an override.
		if options.IsObjectType() && rt.objPtr(options) != nil {
			tzv, e := rt.getField(options, "timeZone")
			if e != nil {
				return mkundef(), e
			}
			if !tzv.IsUndefined() {
				return mkundef(), rt.typeError(
					"a ZonedDateTime is formatted in its own time zone; the timeZone option is not allowed")
			}
		}
	}
	d, e := rt.initDateTimeOptionsFor(options, requested, set.required, set.defaults)
	if e != nil {
		return mkundef(), e
	}
	o := rt.objPtr(v)
	var ms float64
	if kind == kindZonedDateTime {
		d.timeZone = rt.tTimeZone(o)
		if cal := rt.tCalendar(o); cal != "iso8601" && cal != d.calendar {
			return mkundef(), rt.rangeError("this value is in the " + cal +
				" calendar and the formatter is in the " + d.calendar + " one")
		}
		eff, ok := temporalEffective(d, kind)
		if !ok {
			return mkundef(), rt.typeError("this formatter asks only for fields this value does not have")
		}
		d = eff
		f, _ := bigRatFloat(tEpochNs(o), nsPerMilli)
		ms = f
	} else {
		got, e := rt.temporalFormatEpochMs(v, d)
		if e != nil {
			return mkundef(), e
		}
		ms = got
		eff, _ := temporalEffective(d, kind)
		d = eff
	}
	loc := zoneFor(d.timeZone)
	if temporalIsPlain(kind) {
		loc = time.UTC
	}
	var b []rune
	for _, p := range d.dateTimeParts(msInZone(ms, loc)) {
		b = append(b, []rune(p.val)...)
	}
	return rt.newString(string(b)), nil
}

// bigRatFloat divides an exact count by a unit and answers the nearest double,
// rounding towards negative infinity so that a millisecond before the epoch
// lands on the millisecond it is inside.
func bigRatFloat(n *big.Int, unit int64) (float64, bool) {
	q := new(big.Int).Div(n, bigInt(unit))
	f, acc := new(big.Float).SetInt(q).Float64()
	return f, acc == big.Exact
}
