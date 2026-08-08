package engine

// Temporal.Duration.

import (
	"math"
	"math/big"
	"strings"
)

// defaultLargestUnit is the largest unit a duration actually uses, which is
// what "as large as it already is" means when no largestUnit was asked for.
func defaultLargestUnit(d durationRec) int {
	f := d.fields()
	for i, v := range f {
		if v != 0 {
			return i
		}
	}
	return unitNanosecond
}

func (rt *Runtime) initTemporalDuration(ns *object) {
	co, po := rt.defineTemporalCtor(ns, kindDuration, 0,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if e := rt.requireNew("Duration"); e != nil {
				return mkundef(), e
			}
			var f [10]float64
			for i := 0; i < 10; i++ {
				v := arg(args, i)
				if v.IsUndefined() {
					continue
				}
				n, e := rt.toNumber(v)
				if e != nil {
					return mkundef(), e
				}
				if !isIntegralNumber(n) {
					return mkundef(), rt.rangeError("every duration field must be a whole number")
				}
				f[i] = n
			}
			if !isValidDuration(f) {
				return mkundef(), rt.rangeError("invalid duration")
			}
			return rt.createTemporalDuration(durationFromFields(f))
		})

	co.defineOwn("from", rt.newNativeFunc("from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		d, e := rt.toTemporalDuration(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalDuration(d)
	}), attrWritable|attrConfigurable)

	co.defineOwn("compare", rt.newNativeFunc("compare", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		one, e := rt.toTemporalDuration(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		two, e := rt.toTemporalDuration(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		opts, e := rt.temporalOptions(arg(args, 2))
		if e != nil {
			return mkundef(), e
		}
		rel, e := rt.getRelativeTo(opts)
		if e != nil {
			return mkundef(), e
		}
		if one == two {
			return mknum(0), nil
		}
		a, e := rt.durationTotalNs(one, rel)
		if e != nil {
			return mkundef(), e
		}
		b, e := rt.durationTotalNs(two, rel)
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(a.Cmp(b))), nil
	}), attrWritable|attrConfigurable)

	getter := func(name string, f func(d durationRec) Value) {
		g := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, e := rt.requireTemporal(this, kindDuration)
			if e != nil {
				return mkundef(), e
			}
			return f(tDuration(o)), nil
		})
		po.defineAccessor(name, g, mkundef(), true, false, attrConfigurable)
	}
	getter("years", func(d durationRec) Value { return mknum(d.years) })
	getter("months", func(d durationRec) Value { return mknum(d.months) })
	getter("weeks", func(d durationRec) Value { return mknum(d.weeks) })
	getter("days", func(d durationRec) Value { return mknum(d.days) })
	getter("hours", func(d durationRec) Value { return mknum(d.hours) })
	getter("minutes", func(d durationRec) Value { return mknum(d.minutes) })
	getter("seconds", func(d durationRec) Value { return mknum(d.seconds) })
	getter("milliseconds", func(d durationRec) Value { return mknum(d.ms) })
	getter("microseconds", func(d durationRec) Value { return mknum(d.us) })
	getter("nanoseconds", func(d durationRec) Value { return mknum(d.ns) })
	getter("sign", func(d durationRec) Value { return mknum(float64(d.sign())) })
	getter("blank", func(d durationRec) Value { return mkbool(d.sign() == 0) })

	rt.defMethod(po, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindDuration)
		if e != nil {
			return mkundef(), e
		}
		d := tDuration(o)
		item := arg(args, 0)
		if !item.IsObjectType() || rt.objPtr(item) == nil {
			return mkundef(), rt.typeError("with() takes a bag of fields")
		}
		f := d.fields()
		any := false
		for _, fd := range durationFieldOrder {
			v, e := rt.getField(item, fd.name)
			if e != nil {
				return mkundef(), e
			}
			if v.IsUndefined() {
				continue
			}
			any = true
			n, e := rt.toNumber(v)
			if e != nil {
				return mkundef(), e
			}
			if !isIntegralNumber(n) {
				return mkundef(), rt.rangeError(fd.name + " must be a whole number")
			}
			f[fd.at] = n
		}
		if !any {
			return mkundef(), rt.typeError("no duration fields were given")
		}
		if !isValidDuration(f) {
			return mkundef(), rt.rangeError("invalid duration")
		}
		return rt.createTemporalDuration(durationFromFields(f))
	})

	rt.defMethod(po, "negated", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindDuration)
		if e != nil {
			return mkundef(), e
		}
		f := tDuration(o).fields()
		for i := range f {
			f[i] = -f[i]
		}
		return rt.createTemporalDuration(durationFromFields(f))
	})

	rt.defMethod(po, "abs", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindDuration)
		if e != nil {
			return mkundef(), e
		}
		f := tDuration(o).fields()
		for i := range f {
			f[i] = math.Abs(f[i])
		}
		return rt.createTemporalDuration(durationFromFields(f))
	})

	addSub := func(subtract bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, e := rt.requireTemporal(this, kindDuration)
			if e != nil {
				return mkundef(), e
			}
			d := tDuration(o)
			other, e := rt.toTemporalDuration(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			if subtract {
				f := other.fields()
				for i := range f {
					f[i] = -f[i]
				}
				other = durationFromFields(f)
			}
			largest := largerUnit(defaultLargestUnit(d), defaultLargestUnit(other))
			if isCalendarUnit(largest) {
				return mkundef(), rt.rangeError("adding durations with years, months or weeks needs a relativeTo")
			}
			t := new(big.Int).Add(d.toInternal24().time, other.toInternal24().time)
			out, ok := durationFromInternal(newInternal(dateDuration{}, t), largest)
			if !ok {
				return mkundef(), rt.rangeError("duration is out of range")
			}
			return rt.createTemporalDuration(out)
		}
	}
	rt.defMethod(po, "add", 1, addSub(false))
	rt.defMethod(po, "subtract", 1, addSub(true))

	rt.defMethod(po, "round", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindDuration)
		if e != nil {
			return mkundef(), e
		}
		d := tDuration(o)
		roundTo, e := rt.unitOrOptions(arg(args, 0), "smallestUnit")
		if e != nil {
			return mkundef(), e
		}
		largest, e := rt.getTemporalUnit(roundTo, "largestUnit", unitYear, unitNanosecond, unitAuto-1, "auto")
		if e != nil {
			return mkundef(), e
		}
		rel, e := rt.getRelativeTo(roundTo)
		if e != nil {
			return mkundef(), e
		}
		increment, e := rt.getRoundingIncrement(roundTo)
		if e != nil {
			return mkundef(), e
		}
		mode, e := rt.getRoundingMode(roundTo, "halfExpand")
		if e != nil {
			return mkundef(), e
		}
		smallest, e := rt.getTemporalUnit(roundTo, "smallestUnit", unitYear, unitNanosecond, unitAuto-1)
		if e != nil {
			return mkundef(), e
		}
		smallestPresent := smallest != unitAuto-1
		if !smallestPresent {
			smallest = unitNanosecond
		}
		existing := defaultLargestUnit(d)
		defLargest := largerUnit(existing, smallest)
		largestPresent := largest != unitAuto-1
		if !largestPresent || largest == unitAuto {
			largest = defLargest
		}
		if !largestPresent && !smallestPresent {
			return mkundef(), rt.rangeError("round() needs a smallestUnit or a largestUnit")
		}
		if largerUnit(largest, smallest) != largest {
			return mkundef(), rt.rangeError("largestUnit must be larger than smallestUnit")
		}
		if max, ok := maximumRoundingIncrement(smallest); ok {
			if e := rt.validateRoundingIncrement(increment, max, false); e != nil {
				return mkundef(), e
			}
		}
		s := differenceSettings{largest, smallest, increment, mode}
		out, e := rt.roundDuration(d, rel, s)
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalDuration(out)
	})

	rt.defMethod(po, "total", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindDuration)
		if e != nil {
			return mkundef(), e
		}
		d := tDuration(o)
		totalOf, e := rt.unitOrOptions(arg(args, 0), "unit")
		if e != nil {
			return mkundef(), e
		}
		rel, e := rt.getRelativeTo(totalOf)
		if e != nil {
			return mkundef(), e
		}
		unit, e := rt.getTemporalUnit(totalOf, "unit", unitYear, unitNanosecond, unitAuto-1)
		if e != nil {
			return mkundef(), e
		}
		if unit == unitAuto-1 {
			return mkundef(), rt.rangeError("total() needs a unit")
		}
		r, e := rt.totalDuration(d, rel, unit)
		if e != nil {
			return mkundef(), e
		}
		f, _ := r.Float64()
		return mknum(f), nil
	})

	rt.defMethod(po, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindDuration)
		if e != nil {
			return mkundef(), e
		}
		d := tDuration(o)
		opts, e := rt.temporalOptions(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		digits, e := rt.getFractionalSecondDigits(opts)
		if e != nil {
			return mkundef(), e
		}
		mode, e := rt.getRoundingMode(opts, "trunc")
		if e != nil {
			return mkundef(), e
		}
		smallest, e := rt.getTemporalUnitIn(opts, "smallestUnit", unitSecond, unitNanosecond)
		if e != nil {
			return mkundef(), e
		}
		if e := rt.checkUnitRange(smallest, "smallestUnit", unitSecond, unitNanosecond); e != nil {
			return mkundef(), e
		}
		precision, unit, increment, e := rt.secondsStringPrecision(smallest, digits)
		if e != nil {
			return mkundef(), e
		}
		if unit != unitNanosecond || increment != 1 {
			// Only the seconds and what is under them are rounded. The hours
			// and minutes are written as they stand, so the largest unit the
			// answer is spread over is the largest one the duration already
			// had: "PT30M0S", not the same half hour counted out as
			// "PT1800S".
			internal := d.toInternal()
			rounded := roundNumberToIncrement(internal.time, incrementNs(unit, increment), mode)
			largest := largerUnit(defaultLargestUnit(d), unitSecond)
			out, ok := durationFromInternal(newInternal(internal.date, rounded), largest)
			if !ok {
				return mkundef(), rt.rangeError("duration is out of range")
			}
			d = out
		}
		return rt.newString(formatTemporalDuration(d, precision)), nil
	})

	rt.defMethod(po, "toJSON", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindDuration)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(formatTemporalDuration(tDuration(o), -1)), nil
	})

	// A duration reads in words rather than in ISO 8601, which is what
	// Intl.DurationFormat is for.
	rt.defMethod(po, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindDuration)
		if e != nil {
			return mkundef(), e
		}
		requested, e := rt.canonicalizeLocaleList(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		opts := arg(args, 1)
		if !opts.IsUndefined() && !opts.IsObjectType() {
			return mkundef(), rt.typeError("options must be an object")
		}
		df, e := rt.initDurationOptions(opts, requested)
		if e != nil {
			return mkundef(), e
		}
		tag := defaultLocale
		if len(requested) > 0 {
			_, tag = lookupLocale(requested[0])
		}
		li, _ := lookupLocale(tag)
		var b strings.Builder
		for _, p := range rt.durationParts(df, li, tDuration(o).fields()) {
			b.WriteString(p.val)
		}
		return rt.newString(b.String()), nil
	})
}

// unitOrOptions accepts the shorthand where round() and total() take the unit
// as a string instead of a bag with one property in it.
func (rt *Runtime) unitOrOptions(v Value, key string) (Value, *ThrowError) {
	if v.IsString() {
		o := rt.newObject(mknull())
		rt.objPtr(o).defineOwn(key, v, attrWritable|attrEnumerable|attrConfigurable)
		return o, nil
	}
	if v.IsUndefined() {
		return mkundef(), rt.typeError("a unit is required")
	}
	return rt.temporalOptions(v)
}

// ---- the three ways to measure a duration ----

// roundDuration is Duration.prototype.round, which needs a relativeTo for any
// unit whose length is not fixed.
func (rt *Runtime) roundDuration(d durationRec, rel relativeToRec, s differenceSettings) (durationRec, *ThrowError) {
	switch rel.kind {
	case kindZonedDateTime:
		target, e := rt.addZonedDateTime(rel.epochNs, rel.timeZone, rel.calendar, d.toInternal(), "constrain")
		if e != nil {
			return durationRec{}, e
		}
		out, e := rt.differenceZonedDateTimeWithRounding(rel.epochNs, target, rel.timeZone,
			rel.calendar, s)
		return out, e
	case kindPlainDate:
		internal := d.toInternal24()
		days, t := balanceTime(internal.time)
		dd := adjustDays(internal.date, days.Int64())
		target, err := calendarDateAdd(rel.calendar, rel.date, dd, "constrain")
		if err != nil {
			return durationRec{}, rt.throwFor(err)
		}
		from := isoDateTimeRec{rel.date, midnightTime()}
		to := isoDateTimeRec{target, t}
		if e := rt.checkMeasurable(from, to); e != nil {
			return durationRec{}, e
		}
		return rt.differencePlainDateTimeWithRounding(from, to, rel.calendar, s)
	}
	if isCalendarUnit(defaultLargestUnit(d)) || isCalendarUnit(s.largest) || isCalendarUnit(s.smallest) {
		return durationRec{}, rt.rangeError("rounding years, months or weeks needs a relativeTo")
	}
	internal := d.toInternal24()
	rounded := roundNumberToIncrement(internal.time,
		incrementNs(s.smallest, s.increment), s.mode)
	out, ok := durationFromInternal(newInternal(internal.date, rounded), s.largest)
	if !ok {
		return out, rt.rangeError("duration is out of range")
	}
	return out, nil
}

// totalDuration is Duration.prototype.total: the exact number of one unit the
// duration comes to, fraction and all.
func (rt *Runtime) totalDuration(d durationRec, rel relativeToRec, unit int) (*big.Rat, *ThrowError) {
	switch rel.kind {
	case kindZonedDateTime:
		target, e := rt.addZonedDateTime(rel.epochNs, rel.timeZone, rel.calendar, d.toInternal(), "constrain")
		if e != nil {
			return nil, e
		}
		if unit > unitDay {
			// Nothing above a day was asked for, so the answer is simply how
			// much time passed -- which in a zone is not a count of days.
			return new(big.Rat).SetFrac(new(big.Int).Sub(target, rel.epochNs),
				bigInt(nsPerUnit[unit])), nil
		}
		diff, e := rt.differenceZonedDateTime(rel.epochNs, target, rel.timeZone, rel.calendar, unit)
		if e != nil {
			return nil, e
		}
		z, _ := temporalZoneFor(rel.timeZone)
		return rt.totalRelativeDuration(diff, rel.epochNs, target, z.dateTimeFor(rel.epochNs),
			rel.timeZone, rel.calendar, unit)
	case kindPlainDate:
		internal := d.toInternal24()
		days, t := balanceTime(internal.time)
		dd := adjustDays(internal.date, days.Int64())
		target, err := calendarDateAdd(rel.calendar, rel.date, dd, "constrain")
		if err != nil {
			return nil, rt.throwFor(err)
		}
		from := isoDateTimeRec{rel.date, midnightTime()}
		to := isoDateTimeRec{target, t}
		if e := rt.checkMeasurable(from, to); e != nil {
			return nil, e
		}
		diff := rt.differenceISODateTime(from, to, rel.calendar, unit)
		return rt.totalRelativeDuration(diff, nil, isoDateTimeToEpochNanoseconds(to, 0), from,
			"", rel.calendar, unit)
	}
	if isCalendarUnit(defaultLargestUnit(d)) || isCalendarUnit(unit) {
		return nil, rt.rangeError("totalling years, months or weeks needs a relativeTo")
	}
	internal := d.toInternal24()
	return new(big.Rat).SetFrac(internal.time, bigInt(nsPerUnit[unit])), nil
}

// totalRelativeDuration is the exact total of a duration measured against a
// starting point, which for a calendar unit means asking how far between two
// neighbouring months the end fell.
func (rt *Runtime) totalRelativeDuration(d internalDuration, originNs, destNs *big.Int,
	dt isoDateTimeRec, tz, calendar string, unit int) (*big.Rat, *ThrowError) {
	if isCalendarUnit(unit) || (tz != "" && unit == unitDay) {
		sign := 1
		if d.sign() < 0 {
			sign = -1
		}
		nr, e := rt.nudgeToCalendarUnit(sign, d, originNs, destNs, dt, tz, calendar, 1, unit, "trunc")
		if e != nil {
			return nil, e
		}
		return nr.total, nil
	}
	t := add24HourDays(d.time, d.date.days)
	return new(big.Rat).SetFrac(t, bigInt(nsPerUnit[unit])), nil
}

// durationTotalNs is what Duration.compare needs: both durations measured in
// nanoseconds from the same starting point.
func (rt *Runtime) durationTotalNs(d durationRec, rel relativeToRec) (*big.Int, *ThrowError) {
	switch rel.kind {
	case kindZonedDateTime:
		if d.dateSign() != 0 {
			target, e := rt.addZonedDateTime(rel.epochNs, rel.timeZone, rel.calendar, d.toInternal(), "constrain")
			if e != nil {
				return nil, e
			}
			return new(big.Int).Sub(target, rel.epochNs), nil
		}
	case kindPlainDate:
		if d.dateSign() != 0 {
			internal := d.toInternal24()
			days, t := balanceTime(internal.time)
			dd := adjustDays(internal.date, days.Int64())
			target, err := calendarDateAdd(rel.calendar, rel.date, dd, "constrain")
			if err != nil {
				return nil, rt.throwFor(err)
			}
			from := isoDateTimeRec{rel.date, midnightTime()}
			to := isoDateTimeRec{target, t}
			return new(big.Int).Sub(isoDateTimeToEpochNanoseconds(to, 0),
				isoDateTimeToEpochNanoseconds(from, 0)), nil
		}
	default:
		if d.years != 0 || d.months != 0 || d.weeks != 0 {
			return nil, rt.rangeError("comparing durations with years, months or weeks needs a relativeTo")
		}
	}
	return d.toInternal24().time, nil
}

// checkMeasurable reports whether a stretch between two local date-times can be
// measured at all. Measuring it means reading both ends as instants, and
// midnight on the earliest date there is falls a day before the earliest
// instant -- so a perfectly good PlainDate is not always a place to measure
// from. A stretch of no length is never measured, so it is never refused: that
// is the early return a zero duration takes.
func (rt *Runtime) checkMeasurable(from, to isoDateTimeRec) *ThrowError {
	if compareISODateTime(from, to) == 0 {
		return nil
	}
	if !isoDateTimeWithinLimits(from) || !isoDateTimeWithinLimits(to) {
		return rt.rangeError("date is outside the representable range")
	}
	return nil
}
