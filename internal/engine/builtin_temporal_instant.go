package engine

// Temporal.Instant: a fixed point in time, with no calendar, no zone and no
// local reading of any kind. Everything else in Temporal is either this with a
// point of view attached, or a reading with no instant behind it.

import "math/big"

func (rt *Runtime) initTemporalInstant(ns *object) {
	co, po := rt.defineTemporalCtor(ns, kindInstant, 1,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if e := rt.requireNew("Instant"); e != nil {
				return mkundef(), e
			}
			n, e := rt.toBigIntValue(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			return rt.createTemporalInstant(n)
		})

	co.defineOwn("from", rt.newNativeFunc("from", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.toTemporalInstant(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalInstant(n)
	}), attrWritable|attrConfigurable)

	co.defineOwn("fromEpochMilliseconds", rt.newNativeFunc("fromEpochMilliseconds", 1,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			f, e := rt.toNumber(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			if !isIntegralNumber(f) {
				return mkundef(), rt.rangeError("epoch milliseconds must be a whole number")
			}
			return rt.createTemporalInstant(new(big.Int).Mul(bigInt(int64(f)), bigInt(nsPerMilli)))
		}), attrWritable|attrConfigurable)

	co.defineOwn("fromEpochNanoseconds", rt.newNativeFunc("fromEpochNanoseconds", 1,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			n, e := rt.toBigIntValue(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			return rt.createTemporalInstant(n)
		}), attrWritable|attrConfigurable)

	co.defineOwn("compare", rt.newNativeFunc("compare", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		a, e := rt.toTemporalInstant(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		b, e := rt.toTemporalInstant(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(a.Cmp(b))), nil
	}), attrWritable|attrConfigurable)

	getter := func(name string, f func(ns *big.Int) Value) {
		g := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, e := rt.requireTemporal(this, kindInstant)
			if e != nil {
				return mkundef(), e
			}
			return f(tEpochNs(o)), nil
		})
		po.defineAccessor(name, g, mkundef(), true, false, attrConfigurable)
	}
	getter("epochMilliseconds", func(ns *big.Int) Value {
		q := new(big.Int).Div(ns, bigInt(nsPerMilli))
		f, _ := new(big.Float).SetInt(q).Float64()
		return mknum(f)
	})
	getter("epochNanoseconds", func(ns *big.Int) Value { return rt.newBigInt(new(big.Int).Set(ns)) })

	addSub := func(subtract bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, e := rt.requireTemporal(this, kindInstant)
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
			if defaultLargestUnit(d) <= unitDay {
				return mkundef(), rt.rangeError("an instant cannot be moved by days, weeks, months or years")
			}
			out, e := rt.addInstant(tEpochNs(o), d.toInternal24().time)
			if e != nil {
				return mkundef(), e
			}
			return rt.createTemporalInstant(out)
		}
	}
	rt.defMethod(po, "add", 1, addSub(false))
	rt.defMethod(po, "subtract", 1, addSub(true))

	diff := func(since bool) nativeFunc {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, e := rt.requireTemporal(this, kindInstant)
			if e != nil {
				return mkundef(), e
			}
			other, e := rt.toTemporalInstant(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			opts, e := rt.temporalOptions(arg(args, 1))
			if e != nil {
				return mkundef(), e
			}
			s, e := rt.differenceSettings(opts, since, unitHour, unitNanosecond,
				unitNanosecond, unitSecond)
			if e != nil {
				return mkundef(), e
			}
			a, b := tEpochNs(o), other
			out, e := rt.differenceInstant(a, b, s)
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
		o, e := rt.requireTemporal(this, kindInstant)
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
		// An instant has no calendar, so the increment may be as large as the
		// number of that unit in a day.
		if e := rt.validateRoundingIncrement(increment, nsPerDay/nsPerUnit[unit], true); e != nil {
			return mkundef(), e
		}
		rounded := roundNumberToIncrement(tEpochNs(o), bigInt(nsPerUnit[unit]*increment), mode)
		return rt.createTemporalInstant(rounded)
	})

	rt.defMethod(po, "equals", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindInstant)
		if e != nil {
			return mkundef(), e
		}
		other, e := rt.toTemporalInstant(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return mkbool(tEpochNs(o).Cmp(other) == 0), nil
	})

	rt.defMethod(po, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindInstant)
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
		tzv, e := rt.getField(opts, "timeZone")
		if e != nil {
			return mkundef(), e
		}
		tz := ""
		if !tzv.IsUndefined() {
			id, e := rt.toTimeZoneID(tzv)
			if e != nil {
				return mkundef(), e
			}
			tz = id
		}
		if smallest == unitHour {
			return mkundef(), rt.rangeError("smallestUnit cannot be hour here")
		}
		precision, unit, increment, e := rt.secondsStringPrecision(smallest, digits)
		if e != nil {
			return mkundef(), e
		}
		ns := roundNumberToIncrement(tEpochNs(o), bigInt(nsPerUnit[unit]*increment), mode)
		if !epochNsWithinLimits(ns) {
			return mkundef(), rt.rangeError("rounding took the instant out of range")
		}
		return rt.newString(rt.formatInstantString(ns, tz, precision)), nil
	})

	rt.defMethod(po, "toJSON", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindInstant)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(rt.formatInstantString(tEpochNs(o), "", -1)), nil
	})

	rt.defMethod(po, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if _, e := rt.requireTemporal(this, kindInstant); e != nil {
			return mkundef(), e
		}
		return rt.temporalLocaleString(this, kindInstant, arg(args, 0), arg(args, 1))
	})

	rt.defMethod(po, "toZonedDateTimeISO", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := rt.requireTemporal(this, kindInstant)
		if e != nil {
			return mkundef(), e
		}
		tz, e := rt.toTimeZoneID(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalZonedDateTime(tEpochNs(o), tz, "iso8601")
	})
}

// formatInstantString writes an instant, in UTC unless a zone was asked for.
func (rt *Runtime) formatInstantString(ns *big.Int, tz string, precision int) string {
	offset := "Z"
	dt := getISODateTimeFor(0, ns)
	if tz != "" {
		z, _ := temporalZoneFor(tz)
		off := z.offsetNs(ns)
		dt = getISODateTimeFor(off, ns)
		offset = formatOffsetNanosecondsRounded(off)
	}
	return formatISODateTime(dt, precision) + offset
}

// toBigIntValue is ToBigInt for the two places Temporal takes one: a BigInt is
// itself, and everything but a string, a boolean and a BigInt is a mistake.
func (rt *Runtime) toBigIntValue(v Value) (*big.Int, *ThrowError) {
	b, e := rt.toBigInt(v)
	if e != nil {
		return nil, e
	}
	return b, nil
}
