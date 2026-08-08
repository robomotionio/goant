package engine

// Date constructor + Date.prototype (ant modules/date.c). Time is stored as a
// float64 millisecond offset from the Unix epoch in the object's boxed cell.
//
// The stored value is always UTC. Local accessors resolve it against the host
// zone (see localtime.go); UTC accessors, Date.UTC, toISOString and toUTCString
// do not. Conformance is run with TZ=UTC so the two coincide there.

import (
	"math/big"
	"math"
	"strconv"
	"strings"
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
		if _, e := rt.dateMs(this); e != nil { // RequireInternalSlot([[DateValue]]) before ToNumber
			return mkundef(), e
		}
		ms, e := rt.toNumber(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		nm := timeClip(ms)
		rt.setDateMs(this, nm)
		return mknum(nm), nil
	})

	// Component getters. Each field has a local and a UTC reading; they differ by
	// the host zone's offset at that instant, so the pair only coincides on a
	// UTC host.
	getter := func(name string, conv func(float64) time.Time, f func(t time.Time) int) {
		g := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			v, e := rt.dateMs(this)
			if e != nil {
				return mkundef(), e
			}
			ms := v.Number()
			if math.IsNaN(ms) {
				return mknum(math.NaN()), nil
			}
			return mknum(float64(f(conv(ms)))), nil
		}
		rt.defMethod(proto, name, 0, g)
	}
	field := func(local, utc string, f func(t time.Time) int) {
		getter(local, msToLocal, f)
		getter(utc, msToTime, f)
	}
	field("getFullYear", "getUTCFullYear", func(t time.Time) int { return t.Year() })
	field("getMonth", "getUTCMonth", func(t time.Time) int { return int(t.Month()) - 1 })
	field("getDate", "getUTCDate", func(t time.Time) int { return t.Day() })
	field("getDay", "getUTCDay", func(t time.Time) int { return int(t.Weekday()) })
	field("getHours", "getUTCHours", func(t time.Time) int { return t.Hour() })
	field("getMinutes", "getUTCMinutes", func(t time.Time) int { return t.Minute() })
	field("getSeconds", "getUTCSeconds", func(t time.Time) int { return t.Second() })
	field("getMilliseconds", "getUTCMilliseconds", func(t time.Time) int { return t.Nanosecond() / 1e6 })
	rt.defMethod(proto, "getTimezoneOffset", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(v.Number()) {
			return mknum(math.NaN()), nil
		}
		// Positive west of Greenwich, so it is the negation of the zone's offset.
		// The "+ 0" is not redundant: negating a zero offset would otherwise
		// yield -0, and test262's without-utc-offset.js compares with SameValue.
		return mknum(-localOffsetMs(v.Number())/msPerMinute + 0), nil
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
	setter := func(name string, length int, nanToZero bool, loc *time.Location, apply func(t time.Time, v []float64, loc *time.Location) time.Time) {
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
					// t (the time value read before ToNumber) is NaN: return NaN
					// without writing [[DateValue]], so a valueOf side effect that
					// changed the date during argument coercion persists.
					return mknum(math.NaN()), nil
				}
				base = 0
			}
			if bad {
				rt.setDateMs(this, math.NaN())
				return mknum(math.NaN()), nil
			}
			// The components are read and rewritten in loc's wall clock, so a local
			// setter that crosses a DST boundary shifts the instant by the offset
			// change, as it should.
			newMs := timeClip(float64(apply(time.UnixMilli(int64(base)).In(loc), v, loc).UnixMilli()))
			rt.setDateMs(this, newMs)
			return mknum(newMs), nil
		}
		rt.defMethod(proto, name, length, s)
	}
	setFullYear := func(t time.Time, v []float64, loc *time.Location) time.Time {
		return time.Date(pick(v, 0, t.Year()), time.Month(pick(v, 1, int(t.Month())-1)+1), pick(v, 2, t.Day()),
			t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
	}
	setMonth := func(t time.Time, v []float64, loc *time.Location) time.Time {
		return time.Date(t.Year(), time.Month(pick(v, 0, int(t.Month())-1)+1), pick(v, 1, t.Day()),
			t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
	}
	setDate := func(t time.Time, v []float64, loc *time.Location) time.Time {
		return time.Date(t.Year(), t.Month(), pick(v, 0, t.Day()), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
	}
	setHours := func(t time.Time, v []float64, loc *time.Location) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), pick(v, 0, t.Hour()), pick(v, 1, t.Minute()),
			pick(v, 2, t.Second()), pick(v, 3, t.Nanosecond()/1e6)*1e6, loc)
	}
	setMinutes := func(t time.Time, v []float64, loc *time.Location) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), pick(v, 0, t.Minute()),
			pick(v, 1, t.Second()), pick(v, 2, t.Nanosecond()/1e6)*1e6, loc)
	}
	setSeconds := func(t time.Time, v []float64, loc *time.Location) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(),
			pick(v, 0, t.Second()), pick(v, 1, t.Nanosecond()/1e6)*1e6, loc)
	}
	setMillis := func(t time.Time, v []float64, loc *time.Location) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), pick(v, 0, t.Nanosecond()/1e6)*1e6, loc)
	}
	local, utc := localLoc(), time.UTC
	setter("setFullYear", 3, true, local, setFullYear)
	setter("setMonth", 2, false, local, setMonth)
	setter("setDate", 1, false, local, setDate)
	setter("setHours", 4, false, local, setHours)
	setter("setMinutes", 3, false, local, setMinutes)
	setter("setSeconds", 2, false, local, setSeconds)
	setter("setMilliseconds", 1, false, local, setMillis)
	// The UTC setters run the same field arithmetic against UTC's wall clock.
	setter("setUTCFullYear", 3, true, utc, setFullYear)
	setter("setUTCMonth", 2, false, utc, setMonth)
	setter("setUTCDate", 1, false, utc, setDate)
	setter("setUTCHours", 4, false, utc, setHours)
	setter("setUTCMinutes", 3, false, utc, setMinutes)
	setter("setUTCSeconds", 2, false, utc, setSeconds)
	setter("setUTCMilliseconds", 1, false, utc, setMillis)

	// String conversions.
	// Temporal.Instant is the type a Date is really a wrapper around, and this
	// is the way across. It reads the time value directly rather than through
	// valueOf, so a Date whose prototype has been rewritten still converts.
	rt.defMethod(proto, "toTemporalInstant", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(v.Number()) {
			return mkundef(), rt.rangeError("Invalid time value")
		}
		ns := new(big.Int).Mul(big.NewInt(int64(v.Number())), bigInt(nsPerMilli))
		return rt.createTemporalInstant(ns)
	})
	rt.defMethod(proto, "toISOString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(v.Number()) {
			return mkundef(), rt.rangeError("Invalid time value")
		}
		// ISO 8601 uses a 4-digit year in [0, 9999]; a year outside that range is
		// an expanded representation, ±YYYYYY (Go's Format never signs it).
		t := msToTime(v.Number())
		year := t.Year()
		pad := func(n, width int) string {
			s := strconv.Itoa(n)
			for len(s) < width {
				s = "0" + s
			}
			return s
		}
		var ys string
		switch {
		case year >= 0 && year <= 9999:
			ys = pad(year, 4)
		case year < 0:
			ys = "-" + pad(-year, 6)
		default:
			ys = "+" + pad(year, 6)
		}
		return rt.newString(ys + t.Format("-01-02T15:04:05.000Z")), nil
	})
	toStr := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(v.Number()) {
			return rt.internString("Invalid Date"), nil
		}
		return rt.newString(localDateTimeString(v.Number())), nil
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
				hint = rt.strGo(h)
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
		if math.IsNaN(v.Number()) {
			return rt.internString("Invalid Date"), nil
		}
		return rt.newString(msToLocal(v.Number()).Format("Mon Jan 02 2006")), nil
	})
	rt.defMethod(proto, "toTimeString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(v.Number()) {
			return rt.internString("Invalid Date"), nil
		}
		t := msToLocal(v.Number())
		return rt.newString(t.Format("15:04:05 GMT-0700") + " (" + zoneDisplayName(t) + ")"), nil
	})
	// The toLocale* trio formats through the same locale data as Intl (see
	// builtin_intl.go), in the zone the options bag asks for and otherwise the
	// host's.
	//
	// The options are read even for an Invalid Date, which returns early with a
	// fixed string: the spec constructs the formatter before it looks at the
	// time value, so `new Date(NaN).toLocaleString("en", {timeZone: "Nope"})`
	// throws rather than answering "Invalid Date".
	// Specified as an Intl.DateTimeFormat built per call, so this goes through
	// the same option handling and the same renderer rather than keeping a
	// second one that would drift. `required` and `defaults` are what make
	// toLocaleDateString show only a date and toLocaleTimeString only a time.
	localeFmt := func(required, defaults string) func(*Runtime, Value, []Value) (Value, *ThrowError) {
		return func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			v, e := rt.dateMs(this)
			if e != nil {
				return mkundef(), e
			}
			tags, e := rt.canonicalizeLocaleList(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			d, e := rt.initDateTimeOptionsFor(arg(args, 1), tags, required, defaults)
			if e != nil {
				return mkundef(), e
			}
			if math.IsNaN(v.Number()) {
				return rt.internString("Invalid Date"), nil
			}
			var b strings.Builder
			for _, p := range d.dateTimeParts(msInZone(v.Number(), zoneFor(d.timeZone))) {
				b.WriteString(p.val)
			}
			return rt.newString(b.String()), nil
		}
	}
	rt.defMethod(proto, "toLocaleString", 0, localeFmt("any", "all"))
	rt.defMethod(proto, "toLocaleDateString", 0, localeFmt("date", "date"))
	rt.defMethod(proto, "toLocaleTimeString", 0, localeFmt("time", "time"))
	// Annex-B legacy methods.
	rt.defMethod(proto, "getYear", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		if math.IsNaN(v.Number()) {
			return mknum(math.NaN()), nil
		}
		return mknum(float64(msToLocal(v.Number()).Year() - 1900)), nil
	})
	rt.defMethod(proto, "setYear", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		cur, e := rt.dateMs(this)
		if e != nil {
			return mkundef(), e
		}
		t := msToLocal(cur.Number())
		if math.IsNaN(cur.Number()) {
			t = msToLocal(0)
		}
		// y = ToNumber(year) (called exactly once, after reading the date value; may
		// throw). A NaN/infinite year — or one outside the representable range —
		// makes the date NaN.
		y, e := rt.toNumber(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		yi := math.Trunc(y) // ToIntegerOrInfinity
		yyyy := yi
		if yi >= 0 && yi <= 99 {
			yyyy = 1900 + yi
		}
		if math.IsNaN(y) || math.Abs(yyyy) > 1e6 {
			rt.setDateMs(this, math.NaN())
			return mknum(math.NaN()), nil
		}
		nt := time.Date(int(yyyy), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), localLoc())
		ms := timeClip(float64(nt.UnixMilli()))
		rt.setDateMs(this, ms)
		return mknum(ms), nil
	})
	// Date.prototype.toGMTString is the very same function object as toUTCString.
	if utc, ok := proto.getOwn("toUTCString"); ok {
		proto.defineOwn("toGMTString", utc, attrWritable|attrConfigurable)
	}

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
			return rt.newString(localDateTimeString(float64(time.Now().UnixMilli()))), nil
		}
		ms, e := rt.computeDateMs(args)
		if e != nil {
			return mkundef(), e
		}
		o := rt.objPtr(this)
		// OrdinaryCreateFromConstructor(newTarget, "%Date.prototype%"): a new target
		// whose `prototype` is not an object falls back to %Date.prototype%, not to
		// %Object.prototype%.
		pr, e := rt.newTargetProtoE(dateProto)
		if e != nil {
			return mkundef(), e
		}
		o.proto = pr
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
		// Date.UTC reads its components as UTC; the constructor reads the same
		// components as local time. That is the only difference between them.
		ms, e := rt.dateFromComponents(args, false)
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
		// TimeClip the parsed value: a time 1 ms outside ±8.64e15 is not a valid
		// Date and parses to NaN (idempotent for an in-range value).
		return mknum(timeClip(parseDate(string(b)))), nil
	})
	rt.defGlobal("Date", ctor)
}

// ---- helpers ----

func msToTime(ms float64) time.Time {
	return time.UnixMilli(int64(ms)).UTC()
}

// localDateTimeString is Date.prototype.toString()'s format: the local wall
// clock, the numeric UTC offset, and the zone's long name in parentheses.
func localDateTimeString(ms float64) string {
	t := msToLocal(ms)
	return t.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (" + zoneDisplayName(t) + ")"
}

// maxTimeValue is the largest magnitude a Date can hold, 100 million days.
const maxTimeValue = 8.64e15

// timeClip implements the ECMAScript TimeClip abstract operation.
func timeClip(ms float64) float64 {
	if math.IsNaN(ms) || math.Abs(ms) > maxTimeValue {
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
		// Date(value): if value is a Date object, copy its [[DateValue]] directly
		// (thisTimeValue) — no ToPrimitive/ToString/ToNumber on the argument.
		if o := rt.objPtr(a); o != nil && o.brandID() == brandDate {
			return o.boxed.Number(), nil
		}
		if a.IsString() {
			return parseDate(rt.strGo(a)), nil
		}
		if a.IsObjectType() {
			p, e := rt.toPrimitive(a, "default")
			if e != nil {
				return 0, e
			}
			if p.IsString() {
				return parseDate(rt.strGo(p)), nil
			}
			a = p
		}
		n, e := rt.toNumber(a)
		if e != nil {
			return 0, e
		}
		return timeClip(n), nil
	default:
		return rt.dateFromComponents(args, true)
	}
}

// dateFromComponents builds ms from (year, month, day, h, m, s, ms) args, as
// used by Date.UTC and the 2-or-more-argument constructor. Every supplied
// argument is ToNumber'd in order (exactly once, propagating abrupt
// completions); the year (argument 0) is always read (undefined → NaN). A
// non-finite component yields NaN.
//
// When local is set the components denote a wall clock in the host zone and the
// result is shifted to UTC, which is what `new Date(2026, 6, 30)` does; Date.UTC
// clears it and reads the very same components as UTC.
func (rt *Runtime) dateFromComponents(args []Value, local bool) (float64, *ThrowError) {
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
	tv := makeDate(makeDay(year, month, day), makeTime(hour, minu, sec, msv))
	if local {
		tv = utcFromLocalMs(tv)
	}
	return timeClip(tv), nil
}

const (
	msPerSecond = 1000
	msPerMinute = 60 * msPerSecond
	msPerHour   = 60 * msPerMinute
	msPerDay    = 24 * msPerHour
)

// The MakeTime / MakeDay / MakeDate abstract operations, in IEEE 754 arithmetic
// and in the spec's exact order. Going through Go's time.Date instead loses both
// the rounding (the products overflow float64's integer range long before the
// sum does) and the range (int conversions wrap).

func makeTime(h, m, sec, milli float64) float64 {
	if !isFiniteNum(h) || !isFiniteNum(m) || !isFiniteNum(sec) || !isFiniteNum(milli) {
		return math.NaN()
	}
	return ((math.Trunc(h)*msPerHour + math.Trunc(m)*msPerMinute) + math.Trunc(sec)*msPerSecond) + math.Trunc(milli)
}

func makeDay(year, month, date float64) float64 {
	if !isFiniteNum(year) || !isFiniteNum(month) || !isFiniteNum(date) {
		return math.NaN()
	}
	y, m, dt := math.Trunc(year), math.Trunc(month), math.Trunc(date)
	ym := y + math.Floor(m/12)
	if !isFiniteNum(ym) {
		return math.NaN()
	}
	mn := math.Mod(m, 12)
	if mn < 0 {
		mn += 12
	}
	return dayOfFirstOfMonth(ym, mn) + dt - 1
}

func makeDate(day, t float64) float64 {
	if !isFiniteNum(day) || !isFiniteNum(t) {
		return math.NaN()
	}
	// The conversion is not redundant. MakeDate is specified as "day × msPerDay
	// plus time", with the product rounded to a double before the addition, and
	// Go permits a compiler to fuse the two into a single multiply-add — which
	// arm64 has and amd64 does not, so the same source computes two different
	// numbers on the two. An explicit conversion is how the language says round
	// here; test/built-ins/Date/UTC/fp-evaluation-order.js is what says it
	// matters.
	tv := float64(day*msPerDay) + t
	if !isFiniteNum(tv) {
		return math.NaN()
	}
	return tv
}

func isFiniteNum(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// dayOfFirstOfMonth is Day(t) for the first day of month mn (0-based) of year y,
// i.e. days since 1970-01-01 in the proleptic Gregorian calendar.
func dayOfFirstOfMonth(y, mn float64) float64 {
	mm := mn + 1 // 1..12
	if mm <= 2 {
		y--
	}
	era := math.Floor(y / 400)
	yoe := y - era*400 // [0, 399]
	mp := mm - 3
	if mm <= 2 {
		mp = mm + 9
	}
	doy := math.Floor((153*mp + 2) / 5)
	doe := yoe*365 + math.Floor(yoe/4) - math.Floor(yoe/100) + doy
	return era*146097 + doe - 719468
}

// parseDate parses a date string (ISO 8601 + a few common formats).
func parseDate(s string) float64 {
	// Date.prototype.toString appends a parenthesized zone name that no Go layout
	// covers, e.g. "... GMT+0000 (Coordinated Universal Time)"; drop the trailing
	// "(...)" (and the space before it) so the GMT-offset layout matches.
	if n := len(s); n > 0 && s[n-1] == ')' {
		for i := n - 2; i >= 0; i-- {
			if s[i] == '(' {
				j := i
				for j > 0 && s[j-1] == ' ' {
					j--
				}
				s = s[:j]
				break
			}
		}
	}
	// Order matters: the first layout that consumes the whole string wins, so the
	// zero-padded ISO forms have to precede the lenient ones that would also
	// match them but with the wrong zone rule.
	//
	// A missing zone offset means UTC for the ISO date-only forms and local time
	// everywhere else. That split is not arbitrary: the spec fixes date-only
	// forms at UTC and date-time forms at local time, and the non-ISO formats are
	// implementation-defined, where V8 has always chosen local.
	formats := []struct {
		layout string
		local  bool
	}{
		{"2006-01-02T15:04:05.000Z07:00", false},
		{"2006-01-02T15:04:05Z07:00", false},
		{"2006-01-02T15:04Z07:00", false},
		{"2006-01-02T15:04:05", true},
		{"2006-01-02T15:04", true},
		{"2006-01-02", false},
		// Date-only ISO forms may omit the day and the month: YYYY-MM and YYYY are
		// complete Date Time Strings, interpreted as UTC.
		{"2006-01", false},
		{"2006", false},
		// Everything below is outside the Date Time String Format, matched for
		// compatibility with what V8 accepts.
		{"2006-01-02 15:04:05", true},
		{"2006-01-02 15:04", true},
		{"2006/01/02 15:04:05", true},
		{"2006/01/02", true},
		{"2006.01.02", true},
		{"2006-1-2", true},
		{time.RFC1123, false},
		{time.RFC1123Z, false},
		{"Mon Jan 02 2006 15:04:05 GMT-0700", false},
		{"Mon Jan 2 2006 15:04:05", true},
		{"Mon Jan 2 2006", true},
		{"Jan 2, 2006 15:04:05", true},
		{"Jan 2, 2006", true},
		{"Jan 02 2006", true},
		{"Jan-2-2006", true},
		{"January 2, 2006 15:04:05", true},
		{"January 2, 2006", true},
		{"2 Jan 2006 15:04:05", true},
		{"2 Jan 2006", true},
		{"2-Jan-2006", true},
		{"2 January 2006", true},
		{"1/2/2006 15:04:05", true},
		{"1/2/2006", true},
	}
	// Extended-year ISO 8601 (±YYYYYY-MM-DD…): Go's time.Parse has no 6-digit
	// signed-year layout, so pull the year off, parse the remainder with a
	// placeholder year, and re-apply the real (possibly negative) year.
	if len(s) >= 7 && (s[0] == '+' || s[0] == '-') && isSixDigits(s[1:7]) && (len(s) == 7 || s[7] == '-') {
		yr, _ := strconv.Atoi(s[1:7])
		if s[0] == '-' {
			if yr == 0 {
				return math.NaN() // "-000000" (minus-zero extended year) is invalid
			}
			yr = -yr
		}
		for _, f := range formats {
			if t, err := time.Parse(f.layout, "2000"+s[7:]); err == nil {
				u := time.Date(yr, t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
				return float64(u.UTC().UnixMilli())
			}
		}
		return math.NaN()
	}
	for _, f := range formats {
		t, err := time.Parse(f.layout, s)
		if err != nil {
			continue
		}
		if f.local {
			// time.Parse hands back UTC for a layout carrying no zone, so the wall
			// clock it read has to be re-seated in the host zone.
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), localLoc())
		}
		return float64(t.UTC().UnixMilli())
	}
	return math.NaN()
}

// isSixDigits reports whether s is exactly six ASCII digits.
func isSixDigits(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < 6; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
