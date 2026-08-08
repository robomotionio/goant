package engine

// Durations, and the arithmetic that balances and rounds them.
//
// A Temporal duration is ten numbers, but almost nothing works on all ten at
// once. The spec splits it in two -- a date part counted in years, months,
// weeks and days, and a time part counted as one exact number of nanoseconds --
// because the date half needs a calendar and a starting point to mean anything
// and the time half never does. Everything here follows that split.

import (
	"math"
	"math/big"
)

// The units, smallest last, in the order the spec compares them.
const (
	unitYear = iota
	unitMonth
	unitWeek
	unitDay
	unitHour
	unitMinute
	unitSecond
	unitMillisecond
	unitMicrosecond
	unitNanosecond
	unitCount
	unitAuto = -1
)

var temporalUnitNames = [unitCount]string{"year", "month", "week", "day", "hour",
	"minute", "second", "millisecond", "microsecond", "nanosecond"}

var temporalUnitPlurals = [unitCount]string{"years", "months", "weeks", "days",
	"hours", "minutes", "seconds", "milliseconds", "microseconds", "nanoseconds"}

// unitFromName maps both the singular and the plural, since Temporal accepts
// either wherever it takes a unit.
func unitFromName(s string) (int, bool) {
	for i, n := range temporalUnitNames {
		if s == n || s == temporalUnitPlurals[i] {
			return i, true
		}
	}
	return 0, false
}

// nsPerUnit is how many nanoseconds a unit holds, for the units that hold a
// fixed number. Days are in the table because a duration without a time zone
// treats them as 24 hours.
var nsPerUnit = [unitCount]int64{0, 0, 0, nsPerDay, nsPerHour, nsPerMinute,
	nsPerSecond, nsPerMilli, nsPerMicro, 1}

// maxIncrement is the largest roundingIncrement each unit accepts, past which
// the increment would step over the next unit up.
var maxIncrement = [unitCount]int64{0, 0, 0, 0, 24, 60, 60, 1000, 1000, 1000}

// ---- the record ----

// durationRec is a duration as the JavaScript object holds it: ten values,
// every one an integer, and all of one sign.
type durationRec struct {
	years, months, weeks, days             float64
	hours, minutes, seconds                float64
	ms, us, ns                             float64
}

func (d durationRec) fields() [10]float64 {
	return [10]float64{d.years, d.months, d.weeks, d.days, d.hours, d.minutes,
		d.seconds, d.ms, d.us, d.ns}
}

func durationFromFields(f [10]float64) durationRec {
	return durationRec{f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7], f[8], f[9]}
}

// sign is the sign of the first field that is not zero. A duration whose fields
// disagree about their sign is not a duration at all, which isValidDuration
// checks separately.
func (d durationRec) sign() int {
	for _, v := range d.fields() {
		if v < 0 {
			return -1
		}
		if v > 0 {
			return 1
		}
	}
	return 0
}

func (d durationRec) isZero() bool { return d.sign() == 0 }

// dateSign and timeSign look at only half of it, which the rounding rules need
// separately.
func (d durationRec) dateSign() int {
	f := d.fields()
	for _, v := range f[:4] {
		if v < 0 {
			return -1
		}
		if v > 0 {
			return 1
		}
	}
	return 0
}

// isValidDuration is the whole of the spec's test: every field finite and of
// one sign, the calendar units inside 2^32, and the whole thing inside 2^53
// seconds.
func isValidDuration(f [10]float64) bool {
	sign := 0
	for _, v := range f {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
		if v != 0 {
			s := 1
			if v < 0 {
				s = -1
			}
			if sign == 0 {
				sign = s
			} else if sign != s {
				return false
			}
		}
	}
	for _, v := range f[:3] {
		if math.Abs(v) >= math.Pow(2, 32) {
			return false
		}
	}
	// The seconds the duration comes to, computed exactly rather than in
	// floating point, where the sum would lose the small units against the big.
	total := new(big.Rat)
	weights := []*big.Rat{
		big.NewRat(secsPerDay, 1), big.NewRat(3600, 1), big.NewRat(60, 1),
		big.NewRat(1, 1), big.NewRat(1, 1000), big.NewRat(1, 1000000),
		big.NewRat(1, 1000000000),
	}
	for i, v := range f[3:] {
		r := new(big.Rat).SetFloat64(v)
		if r == nil {
			return false
		}
		total.Add(total, r.Mul(r, weights[i]))
	}
	limit := new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 53))
	return total.Abs(total).Cmp(limit) < 0
}

// ---- the internal form ----

// dateDuration is the calendar half: whole years, months, weeks and days.
type dateDuration struct{ years, months, weeks, days int64 }

func (d dateDuration) sign() int {
	for _, v := range []int64{d.years, d.months, d.weeks, d.days} {
		if v < 0 {
			return -1
		}
		if v > 0 {
			return 1
		}
	}
	return 0
}

// internalDuration is how the spec carries a duration through arithmetic: the
// calendar part as counts, and everything below a day as one exact number of
// nanoseconds.
type internalDuration struct {
	date dateDuration
	time *big.Int
}

func newInternal(date dateDuration, time *big.Int) internalDuration {
	if time == nil {
		time = new(big.Int)
	}
	return internalDuration{date, time}
}

func (d internalDuration) sign() int {
	if s := d.date.sign(); s != 0 {
		return s
	}
	return d.time.Sign()
}

// timeDurationFromComponents adds up everything below a day.
func timeDurationFromComponents(h, mi, s, ms, us, ns float64) *big.Int {
	total := new(big.Int)
	add := func(v float64, mul int64) {
		// Exactly as in formatTemporalDuration: take the integer the float is
		// before scaling it, because a big.Float would round the product back
		// to fifty-three bits and a duration may hold more than that.
		i, _ := new(big.Float).SetFloat64(v).Int(nil)
		total.Add(total, i.Mul(i, bigInt(mul)))
	}
	add(h, nsPerHour)
	add(mi, nsPerMinute)
	add(s, nsPerSecond)
	add(ms, nsPerMilli)
	add(us, nsPerMicro)
	add(ns, 1)
	return total
}

// toInternal splits a duration the usual way.
func (d durationRec) toInternal() internalDuration {
	return newInternal(dateDuration{int64(d.years), int64(d.months),
		int64(d.weeks), int64(d.days)},
		timeDurationFromComponents(d.hours, d.minutes, d.seconds, d.ms, d.us, d.ns))
}

// toInternal24 splits it the other way, folding the days into the time as
// twenty-four hours each. It is what a duration means when nothing says which
// day it belongs to.
func (d durationRec) toInternal24() internalDuration {
	t := timeDurationFromComponents(d.hours, d.minutes, d.seconds, d.ms, d.us, d.ns)
	t.Add(t, new(big.Int).Mul(bigInt(int64(d.days)), bigNsPerDay))
	return newInternal(dateDuration{int64(d.years), int64(d.months),
		int64(d.weeks), 0}, t)
}

// durationFromInternal puts a duration back together, spreading the nanoseconds
// across every unit down from largestUnit.
func durationFromInternal(d internalDuration, largestUnit int) (durationRec, bool) {
	sign := int64(d.time.Sign())
	n := new(big.Int).Abs(d.time)
	var days, hours, minutes, seconds, ms, us float64
	// Every count comes out as the float the field holds, taken from the exact
	// value rather than from arithmetic done in floats. A field may be larger
	// than an int64 -- eight sextillion microseconds is a number a double can
	// carry -- and whether the whole duration is one a duration may be is
	// isValidDuration's question, asked at the end, on the totals.
	divmod := func(by int64) float64 {
		q, r := new(big.Int).QuoRem(n, bigInt(by), new(big.Int))
		n = r
		f, _ := new(big.Float).SetInt(q).Float64()
		return f
	}
	switch largestUnit {
	case unitYear, unitMonth, unitWeek, unitDay:
		days = divmod(nsPerDay)
		hours = divmod(nsPerHour)
		minutes = divmod(nsPerMinute)
		seconds = divmod(nsPerSecond)
		ms = divmod(nsPerMilli)
		us = divmod(nsPerMicro)
	case unitHour:
		hours = divmod(nsPerHour)
		minutes = divmod(nsPerMinute)
		seconds = divmod(nsPerSecond)
		ms = divmod(nsPerMilli)
		us = divmod(nsPerMicro)
	case unitMinute:
		minutes = divmod(nsPerMinute)
		seconds = divmod(nsPerSecond)
		ms = divmod(nsPerMilli)
		us = divmod(nsPerMicro)
	case unitSecond:
		seconds = divmod(nsPerSecond)
		ms = divmod(nsPerMilli)
		us = divmod(nsPerMicro)
	case unitMillisecond:
		ms = divmod(nsPerMilli)
		us = divmod(nsPerMicro)
	case unitMicrosecond:
		us = divmod(nsPerMicro)
	}
	ns, _ := new(big.Float).SetInt(n).Float64()
	s := float64(sign)
	out := durationRec{
		years: float64(d.date.years), months: float64(d.date.months),
		weeks: float64(d.date.weeks), days: float64(d.date.days) + days*s,
		hours: hours * s, minutes: minutes * s,
		seconds: seconds * s, ms: ms * s,
		us: us * s, ns: ns * s,
	}
	if !isValidDuration(out.fields()) {
		return out, false
	}
	return out, true
}

// ---- time arithmetic ----

func addTimeDuration(a, b *big.Int) *big.Int { return new(big.Int).Add(a, b) }

// timeDurationFromEpochDifference is the exact time between two instants.
func timeDurationFromEpochDifference(one, two *big.Int) *big.Int {
	return new(big.Int).Sub(one, two)
}

// roundTimeDurationToIncrement rounds the time half to a multiple of a unit.
func roundTimeDurationToIncrement(t *big.Int, increment int64, mode string) *big.Int {
	return roundNumberToIncrement(t, bigInt(increment), mode)
}

// divideTimeDuration is the exact ratio of a time duration to a unit, which is
// what "total" answers.
func divideTimeDuration(t *big.Int, unitNs int64) *big.Rat {
	return new(big.Rat).SetFrac(t, bigInt(unitNs))
}

// ---- validity of the whole ----

func durationWithinLimits(d durationRec) bool { return isValidDuration(d.fields()) }

// bigToFloat is the conversion used wherever an exact count becomes a duration
// field. Anything that will not survive it makes the duration invalid, which
// the caller then reports.
func bigToFloat(i *big.Int) float64 {
	f, _ := new(big.Float).SetInt(i).Float64()
	return f
}

// dateDaysOf is ToDateDurationRecordWithoutTime's day count: the whole days a
// duration holds, counted TOWARDS ZERO. A date has no time for the remainder to
// live in, so a duration of minus one nanosecond short of a day is no days at
// all -- flooring it would make it minus one, and a date at the edge of the
// representable range would fall off it.
func dateDaysOf(t *big.Int) int64 {
	q := new(big.Int).Quo(t, bigNsPerDay)
	if !q.IsInt64() {
		return 0
	}
	return q.Int64()
}
