package engine

// Temporal.PlainTime: a time of day with no date under it.

func (rt *Runtime) initTemporalPlainTime(ns *object) {
	co, po := rt.defineTemporalCtor(ns, kindPlainTime, 0,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if e := rt.requireNew("PlainTime"); e != nil {
				return mkundef(), e
			}
			var f [6]int
			for i := 0; i < 6; i++ {
				v := arg(args, i)
				if v.IsUndefined() {
					continue
				}
				n, e := rt.toIntegerWithTruncation(v)
				if e != nil {
					return mkundef(), e
				}
				f[i] = n
			}
			if !isValidTime(f[0], f[1], f[2], f[3], f[4], f[5]) {
				return mkundef(), rt.rangeError("time is out of range")
			}
			return rt.createTemporalTime(isoTimeRec{f[0], f[1], f[2], f[3], f[4], f[5]})
		})

	co.defineOwn("from", rt.newNativeFunc("from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		t, e := rt.toTemporalTime(arg(args, 0), arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalTime(t)
	}), attrWritable|attrConfigurable)

	co.defineOwn("compare", rt.newNativeFunc("compare", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		a, e := rt.toTemporalTime(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		b, e := rt.toTemporalTime(arg(args, 1), mkundef())
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(compareTimeRec(a, b))), nil
	}), attrWritable|attrConfigurable)

	getter := func(name string, f func(t isoTimeRec) Value) {
		g := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, e := rt.requireTemporal(this, kindPlainTime)
			if e != nil {
				return mkundef(), e
			}
			return f(tTime(o)), nil
		})
		po.defineAccessor(name, g, mkundef(), true, false, attrConfigurable)
	}
	getter("hour", func(t isoTimeRec) Value { return mknum(float64(t.hour)) })
	getter("minute", func(t isoTimeRec) Value { return mknum(float64(t.minute)) })
	getter("second", func(t isoTimeRec) Value { return mknum(float64(t.second)) })
	getter("millisecond", func(t isoTimeRec) Value { return mknum(float64(t.ms)) })
	getter("microsecond", func(t isoTimeRec) Value { return mknum(float64(t.us)) })
	getter("nanosecond", func(t isoTimeRec) Value { return mknum(float64(t.ns)) })

	addSub := func(subtract bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, e := rt.requireTemporal(this, kindPlainTime)
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
			_, t := addTimeToTime(tTime(o), d.toInternal24().time)
			return rt.createTemporalTime(t)
		}
	}
	rt.defMethod(po, "add", 1, addSub(false))
	rt.defMethod(po, "subtract", 1, addSub(true))

	rt.defMethod(po, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindPlainTime)
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
		f, e := rt.readCalendarFields(item, keysTime, 0, true)
		if e != nil {
			return mkundef(), e
		}
		opts, e := rt.temporalOptions(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		overflow, e := rt.getOverflow(opts)
		if e != nil {
			return mkundef(), e
		}
		t := mergeTime(tTime(o), f)
		out, e := rt.regulateFieldsTime(temporalFields{time: t}, overflow)
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalTime(out)
	})

	diff := func(since bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, e := rt.requireTemporal(this, kindPlainTime)
			if e != nil {
				return mkundef(), e
			}
			other, e := rt.toTemporalTime(arg(args, 0), mkundef())
			if e != nil {
				return mkundef(), e
			}
			opts, e := rt.temporalOptions(arg(args, 1))
			if e != nil {
				return mkundef(), e
			}
			s, e := rt.differenceSettings(opts, since, unitHour, unitNanosecond,
				unitNanosecond, unitHour)
			if e != nil {
				return mkundef(), e
			}
			a, b := tTime(o), other
			t := differenceTime(a, b)
			t = roundNumberToIncrement(t, bigInt(nsPerUnit[s.smallest]*s.increment), s.mode)
			out, ok := durationFromInternal(newInternal(dateDuration{}, t), s.largest)
			if !ok {
				return mkundef(), rt.rangeError("duration is out of range")
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
		o, e := rt.requireTemporal(this, kindPlainTime)
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
		unit, e := rt.getTemporalUnit(roundTo, "smallestUnit", unitHour, unitNanosecond, unitAuto-1)
		if e != nil {
			return mkundef(), e
		}
		if unit == unitAuto-1 {
			return mkundef(), rt.rangeError("round() needs a smallestUnit")
		}
		max, _ := maximumRoundingIncrement(unit)
		if e := rt.validateRoundingIncrement(increment, max, false); e != nil {
			return mkundef(), e
		}
		_, t := roundTimeRec(tTime(o), increment, unit, mode)
		return rt.createTemporalTime(t)
	})

	rt.defMethod(po, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindPlainTime)
		if e != nil {
			return mkundef(), e
		}
		other, e := rt.toTemporalTime(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		return mkbool(compareTimeRec(tTime(o), other) == 0), nil
	})

	rt.defMethod(po, "toString", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindPlainTime)
		if e != nil {
			return mkundef(), e
		}
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
		_, t := roundTimeRec(tTime(o), increment, unit, mode)
		return rt.newString(formatTimeString(t, precision)), nil
	})

	toJSON := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindPlainTime)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(formatTimeString(tTime(o), -1)), nil
	}
	rt.defMethod(po, "toJSON", 0, toJSON)
	rt.defMethod(po, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if _, e := rt.requireTemporal(this, kindPlainTime); e != nil {
			return mkundef(), e
		}
		return rt.temporalLocaleString(this, kindPlainTime, arg(args, 0), arg(args, 1))
	})
}

// mergeTime overlays whichever time fields were given onto an existing time.
func mergeTime(base isoTimeRec, f temporalFields) isoTimeRec {
	if f.hasTime&keyHour != 0 {
		base.hour = f.time.hour
	}
	if f.hasTime&keyMinute != 0 {
		base.minute = f.time.minute
	}
	if f.hasTime&keySecond != 0 {
		base.second = f.time.second
	}
	if f.hasTime&keyMillisecond != 0 {
		base.ms = f.time.ms
	}
	if f.hasTime&keyMicrosecond != 0 {
		base.us = f.time.us
	}
	if f.hasTime&keyNanosecond != 0 {
		base.ns = f.time.ns
	}
	return base
}

// rejectCalendarOrTimeZone is the guard on every with(): those two are changed
// with withCalendar and withTimeZone, and passing them here is a mistake worth
// reporting rather than ignoring.
func (rt *Runtime) rejectCalendarOrTimeZone(item Value) *ThrowError {
	for _, name := range []string{"calendar", "timeZone"} {
		v, e := rt.getField(item, name)
		if e != nil {
			return e
		}
		if !v.IsUndefined() {
			return rt.typeError("with() does not take a " + name)
		}
	}
	return nil
}
