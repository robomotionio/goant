package engine

// Temporal.PlainYearMonth and Temporal.PlainMonthDay: the two readings that
// leave part of a date out. Each still stores a whole ISO date, because the ISO
// calendar is the only place a date can be kept -- the day of a year-month and
// the year of a month-day are references nobody is meant to read.

// ---- conversions ----

func (rt *Runtime) toTemporalYearMonth(item, options Value) (isoDateRec, string, *ThrowError) {
	if item.IsObjectType() && rt.objPtr(item) != nil {
		o := rt.objPtr(item)
		if rt.temporalKindOf(item) == kindPlainYearMonth {
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return isoDateRec{}, "", e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return isoDateRec{}, "", e
			}
			return tDate(o), rt.tCalendar(o), nil
		}
		cal, e := rt.calendarOfItem(item)
		if e != nil {
			return isoDateRec{}, "", e
		}
		f, e := rt.readCalendarFields(item, calendarDateKeys(cal, keysYearMonth), 0, true)
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
		iso, err := calendarYearMonthFromFields(cal, f.cal, overflow)
		if err != nil {
			return isoDateRec{}, "", rt.throwFor(err)
		}
		return iso, cal, nil
	}
	if !item.IsString() {
		return isoDateRec{}, "", rt.typeError("a Temporal.PlainYearMonth, a bag of fields or a string was expected")
	}
	s := rt.strGo(item)
	p, ok := parseTemporalYearMonthString(s)
	if !ok {
		return isoDateRec{}, "", rt.rangeError("cannot parse " + s + " as a year-month")
	}
	if p.z {
		return isoDateRec{}, "", rt.rangeError("a year-month string may not carry a UTC designator")
	}
	cal, ok := calendarFromParse(p)
	if !ok {
		return isoDateRec{}, "", rt.rangeError("unknown calendar in " + s)
	}
	opts, e := rt.temporalOptions(options)
	if e != nil {
		return isoDateRec{}, "", e
	}
	if _, e := rt.getOverflow(opts); e != nil {
		return isoDateRec{}, "", e
	}
	iso := isoDateRec{p.year, p.month, p.day}
	// The day a year-month string carries is not part of what it names, so the
	// reference day is recomputed in the calendar it named.
	f := fieldsOfDate(cal, iso)
	f.has &^= fDay
	out, err := calendarYearMonthFromFields(cal, f, "constrain")
	if err != nil {
		return isoDateRec{}, "", rt.throwFor(err)
	}
	return out, cal, nil
}

func (rt *Runtime) toTemporalMonthDay(item, options Value) (isoDateRec, string, *ThrowError) {
	if item.IsObjectType() && rt.objPtr(item) != nil {
		o := rt.objPtr(item)
		if rt.temporalKindOf(item) == kindPlainMonthDay {
			opts, e := rt.temporalOptions(options)
			if e != nil {
				return isoDateRec{}, "", e
			}
			if _, e := rt.getOverflow(opts); e != nil {
				return isoDateRec{}, "", e
			}
			return tDate(o), rt.tCalendar(o), nil
		}
		cal, e := rt.calendarOfItem(item)
		if e != nil {
			return isoDateRec{}, "", e
		}
		f, e := rt.readCalendarFields(item, calendarDateKeys(cal, keysMonthDay), 0, true)
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
		iso, err := calendarMonthDayFromFields(cal, f.cal, overflow)
		if err != nil {
			return isoDateRec{}, "", rt.throwFor(err)
		}
		return iso, cal, nil
	}
	if !item.IsString() {
		return isoDateRec{}, "", rt.typeError("a Temporal.PlainMonthDay, a bag of fields or a string was expected")
	}
	s := rt.strGo(item)
	p, ok := parseTemporalMonthDayString(s)
	if !ok {
		return isoDateRec{}, "", rt.rangeError("cannot parse " + s + " as a month-day")
	}
	if p.z {
		return isoDateRec{}, "", rt.rangeError("a month-day string may not carry a UTC designator")
	}
	cal, ok := calendarFromParse(p)
	if !ok {
		return isoDateRec{}, "", rt.rangeError("unknown calendar in " + s)
	}
	opts, e := rt.temporalOptions(options)
	if e != nil {
		return isoDateRec{}, "", e
	}
	if _, e := rt.getOverflow(opts); e != nil {
		return isoDateRec{}, "", e
	}
	// The year in a month-day string is a reference year, and a reference year
	// only means anything in the calendar that defines it. A string may name
	// the ISO calendar and no other, whether or not it carries a year --
	// "-999999-10-01" is a fine month-day and "-999999-01-01[u-ca=gregory]" is
	// not, and the only difference is the annotation.
	if cal != "iso8601" {
		return isoDateRec{}, "", rt.rangeError("a month-day string needs the ISO calendar")
	}
	iso := isoDateRec{p.year, p.month, p.day}
	if p.yearAbsent {
		return iso, cal, nil
	}
	f := fieldsOfDate(cal, iso)
	f.has &^= fYear | fEra | fEraYear | fMonth
	out, err := calendarMonthDayFromFields(cal, f, "constrain")
	if err != nil {
		return isoDateRec{}, "", rt.throwFor(err)
	}
	return out, cal, nil
}

// ---- PlainYearMonth ----

func (rt *Runtime) initTemporalPlainYearMonth(ns *object) {
	co, po := rt.defineTemporalCtor(ns, kindPlainYearMonth, 2,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if e := rt.requireNew("PlainYearMonth"); e != nil {
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
			cal, e := rt.calendarArg(arg(args, 2))
			if e != nil {
				return mkundef(), e
			}
			d := 1
			if v := arg(args, 3); !v.IsUndefined() {
				n, e := rt.toIntegerWithTruncation(v)
				if e != nil {
					return mkundef(), e
				}
				d = n
			}
			if !isValidISODate(y, m, d) {
				return mkundef(), rt.rangeError("year-month is out of range")
			}
			return rt.createTemporalYearMonth(isoDateRec{y, m, d}, cal)
		})

	co.defineOwn("from", rt.newNativeFunc("from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := rt.toTemporalYearMonth(arg(args, 0), arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalYearMonth(iso, cal)
	}), attrWritable|attrConfigurable)

	co.defineOwn("compare", rt.newNativeFunc("compare", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		a, _, e := rt.toTemporalYearMonth(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		b, _, e := rt.toTemporalYearMonth(arg(args, 1), mkundef())
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(compareISODate(a, b))), nil
	}), attrWritable|attrConfigurable)

	rt.defCalendarGetters(po, kindPlainYearMonth,
		getYear|getMonth|getYearThings|getMonthThings,
		func(o *object) (isoDateRec, string) { return tDate(o), rt.tCalendar(o) })

	self := func(this Value) (isoDateRec, string, *ThrowError) {
		o, e := rt.requireTemporal(this, kindPlainYearMonth)
		if e != nil {
			return isoDateRec{}, "", e
		}
		return tDate(o), rt.tCalendar(o), nil
	}

	addSub := func(subtract bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			iso, cal, e := self(this)
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
			// A year-month has no day for a smaller unit to land on, so a
			// duration carrying one is refused rather than quietly ignored --
			// even where it would not have changed the month.
			for _, v := range []float64{d.days, d.hours, d.minutes, d.seconds, d.ms, d.us, d.ns} {
				if v != 0 {
					return mkundef(), rt.rangeError(
						"a year-month cannot be moved by a unit smaller than a week")
				}
			}
			// Going backwards, the duration is measured from the end of the
			// month rather than its start: a month minus a day is the day
			// before the month ended, not the day before it began.
			//
			// Both ends are counted in days rather than built as dates,
			// because the day this walks through need not be representable
			// even when the answer is: the last month there is runs out on the
			// thirteenth, and its end and the first of the month after it are
			// both past the end of time. Only the year-month that comes out is
			// checked.
			c := calendarFor(cal)
			cd := c.dateFromDay(iso.epochDays())
			day := c.dayFromDate(cd.year, cd.month, 1)
			if d.sign() < 0 {
				day += c.daysInMonth(cd.year, cd.month) - 1
			}
			start := epochDaysToISODate(day)
			internal := d.toInternal24()
			days := dateDaysOf(internal.time)
			dd := adjustDays(internal.date, days)
			// The day was put there by this algorithm rather than by the
			// caller, so it is constrained whatever the caller asked for. A
			// month the target year has not got is still a rejection.
			addOverflow := "constrain"
			if overflow == "reject" {
				addOverflow = "reject-month"
			}
			added, err := calendarDateAdd(cal, start, dd, addOverflow)
			if err != nil {
				return mkundef(), rt.throwFor(err)
			}
			af := fieldsOfDate(cal, added)
			af.has &^= fDay
			out, err := calendarYearMonthFromFields(cal, af, overflow)
			if err != nil {
				return mkundef(), rt.throwFor(err)
			}
			return rt.createTemporalYearMonth(out, cal)
		}
	}
	rt.defMethod(po, "add", 1, addSub(false))
	rt.defMethod(po, "subtract", 1, addSub(true))

	rt.defMethod(po, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := self(this)
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
		f, e := rt.readCalendarFields(item, calendarDateKeys(cal, keysYearMonth), 0, true)
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
		existing := fieldsOfDate(cal, iso)
		existing.has &^= fDay
		merged := mergeCalendarFields(existing, f.cal)
		merged.has &^= fDay
		out, err := calendarYearMonthFromFields(cal, merged, overflow)
		if err != nil {
			return mkundef(), rt.throwFor(err)
		}
		return rt.createTemporalYearMonth(out, cal)
	})

	diff := func(since bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			iso, cal, e := self(this)
			if e != nil {
				return mkundef(), e
			}
			otherISO, otherCal, e := rt.toTemporalYearMonth(arg(args, 0), mkundef())
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
			s, e := rt.differenceSettings(opts, since, unitYear, unitMonth, unitMonth, unitYear)
			if e != nil {
				return mkundef(), e
			}
			if compareISODate(iso, otherISO) == 0 {
				return rt.createTemporalDuration(durationRec{})
			}
			first := func(d isoDateRec) (isoDateRec, *ThrowError) {
				f := fieldsOfDate(cal, d)
				f.day, f.has = 1, f.has|fDay
				out, err := calendarDateFromFields(cal, f, "constrain")
				if err != nil {
					return out, rt.throwFor(err)
				}
				return out, nil
			}
			a, e := first(iso)
			if e != nil {
				return mkundef(), e
			}
			b, e := first(otherISO)
			if e != nil {
				return mkundef(), e
			}
			dd := calendarDateUntil(cal, a, b, s.largest)
			dd.weeks, dd.days = 0, 0
			var out durationRec
			if s.smallest == unitMonth && s.increment == 1 {
				ok := false
				out, ok = durationFromInternal(newInternal(dd, nil), s.largest)
				if !ok {
					return mkundef(), rt.rangeError("duration is out of range")
				}
			} else {
				destNs := isoDateTimeToEpochNanoseconds(isoDateTimeRec{b, midnightTime()}, 0)
				out, e = rt.roundRelativeDuration(newInternal(dd, nil), nil, destNs,
					isoDateTimeRec{a, midnightTime()}, "", cal, s.largest, s.increment,
					s.smallest, s.mode)
				if e != nil {
					return mkundef(), e
				}
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
		iso, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		otherISO, otherCal, e := rt.toTemporalYearMonth(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		return mkbool(compareISODate(iso, otherISO) == 0 && cal == otherCal), nil
	})

	rt.defMethod(po, "toPlainDate", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		item := arg(args, 0)
		if !item.IsObjectType() || rt.objPtr(item) == nil {
			return mkundef(), rt.typeError("toPlainDate() takes a bag with a day in it")
		}
		f, e := rt.readCalendarFields(item, keyDay, keyDay, false)
		if e != nil {
			return mkundef(), e
		}
		merged := fieldsOfDate(cal, iso)
		merged.day = f.cal.day
		out, err := calendarDateFromFields(cal, merged, "constrain")
		if err != nil {
			return mkundef(), rt.throwFor(err)
		}
		return rt.createTemporalDate(out, cal)
	})

	rt.defMethod(po, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := self(this)
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
		return rt.newString(formatYearMonth(iso, cal, show)), nil
	})

	toJSON := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(formatYearMonth(iso, cal, "auto")), nil
	}
	rt.defMethod(po, "toJSON", 0, toJSON)
	rt.defMethod(po, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if _, e := rt.requireTemporal(this, kindPlainYearMonth); e != nil {
			return mkundef(), e
		}
		return rt.temporalLocaleString(this, kindPlainYearMonth, arg(args, 0), arg(args, 1))
	})
}

// formatYearMonth writes "2020-01", or the whole date where the calendar is not
// the ISO one and the reference day would otherwise be lost.
func formatYearMonth(iso isoDateRec, cal, show string) string {
	s := formatISOYear(iso.year) + "-" + pad(iso.month, 2)
	if cal != "iso8601" || show == "always" || show == "critical" {
		s += "-" + pad(iso.day, 2)
	}
	return s + formatCalendarAnnotation(cal, show)
}

// ---- PlainMonthDay ----

func (rt *Runtime) initTemporalPlainMonthDay(ns *object) {
	co, po := rt.defineTemporalCtor(ns, kindPlainMonthDay, 2,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if e := rt.requireNew("PlainMonthDay"); e != nil {
				return mkundef(), e
			}
			m, e := rt.toIntegerWithTruncation(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			d, e := rt.toIntegerWithTruncation(arg(args, 1))
			if e != nil {
				return mkundef(), e
			}
			cal, e := rt.calendarArg(arg(args, 2))
			if e != nil {
				return mkundef(), e
			}
			y := 1972
			if v := arg(args, 3); !v.IsUndefined() {
				n, e := rt.toIntegerWithTruncation(v)
				if e != nil {
					return mkundef(), e
				}
				y = n
			}
			if !isValidISODate(y, m, d) {
				return mkundef(), rt.rangeError("month-day is out of range")
			}
			return rt.createTemporalMonthDay(isoDateRec{y, m, d}, cal)
		})

	co.defineOwn("from", rt.newNativeFunc("from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := rt.toTemporalMonthDay(arg(args, 0), arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalMonthDay(iso, cal)
	}), attrWritable|attrConfigurable)

	rt.defCalendarGetters(po, kindPlainMonthDay, getDay,
		func(o *object) (isoDateRec, string) { return tDate(o), rt.tCalendar(o) })

	self := func(this Value) (isoDateRec, string, *ThrowError) {
		o, e := rt.requireTemporal(this, kindPlainMonthDay)
		if e != nil {
			return isoDateRec{}, "", e
		}
		return tDate(o), rt.tCalendar(o), nil
	}

	rt.defMethod(po, "with", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := self(this)
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
		f, e := rt.readCalendarFields(item, calendarDateKeys(cal, keysMonthDay), 0, true)
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
		if !f.cal.got(fYear) && !f.cal.got(fEra) {
			merged.has &^= fYear | fEra | fEraYear
		}
		merged.has &^= fMonth
		out, err := calendarMonthDayFromFields(cal, merged, overflow)
		if err != nil {
			return mkundef(), rt.throwFor(err)
		}
		return rt.createTemporalMonthDay(out, cal)
	})

	rt.defMethod(po, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		otherISO, otherCal, e := rt.toTemporalMonthDay(arg(args, 0), mkundef())
		if e != nil {
			return mkundef(), e
		}
		return mkbool(compareISODate(iso, otherISO) == 0 && cal == otherCal), nil
	})

	rt.defMethod(po, "toPlainDate", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		item := arg(args, 0)
		if !item.IsObjectType() || rt.objPtr(item) == nil {
			return mkundef(), rt.typeError("toPlainDate() takes a bag with a year in it")
		}
		want := keyYear
		if calendarHasEras(cal) {
			want |= keyEra | keyEraYear
		}
		f, e := rt.readCalendarFields(item, want, 0, true)
		if e != nil {
			return mkundef(), e
		}
		v := calendarViewOf(cal, iso)
		merged := f.cal
		merged.monthCode, merged.has = v.monthCode, merged.has|fMonthCode
		merged.day, merged.has = v.day, merged.has|fDay
		out, err := calendarDateFromFields(cal, merged, "constrain")
		if err != nil {
			return mkundef(), rt.throwFor(err)
		}
		return rt.createTemporalDate(out, cal)
	})

	rt.defMethod(po, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := self(this)
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
		return rt.newString(formatMonthDay(iso, cal, show)), nil
	})

	toJSON := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		iso, cal, e := self(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(formatMonthDay(iso, cal, "auto")), nil
	}
	rt.defMethod(po, "toJSON", 0, toJSON)
	rt.defMethod(po, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if _, e := rt.requireTemporal(this, kindPlainMonthDay); e != nil {
			return mkundef(), e
		}
		return rt.temporalLocaleString(this, kindPlainMonthDay, arg(args, 0), arg(args, 1))
	})
}

// formatMonthDay writes "--12-31", keeping the reference year where the
// calendar is not the ISO one and the year is part of what fixes the date.
func formatMonthDay(iso isoDateRec, cal, show string) string {
	s := pad(iso.month, 2) + "-" + pad(iso.day, 2)
	if cal != "iso8601" || show == "always" || show == "critical" {
		s = formatISOYear(iso.year) + "-" + pad(iso.month, 2) + "-" + pad(iso.day, 2)
	}
	return s + formatCalendarAnnotation(cal, show)
}
