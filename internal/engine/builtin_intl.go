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
	//
	// requireNew separates the three constructors that predate ES2015 -- which
	// are specified to work without `new`, and cannot stop doing so -- from the
	// ones added since, which throw.
	defineService := func(name string, requireNew bool, initOpts func(inst *object, options Value, requested []string) *ThrowError, methods func(po *object)) {
		proto := rt.newObject(rt.objectProto)
		po := rt.objPtr(proto)
		ctor := rt.newNativeFunc(name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if requireNew && !rt.constructing() {
				return mkundef(), rt.typeError("Constructor Intl." + name + " requires 'new'")
			}
			requested, e := rt.canonicalizeLocaleList(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			tag := defaultLocale
			if len(requested) > 0 {
				_, tag = lookupLocale(requested[0])
			}
			// A fresh instance whose [[Prototype]] honours new.target (both `new
			// Intl.X()` and `Intl.X()` return an instance). The tag it resolved to
			// rides along in a slot so format() and resolvedOptions() agree.
			inst := rt.newObject(rt.newTargetProto(proto))
			insto := rt.objPtr(inst)
			insto.setSlot(slotIntlLocale, rt.newString(tag))
			if initOpts != nil {
				if e := initOpts(insto, arg(args, 1), requested); e != nil {
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

	defineService("Collator", false, func(inst *object, options Value, requested []string) *ThrowError {
		c, e := rt.initCollatorOptions(options, requested)
		if e != nil {
			return e
		}
		inst.setSlot(slotIntlCollatorOpts, rt.newString(c.String()))
		return nil
	}, func(po *object) {
		// compare is an accessor returning a bound comparison function.
		getter := rt.newNativeFunc("get compare", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			c, e := rt.requireCollator(this)
			if e != nil {
				return mkundef(), e
			}
			return rt.newNativeFunc("", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
				a, e := rt.toStringValue(arg(args, 0))
				if e != nil {
					return mkundef(), e
				}
				b, e := rt.toStringValue(arg(args, 1))
				if e != nil {
					return mkundef(), e
				}
				return mknum(float64(c.compare(rt.strGo(a), rt.strGo(b)))), nil
			}), nil
		})
		po.defineAccessor("compare", getter, mkundef(), true, false, attrConfigurable)
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			c, e := rt.requireCollator(this)
			if e != nil {
				return mkundef(), e
			}
			o := rt.newPlainObject()
			oo := rt.objPtr(o)
			oo.defineOwn("locale", rt.newString(c.tag), attrDefault)
			oo.defineOwn("usage", rt.newString(c.usage), attrDefault)
			oo.defineOwn("sensitivity", rt.newString(c.sensitivity), attrDefault)
			oo.defineOwn("ignorePunctuation", mkbool(c.ignorePunct), attrDefault)
			oo.defineOwn("collation", rt.newString(c.collation), attrDefault)
			oo.defineOwn("numeric", mkbool(c.numeric), attrDefault)
			oo.defineOwn("caseFirst", rt.newString(c.caseFirst), attrDefault)
			return o, nil
		})
	})
	defineService("NumberFormat", false, func(inst *object, options Value, requested []string) *ThrowError {
		n, e := rt.initNumberOptions(options, requested)
		if e != nil {
			return e
		}
		inst.setSlot(slotIntlNumberOpts, rt.newString(n.String()))
		return nil
	}, func(po *object) {
		getter := rt.newNativeFunc("get format", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			li := rt.intlLocaleOf(this)
			opts, e := rt.requireNumberFormat(this)
			if e != nil {
				return mkundef(), e
			}
			return rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
				n, e := rt.toNumber(arg(args, 0))
				if e != nil {
					return mkundef(), e
				}
				return rt.newString(rt.formatNumberWith(opts, li, n)), nil
			}), nil
		})
		po.defineAccessor("format", getter, mkundef(), true, false, attrConfigurable)
		rt.defMethod(po, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			opts, e := rt.requireNumberFormat(this)
			if e != nil {
				return mkundef(), e
			}
			v, e := rt.toNumber(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			return rt.formatNumberParts(opts, rt.intlLocaleOf(this), v), nil
		})
		// The key order is the specification's and a test reads it back with
		// Reflect.ownKeys.
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			n, e := rt.requireNumberFormat(this)
			if e != nil {
				return mkundef(), e
			}
			o := rt.newPlainObject()
			oo := rt.objPtr(o)
			str := func(k, v string) {
				if v != "" {
					oo.defineOwn(k, rt.newString(v), attrDefault)
				}
			}
			oo.defineOwn("locale", rt.newString(n.tag), attrDefault)
			oo.defineOwn("numberingSystem", rt.newString("latn"), attrDefault)
			oo.defineOwn("style", rt.newString(n.style), attrDefault)
			str("currency", n.currency)
			str("currencyDisplay", n.currencyDisplay)
			str("currencySign", n.currencySign)
			str("unit", n.unit)
			str("unitDisplay", n.unitDisplay)
			oo.defineOwn("minimumIntegerDigits", mknum(float64(n.digits.minInt)), attrDefault)
			if n.digits.maxSig > 0 {
				oo.defineOwn("minimumSignificantDigits", mknum(float64(n.digits.minSig)), attrDefault)
				oo.defineOwn("maximumSignificantDigits", mknum(float64(n.digits.maxSig)), attrDefault)
			} else {
				oo.defineOwn("minimumFractionDigits", mknum(float64(n.digits.minFrac)), attrDefault)
				oo.defineOwn("maximumFractionDigits", mknum(float64(n.digits.maxFrac)), attrDefault)
			}
			if n.useGrouping == "" {
				oo.defineOwn("useGrouping", mkbool(false), attrDefault)
			} else {
				oo.defineOwn("useGrouping", rt.newString(n.useGrouping), attrDefault)
			}
			oo.defineOwn("notation", rt.newString(n.notation), attrDefault)
			str("compactDisplay", n.compactDisplay)
			oo.defineOwn("signDisplay", rt.newString(n.signDisplay), attrDefault)
			oo.defineOwn("roundingIncrement", mknum(1), attrDefault)
			oo.defineOwn("roundingMode", rt.newString("halfExpand"), attrDefault)
			oo.defineOwn("roundingPriority", rt.newString("auto"), attrDefault)
			oo.defineOwn("trailingZeroDisplay", rt.newString("auto"), attrDefault)
			return o, nil
		})
	})
	defineService("DateTimeFormat", false, func(inst *object, options Value, _ []string) *ThrowError {
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

	defineService("PluralRules", true, func(inst *object, options Value, requested []string) *ThrowError {
		// localeMatcher is read and discarded: both matchers answer the same
		// here, but reading it is observable through a getter on the bag.
		if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
			return e
		}
		kind, _, e := rt.intlStringOption(options, "type", []string{"cardinal", "ordinal"})
		if e != nil {
			return e
		}
		notation, _, e := rt.intlStringOption(options, "notation",
			[]string{"standard", "scientific", "engineering", "compact"})
		if e != nil {
			return e
		}
		p, e := rt.intlDigitOptions(options, 0, 3)
		if e != nil {
			return e
		}
		compact, _, e := rt.intlStringOption(options, "compactDisplay", []string{"short", "long"})
		if e != nil {
			return e
		}
		p.ordinal = kind == "ordinal"
		if notation != "" {
			p.notation = notation
		}
		if p.notation == "compact" {
			p.compact = "short"
			if compact != "" {
				p.compact = compact
			}
		}
		// The plural rules cover every CLDR locale, so the tag they match on is
		// the one that was asked for -- not the one localeTable fell back to,
		// which knows only about the locales this engine can FORMAT. Arabic has
		// six plural categories and no entry in that table.
		if len(requested) > 0 {
			p.tag = requested[0]
		}
		inst.setSlot(slotIntlPluralOpts, rt.newString(p.String()))
		return nil
	}, func(po *object) {
		rt.defMethod(po, "select", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			n, e := rt.toNumber(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			p, e2 := rt.requirePluralRules(this)
			if e2 != nil {
				return mkundef(), e2
			}
			return rt.newString(p.selectForm(pluralTag(p.tag), n)), nil
		})
		rt.defMethod(po, "selectRange", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if arg(args, 0).IsUndefined() || arg(args, 1).IsUndefined() {
				return mkundef(), rt.typeError("selectRange requires two numbers")
			}
			lo, e := rt.toNumber(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			hi, e := rt.toNumber(arg(args, 1))
			if e != nil {
				return mkundef(), e
			}
			if math.IsNaN(lo) || math.IsNaN(hi) {
				return mkundef(), rt.rangeError("selectRange bounds must not be NaN")
			}
			// CLDR carries a separate table of range rules that x/text does not
			// expose. Where both ends agree the range agrees with them, which is
			// what the range rules say for most locales; where they differ this
			// answers "other", which is the fallback the rules themselves use.
			p, e2 := rt.requirePluralRules(this)
			if e2 != nil {
				return mkundef(), e2
			}
			tag := pluralTag(p.tag)
			a, b := p.selectForm(tag, lo), p.selectForm(tag, hi)
			if a == b {
				return rt.newString(a), nil
			}
			return rt.newString("other"), nil
		})
		// The key order is the specification's and a test reads it back with
		// Reflect.ownKeys. Note there is no numberingSystem here, which is why
		// this does not go through intlResolvedOptions.
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			p, e := rt.requirePluralRules(this)
			if e != nil {
				return mkundef(), e
			}
			o := rt.newPlainObject()
			oo := rt.objPtr(o)
			kind := "cardinal"
			if p.ordinal {
				kind = "ordinal"
			}
			oo.defineOwn("locale", rt.newString(p.tag), attrDefault)
			oo.defineOwn("type", rt.newString(kind), attrDefault)
			oo.defineOwn("notation", rt.newString(p.notation), attrDefault)
			oo.defineOwn("minimumIntegerDigits", mknum(float64(p.minInt)), attrDefault)
			if p.maxSig > 0 {
				oo.defineOwn("minimumSignificantDigits", mknum(float64(p.minSig)), attrDefault)
				oo.defineOwn("maximumSignificantDigits", mknum(float64(p.maxSig)), attrDefault)
			} else {
				oo.defineOwn("minimumFractionDigits", mknum(float64(p.minFrac)), attrDefault)
				oo.defineOwn("maximumFractionDigits", mknum(float64(p.maxFrac)), attrDefault)
			}
			oo.defineOwn("pluralCategories",
				rt.newArrayOfStrings(p.categories(pluralTag(p.tag))), attrDefault)
			oo.defineOwn("roundingIncrement", mknum(1), attrDefault)
			oo.defineOwn("roundingMode", rt.newString("halfExpand"), attrDefault)
			oo.defineOwn("roundingPriority", rt.newString("auto"), attrDefault)
			oo.defineOwn("trailingZeroDisplay", rt.newString("auto"), attrDefault)
			if p.notation == "compact" {
				oo.defineOwn("compactDisplay", rt.newString(p.compact), attrDefault)
			}
			return o, nil
		})
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

// intlLocaleTag is the raw tag an instance resolved to, for the services whose
// data comes from somewhere other than localeTable.
func (rt *Runtime) intlLocaleTag(this Value) string {
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlLocale); v.IsString() {
			return rt.strGo(v)
		}
	}
	return defaultLocale
}
