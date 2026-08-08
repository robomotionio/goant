package engine

// Differences and rounding: the part of Temporal that has to decide what "one
// month" is worth.
//
// Below a day everything is a fixed number of nanoseconds and rounding is
// arithmetic. A day is not fixed once a time zone is involved, and a month is
// never fixed, so rounding those means asking the calendar where the neighbours
// are and measuring how far along we got. That is what the nudge functions do.

import (
	"math/big"
)

// isCalendarUnit is true of the three units no fixed number of nanoseconds
// answers for.
func isCalendarUnit(u int) bool { return u <= unitWeek }

// largerUnit is the coarser of two units, which is the one with the smaller
// index.
func largerUnit(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// maximumRoundingIncrement is how large an increment may be for a unit before
// it would step over the unit above. The calendar units have no maximum.
func maximumRoundingIncrement(u int) (int64, bool) {
	switch u {
	case unitHour:
		return 24, true
	case unitMinute, unitSecond:
		return 60, true
	case unitMillisecond, unitMicrosecond, unitNanosecond:
		return 1000, true
	}
	return 0, false
}

// differenceSettings reads the four options every until and since takes, in the
// order the spec reads them.
type differenceSettings struct {
	largest   int
	smallest  int
	increment int64
	mode      string
}

func (rt *Runtime) differenceSettings(opts Value, since bool, minUnit, maxUnit int,
	fallbackSmallest, defaultLargest int) (differenceSettings, *ThrowError) {
	var s differenceSettings
	largest, e := rt.getTemporalUnit(opts, "largestUnit", minUnit, maxUnit, unitAuto, "auto")
	if e != nil {
		return s, e
	}
	increment, e := rt.getRoundingIncrement(opts)
	if e != nil {
		return s, e
	}
	mode, e := rt.getRoundingMode(opts, "trunc")
	if e != nil {
		return s, e
	}
	if since {
		mode = negateRoundingMode(mode)
	}
	smallest, e := rt.getTemporalUnit(opts, "smallestUnit", minUnit, maxUnit, fallbackSmallest)
	if e != nil {
		return s, e
	}
	defLargest := largerUnit(defaultLargest, smallest)
	if largest == unitAuto {
		largest = defLargest
	}
	if largerUnit(largest, smallest) != largest {
		return s, rt.rangeError("largestUnit must be larger than smallestUnit")
	}
	if max, ok := maximumRoundingIncrement(smallest); ok {
		if e := rt.validateRoundingIncrement(increment, max, false); e != nil {
			return s, e
		}
	}
	return differenceSettings{largest, smallest, increment, mode}, nil
}

// ---- exact rounding of a rational ----

// roundRatToIncrement rounds an exact fraction to a multiple of an increment.
// The nudge functions produce fractions -- how far between two months an
// instant fell -- and rounding them in floating point would lose the tie cases
// that the half-modes exist to decide.
func roundRatToIncrement(x *big.Rat, increment *big.Int, mode string) *big.Int {
	incR := new(big.Rat).SetInt(increment)
	q := new(big.Rat).Quo(x, incR)
	fl := new(big.Int).Div(q.Num(), q.Denom())
	r1 := new(big.Int).Mul(fl, increment)
	if new(big.Rat).SetInt(r1).Cmp(x) == 0 {
		return r1
	}
	r2 := new(big.Int).Add(r1, increment)
	positive := x.Sign() > 0
	var up bool
	switch mode {
	case "ceil":
		up = true
	case "floor":
		up = false
	case "expand":
		up = positive
	case "trunc":
		up = !positive
	default:
		frac := new(big.Rat).Sub(x, new(big.Rat).SetInt(r1))
		twice := new(big.Rat).Add(frac, frac)
		switch c := twice.Cmp(incR); {
		case c < 0:
			up = false
		case c > 0:
			up = true
		default:
			switch mode {
			case "halfCeil":
				up = true
			case "halfFloor":
				up = false
			case "halfExpand":
				up = positive
			case "halfTrunc":
				up = !positive
			default:
				up = fl.Bit(0) == 1
			}
		}
	}
	if up {
		return r2
	}
	return r1
}

// ---- the pieces the nudges are built from ----

func add24HourDays(t *big.Int, days int64) *big.Int {
	return new(big.Int).Add(t, new(big.Int).Mul(bigInt(days), bigNsPerDay))
}

func differenceTime(a, b isoTimeRec) *big.Int {
	return bigInt(b.nanosecond() - a.nanosecond())
}

func epochNsPlus(ns, t *big.Int) *big.Int { return new(big.Int).Add(ns, t) }

// adjustDays replaces the day count of a date duration, keeping the rest.
func adjustDays(d dateDuration, days int64) dateDuration {
	d.days = days
	return d
}

// epochNsOf is the instant a local date-time names, in a zone or in UTC when
// there is no zone.
func (rt *Runtime) epochNsOf(dt isoDateTimeRec, tz string) (*big.Int, *ThrowError) {
	if tz == "" {
		return isoDateTimeToEpochNanoseconds(dt, 0), nil
	}
	z, ok := temporalZoneFor(tz)
	if !ok {
		return nil, rt.rangeError("unknown time zone: " + tz)
	}
	ns, ok := z.disambiguate(dt, "compatible")
	if !ok {
		return nil, rt.rangeError("this local time does not exist in " + tz)
	}
	return ns, nil
}

// ---- nudging ----

type nudgeResult struct {
	duration  internalDuration
	nudgedNs  *big.Int
	didExpand bool
	total     *big.Rat // the exact value before rounding, which total() wants
}

// nudgeToCalendarUnit rounds a duration whose smallest unit is a year, a month,
// a week, or a day in a time zone -- the units whose length has to be looked up
// rather than known.
func (rt *Runtime) nudgeToCalendarUnit(sign int, d internalDuration, destNs *big.Int,
	dt isoDateTimeRec, tz, calendar string, increment int64, unit int,
	mode string) (nudgeResult, *ThrowError) {
	var r1, r2 int64
	var startDur, endDur dateDuration
	switch unit {
	case unitYear:
		r1 = truncToIncrement(d.date.years, increment)
		r2 = r1 + increment*int64(sign)
		startDur = dateDuration{years: r1}
		endDur = dateDuration{years: r2}
	case unitMonth:
		r1 = truncToIncrement(d.date.months, increment)
		r2 = r1 + increment*int64(sign)
		startDur = dateDuration{years: d.date.years, months: r1}
		endDur = dateDuration{years: d.date.years, months: r2}
	case unitWeek:
		yearsMonths := dateDuration{years: d.date.years, months: d.date.months}
		weeksStart, err := calendarDateAdd(calendar, dt.date, yearsMonths, "constrain")
		if err != nil {
			return nudgeResult{}, rt.throwFor(err)
		}
		weeksEnd := epochDaysToISODate(weeksStart.epochDays() + int(d.date.days))
		until := calendarDateUntil(calendar, weeksStart, weeksEnd, unitWeek)
		r1 = truncToIncrement(d.date.weeks+until.weeks, increment)
		r2 = r1 + increment*int64(sign)
		startDur = dateDuration{years: d.date.years, months: d.date.months, weeks: r1}
		endDur = dateDuration{years: d.date.years, months: d.date.months, weeks: r2}
	default: // day
		r1 = truncToIncrement(d.date.days, increment)
		r2 = r1 + increment*int64(sign)
		startDur = adjustDays(d.date, r1)
		endDur = adjustDays(d.date, r2)
	}
	start, err := calendarDateAdd(calendar, dt.date, startDur, "constrain")
	if err != nil {
		return nudgeResult{}, rt.throwFor(err)
	}
	end, err := calendarDateAdd(calendar, dt.date, endDur, "constrain")
	if err != nil {
		return nudgeResult{}, rt.throwFor(err)
	}
	startNs, e := rt.epochNsOf(isoDateTimeRec{start, dt.time}, tz)
	if e != nil {
		return nudgeResult{}, e
	}
	endNs, e := rt.epochNsOf(isoDateTimeRec{end, dt.time}, tz)
	if e != nil {
		return nudgeResult{}, e
	}
	if startNs.Cmp(endNs) == 0 {
		return nudgeResult{}, rt.rangeError("the two ends of the rounding interval are the same instant")
	}
	// How far along the interval the target instant fell, exactly.
	progress := new(big.Rat).SetFrac(new(big.Int).Sub(destNs, startNs),
		new(big.Int).Sub(endNs, startNs))
	total := new(big.Rat).SetInt64(r1)
	step := new(big.Rat).SetInt64(increment * int64(sign))
	total.Add(total, progress.Mul(progress, step))
	exact := new(big.Rat).Set(total)
	rounded := roundRatToIncrement(total, bigInt(increment), mode)
	didExpand := rounded.Int64() == r2 && r1 != r2

	var resultDate dateDuration
	var resultNs *big.Int
	if didExpand {
		resultDate, resultNs = endDur, endNs
	} else {
		resultDate, resultNs = startDur, startNs
	}
	return nudgeResult{newInternal(resultDate, new(big.Int)), resultNs, didExpand, exact}, nil
}

func truncToIncrement(v, increment int64) int64 {
	return v - v%increment
}

// nudgeToZonedTime rounds a time of day inside a zoned day, which may be
// shorter or longer than twenty-four hours.
func (rt *Runtime) nudgeToZonedTime(sign int, d internalDuration, dt isoDateTimeRec,
	tz, calendar string, increment int64, unit int, mode string) (nudgeResult, *ThrowError) {
	start, err := calendarDateAdd(calendar, dt.date, d.date, "constrain")
	if err != nil {
		return nudgeResult{}, rt.throwFor(err)
	}
	startDT := isoDateTimeRec{start, dt.time}
	endDate := epochDaysToISODate(start.epochDays() + sign)
	endDT := isoDateTimeRec{endDate, dt.time}
	startNs, e := rt.epochNsOf(startDT, tz)
	if e != nil {
		return nudgeResult{}, e
	}
	endNs, e := rt.epochNsOf(endDT, tz)
	if e != nil {
		return nudgeResult{}, e
	}
	daySpan := new(big.Int).Sub(endNs, startNs)
	if daySpan.Sign() != sign {
		return nudgeResult{}, rt.rangeError("the day runs the wrong way in this zone")
	}
	unitNs := nsPerUnit[unit] * increment
	rounded := roundNumberToIncrement(d.time, bigInt(unitNs), mode)
	beyond := new(big.Int).Sub(rounded, daySpan)
	var dayDelta int64
	var nudged *big.Int
	didExpand := false
	if beyond.Sign() == 0 || beyond.Sign() == sign {
		didExpand = true
		dayDelta = int64(sign)
		rounded = roundNumberToIncrement(beyond, bigInt(unitNs), mode)
		nudged = epochNsPlus(endNs, rounded)
	} else {
		nudged = epochNsPlus(startNs, rounded)
	}
	date := adjustDays(d.date, d.date.days+dayDelta)
	return nudgeResult{newInternal(date, rounded), nudged, didExpand, nil}, nil
}

// nudgeToDayOrTime rounds a duration whose smallest unit is fixed: a day of
// twenty-four hours, or anything under it.
func (rt *Runtime) nudgeToDayOrTime(d internalDuration, destNs *big.Int, largestUnit int,
	increment int64, unit int, mode string) (nudgeResult, *ThrowError) {
	t := add24HourDays(d.time, d.date.days)
	unitNs := nsPerUnit[unit] * increment
	rounded := roundNumberToIncrement(t, bigInt(unitNs), mode)
	diff := new(big.Int).Sub(rounded, t)
	wholeDays := new(big.Int).Quo(t, bigNsPerDay).Int64()
	roundedWholeDays := new(big.Int).Quo(rounded, bigNsPerDay).Int64()
	dayDelta := roundedWholeDays - wholeDays
	didExpand := sign64(dayDelta) == t.Sign() && dayDelta != 0
	nudged := epochNsPlus(destNs, diff)
	days := int64(0)
	remainder := rounded
	if largestUnit <= unitDay {
		days = roundedWholeDays
		remainder = new(big.Int).Sub(rounded, new(big.Int).Mul(bigInt(roundedWholeDays), bigNsPerDay))
	}
	return nudgeResult{newInternal(adjustDays(d.date, days), remainder), nudged, didExpand, nil}, nil
}

// bubbleRelativeDuration carries an expansion upwards: rounding the days up may
// have filled a month, which fills a year.
func (rt *Runtime) bubbleRelativeDuration(sign int, d internalDuration, nudgedNs *big.Int,
	dt isoDateTimeRec, tz, calendar string, largestUnit, smallestUnit int) (internalDuration, *ThrowError) {
	if smallestUnit == largestUnit {
		return d, nil
	}
	for unit := smallestUnit - 1; unit >= largestUnit; unit-- {
		if unit == unitWeek && largestUnit != unitWeek {
			continue
		}
		var endDur dateDuration
		switch unit {
		case unitYear:
			endDur = dateDuration{years: d.date.years + int64(sign)}
		case unitMonth:
			endDur = dateDuration{years: d.date.years, months: d.date.months + int64(sign)}
		case unitWeek:
			endDur = dateDuration{years: d.date.years, months: d.date.months,
				weeks: d.date.weeks + int64(sign)}
		default:
			continue
		}
		end, err := calendarDateAdd(calendar, dt.date, endDur, "constrain")
		if err != nil {
			return d, rt.throwFor(err)
		}
		endNs, e := rt.epochNsOf(isoDateTimeRec{end, dt.time}, tz)
		if e != nil {
			return d, e
		}
		beyond := new(big.Int).Sub(nudgedNs, endNs)
		if beyond.Sign() == 0 || beyond.Sign() == sign {
			d = newInternal(endDur, new(big.Int))
			continue
		}
		break
	}
	return d, nil
}

// roundRelativeDuration is the whole of it: nudge to the smallest unit, then
// carry any expansion up as far as the largest.
func (rt *Runtime) roundRelativeDuration(d internalDuration, destNs *big.Int,
	dt isoDateTimeRec, tz, calendar string, largestUnit int, increment int64,
	smallestUnit int, mode string) (durationRec, *ThrowError) {
	irregular := isCalendarUnit(smallestUnit) || (tz != "" && smallestUnit == unitDay)
	sign := 1
	if d.sign() < 0 {
		sign = -1
	}
	var nr nudgeResult
	var e *ThrowError
	switch {
	case irregular:
		nr, e = rt.nudgeToCalendarUnit(sign, d, destNs, dt, tz, calendar, increment, smallestUnit, mode)
	case tz != "":
		nr, e = rt.nudgeToZonedTime(sign, d, dt, tz, calendar, increment, smallestUnit, mode)
	default:
		nr, e = rt.nudgeToDayOrTime(d, destNs, largestUnit, increment, smallestUnit, mode)
	}
	if e != nil {
		return durationRec{}, e
	}
	d = nr.duration
	if nr.didExpand && smallestUnit != unitWeek {
		start := largerUnit(smallestUnit, unitDay)
		d, e = rt.bubbleRelativeDuration(sign, d, nr.nudgedNs, dt, tz, calendar, largestUnit, start)
		if e != nil {
			return durationRec{}, e
		}
	}
	out := largestUnit
	if largestUnit <= unitDay {
		out = unitHour
	}
	res, ok := durationFromInternal(d, out)
	if !ok {
		return res, rt.rangeError("duration is out of range")
	}
	return res, nil
}

// ---- differences ----

// differenceISODateTime is the gap between two local date-times in a calendar,
// with the time part borrowing a day where it has to.
func (rt *Runtime) differenceISODateTime(a, b isoDateTimeRec, calendar string,
	largestUnit int) internalDuration {
	timeDur := differenceTime(a.time, b.time)
	timeSign := timeDur.Sign()
	dateSign := compareISODate(b.date, a.date)
	adjusted := a.date
	if dateSign == -timeSign && timeSign != 0 {
		adjusted = epochDaysToISODate(a.date.epochDays() + timeSign)
		timeDur = add24HourDays(timeDur, int64(-timeSign))
	}
	dateLargest := largerUnit(unitDay, largestUnit)
	dateDiff := calendarDateUntil(calendar, adjusted, b.date, dateLargest)
	if largestUnit > unitDay {
		timeDur = add24HourDays(timeDur, dateDiff.days)
		dateDiff.days = 0
	}
	return newInternal(dateDiff, timeDur)
}

// differencePlainDateTimeWithRounding is until/since for the two plain types
// that carry a time.
func (rt *Runtime) differencePlainDateTimeWithRounding(a, b isoDateTimeRec,
	calendar string, s differenceSettings) (durationRec, *ThrowError) {
	if compareISODateTime(a, b) == 0 {
		return durationRec{}, nil
	}
	diff := rt.differenceISODateTime(a, b, calendar, s.largest)
	if s.smallest == unitNanosecond && s.increment == 1 {
		out, ok := durationFromInternal(diff, s.largest)
		if !ok {
			return out, rt.rangeError("duration is out of range")
		}
		return out, nil
	}
	destNs := isoDateTimeToEpochNanoseconds(b, 0)
	return rt.roundRelativeDuration(diff, destNs, a, "", calendar, s.largest,
		s.increment, s.smallest, s.mode)
}

// differenceZonedDateTime is the gap between two instants read in a zone, where
// a day is whatever the zone made it.
func (rt *Runtime) differenceZonedDateTime(ns1, ns2 *big.Int, tz, calendar string,
	largestUnit int) (internalDuration, *ThrowError) {
	if ns1.Cmp(ns2) == 0 {
		return newInternal(dateDuration{}, new(big.Int)), nil
	}
	z, ok := temporalZoneFor(tz)
	if !ok {
		return internalDuration{}, rt.rangeError("unknown time zone: " + tz)
	}
	startDT := z.dateTimeFor(ns1)
	endDT := z.dateTimeFor(ns2)
	sign := -compareISODateTime(startDT, endDT)
	maxCorrection := 1
	if sign == 1 {
		maxCorrection = 2
	}
	timeDur := differenceTime(startDT.time, endDT.time)
	correction := 0
	if timeDur.Sign() == -sign {
		correction = 1
	}
	intermediate := endDT.date
	for ; correction <= maxCorrection; correction++ {
		intermediate = epochDaysToISODate(endDT.date.epochDays() - correction*sign)
		mid, e := rt.epochNsOf(isoDateTimeRec{intermediate, startDT.time}, tz)
		if e != nil {
			return internalDuration{}, e
		}
		timeDur = new(big.Int).Sub(ns2, mid)
		if timeDur.Sign() != -sign {
			break
		}
	}
	dateLargest := largerUnit(unitDay, largestUnit)
	dateDiff := calendarDateUntil(calendar, startDT.date, intermediate, dateLargest)
	if largestUnit > unitDay {
		timeDur = add24HourDays(timeDur, dateDiff.days)
		dateDiff.days = 0
	}
	return newInternal(dateDiff, timeDur), nil
}

// differenceZonedDateTimeWithRounding is until/since for ZonedDateTime.
func (rt *Runtime) differenceZonedDateTimeWithRounding(ns1, ns2 *big.Int, tz,
	calendar string, s differenceSettings) (durationRec, *ThrowError) {
	if s.largest > unitDay {
		// Nothing above a day is asked for, so the zone cannot matter.
		return rt.differenceInstant(ns1, ns2, s)
	}
	diff, e := rt.differenceZonedDateTime(ns1, ns2, tz, calendar, s.largest)
	if e != nil {
		return durationRec{}, e
	}
	if s.smallest == unitNanosecond && s.increment == 1 {
		out, ok := durationFromInternal(diff, s.largest)
		if !ok {
			return out, rt.rangeError("duration is out of range")
		}
		return out, nil
	}
	z, _ := temporalZoneFor(tz)
	return rt.roundRelativeDuration(diff, ns2, z.dateTimeFor(ns1), tz, calendar,
		s.largest, s.increment, s.smallest, s.mode)
}

// differenceInstant is the gap between two instants, which needs no calendar
// and no zone.
func (rt *Runtime) differenceInstant(ns1, ns2 *big.Int, s differenceSettings) (durationRec, *ThrowError) {
	t := new(big.Int).Sub(ns2, ns1)
	t = roundNumberToIncrement(t, bigInt(nsPerUnit[s.smallest]*s.increment), s.mode)
	out, ok := durationFromInternal(newInternal(dateDuration{}, t), s.largest)
	if !ok {
		return out, rt.rangeError("duration is out of range")
	}
	return out, nil
}
