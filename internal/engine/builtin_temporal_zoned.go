package engine

// Temporal.ZonedDateTime: an instant with a place to read it in. It is the only
// Temporal type that is both -- which is why it is the only one where a day can
// be twenty-three hours long.

import (
	"math/big"
	"time"
)

func (rt *Runtime) initTemporalZonedDateTime(ns *object) {
	co, po := rt.defineTemporalCtor(ns, kindZonedDateTime, 2,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if e := rt.requireNew("ZonedDateTime"); e != nil {
				return mkundef(), e
			}
			epoch, e := rt.toBigIntValue(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			tzv := arg(args, 1)
			if !tzv.IsString() {
				return mkundef(), rt.typeError("time zone must be a string")
			}
			z, ok := temporalZoneFor(rt.strGo(tzv))
			if !ok {
				return mkundef(), rt.rangeError("invalid time zone identifier: " + rt.strGo(tzv))
			}
			cal, e := rt.calendarArg(arg(args, 2))
			if e != nil {
				return mkundef(), e
			}
			return rt.createTemporalZonedDateTime(epoch, z.id, cal)
		})

	co.defineOwn("from", rt.newNativeFunc("from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		epoch, tz, cal, e := rt.toTemporalZonedDateTime(arg(args, 0), arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalZonedDateTime(epoch, tz, cal)
	}), attrWritable|attrConfigurable)

	co.defineOwn("compare", rt.newNativeFunc("compare", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		a, _, _, e := rt.toTemporalZonedDateTime(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		b, _, _, e := rt.toTemporalZonedDateTime(arg(args, 1), mkundef())
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(a.Cmp(b))), nil
	}), attrWritable|attrConfigurable)

	local := func(o *object) isoDateTimeRec {
		z, _ := temporalZoneFor(rt.tTimeZone(o))
		return z.dateTimeFor(tEpochNs(o))
	}
	rt.defCalendarGetters(po, kindZonedDateTime,
		getYear|getMonth|getDay|getDayOfThings|getYearThings|getMonthThings,
		func(o *object) (isoDateRec, string) { return local(o).date, rt.tCalendar(o) })
	rt.defTimeGetters(po, kindZonedDateTime, func(o *object) isoTimeRec { return local(o).time })

	getter := func(name string, f func(o *object) (Value, *ThrowError)) {
		g := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, e := rt.requireTemporal(this, kindZonedDateTime)
			if e != nil {
				return mkundef(), e
			}
			return f(o)
		})
		po.defineAccessor(name, g, mkundef(), true, false, attrConfigurable)
	}
	getter("timeZoneId", func(o *object) (Value, *ThrowError) {
		return rt.newString(rt.tTimeZone(o)), nil
	})
	getter("epochMilliseconds", func(o *object) (Value, *ThrowError) {
		q := new(big.Int).Div(tEpochNs(o), bigInt(nsPerMilli))
		f, _ := new(big.Float).SetInt(q).Float64()
		return mknum(f), nil
	})
	getter("epochNanoseconds", func(o *object) (Value, *ThrowError) {
		return rt.newBigInt(tEpochNs(o)), nil
	})
	getter("offsetNanoseconds", func(o *object) (Value, *ThrowError) {
		z, _ := temporalZoneFor(rt.tTimeZone(o))
		return mknum(float64(z.offsetNs(tEpochNs(o)))), nil
	})
	getter("offset", func(o *object) (Value, *ThrowError) {
		z, _ := temporalZoneFor(rt.tTimeZone(o))
		return rt.newString(formatOffsetNanoseconds(z.offsetNs(tEpochNs(o)))), nil
	})
	getter("hoursInDay", func(o *object) (Value, *ThrowError) {
		z, _ := temporalZoneFor(rt.tTimeZone(o))
		d := local(o).date
		start, ok := z.startOfDay(d)
		if !ok {
			return mkundef(), rt.rangeError("this day does not start in " + z.id)
		}
		end, ok := z.startOfDay(epochDaysToISODate(d.epochDays() + 1))
		if !ok {
			return mkundef(), rt.rangeError("the next day does not start in " + z.id)
		}
		span := new(big.Rat).SetFrac(new(big.Int).Sub(end, start), bigInt(nsPerHour))
		f, _ := span.Float64()
		return mknum(f), nil
	})

	self := func(this Value) (*object, *big.Int, string, string, *ThrowError) {
		o, e := rt.requireTemporal(this, kindZonedDateTime)
		if e != nil {
			return nil, nil, "", "", e
		}
		return o, tEpochNs(o), rt.tTimeZone(o), rt.tCalendar(o), nil
	}

	addSub := func(subtract bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			_, epoch, tz, cal, e := self(this)
			if e != nil {
				return mkundef(), e
			}
			d, e := rt.toTemporalDuration(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			if subtract {
				d = negateDuration(d)
			}
			opts, e := rt.temporalOptions(arg(args, 1))
			if e != nil {
				return mkundef(), e
			}
			overflow, e := rt.getOverflow(opts)
			if e != nil {
				return mkundef(), e
			}
			out, e := rt.addZonedDateTime(epoch, tz, cal, d.toInternal(), overflow)
			if e != nil {
				return mkundef(), e
			}
			return rt.createTemporalZonedDateTime(out, tz, cal)
		}
	}
	rt.defMethod(po, "add", 1, addSub(false))
	rt.defMethod(po, "subtract", 1, addSub(true))

	rt.defMethod(po, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, epoch, tz, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		item := arg(args, 0)
		if !item.IsObjectType() || rt.objPtr(item) == nil {
			return mkundef(), rt.typeError("with() takes a bag of fields")
		}
		if rt.temporalKindOf(item) != kindNone {
			return mkundef(), rt.typeError("with() does not take a Temporal object")
		}
		if e := rt.rejectCalendarOrTimeZone(item); e != nil {
			return mkundef(), e
		}
		f, e := rt.readCalendarFields(item,
			calendarDateKeys(cal, keysDate)|keysTime|keyOffset, 0, true)
		if e != nil {
			return mkundef(), e
		}
		opts, e := rt.temporalOptions(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		disambiguation, e := rt.getDisambiguation(opts)
		if e != nil {
			return mkundef(), e
		}
		offsetOpt, e := rt.getOffsetOption(opts, "prefer")
		if e != nil {
			return mkundef(), e
		}
		overflow, e := rt.getOverflow(opts)
		if e != nil {
			return mkundef(), e
		}
		z, _ := temporalZoneFor(tz)
		dt := z.dateTimeFor(epoch)
		merged := mergeCalendarFields(fieldsOfDate(cal, dt.date), f.cal)
		iso, err := calendarDateFromFields(cal, merged, overflow)
		if err != nil {
			return mkundef(), rt.throwFor(err)
		}
		t, e := rt.regulateFieldsTime(temporalFields{time: mergeTime(dt.time, f)}, overflow)
		if e != nil {
			return mkundef(), e
		}
		offsetNs := z.offsetNs(epoch)
		if f.hasOffset {
			got, ok := parseDateTimeUTCOffset(f.offset)
			if !ok {
				return mkundef(), rt.rangeError("invalid offset: " + f.offset)
			}
			offsetNs = got
		}
		_ = o
		out, e := rt.interpretISODateTimeOffset(isoDateTimeRec{iso, t}, true, offsetNs,
			tz, disambiguation, offsetOpt, false)
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalZonedDateTime(out, tz, cal)
	})

	rt.defMethod(po, "withPlainTime", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, tz, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		z, _ := temporalZoneFor(tz)
		d := z.dateTimeFor(epoch).date
		if arg(args, 0).IsUndefined() {
			start, ok := z.startOfDay(d)
			if !ok {
				return mkundef(), rt.rangeError("this day does not start in " + tz)
			}
			return rt.createTemporalZonedDateTime(start, tz, cal)
		}
		t, e := rt.toTemporalTime(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		out, ok := z.disambiguate(isoDateTimeRec{d, t}, "compatible")
		if !ok {
			return mkundef(), rt.rangeError("this local time does not exist in " + tz)
		}
		return rt.createTemporalZonedDateTime(out, tz, cal)
	})

	rt.defMethod(po, "withTimeZone", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, _, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		tz, e := rt.toTimeZoneID(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalZonedDateTime(epoch, tz, cal)
	})

	rt.defMethod(po, "withCalendar", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, tz, _, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		cal, e := rt.toCalendarID(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalZonedDateTime(epoch, tz, cal)
	})

	diff := func(since bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			_, epoch, tz, cal, e := self(this)
			if e != nil {
				return mkundef(), e
			}
			otherNs, otherTz, otherCal, e := rt.toTemporalZonedDateTime(arg(args, 0), mkundef())
			if e != nil {
				return mkundef(), e
			}
			if otherCal != cal {
				return mkundef(), rt.rangeError("cannot measure between two calendars")
			}
			opts, e := rt.temporalOptions(arg(args, 1))
			if e != nil {
				return mkundef(), e
			}
			s, e := rt.differenceSettings(opts, since, unitYear, unitNanosecond,
				unitNanosecond, unitHour)
			if e != nil {
				return mkundef(), e
			}
			a, b := epoch, otherNs
			var out durationRec
			if s.largest > unitDay {
				out, e = rt.differenceInstant(a, b, s)
			} else {
				if !timeZoneEquals(tz, otherTz) {
					return mkundef(), rt.rangeError("cannot measure days or more between two time zones")
				}
				out, e = rt.differenceZonedDateTimeWithRounding(a, b, tz, cal, s)
			}
			if e != nil {
				return mkundef(), e
			}
			if since {
				out = negateDuration(out)
			}
			return rt.createTemporalDuration(out)
		}
	}
	rt.defMethod(po, "until", 1, diff(false))
	rt.defMethod(po, "since", 1, diff(true))

	rt.defMethod(po, "round", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, tz, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		roundTo, e := rt.unitOrOptions(arg(args, 0), "smallestUnit")
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
		unit, e := rt.getTemporalUnit(roundTo, "smallestUnit", unitDay, unitNanosecond, unitAuto-1)
		if e != nil {
			return mkundef(), e
		}
		if unit == unitAuto-1 {
			return mkundef(), rt.rangeError("round() needs a smallestUnit")
		}
		if unit == unitDay {
			if increment != 1 {
				return mkundef(), rt.rangeError("rounding to days takes an increment of 1")
			}
		} else {
			max, _ := maximumRoundingIncrement(unit)
			if e := rt.validateRoundingIncrement(increment, max, false); e != nil {
				return mkundef(), e
			}
		}
		z, _ := temporalZoneFor(tz)
		dt := z.dateTimeFor(epoch)
		if unit == unitDay {
			start, ok := z.startOfDay(dt.date)
			if !ok {
				return mkundef(), rt.rangeError("this day does not start in " + tz)
			}
			end, ok := z.startOfDay(epochDaysToISODate(dt.date.epochDays() + 1))
			if !ok {
				return mkundef(), rt.rangeError("the next day does not start in " + tz)
			}
			length := new(big.Int).Sub(end, start)
			if length.Sign() == 0 {
				return mkundef(), rt.rangeError("this day is of no length in " + tz)
			}
			progress := new(big.Int).Sub(epoch, start)
			rounded := roundNumberToIncrement(progress, length, mode)
			return rt.createTemporalZonedDateTime(new(big.Int).Add(start, rounded), tz, cal)
		}
		days, t := roundTimeRec(dt.time, increment, unit, mode)
		rounded := isoDateTimeRec{epochDaysToISODate(dt.date.epochDays() + int(days)), t}
		out, e := rt.interpretISODateTimeOffset(rounded, true, z.offsetNs(epoch), tz,
			"compatible", "prefer", false)
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalZonedDateTime(out, tz, cal)
	})

	rt.defMethod(po, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, tz, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		otherNs, otherTz, otherCal, e := rt.toTemporalZonedDateTime(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		return mkbool(epoch.Cmp(otherNs) == 0 && cal == otherCal &&
			timeZoneEquals(tz, otherTz)), nil
	})

	rt.defMethod(po, "startOfDay", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, tz, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		z, _ := temporalZoneFor(tz)
		start, ok := z.startOfDay(z.dateTimeFor(epoch).date)
		if !ok {
			return mkundef(), rt.rangeError("this day does not start in " + tz)
		}
		return rt.createTemporalZonedDateTime(start, tz, cal)
	})

	rt.defMethod(po, "getTimeZoneTransition", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, tz, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		opts, e := rt.unitOrOptions(arg(args, 0), "direction")
		if e != nil {
			return mkundef(), e
		}
		dir, e := rt.temporalStringOption(opts, "direction", []string{"next", "previous"}, "")
		if e != nil {
			return mkundef(), e
		}
		if dir == "" {
			return mkundef(), rt.rangeError("getTimeZoneTransition() needs a direction")
		}
		z, _ := temporalZoneFor(tz)
		at, ok := z.transition(epoch, dir == "next")
		if !ok {
			return mknull(), nil
		}
		return rt.createTemporalZonedDateTime(at, tz, cal)
	})

	rt.defMethod(po, "toInstant", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, _, _, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalInstant(epoch)
	})
	rt.defMethod(po, "toPlainDate", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, tz, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		z, _ := temporalZoneFor(tz)
		return rt.createTemporalDate(z.dateTimeFor(epoch).date, cal)
	})
	rt.defMethod(po, "toPlainTime", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, tz, _, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		z, _ := temporalZoneFor(tz)
		return rt.createTemporalTime(z.dateTimeFor(epoch).time)
	})
	rt.defMethod(po, "toPlainDateTime", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, tz, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		z, _ := temporalZoneFor(tz)
		return rt.createTemporalDateTime(z.dateTimeFor(epoch), cal)
	})

	rt.defMethod(po, "toString", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, tz, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		opts, e := rt.temporalOptions(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		showCal, e := rt.temporalStringOption(opts, "calendarName", showCalendarValues, "auto")
		if e != nil {
			return mkundef(), e
		}
		digits, e := rt.getFractionalSecondDigits(opts)
		if e != nil {
			return mkundef(), e
		}
		showOffset, e := rt.temporalStringOption(opts, "offset", showOffsetValues, "auto")
		if e != nil {
			return mkundef(), e
		}
		mode, e := rt.getRoundingMode(opts, "trunc")
		if e != nil {
			return mkundef(), e
		}
		showZone, e := rt.temporalStringOption(opts, "timeZoneName", showTimeZoneValues, "auto")
		if e != nil {
			return mkundef(), e
		}
		smallest, e := rt.getTemporalUnit(opts, "smallestUnit", unitMinute, unitNanosecond, unitAuto-1)
		if e != nil {
			return mkundef(), e
		}
		if smallest == unitHour {
			return mkundef(), rt.rangeError("smallestUnit cannot be hour here")
		}
		precision, unit, increment, e := rt.secondsStringPrecision(smallest, digits)
		if e != nil {
			return mkundef(), e
		}
		ns := roundNumberToIncrement(epoch, bigInt(nsPerUnit[unit]*increment), mode)
		if !epochNsWithinLimits(ns) {
			return mkundef(), rt.rangeError("rounding took the date-time out of range")
		}
		return rt.newString(rt.formatZonedDateTime(ns, tz, cal, precision, showCal,
			showZone, showOffset)), nil
	})

	toJSON := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, epoch, tz, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(rt.formatZonedDateTime(epoch, tz, cal, -1, "auto", "auto", "auto")), nil
	}
	rt.defMethod(po, "toJSON", 0, toJSON)
	rt.defMethod(po, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if _, e := rt.requireTemporal(this, kindZonedDateTime); e != nil {
			return mkundef(), e
		}
		return rt.temporalLocaleString(this, kindZonedDateTime, arg(args, 0), arg(args, 1))
	})
}

func (rt *Runtime) formatZonedDateTime(ns *big.Int, tz, cal string, precision int,
	showCal, showZone, showOffset string) string {
	z, _ := temporalZoneFor(tz)
	off := z.offsetNs(ns)
	s := formatISODateTime(getISODateTimeFor(off, ns), precision)
	if showOffset != "never" {
		s += formatOffsetNanosecondsRounded(off)
	}
	switch showZone {
	case "never":
	case "critical":
		s += "[!" + tz + "]"
	default:
		s += "[" + tz + "]"
	}
	return s + formatCalendarAnnotation(cal, showCal)
}

// timeZoneEquals compares two identifiers, which are the same zone when they
// spell the same name or when one is a link to the other.
func timeZoneEquals(a, b string) bool {
	if a == b {
		return true
	}
	pa, oka := primaryTimeZone(a)
	pb, okb := primaryTimeZone(b)
	return oka && okb && pa == pb
}

// transition is the next or previous instant at which this zone's offset
// changed. Go keeps the bounds of the period an instant falls in, which is
// exactly the pair of transitions either side of it.
// A zone changes what it calls itself more often than it changes its offset:
// Europe/London stopped calling its clocks "summer time" in 1968 without moving
// them, and Europe/Paris renamed its local mean time in 1891. Go reports both as
// bounds, so a bound that leaves the offset alone is stepped over rather than
// returned. The count is a backstop; no zone has anything like that many names
// for one offset.
const zoneBoundScanLimit = 100

func (z temporalZone) transition(epochNs *big.Int, next bool) (*big.Int, bool) {
	if z.fixed {
		return nil, false
	}
	// changesOffsetAt reports whether the zone reads differently on the two
	// sides of an instant, which is what makes a bound a transition.
	changesOffsetAt := func(at *big.Int) bool {
		return z.offsetNs(at) != z.offsetNs(new(big.Int).Sub(at, bigInt(1)))
	}
	at := new(big.Int).Set(epochNs)
	for i := 0; i < zoneBoundScanLimit; i++ {
		if !at.IsInt64() && at.Cmp(nsMinInstant) < 0 || at.Cmp(nsMaxInstant) > 0 {
			return nil, false
		}
		sec := new(big.Int).Div(at, bigInt(nsPerSecond))
		rem := new(big.Int).Mod(at, bigInt(nsPerSecond))
		if !sec.IsInt64() {
			return nil, false
		}
		start, end := time.Unix(sec.Int64(), rem.Int64()).In(z.loc).ZoneBounds()
		bound := start
		if next {
			bound = end
		}
		if bound.IsZero() {
			return nil, false
		}
		ns := new(big.Int).Mul(bigInt(bound.Unix()), bigInt(nsPerSecond))
		ns.Add(ns, bigInt(int64(bound.Nanosecond())))
		if next {
			if ns.Cmp(nsMaxInstant) > 0 {
				return nil, false
			}
			if changesOffsetAt(ns) {
				return ns, true
			}
			at = ns
			continue
		}
		if ns.Cmp(at) >= 0 {
			// The instant sits exactly on a bound, so the one being asked for
			// is whatever comes before this period begins.
			at = new(big.Int).Sub(ns, bigInt(1))
			continue
		}
		if ns.Cmp(nsMinInstant) < 0 {
			return nil, false
		}
		if changesOffsetAt(ns) {
			return ns, true
		}
		at = new(big.Int).Sub(ns, bigInt(1))
	}
	return nil, false
}
