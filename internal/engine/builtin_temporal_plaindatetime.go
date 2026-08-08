package engine

// Temporal.PlainDateTime: a date and a time with no zone, so it names a
// reading rather than an instant.

func (rt *Runtime) initTemporalPlainDateTime(ns *object) {
	co, po := rt.defineTemporalCtor(ns, kindPlainDateTime, 3,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if e := rt.requireNew("PlainDateTime"); e != nil {
				return mkundef(), e
			}
			var f [9]int
			for i := 0; i < 9; i++ {
				v := arg(args, i)
				if v.IsUndefined() {
					if i == 1 || i == 2 {
						f[i] = 1
					}
					continue
				}
				n, e := rt.toIntegerWithTruncation(v)
				if e != nil {
					return mkundef(), e
				}
				f[i] = n
			}
			cal, e := rt.calendarArg(arg(args, 9))
			if e != nil {
				return mkundef(), e
			}
			if !isValidISODate(f[0], f[1], f[2]) {
				return mkundef(), rt.rangeError("date is out of range")
			}
			if !isValidTime(f[3], f[4], f[5], f[6], f[7], f[8]) {
				return mkundef(), rt.rangeError("time is out of range")
			}
			return rt.createTemporalDateTime(isoDateTimeRec{
				isoDateRec{f[0], f[1], f[2]},
				isoTimeRec{f[3], f[4], f[5], f[6], f[7], f[8]}}, cal)
		})

	co.defineOwn("from", rt.newNativeFunc("from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		dt, cal, e := rt.toTemporalDateTime(arg(args, 0), arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalDateTime(dt, cal)
	}), attrWritable|attrConfigurable)

	co.defineOwn("compare", rt.newNativeFunc("compare", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		a, _, e := rt.toTemporalDateTime(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		b, _, e := rt.toTemporalDateTime(arg(args, 1), mkundef())
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(compareISODateTime(a, b))), nil
	}), attrWritable|attrConfigurable)

	rt.defCalendarGetters(po, kindPlainDateTime,
		getYear|getMonth|getDay|getDayOfThings|getYearThings|getMonthThings,
		func(o *object) (isoDateRec, string) { return tDateTimeDate(o), rt.tCalendar(o) })
	rt.defTimeGetters(po, kindPlainDateTime, tDateTimeTime)

	self := func(this Value) (*object, isoDateTimeRec, string, *ThrowError) {
		o, e := rt.requireTemporal(this, kindPlainDateTime)
		if e != nil {
			return nil, isoDateTimeRec{}, "", e
		}
		return o, tDateTime(o), rt.tCalendar(o), nil
	}

	addSub := func(subtract bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			_, dt, cal, e := self(this)
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
			out, e := rt.addDateTime(dt, cal, d.toInternal(), overflow)
			if e != nil {
				return mkundef(), e
			}
			return rt.createTemporalDateTime(out, cal)
		}
	}
	rt.defMethod(po, "add", 1, addSub(false))
	rt.defMethod(po, "subtract", 1, addSub(true))

	rt.defMethod(po, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, dt, cal, e := self(this)
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
		f, e := rt.readCalendarFields(item, calendarDateKeys(cal, keysDate)|keysTime, 0, true)
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
		merged := mergeCalendarFields(fieldsOfDate(cal, dt.date), f.cal)
		iso, err := calendarDateFromFields(cal, merged, overflow)
		if err != nil {
			return mkundef(), rt.throwFor(err)
		}
		t, e := rt.regulateFieldsTime(temporalFields{time: mergeTime(dt.time, f)}, overflow)
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalDateTime(isoDateTimeRec{iso, t}, cal)
	})

	rt.defMethod(po, "withPlainTime", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, dt, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		t := midnightTime()
		if v := arg(args, 0); !v.IsUndefined() {
			got, e := rt.toTemporalTime(v, mkundef())
			if e != nil {
				return mkundef(), e
			}
			t = got
		}
		return rt.createTemporalDateTime(isoDateTimeRec{dt.date, t}, cal)
	})

	rt.defMethod(po, "withCalendar", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, dt, _, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		cal, e := rt.toCalendarID(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalDateTime(dt, cal)
	})

	diff := func(since bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			_, dt, cal, e := self(this)
			if e != nil {
				return mkundef(), e
			}
			other, otherCal, e := rt.toTemporalDateTime(arg(args, 0), mkundef())
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
				unitNanosecond, unitDay)
			if e != nil {
				return mkundef(), e
			}
			a, b := dt, other
			out, e := rt.differencePlainDateTimeWithRounding(a, b, cal, s)
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
		_, dt, cal, e := self(this)
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
		days, t := roundTimeRec(dt.time, increment, unit, mode)
		out := isoDateTimeRec{epochDaysToISODate(dt.date.epochDays() + int(days)), t}
		return rt.createTemporalDateTime(out, cal)
	})

	rt.defMethod(po, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, dt, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		other, otherCal, e := rt.toTemporalDateTime(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		return mkbool(compareISODateTime(dt, other) == 0 && cal == otherCal), nil
	})

	rt.defMethod(po, "toPlainDate", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, dt, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalDate(dt.date, cal)
	})

	rt.defMethod(po, "toPlainTime", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, dt, _, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalTime(dt.time)
	})

	rt.defMethod(po, "toZonedDateTime", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, dt, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		tz, e := rt.toTimeZoneID(arg(args, 0))
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
		z, _ := temporalZoneFor(tz)
		epoch, ok := z.disambiguate(dt, disambiguation)
		if !ok {
			return mkundef(), rt.rangeError("this local time does not exist in " + tz)
		}
		return rt.createTemporalZonedDateTime(epoch, tz, cal)
	})

	rt.defMethod(po, "toString", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, dt, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		opts, e := rt.temporalOptions(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		show, e := rt.temporalStringOption(opts, "calendarName", showCalendarValues, "auto")
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
		days, t := roundTimeRec(dt.time, increment, unit, mode)
		out := isoDateTimeRec{epochDaysToISODate(dt.date.epochDays() + int(days)), t}
		if !isoDateTimeWithinLimits(out) {
			return mkundef(), rt.rangeError("rounding took the date-time out of range")
		}
		return rt.newString(formatISODateTime(out, precision) +
			formatCalendarAnnotation(cal, show)), nil
	})

	toJSON := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, dt, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(formatISODateTime(dt, -1) +
			formatCalendarAnnotation(cal, "auto")), nil
	}
	rt.defMethod(po, "toJSON", 0, toJSON)
	rt.defMethod(po, "toLocaleString", 0, toJSON)
}

// defTimeGetters installs hour through nanosecond for the types that carry a
// time.
func (rt *Runtime) defTimeGetters(po *object, kind temporalKind, get func(o *object) isoTimeRec) {
	getter := func(name string, f func(t isoTimeRec) int) {
		g := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, e := rt.requireTemporal(this, kind)
			if e != nil {
				return mkundef(), e
			}
			return mknum(float64(f(get(o)))), nil
		})
		po.defineAccessor(name, g, mkundef(), true, false, attrConfigurable)
	}
	getter("hour", func(t isoTimeRec) int { return t.hour })
	getter("minute", func(t isoTimeRec) int { return t.minute })
	getter("second", func(t isoTimeRec) int { return t.second })
	getter("millisecond", func(t isoTimeRec) int { return t.ms })
	getter("microsecond", func(t isoTimeRec) int { return t.us })
	getter("nanosecond", func(t isoTimeRec) int { return t.ns })
}
