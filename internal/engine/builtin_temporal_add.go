package engine

// Adding a duration to each of the things one can be added to, and rounding a
// time of day.

import "math/big"

// addTimeToTime moves a time of day by a count of nanoseconds, answering the
// days it spilled into as well as the time it landed on.
func addTimeToTime(t isoTimeRec, ns *big.Int) (*big.Int, isoTimeRec) {
	total := new(big.Int).Add(bigInt(t.nanosecond()), ns)
	return balanceTime(total)
}

// roundTimeRec rounds a time of day to a unit, answering the day it may have
// rolled over into.
func roundTimeRec(t isoTimeRec, increment int64, unit int, mode string) (int64, isoTimeRec) {
	rounded := roundNumberToIncrement(bigInt(t.nanosecond()),
		incrementNs(unit, increment), mode)
	days, out := balanceTime(rounded)
	return days.Int64(), out
}

// addInstant moves an instant by a time duration.
func (rt *Runtime) addInstant(ns, t *big.Int) (*big.Int, *ThrowError) {
	out := new(big.Int).Add(ns, t)
	if !epochNsWithinLimits(out) {
		return nil, rt.rangeError("instant is outside the representable range")
	}
	return out, nil
}

// addDateTime moves a local date-time by a duration: the time part first,
// because the days it spills into belong to the calendar step.
func (rt *Runtime) addDateTime(dt isoDateTimeRec, calendar string, d internalDuration,
	overflow string) (isoDateTimeRec, *ThrowError) {
	days, t := addTimeToTime(dt.time, d.time)
	dd := adjustDays(d.date, d.date.days+days.Int64())
	added, err := calendarDateAdd(calendar, dt.date, dd, overflow)
	if err != nil {
		return isoDateTimeRec{}, rt.throwFor(err)
	}
	return isoDateTimeRec{added, t}, nil
}

// addZonedDateTime moves an instant by a duration as read in a zone: the
// calendar part is added to the local date, and only then is the time added to
// the instant that lands on.
func (rt *Runtime) addZonedDateTime(ns *big.Int, tz, calendar string, d internalDuration,
	overflow string) (*big.Int, *ThrowError) {
	if d.date.sign() == 0 && d.date.days == 0 {
		return rt.addInstant(ns, d.time)
	}
	z, ok := temporalZoneFor(tz)
	if !ok {
		return nil, rt.rangeError("unknown time zone: " + tz)
	}
	dt := z.dateTimeFor(ns)
	added, err := calendarDateAdd(calendar, dt.date, d.date, overflow)
	if err != nil {
		return nil, rt.throwFor(err)
	}
	mid, e := rt.epochNsOf(isoDateTimeRec{added, dt.time}, tz)
	if e != nil {
		return nil, e
	}
	return rt.addInstant(mid, d.time)
}

// negateDuration is the duration that undoes this one.
func negateDuration(d durationRec) durationRec {
	f := d.fields()
	for i := range f {
		f[i] = -f[i]
	}
	return durationFromFields(f)
}
