package engine

// Temporal: the namespace, the plumbing every one of its eight types shares,
// and Temporal.Now.
//
// A Temporal object carries its whole state in internal slots and never
// changes: every method that looks like it modifies one returns a new one. The
// slots hold as little as will do -- a date is one day number, a time is one
// count of nanoseconds -- and the calendar and the time zone are identifiers
// beside them, because since the Calendar and TimeZone objects were dropped
// from the proposal that is all they ever are.

import (
	"math"
	"math/big"
	"strings"
	"time"
)

type temporalKind int

const (
	kindNone temporalKind = iota
	kindInstant
	kindPlainDate
	kindPlainTime
	kindPlainDateTime
	kindPlainYearMonth
	kindPlainMonthDay
	kindZonedDateTime
	kindDuration
	kindCount
)

var temporalKindNames = [kindCount]string{"", "Instant", "PlainDate",
	"PlainTime", "PlainDateTime", "PlainYearMonth", "PlainMonthDay",
	"ZonedDateTime", "Duration"}

// ---- the payload ----

func tset(o *object, i int, v float64) {
	o.setSlot(slotTemporal0+internalSlot(i), mknum(v))
}

func tnum(o *object, i int) float64 {
	return o.getSlot(slotTemporal0 + internalSlot(i)).Number()
}

func (rt *Runtime) temporalKindOf(v Value) temporalKind {
	o := rt.objPtr(v)
	if o == nil {
		return kindNone
	}
	k := o.getSlot(slotTemporalKind)
	if k.Type() != TNum {
		return kindNone
	}
	return temporalKind(k.Number())
}

// requireTemporal is RequireInternalSlot: the receiver must be one of ours.
func (rt *Runtime) requireTemporal(v Value, k temporalKind) (*object, *ThrowError) {
	if rt.temporalKindOf(v) != k {
		return nil, rt.typeError("not a Temporal." + temporalKindNames[k])
	}
	return rt.objPtr(v), nil
}

// newTemporalObject makes an instance whose prototype honours new.target when
// one is constructing, and is the built-in prototype otherwise.
func (rt *Runtime) newTemporalObject(kind temporalKind) (Value, *object, *ThrowError) {
	proto := rt.temporalProto[kind]
	if rt.constructing() {
		p, e := rt.newTargetProtoE(proto)
		if e != nil {
			return mkundef(), nil, e
		}
		proto = p
	}
	v := rt.newObject(proto)
	o := rt.objPtr(v)
	o.setSlot(slotTemporalKind, mknum(float64(kind)))
	return v, o, nil
}

// ---- reading each kind back ----

func tDate(o *object) isoDateRec         { return epochDaysToISODate(int(tnum(o, 0))) }
func tTime(o *object) isoTimeRec         { return timeFromNanoseconds(int64(tnum(o, 0))) }
func tDateTimeDate(o *object) isoDateRec { return epochDaysToISODate(int(tnum(o, 0))) }
func tDateTimeTime(o *object) isoTimeRec { return timeFromNanoseconds(int64(tnum(o, 1))) }

func tDateTime(o *object) isoDateTimeRec {
	return isoDateTimeRec{tDateTimeDate(o), tDateTimeTime(o)}
}

func tEpochNs(o *object) *big.Int {
	ns := new(big.Int).Mul(bigInt(int64(tnum(o, 0))), bigInt(nsPerSecond))
	return ns.Add(ns, bigInt(int64(tnum(o, 1))))
}

func tSetEpochNs(o *object, ns *big.Int) {
	sec, rem := new(big.Int), new(big.Int)
	sec.DivMod(ns, bigInt(nsPerSecond), rem)
	tset(o, 0, float64(sec.Int64()))
	tset(o, 1, float64(rem.Int64()))
}

func (rt *Runtime) tCalendar(o *object) string {
	v := o.getSlot(slotTemporalCalendar)
	if !v.IsString() {
		return "iso8601"
	}
	return rt.strGo(v)
}

func (rt *Runtime) tTimeZone(o *object) string {
	v := o.getSlot(slotTemporalTimeZone)
	if !v.IsString() {
		return "UTC"
	}
	return rt.strGo(v)
}

func tDuration(o *object) durationRec {
	var f [10]float64
	for i := range f {
		f[i] = tnum(o, i)
	}
	return durationFromFields(f)
}

// ---- making each kind ----

func (rt *Runtime) createTemporalDate(d isoDateRec, calendar string) (Value, *ThrowError) {
	if !isoDateWithinLimits(d) {
		return mkundef(), rt.rangeError("date is outside the representable range")
	}
	v, o, e := rt.newTemporalObject(kindPlainDate)
	if e != nil {
		return mkundef(), e
	}
	tset(o, 0, float64(d.epochDays()))
	o.setSlot(slotTemporalCalendar, rt.newString(calendar))
	return v, nil
}

func (rt *Runtime) createTemporalTime(t isoTimeRec) (Value, *ThrowError) {
	v, o, e := rt.newTemporalObject(kindPlainTime)
	if e != nil {
		return mkundef(), e
	}
	tset(o, 0, float64(t.nanosecond()))
	return v, nil
}

func (rt *Runtime) createTemporalDateTime(dt isoDateTimeRec, calendar string) (Value, *ThrowError) {
	if !isoDateTimeWithinLimits(dt) {
		return mkundef(), rt.rangeError("date-time is outside the representable range")
	}
	v, o, e := rt.newTemporalObject(kindPlainDateTime)
	if e != nil {
		return mkundef(), e
	}
	tset(o, 0, float64(dt.date.epochDays()))
	tset(o, 1, float64(dt.time.nanosecond()))
	o.setSlot(slotTemporalCalendar, rt.newString(calendar))
	return v, nil
}

func (rt *Runtime) createTemporalYearMonth(d isoDateRec, calendar string) (Value, *ThrowError) {
	// A year-month is in range when some day of it is, which is a wider window
	// than its reference day -- the first -- would allow on its own.
	if !isoYearMonthWithinLimits(d) {
		return mkundef(), rt.rangeError("year-month is outside the representable range")
	}
	v, o, e := rt.newTemporalObject(kindPlainYearMonth)
	if e != nil {
		return mkundef(), e
	}
	tset(o, 0, float64(d.epochDays()))
	o.setSlot(slotTemporalCalendar, rt.newString(calendar))
	return v, nil
}

func (rt *Runtime) createTemporalMonthDay(d isoDateRec, calendar string) (Value, *ThrowError) {
	if !isoDateWithinLimits(d) {
		return mkundef(), rt.rangeError("month-day is outside the representable range")
	}
	v, o, e := rt.newTemporalObject(kindPlainMonthDay)
	if e != nil {
		return mkundef(), e
	}
	tset(o, 0, float64(d.epochDays()))
	o.setSlot(slotTemporalCalendar, rt.newString(calendar))
	return v, nil
}

func (rt *Runtime) createTemporalInstant(ns *big.Int) (Value, *ThrowError) {
	if !epochNsWithinLimits(ns) {
		return mkundef(), rt.rangeError("instant is outside the representable range")
	}
	v, o, e := rt.newTemporalObject(kindInstant)
	if e != nil {
		return mkundef(), e
	}
	tSetEpochNs(o, ns)
	return v, nil
}

func (rt *Runtime) createTemporalZonedDateTime(ns *big.Int, tz, calendar string) (Value, *ThrowError) {
	if !epochNsWithinLimits(ns) {
		return mkundef(), rt.rangeError("instant is outside the representable range")
	}
	v, o, e := rt.newTemporalObject(kindZonedDateTime)
	if e != nil {
		return mkundef(), e
	}
	tSetEpochNs(o, ns)
	o.setSlot(slotTemporalTimeZone, rt.newString(tz))
	o.setSlot(slotTemporalCalendar, rt.newString(calendar))
	return v, nil
}

func (rt *Runtime) createTemporalDuration(d durationRec) (Value, *ThrowError) {
	if !isValidDuration(d.fields()) {
		return mkundef(), rt.rangeError("invalid duration")
	}
	v, o, e := rt.newTemporalObject(kindDuration)
	if e != nil {
		return mkundef(), e
	}
	for i, f := range d.fields() {
		tset(o, i, f+0) // +0 turns a negative zero into zero
	}
	return v, nil
}

// ---- options ----

// temporalOptions is GetOptionsObject: undefined gives a fresh bag with nothing
// in it, an object is itself, and anything else is a mistake.
func (rt *Runtime) temporalOptions(v Value) (Value, *ThrowError) {
	if v.IsUndefined() {
		return rt.newObject(mknull()), nil
	}
	if !v.IsObjectType() || rt.objPtr(v) == nil {
		return mkundef(), rt.typeError("options must be an object")
	}
	return v, nil
}

// temporalStringOption is GetOption for a string: the value is coerced, and a
// value outside the list is a RangeError rather than a TypeError.
func (rt *Runtime) temporalStringOption(opts Value, name string, allowed []string, def string) (string, *ThrowError) {
	v, e := rt.getField(opts, name)
	if e != nil {
		return "", e
	}
	if v.IsUndefined() {
		return def, nil
	}
	sv, e := rt.toStringValue(v)
	if e != nil {
		return "", e
	}
	s := rt.strGo(sv)
	for _, a := range allowed {
		if s == a {
			return s, nil
		}
	}
	return "", rt.rangeError(name + " must be one of " + strings.Join(allowed, ", ") + ", got " + s)
}

var overflowValues = []string{"constrain", "reject"}
var disambiguationValues = []string{"compatible", "earlier", "later", "reject"}
var offsetValues = []string{"prefer", "use", "ignore", "reject"}
var temporalRoundingModes = []string{"ceil", "floor", "expand", "trunc", "halfCeil",
	"halfFloor", "halfExpand", "halfTrunc", "halfEven"}
var showCalendarValues = []string{"auto", "always", "never", "critical"}
var showTimeZoneValues = []string{"auto", "never", "critical"}
var showOffsetValues = []string{"auto", "never"}

func (rt *Runtime) getOverflow(opts Value) (string, *ThrowError) {
	return rt.temporalStringOption(opts, "overflow", overflowValues, "constrain")
}

func (rt *Runtime) getDisambiguation(opts Value) (string, *ThrowError) {
	return rt.temporalStringOption(opts, "disambiguation", disambiguationValues, "compatible")
}

func (rt *Runtime) getOffsetOption(opts Value, def string) (string, *ThrowError) {
	return rt.temporalStringOption(opts, "offset", offsetValues, def)
}

func (rt *Runtime) getRoundingMode(opts Value, def string) (string, *ThrowError) {
	return rt.temporalStringOption(opts, "roundingMode", temporalRoundingModes, def)
}

// negateRoundingMode swaps a mode for its mirror, which is what "since" does so
// that it rounds the same way "until" would have.
func negateRoundingMode(mode string) string {
	switch mode {
	case "ceil":
		return "floor"
	case "floor":
		return "ceil"
	case "halfCeil":
		return "halfFloor"
	case "halfFloor":
		return "halfCeil"
	}
	return mode
}

// getRoundingIncrement reads roundingIncrement, which must be a whole number
// from 1 to a billion.
func (rt *Runtime) getRoundingIncrement(opts Value) (int64, *ThrowError) {
	v, e := rt.getField(opts, "roundingIncrement")
	if e != nil {
		return 0, e
	}
	if v.IsUndefined() {
		return 1, nil
	}
	f, e := rt.toNumber(v)
	if e != nil {
		return 0, e
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, rt.rangeError("roundingIncrement must be finite")
	}
	n := math.Trunc(f)
	if n < 1 || n > 1e9 {
		return 0, rt.rangeError("roundingIncrement out of range")
	}
	return int64(n), nil
}

// validateRoundingIncrement is ValidateTemporalRoundingIncrement: an increment
// has to divide the unit above it, or it would step across it.
func (rt *Runtime) validateRoundingIncrement(increment, dividend int64, inclusive bool) *ThrowError {
	max := dividend
	if !inclusive {
		max = dividend - 1
	}
	if increment > max {
		return rt.rangeError("roundingIncrement is too large for the unit")
	}
	if dividend%increment != 0 {
		return rt.rangeError("roundingIncrement must divide the unit above it")
	}
	return nil
}

// getTemporalUnit reads a unit-valued option, accepting the singular, the
// plural, and whatever extra words this particular option allows.
func (rt *Runtime) getTemporalUnit(opts Value, name string, min, max int, def int, extra ...string) (int, *ThrowError) {
	var allowed []string
	allowed = append(allowed, extra...)
	for u := min; u <= max; u++ {
		allowed = append(allowed, temporalUnitNames[u], temporalUnitPlurals[u])
	}
	dflt := ""
	switch {
	case def == unitAuto:
		dflt = "auto"
	case def >= 0:
		dflt = temporalUnitNames[def]
	}
	s, e := rt.temporalStringOption(opts, name, allowed, dflt)
	if e != nil {
		return 0, e
	}
	if s == "" {
		return unitAuto - 1, nil // "required and absent"
	}
	if u, ok := unitFromName(s); ok {
		return u, nil
	}
	return unitAuto, nil // "auto", or one of the extra words
}

// getTemporalUnitIn reads a smallestUnit that only some units are allowed for,
// and keeps the two halves apart: the unit is READ against every unit there is,
// and judged against the ones this caller can use afterwards. A toString that
// throws where the unit is read never gets as far as the options after it, and
// a script can watch it not get there. The pending value is returned as-is so
// the caller can spot an absent option.
func (rt *Runtime) getTemporalUnitIn(opts Value, name string, min, max int) (int, *ThrowError) {
	return rt.getTemporalUnit(opts, name, unitYear, unitNanosecond, unitAuto-1)
}

// checkUnitRange is the judging half, called once every option has been read.
func (rt *Runtime) checkUnitRange(u int, name string, min, max int) *ThrowError {
	if u < 0 || u >= unitCount || (u >= min && u <= max) {
		return nil
	}
	return rt.rangeError(name + " must be between " +
		temporalUnitNames[min] + " and " + temporalUnitNames[max])
}

// getFractionalSecondDigits reads fractionalSecondDigits: "auto", or a count
// from none to nine.
func (rt *Runtime) getFractionalSecondDigits(opts Value) (int, *ThrowError) {
	v, e := rt.getField(opts, "fractionalSecondDigits")
	if e != nil {
		return -1, e
	}
	if v.IsUndefined() {
		return -1, nil
	}
	if !v.IsNumber() {
		sv, e := rt.toStringValue(v)
		if e != nil {
			return -1, e
		}
		if rt.strGo(sv) != "auto" {
			return -1, rt.rangeError("fractionalSecondDigits must be auto or a digit count")
		}
		return -1, nil
	}
	f := v.Number()
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return -1, rt.rangeError("fractionalSecondDigits must be finite")
	}
	n := int(math.Floor(f))
	if n < 0 || n > 9 {
		return -1, rt.rangeError("fractionalSecondDigits out of range")
	}
	return n, nil
}

// secondsStringPrecision folds smallestUnit and fractionalSecondDigits into the
// one answer the writers need: how many fractional digits, which unit to round
// to, and by how much.
func (rt *Runtime) secondsStringPrecision(smallest int, digits int) (precision int, unit int, increment int64, e *ThrowError) {
	switch smallest {
	case unitMinute:
		return -2, unitMinute, 1, nil
	case unitSecond:
		return 0, unitSecond, 1, nil
	case unitMillisecond:
		return 3, unitMillisecond, 1, nil
	case unitMicrosecond:
		return 6, unitMicrosecond, 1, nil
	case unitNanosecond:
		return 9, unitNanosecond, 1, nil
	}
	switch {
	case digits < 0:
		return -1, unitNanosecond, 1, nil
	case digits == 0:
		return 0, unitSecond, 1, nil
	case digits <= 3:
		return digits, unitMillisecond, pow10(3 - digits), nil
	case digits <= 6:
		return digits, unitMicrosecond, pow10(6 - digits), nil
	}
	return digits, unitNanosecond, pow10(9 - digits), nil
}

func pow10(n int) int64 {
	p := int64(1)
	for i := 0; i < n; i++ {
		p *= 10
	}
	return p
}

// ---- numbers out of fields ----

// toIntegerWithTruncation is what every Temporal field reader uses: a number
// with anything after the point thrown away, and no room for a NaN.
func (rt *Runtime) toIntegerWithTruncation(v Value) (int, *ThrowError) {
	f, e := rt.toNumber(v)
	if e != nil {
		return 0, e
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, rt.rangeError("value must be a finite number")
	}
	return int(math.Trunc(f)), nil
}

func (rt *Runtime) toPositiveIntegerWithTruncation(v Value) (int, *ThrowError) {
	n, e := rt.toIntegerWithTruncation(v)
	if e != nil {
		return 0, e
	}
	if n <= 0 {
		return 0, rt.rangeError("value must be positive")
	}
	return n, nil
}

// ---- calendars and zones as arguments ----

// toCalendarID validates a calendar identifier and answers the spelling to
// store, which is the lower-cased one the tables use.
func (rt *Runtime) toCalendarID(v Value) (string, *ThrowError) {
	if o := rt.objPtr(v); o != nil && rt.temporalKindOf(v) != kindNone {
		switch rt.temporalKindOf(v) {
		case kindPlainDate, kindPlainDateTime, kindPlainYearMonth,
			kindPlainMonthDay, kindZonedDateTime:
			return rt.tCalendar(o), nil
		}
	}
	if !v.IsString() {
		return "", rt.typeError("calendar must be a string")
	}
	s := rt.strGo(v)
	if id, ok := canonicalCalendarID(s); ok {
		return id, nil
	}
	// It may instead be a whole Temporal string with a [u-ca=] on the end,
	// which names its calendar the same way.
	for _, parse := range []func(string) (temporalParse, bool){
		parseISODateTime, parseTemporalTimeString,
		parseTemporalYearMonthString, parseTemporalMonthDayString,
	} {
		if p, ok := parse(s); ok {
			if id, good := calendarFromParse(p); good {
				return id, nil
			}
			break
		}
	}
	return "", rt.rangeError("invalid calendar identifier: " + s)
}

// canonicalCalendarID accepts the identifier in any case and answers the one
// spelling the engine uses.
func canonicalCalendarID(s string) (string, bool) {
	lower := asciiLower(s)
	if lower == "iso8601" {
		return "iso8601", true
	}
	if id, ok := supportedCalendar(lower); ok {
		return id, true
	}
	return "", false
}

// toTimeZoneID validates a time zone identifier.
func (rt *Runtime) toTimeZoneID(v Value) (string, *ThrowError) {
	if rt.temporalKindOf(v) == kindZonedDateTime {
		return rt.tTimeZone(rt.objPtr(v)), nil
	}
	if !v.IsString() {
		return "", rt.typeError("time zone must be a string")
	}
	s := rt.strGo(v)
	// A whole ZonedDateTime string names its zone in brackets.
	if p, ok := parseISODateTime(s); ok && p.hasTZ {
		s = p.tzName
	} else if p, ok := parseISODateTime(s); ok && (p.z || p.hasOffset) {
		if p.z {
			s = "UTC"
		} else {
			s = p.offsetStr
		}
	}
	z, ok := temporalZoneFor(s)
	if !ok {
		return "", rt.rangeError("invalid time zone identifier: " + s)
	}
	return z.id, nil
}

// ---- the namespace ----

func (rt *Runtime) initTemporal() {
	ns := rt.newObject(rt.objectProto)
	no := rt.objPtr(ns)
	rt.setStringTag(ns, "Temporal")

	rt.initTemporalInstant(no)
	rt.initTemporalPlainDate(no)
	rt.initTemporalPlainTime(no)
	rt.initTemporalPlainDateTime(no)
	rt.initTemporalPlainYearMonth(no)
	rt.initTemporalPlainMonthDay(no)
	rt.initTemporalZonedDateTime(no)
	rt.initTemporalDuration(no)
	rt.initTemporalNow(no)

	rt.defGlobal("Temporal", ns)
}

// defineTemporalCtor installs one of the eight: a constructor that refuses to
// be called without `new`, its prototype, and the two properties that tie them
// together.
func (rt *Runtime) defineTemporalCtor(ns *object, kind temporalKind, length int,
	ctor nativeFunc) (*object, *object) {
	name := temporalKindNames[kind]
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)
	rt.temporalProto[kind] = proto
	fn := rt.newNativeFunc(name, length, ctor)
	fo := rt.objPtr(fn)
	fo.defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", fn, attrWritable|attrConfigurable)
	rt.setStringTag(proto, "Temporal."+name)
	ns.defineOwn(name, fn, attrWritable|attrConfigurable)
	// valueOf on any of them throws: two Temporal objects compared with < would
	// otherwise silently compare as strings or as NaN.
	rt.defMethod(po, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkundef(), rt.typeError("Temporal." + name +
			" cannot be converted to a primitive; use compare() or equals()")
	})
	return fo, po
}

// requireNew is the guard every Temporal constructor opens with.
func (rt *Runtime) requireNew(name string) *ThrowError {
	if !rt.constructing() {
		return rt.typeError("Constructor Temporal." + name + " requires 'new'")
	}
	return nil
}

// ---- Temporal.Now ----

func (rt *Runtime) initTemporalNow(ns *object) {
	now := rt.newObject(rt.objectProto)
	no := rt.objPtr(now)
	rt.setStringTag(now, "Temporal.Now")

	// zoneArg resolves the optional time zone argument the four local readings
	// take, defaulting to the host's.
	zoneArg := func(v Value) (string, *ThrowError) {
		if v.IsUndefined() {
			return localZoneID(), nil
		}
		return rt.toTimeZoneID(v)
	}

	rt.defMethod(no, "timeZoneId", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.newString(localZoneID()), nil
	})
	rt.defMethod(no, "instant", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.createTemporalInstant(rt.nowEpochNs())
	})
	rt.defMethod(no, "zonedDateTimeISO", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		tz, e := zoneArg(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return rt.createTemporalZonedDateTime(rt.nowEpochNs(), tz, "iso8601")
	})
	rt.defMethod(no, "plainDateTimeISO", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		tz, e := zoneArg(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		z, _ := temporalZoneFor(tz)
		return rt.createTemporalDateTime(z.dateTimeFor(rt.nowEpochNs()), "iso8601")
	})
	rt.defMethod(no, "plainDateISO", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		tz, e := zoneArg(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		z, _ := temporalZoneFor(tz)
		return rt.createTemporalDate(z.dateTimeFor(rt.nowEpochNs()).date, "iso8601")
	})
	rt.defMethod(no, "plainTimeISO", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		tz, e := zoneArg(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		z, _ := temporalZoneFor(tz)
		return rt.createTemporalTime(z.dateTimeFor(rt.nowEpochNs()).time)
	})
	ns.defineOwn("Now", now, attrWritable|attrConfigurable)
}

// nowEpochNs is the clock, in nanoseconds. The host clock has less resolution
// than that, which is why the value is built from milliseconds.
func (rt *Runtime) nowEpochNs() *big.Int {
	return new(big.Int).Mul(bigInt(time.Now().UnixMilli()), bigInt(nsPerMilli))
}

// ---- shared writing ----

// formatCalendarAnnotation writes the [u-ca=] a string carries, under the
// showCalendar rule asked for.
func formatCalendarAnnotation(id string, show string) string {
	switch show {
	case "never":
		return ""
	case "critical":
		return "[!u-ca=" + id + "]"
	case "always":
		return "[u-ca=" + id + "]"
	}
	if id == "iso8601" {
		return ""
	}
	return "[u-ca=" + id + "]"
}
