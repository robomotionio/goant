package engine

// Date constructor + Date.prototype (ant modules/date.c). Time is stored as a
// float64 millisecond offset from the Unix epoch in the object's boxed cell.
//
// To keep conformance timezone-robust, "local" time is treated as UTC
// (getTimezoneOffset() === 0), so local and UTC accessors agree — matching the
// common TZ=UTC test environment.

import (
	"math"
	"time"
)

const brandDate = 1000

func (rt *Runtime) initDateBuiltin() {
	dateProto := rt.newObject(rt.objectProto)
	proto := rt.objPtr(dateProto)

	// getTime / valueOf
	rt.defMethod(proto, "getTime", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.dateMs(this)
	})
	rt.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.dateMs(this)
	})
	rt.defMethod(proto, "setTime", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		ms, e := rt.toNumber(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		rt.setDateMs(this, timeClip(ms))
		return mknum(timeClip(ms)), nil
	})

	// Component getters (UTC == local here).
	getter := func(name string, f func(t time.Time) int) {
		g := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			v, e := rt.dateMs(this)
			if e != nil {
				return mkundef(), e
			}
			ms := v.Number()
			if math.IsNaN(ms) {
				return mknum(math.NaN()), nil
			}
			return mknum(float64(f(msToTime(ms)))), nil
		}
		rt.defMethod(proto, name, 0, g)
	}
	getter("getFullYear", func(t time.Time) int { return t.Year() })
	getter("getUTCFullYear", func(t time.Time) int { return t.Year() })
	getter("getMonth", func(t time.Time) int { return int(t.Month()) - 1 })
	getter("getUTCMonth", func(t time.Time) int { return int(t.Month()) - 1 })
	getter("getDate", func(t time.Time) int { return t.Day() })
	getter("getUTCDate", func(t time.Time) int { return t.Day() })
	getter("getDay", func(t time.Time) int { return int(t.Weekday()) })
	getter("getUTCDay", func(t time.Time) int { return int(t.Weekday()) })
	getter("getHours", func(t time.Time) int { return t.Hour() })
	getter("getUTCHours", func(t time.Time) int { return t.Hour() })
	getter("getMinutes", func(t time.Time) int { return t.Minute() })
	getter("getUTCMinutes", func(t time.Time) int { return t.Minute() })
	getter("getSeconds", func(t time.Time) int { return t.Second() })
	getter("getUTCSeconds", func(t time.Time) int { return t.Second() })
	getter("getMilliseconds", func(t time.Time) int { return t.Nanosecond() / 1e6 })
	getter("getUTCMilliseconds", func(t time.Time) int { return t.Nanosecond() / 1e6 })
	rt.defMethod(proto, "getTimezoneOffset", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mknum(0), nil
	})

	// pick returns the i-th coerced component value (as an int) if it was
	// supplied, otherwise the current field value cur.
	pick := func(v []float64, i, cur int) int {
		if i < len(v) {
			return int(v[i])
		}
		return cur
	}
	// setter builds a Date.prototype component setter of the given .length. All of
	// its supplied arguments (and the first even when absent → undefined) are
	// ToNumber'd in order, exactly once, propagating an abrupt completion — before
	// the date's validity is consulted. A non-finite component makes the result
	// NaN; for setFullYear/setUTCFullYear an invalid Date is first reset to +0.
	setter := func(name string, length int, nanToZero bool, apply func(t time.Time, v []float64) time.Time) {
		s := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			cur, e := rt.dateMs(this)
			if e != nil {
				return mkundef(), e
			}
			ms := cur.Number()
			count := min(len(args), length)
			if count < 1 {
				count = 1 // the first argument is always read (undefined → NaN)
			}
			v := make([]float64, count)
			bad := false
			for i := 0; i < count; i++ {
				a := mkundef()
				if i < len(args) {
					a = args[i]
				}
				x, e := rt.toNumber(a)
				if e != nil {
					return mkundef(), e
				}
				v[i] = x
				if math.IsNaN(x) || math.IsInf(x, 0) {
					bad = true
				}
			}
			base := ms
			if math.IsNaN(ms) {
				if !nanToZero {
					rt.setDateMs(this, math.NaN())
					return mknum(math.NaN()), nil
				}
				base = 0
			}
			if bad {
				rt.setDateMs(this, math.NaN())
				return mknum(math.NaN()), nil
			}
			newMs := timeClip(float64(apply(msToTime(base), v).UnixMilli()))
			rt.setDateMs(this, newMs)
			return mknum(newMs), nil
		}
		rt.defMethod(proto, name, length, s)
	}
	setFullYear := func(t time.Time, v []float64) time.Time {
		return time.Date(pick(v, 0, t.Year()), time.Month(pick(v, 1, int(t.Month())-1)+1), pick(v, 2, t.Day()),
			t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	}
	setMonth := func(t time.Time, v []float64) time.Time {
		return time.Date(t.Year(), time.Month(pick(v, 0, int(t.Month())-1)+1), pick(v, 1, t.Day()),
			t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	}
	setDate := func(t time.Time, v []float64) time.Time {
		return time.Date(t.Year(), t.Month(), pick(v, 0, t.Day()), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	}
	setHours := func(t time.Time, v []float64) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), pick(v, 0, t.Hour()), pick(v, 1, t.Minute()),
			pick(v, 2, t.Second()), pick(v, 3, t.Nanosecond()/1e6)*1e6, time.UTC)
	}
	setMinutes := func(t time.Time, v []float64) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), pick(v, 0, t.Minute()),
			pick(v, 1, t.Second()), pick(v, 2, t.Nanosecond()/1e6)*1e6, time.UTC)
	}
	setSeconds := func(t time.Time, v []float64) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(),
			pick(v, 0, t.Second()), pick(v, 1, t.Nanosecond()/1e6)*1e6, time.UTC)
	}
	setMillis := func(t time.Time, v []float64) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), pick(v, 0, t.Nanosecond()/1e6)*1e6, time.UTC)
	}
	setter("setFullYear", 3, true, setFullYear)
	setter("setMonth", 2, false, setMonth)
	setter("setDate", 1, false, setDate)
	setter("setHours", 4, false, setHours)
	setter("setMinutes", 3, false, setMinutes)
	setter("setSeconds", 2, false, setSeconds)
	setter("setMilliseconds", 1, false, setMillis)
	// UTC setters share the implementations (local == UTC here).
	setter("setUTCFullYear", 3, true, setFullYear)
	setter("setUTCMonth", 2, false, setMonth)
	setter("setUTCDate", 1, false, setDate)
	setter("setUTCHours", 4, false, setHours)
	setter("setUTCMinutes", 3, false, setMinutes)
	setter("setUTCSeconds", 2, false, setSeconds)
	setter("setUTCMilliseconds", 1, false, setMillis)

	// String conversions.
	rt.defMethod(proto, "toISOString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(v.Number()) {
			return mkundef(), rt.rangeError("Invalid time value")
		}
		return rt.newString(msToTime(v.Number()).Format("2006-01-02T15:04:05.000Z")), nil
	})
	toStr := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(v.Number()) {
			return rt.internString("Invalid Date"), nil
		}
		return rt.newString(msToTime(v.Number()).Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)")), nil
	}
	rt.defMethod(proto, "toString", 0, toStr)
	// Date.prototype[Symbol.toPrimitive]: default and string hints use toString
	// (so `date + ""` yields the date string, not its numeric valueOf).
	if rt.symToPrimitive != 0 {
		proto.defineOwnSymbol(rt.symToPrimitive.handle(), rt.newNativeFunc("[Symbol.toPrimitive]", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if !this.IsObjectType() {
				return mkundef(), rt.typeError("Date.prototype[Symbol.toPrimitive] called on non-object")
			}
			hint := ""
			if h := arg(args, 0); h.IsString() {
				hint = string(rt.strBytes(h))
			}
			// "string"/"default" try toString first; "number" tries valueOf first;
			// any other hint is a TypeError. Date defaults to the string form.
			tryFirst := ""
			switch hint {
			case "string", "default":
				tryFirst = "string"
			case "number":
				tryFirst = "number"
			default:
				return mkundef(), rt.typeError("invalid hint passed to Date.prototype[Symbol.toPrimitive]")
			}
			return rt.ordinaryToPrimitive(this, tryFirst)
		}), attrConfigurable)
	}
	rt.defMethod(proto, "toUTCString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(v.Number()) {
			return rt.internString("Invalid Date"), nil
		}
		return rt.newString(msToTime(v.Number()).Format("Mon, 02 Jan 2006 15:04:05 GMT")), nil
	})
	rt.defMethod(proto, "toDateString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(msToTime(v.Number()).Format("Mon Jan 02 2006")), nil
	})
	rt.defMethod(proto, "toTimeString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(msToTime(v.Number()).Format("15:04:05 GMT+0000 (Coordinated Universal Time)")), nil
	})
	// toLocale* aliases (locale-free environment).
	rt.defMethod(proto, "toLocaleString", 0, toStr)
	rt.defMethod(proto, "toLocaleDateString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(msToTime(v.Number()).Format("01/02/2006")), nil
	})
	rt.defMethod(proto, "toLocaleTimeString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(msToTime(v.Number()).Format("15:04:05")), nil
	})
	// Annex-B legacy methods.
	rt.defMethod(proto, "getYear", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(v.Number()) {
			return mknum(math.NaN()), nil
		}
		return mknum(float64(msToTime(v.Number()).Year() - 1900)), nil
	})
	rt.defMethod(proto, "setYear", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cur, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		t := msToTime(cur.Number())
		if math.IsNaN(cur.Number()) {
			t = msToTime(0)
		}
		y := rt.intArg(args, 0)
		if y >= 0 && y <= 99 {
			y += 1900
		}
		nt := time.Date(y, t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
		ms := timeClip(float64(nt.UnixMilli()))
		rt.setDateMs(this, ms)
		return mknum(ms), nil
	})
	rt.defMethod(proto, "toGMTString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		utc, _ := rt.getField(this, "toUTCString")
		return rt.callValue(utc, this, nil)
	})

	rt.defMethod(proto, "toJSON", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// tv = ToPrimitive(O, number); a non-finite time serializes as null, then
		// Invoke(O, "toISOString") (ES Date.prototype.toJSON).
		o, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		tv, e := rt.toPrimitive(o, "number")
		if e != nil {
			return mkundef(), e
		}
		if tv.Type() == TNum {
			if n := tv.Number(); math.IsNaN(n) || math.IsInf(n, 0) {
				return mknull(), nil
			}
		}
		iso, e := rt.getField(o, "toISOString")
		if e != nil {
			return mkundef(), e
		}
		if !rt.isCallable(iso) {
			return mkundef(), rt.typeError("toISOString is not a function")
		}
		return rt.callValue(iso, o, nil)
	})

	// Date constructor.
	ctor := rt.newNativeFunc("Date", 7, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !this.IsObjectType() {
			// Called as a function: return a string (current time). The arguments are
			// ignored and not coerced.
			return rt.newString(msToTime(float64(time.Now().UnixMilli())).Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)")), nil
		}
		ms, e := rt.computeDateMs(args)
		if e != nil {
			return mkundef(), e
		}
		o := rt.objPtr(this)
		o.boxed = mknum(ms)
		o.setSlot(slotBrand, mknum(brandDate))
		return this, nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", dateProto, 0)
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defMethod(cobj, "now", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mknum(float64(time.Now().UnixMilli())), nil
	})
	rt.defMethod(cobj, "UTC", 7, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		ms, e := rt.dateFromComponents(args)
		if e != nil {
			return mkundef(), e
		}
		return mknum(ms), nil
	})
	rt.defMethod(cobj, "parse", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		return mknum(parseDate(string(b))), nil
	})
	rt.defGlobal("Date", ctor)
}

// ---- helpers ----

func msToTime(ms float64) time.Time {
	return time.UnixMilli(int64(ms)).UTC()
}

// timeClip implements the ECMAScript TimeClip abstract operation.
func timeClip(ms float64) float64 {
	if math.IsNaN(ms) || math.Abs(ms) > 8.64e15 {
		return math.NaN()
	}
	// TimeClip returns ToInteger(time) + (+0), which converts -0 to +0.
	if r := math.Trunc(ms); r != 0 {
		return r
	}
	return 0
}

func (rt *Runtime) dateMs(this Value) (Value, *ThrowError) {
	o := rt.objPtr(this)
	if o == nil || o.brandID() != brandDate {
		return mkundef(), rt.typeError("this is not a Date object")
	}
	return o.boxed, nil
}

func (rt *Runtime) setDateMs(this Value, ms float64) {
	if o := rt.objPtr(this); o != nil {
		o.boxed = mknum(ms)
	}
}

// computeDateMs derives the initial millisecond value from constructor args,
// propagating an abrupt ToPrimitive/ToNumber.
func (rt *Runtime) computeDateMs(args []Value) (float64, *ThrowError) {
	switch len(args) {
	case 0:
		return float64(time.Now().UnixMilli()), nil
	case 1:
		a := args[0]
		if a.IsString() {
			return parseDate(string(rt.strBytes(a))), nil
		}
		if a.IsObjectType() {
			p, e := rt.toPrimitive(a, "default")
			if e != nil {
				return 0, e
			}
			if p.IsString() {
				return parseDate(string(rt.strBytes(p))), nil
			}
			a = p
		}
		n, e := rt.toNumber(a)
		if e != nil {
			return 0, e
		}
		return timeClip(n), nil
	default:
		return rt.dateFromComponents(args)
	}
}

// dateFromComponents builds ms from (year, month, day, h, m, s, ms) args, as
// used by Date.UTC and the 2-or-more-argument constructor. Every supplied
// argument is ToNumber'd in order (exactly once, propagating abrupt
// completions); the year (argument 0) is always read (undefined → NaN). A
// non-finite component yields NaN.
func (rt *Runtime) dateFromComponents(args []Value) (float64, *ThrowError) {
	count := min(len(args), 7)
	if count < 1 {
		count = 1 // the year is always read
	}
	vals := make([]float64, count)
	for i := 0; i < count; i++ {
		a := mkundef()
		if i < len(args) {
			a = args[i]
		}
		x, e := rt.toNumber(a)
		if e != nil {
			return 0, e
		}
		vals[i] = x
	}
	comp := func(i, dflt int) float64 {
		if i < len(vals) {
			return vals[i]
		}
		return float64(dflt)
	}
	year := comp(0, 1970)
	if !math.IsNaN(year) && !math.IsInf(year, 0) {
		if iy := int(year); iy >= 0 && iy <= 99 { // MakeFullYear: 0..99 → 1900..1999
			year = float64(1900 + iy)
		}
	}
	month, day := comp(1, 0), comp(2, 1)
	hour, minu, sec, msv := comp(3, 0), comp(4, 0), comp(5, 0), comp(6, 0)
	for _, c := range []float64{year, month, day, hour, minu, sec, msv} {
		if math.IsNaN(c) || math.IsInf(c, 0) {
			return math.NaN(), nil
		}
	}
	t := time.Date(int(year), time.Month(int(month)+1), int(day), int(hour), int(minu), int(sec), int(msv)*1e6, time.UTC)
	return timeClip(float64(t.UnixMilli())), nil
}

// parseDate parses a date string (ISO 8601 + a few common formats).
func parseDate(s string) float64 {
	formats := []string{
		"2006-01-02T15:04:05.000Z07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02",
		"2006/01/02",
		time.RFC1123,
		"Mon Jan 02 2006 15:04:05 GMT-0700",
		"Jan 02 2006",
		"January 2, 2006",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return float64(t.UTC().UnixMilli())
		}
	}
	return math.NaN()
}
