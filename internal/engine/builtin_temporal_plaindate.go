package engine

// Temporal.PlainDate, and the calendar-facing getters that four of the types
// share.

// The groups of calendar getters. A year-month has no day and a month-day has
// no year, so each type installs the ones that mean something for it.
const (
	getYear = 1 << iota
	getMonth
	getDay
	getDayOfThings
	getYearThings
	getMonthThings
)

// defCalendarGetters installs the fields a calendar can answer about a date.
// get pulls the ISO date and the calendar identifier off the receiver, which is
// the only thing the five types differ by here.
func (rt *Runtime) defCalendarGetters(po *object, kind temporalKind, which int,
	get func(o *object) (isoDateRec, string)) {
	getter := func(name string, f func(v calendarView, iso isoDateRec, cal string) Value) {
		g := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, e := rt.requireTemporal(this, kind)
			if e != nil {
				return mkundef(), e
			}
			iso, cal := get(o)
			return f(calendarViewOf(cal, iso), iso, cal), nil
		})
		po.defineAccessor(name, g, mkundef(), true, false, attrConfigurable)
	}
	getter("calendarId", func(v calendarView, iso isoDateRec, cal string) Value {
		return rt.newString(cal)
	})
	if which&getYear != 0 {
		getter("era", func(v calendarView, _ isoDateRec, _ string) Value {
			if !v.hasEra {
				return mkundef()
			}
			return rt.newString(v.era)
		})
		getter("eraYear", func(v calendarView, _ isoDateRec, _ string) Value {
			if !v.hasEra {
				return mkundef()
			}
			return mknum(float64(v.eraYear))
		})
		getter("year", func(v calendarView, _ isoDateRec, _ string) Value {
			return mknum(float64(v.year))
		})
	}
	if which&getMonth != 0 {
		getter("month", func(v calendarView, _ isoDateRec, _ string) Value {
			return mknum(float64(v.month))
		})
	}
	getter("monthCode", func(v calendarView, _ isoDateRec, _ string) Value {
		return rt.newString(v.monthCode)
	})
	if which&getDay != 0 {
		getter("day", func(v calendarView, _ isoDateRec, _ string) Value {
			return mknum(float64(v.day))
		})
	}
	if which&getDayOfThings != 0 {
		getter("dayOfWeek", func(v calendarView, _ isoDateRec, _ string) Value {
			return mknum(float64(v.dayOfWeek))
		})
		getter("dayOfYear", func(v calendarView, _ isoDateRec, _ string) Value {
			return mknum(float64(v.dayOfYear))
		})
		getter("weekOfYear", func(v calendarView, iso isoDateRec, cal string) Value {
			if cal != "iso8601" {
				return mkundef()
			}
			w, _ := isoWeekOfYear(iso)
			return mknum(float64(w))
		})
		getter("yearOfWeek", func(v calendarView, iso isoDateRec, cal string) Value {
			if cal != "iso8601" {
				return mkundef()
			}
			_, y := isoWeekOfYear(iso)
			return mknum(float64(y))
		})
		getter("daysInWeek", func(v calendarView, _ isoDateRec, _ string) Value {
			return mknum(7)
		})
	}
	if which&getMonthThings != 0 {
		getter("daysInMonth", func(v calendarView, _ isoDateRec, _ string) Value {
			return mknum(float64(v.daysInMonth))
		})
	}
	if which&getYearThings != 0 {
		getter("daysInYear", func(v calendarView, _ isoDateRec, _ string) Value {
			return mknum(float64(v.daysInYear))
		})
		getter("monthsInYear", func(v calendarView, _ isoDateRec, _ string) Value {
			return mknum(float64(v.monthsInYear))
		})
		getter("inLeapYear", func(v calendarView, _ isoDateRec, _ string) Value {
			return mkbool(v.inLeapYear)
		})
	}
}

// mergeCalendarFields overlays the fields a caller gave onto the ones the date
// already had. Giving one of a pair drops the other: a month number replaces a
// month code, and a year replaces an era and its year.
func mergeCalendarFields(existing, given calFieldSet) calFieldSet {
	out := given
	if !given.got(fYear) && !given.got(fEra) && !given.got(fEraYear) {
		out.year = existing.year
		out.has |= fYear
		if existing.got(fEra) {
			out.era, out.eraYear = existing.era, existing.eraYear
			out.has |= fEra | fEraYear
		}
	}
	if !given.got(fMonth) && !given.got(fMonthCode) {
		out.monthCode = existing.monthCode
		out.has |= fMonthCode
	}
	if !given.got(fDay) {
		out.day = existing.day
		out.has |= fDay
	}
	return out
}

// fieldsOfDate is the date as a bag of fields, which is what merging starts
// from.
func fieldsOfDate(cal string, iso isoDateRec) calFieldSet {
	v := calendarViewOf(cal, iso)
	f := calFieldSet{year: v.year, month: v.month, monthCode: v.monthCode,
		day: v.day, has: fYear | fMonth | fMonthCode | fDay}
	if v.hasEra {
		f.era, f.eraYear = v.era, v.eraYear
		f.has |= fEra | fEraYear
	}
	return f
}

func (rt *Runtime) initTemporalPlainDate(ns *object) {
	co, po := rt.defineTemporalCtor(ns, kindPlainDate, 3,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if e := rt.requireNew("PlainDate"); e != nil {
				return mkundef(), e
			}
			y, e := rt.toIntegerWithTruncation(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			m, e := rt.toIntegerWithTruncation(arg(args, 1))
			if e != nil {
				return mkundef(), e
			}
			d, e := rt.toIntegerWithTruncation(arg(args, 2))
			if e != nil {
				return mkundef(), e
			}
			cal, e := rt.calendarArg(arg(args, 3))
			if e != nil {
				return mkundef(), e
			}
			if !isValidISODate(y, m, d) {
				return mkundef(), rt.rangeError("date is out of range")
			}
			return rt.createTemporalDate(isoDateRec{y, m, d}, cal)
		})

	co.defineOwn("from", rt.newNativeFunc("from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := rt.toTemporalDate(arg(args, 0), arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalDate(iso, cal)
	}), attrWritable|attrConfigurable)

	co.defineOwn("compare", rt.newNativeFunc("compare", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		a, _, e := rt.toTemporalDate(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		b, _, e := rt.toTemporalDate(arg(args, 1), mkundef())
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(compareISODate(a, b))), nil
	}), attrWritable|attrConfigurable)

	rt.defCalendarGetters(po, kindPlainDate,
		getYear|getMonth|getDay|getDayOfThings|getYearThings|getMonthThings,
		func(o *object) (isoDateRec, string) { return tDate(o), rt.tCalendar(o) })

	self := func(this Value) (*object, isoDateRec, string, *ThrowError) {
		o, e := rt.requireTemporal(this, kindPlainDate)
		if e != nil {
			return nil, isoDateRec{}, "", e
		}
		return o, tDate(o), rt.tCalendar(o), nil
	}

	addSub := func(subtract bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			_, iso, cal, e := self(this)
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
			internal := d.toInternal24()
			days, _ := balanceTime(internal.time)
			dd := adjustDays(internal.date, days.Int64())
			out, err := calendarDateAdd(cal, iso, dd, overflow)
			if err != nil {
				return mkundef(), rt.throwFor(err)
			}
			return rt.createTemporalDate(out, cal)
		}
	}
	rt.defMethod(po, "add", 1, addSub(false))
	rt.defMethod(po, "subtract", 1, addSub(true))

	rt.defMethod(po, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, iso, cal, e := self(this)
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
		f, e := rt.readCalendarFields(item, calendarDateKeys(cal, keysDate), 0, true)
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
		merged := mergeCalendarFields(fieldsOfDate(cal, iso), f.cal)
		out, err := calendarDateFromFields(cal, merged, overflow)
		if err != nil {
			return mkundef(), rt.throwFor(err)
		}
		return rt.createTemporalDate(out, cal)
	})

	rt.defMethod(po, "withCalendar", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, iso, _, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		cal, e := rt.toCalendarID(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalDate(iso, cal)
	})

	diff := func(since bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			_, iso, cal, e := self(this)
			if e != nil {
				return mkundef(), e
			}
			otherISO, otherCal, e := rt.toTemporalDate(arg(args, 0), mkundef())
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
			s, e := rt.differenceSettings(opts, since, unitYear, unitDay, unitDay, unitDay)
			if e != nil {
				return mkundef(), e
			}
			a, b := iso, otherISO
			if since {
				a, b = b, a
			}
			out, e := rt.differenceDate(a, b, cal, s)
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

	rt.defMethod(po, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, iso, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		otherISO, otherCal, e := rt.toTemporalDate(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		return mkbool(compareISODate(iso, otherISO) == 0 && cal == otherCal), nil
	})

	rt.defMethod(po, "toPlainYearMonth", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, iso, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		f := fieldsOfDate(cal, iso)
		f.has &^= fDay
		out, err := calendarYearMonthFromFields(cal, f, "constrain")
		if err != nil {
			return mkundef(), rt.throwFor(err)
		}
		return rt.createTemporalYearMonth(out, cal)
	})

	rt.defMethod(po, "toPlainMonthDay", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, iso, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		f := fieldsOfDate(cal, iso)
		f.has &^= fYear | fEra | fEraYear | fMonth
		out, err := calendarMonthDayFromFields(cal, f, "constrain")
		if err != nil {
			return mkundef(), rt.throwFor(err)
		}
		return rt.createTemporalMonthDay(out, cal)
	})

	rt.defMethod(po, "toPlainDateTime", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, iso, cal, e := self(this)
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
		return rt.createTemporalDateTime(isoDateTimeRec{iso, t}, cal)
	})

	rt.defMethod(po, "toZonedDateTime", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, iso, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		item := arg(args, 0)
		var tzVal, timeVal Value = mkundef(), mkundef()
		if item.IsObjectType() && rt.objPtr(item) != nil && rt.temporalKindOf(item) == kindNone {
			v, e := rt.getField(item, "timeZone")
			if e != nil {
				return mkundef(), e
			}
			if v.IsUndefined() {
				tzVal = item
			} else {
				tzVal = v
				t, e := rt.getField(item, "plainTime")
				if e != nil {
					return mkundef(), e
				}
				timeVal = t
			}
		} else {
			tzVal = item
		}
		tz, e := rt.toTimeZoneID(tzVal)
		if e != nil {
			return mkundef(), e
		}
		z, _ := temporalZoneFor(tz)
		if timeVal.IsUndefined() {
			start, ok := z.startOfDay(iso)
			if !ok {
				return mkundef(), rt.rangeError("this day does not start in " + tz)
			}
			return rt.createTemporalZonedDateTime(start, tz, cal)
		}
		t, e := rt.toTemporalTime(timeVal, mkundef())
		if e != nil {
			return mkundef(), e
		}
		nsv, ok := z.disambiguate(isoDateTimeRec{iso, t}, "compatible")
		if !ok {
			return mkundef(), rt.rangeError("this local time does not exist in " + tz)
		}
		return rt.createTemporalZonedDateTime(nsv, tz, cal)
	})

	rt.defMethod(po, "toString", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, iso, cal, e := self(this)
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
		return rt.newString(formatISODate(iso) + formatCalendarAnnotation(cal, show)), nil
	})

	toJSON := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, iso, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(formatISODate(iso) + formatCalendarAnnotation(cal, "auto")), nil
	}
	rt.defMethod(po, "toJSON", 0, toJSON)
	rt.defMethod(po, "toLocaleString", 0, toJSON)
}

// calendarArg reads the optional calendar argument the three ISO constructors
// take, which defaults to the ISO calendar.
func (rt *Runtime) calendarArg(v Value) (string, *ThrowError) {
	if v.IsUndefined() {
		return "iso8601", nil
	}
	if !v.IsString() {
		return "", rt.typeError("calendar must be a string")
	}
	id, ok := canonicalCalendarID(rt.strGo(v))
	if !ok {
		return "", rt.rangeError("invalid calendar identifier: " + rt.strGo(v))
	}
	return id, nil
}

// differenceDate is until/since for the two date-only types.
func (rt *Runtime) differenceDate(a, b isoDateRec, cal string, s differenceSettings) (durationRec, *ThrowError) {
	if compareISODate(a, b) == 0 {
		return durationRec{}, nil
	}
	diff := calendarDateUntil(cal, a, b, s.largest)
	if s.smallest == unitDay && s.increment == 1 {
		out, ok := durationFromInternal(newInternal(diff, nil), s.largest)
		if !ok {
			return out, rt.rangeError("duration is out of range")
		}
		return out, nil
	}
	destNs := isoDateTimeToEpochNanoseconds(isoDateTimeRec{b, midnightTime()}, 0)
	return rt.roundRelativeDuration(newInternal(diff, nil), destNs,
		isoDateTimeRec{a, midnightTime()}, "", cal, s.largest, s.increment, s.smallest, s.mode)
}
