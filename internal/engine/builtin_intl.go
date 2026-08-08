package engine

import (
	"math"
	"time"
)

// Minimal Intl: the constructors exist, work with and without `new`, expose the
// expected prototype methods, and validate and canonicalise language tags per
// UTS 35 (see intl_langtag.go). Actual locale-sensitive formatting and
// collation still comes from the tables in intl_data.go rather than from CLDR.

func (rt *Runtime) initIntl() {
	intl := rt.newObject(rt.objectProto)
	io := rt.objPtr(intl)
	rt.setStringTag(intl, "Intl")

	// defineService installs an Intl service constructor: callable with or without
	// `new` (both yield an instance), resolving its locales argument, with the
	// given prototype methods. initOpts, where a service reads anything out of
	// the options bag, runs after the locale is resolved and may throw.
	defineService := func(name string, initOpts func(inst *object, options Value) *ThrowError, methods func(po *object)) {
		proto := rt.newObject(rt.objectProto)
		po := rt.objPtr(proto)
		ctor := rt.newNativeFunc(name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			_, tag, e := rt.resolveLocaleArgTag(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			// A fresh instance whose [[Prototype]] honours new.target (both `new
			// Intl.X()` and `Intl.X()` return an instance). The tag it resolved to
			// rides along in a slot so format() and resolvedOptions() agree.
			inst := rt.newObject(rt.newTargetProto(proto))
			insto := rt.objPtr(inst)
			insto.setSlot(slotIntlLocale, rt.newString(tag))
			if initOpts != nil {
				if e := initOpts(insto, arg(args, 1)); e != nil {
					return mkundef(), e
				}
			}
			return inst, nil
		})
		co := rt.objPtr(ctor)
		co.defineOwn("prototype", proto, 0)
		po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
		rt.setStringTag(proto, "Intl."+name)
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return rt.intlResolvedOptions(this, name), nil
		})
		if methods != nil {
			methods(po)
		}
		co.defineOwn("supportedLocalesOf", rt.newNativeFunc("supportedLocalesOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return rt.supportedLocalesOf(arg(args, 0), arg(args, 1))
		}), attrWritable|attrConfigurable)
		io.defineOwn(name, ctor, attrWritable|attrConfigurable)
	}

	defineService("Collator", nil, func(po *object) {
		// compare is an accessor returning a bound comparison function.
		getter := rt.newNativeFunc("get compare", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return rt.newNativeFunc("", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
				a, _ := rt.toStringValue(arg(args, 0))
				b, _ := rt.toStringValue(arg(args, 1))
				return mknum(float64(compareStrings(rt.strBytes(a), rt.strBytes(b)))), nil
			}), nil
		})
		po.defineAccessor("compare", getter, mkundef(), true, false, attrConfigurable)
	})
	defineService("NumberFormat", nil, func(po *object) {
		getter := rt.newNativeFunc("get format", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			li := rt.intlLocaleOf(this)
			return rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
				n, e := rt.toNumber(arg(args, 0))
				if e != nil {
					return mkundef(), e
				}
				return rt.newString(li.formatNumber(n)), nil
			}), nil
		})
		po.defineAccessor("format", getter, mkundef(), true, false, attrConfigurable)
	})
	defineService("DateTimeFormat", func(inst *object, options Value) *ThrowError {
		// [[TimeZone]] is fixed at construction: an unknown zone is a RangeError
		// here rather than on some later format() call in another file.
		id, e := rt.optionTimeZone(options)
		if e != nil {
			return e
		}
		inst.setSlot(slotIntlTimeZone, rt.newString(id))
		return nil
	}, func(po *object) {
		getter := rt.newNativeFunc("get format", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			li := rt.intlLocaleOf(this)
			loc := zoneFor(rt.intlZoneID(this))
			return rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
				// With no argument the spec formats the current time.
				ms := float64(time.Now().UnixMilli())
				if a := arg(args, 0); !a.IsUndefined() {
					n, e := rt.toNumber(a)
					if e != nil {
						return mkundef(), e
					}
					ms = timeClip(n)
				}
				if math.IsNaN(ms) {
					return mkundef(), rt.rangeError("Invalid time value")
				}
				return rt.newString(li.formatDateTime(dtDate, msInZone(ms, loc))), nil
			}), nil
		})
		po.defineAccessor("format", getter, mkundef(), true, false, attrConfigurable)
	})

	rt.defMethod(io, "getCanonicalLocales", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		tags, e := rt.canonicalizeLocaleList(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return rt.newArrayOfStrings(tags), nil
	})
	rt.defMethod(io, "supportedValuesOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.supportedValuesOf(arg(args, 0))
	})
	rt.initIntlLocale(io)

	rt.defGlobal("Intl", intl)
}

// intlLocaleOf reads back the locale a service instance was constructed with,
// falling back to the pinned default when `format` is pulled off the prototype
// rather than an instance.
func (rt *Runtime) intlLocaleOf(this Value) localeInfo {
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlLocale); v.IsString() {
			li, _ := lookupLocale(rt.strGo(v))
			return li
		}
	}
	return localeTable[defaultLocale]
}

// intlResolvedOptions reports what the instance actually resolved to. The
// calendar and numbering system are fixed because localeTable only carries
// Gregorian, Latin-digit locales; anything else resolved to the default.
func (rt *Runtime) intlResolvedOptions(this Value, service string) Value {
	tag := defaultLocale
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlLocale); v.IsString() {
			tag = rt.strGo(v)
		}
	}
	o := rt.newPlainObject()
	oo := rt.objPtr(o)
	oo.defineOwn("locale", rt.newString(tag), attrDefault)
	oo.defineOwn("numberingSystem", rt.newString("latn"), attrDefault)
	if service == "DateTimeFormat" {
		li, _ := lookupLocale(tag)
		oo.defineOwn("calendar", rt.newString("gregory"), attrDefault)
		oo.defineOwn("timeZone", rt.newString(rt.intlZoneID(this)), attrDefault)
		oo.defineOwn("hourCycle", rt.newString(li.hourCycle), attrDefault)
	}
	return o
}
