package engine

import (
	"math"
	"strings"
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
	// The three constructors that predate ES2015 have a normative-optional
	// legacy behaviour: called as a function on an object that is already an
	// instance, they hang the new formatter off it under this symbol and hand
	// the object back. Web content depends on it, which is why it is still
	// specified at all.
	legacySym := rt.newSymbol(rt.newString("IntlLegacyConstructedSymbol"))
	rt.intlLegacySym = legacySym

	defineService := func(name string, requireNew bool, initOpts func(inst *object, options Value, requested []string) *ThrowError, methods func(po *object)) {
		proto := rt.newObject(rt.objectProto)
		po := rt.objPtr(proto)
		var ctorSelf Value
		// The declared length is the number of arguments before the first
		// optional one, which for DisplayNames is both of them: it cannot name
		// anything without a type, and the type lives in the options.
		length := 0
		if name == "DisplayNames" {
			length = 2
		}
		ctor := rt.newNativeFunc(name, length, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if requireNew && !rt.constructing() {
				return mkundef(), rt.typeError("Constructor Intl." + name + " requires 'new'")
			}
			// GetOptionsObject: some constructors take an object or nothing,
			// and a primitive is a mistake rather than a bag with no
			// properties. The others use CoerceOptionsToObject and wrap it --
			// which is not a matter of age, RelativeTimeFormat and PluralRules
			// coerce and Segmenter does not, so the list is written out.
			if tagContains([]string{"ListFormat", "DisplayNames", "Segmenter", "DurationFormat"}, name) {
				if o := arg(args, 1); !o.IsUndefined() && !o.IsObjectType() {
					return mkundef(), rt.typeError("Options must be an object")
				}
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
			// newTarget.prototype is an observable [[Get]] and may throw; the
			// exception is the caller's, not something to fall back from.
			np, e := rt.newTargetProtoE(proto)
			if e != nil {
				return mkundef(), e
			}
			inst := rt.newObject(np)
			insto := rt.objPtr(inst)
			insto.setSlot(slotIntlLocale, rt.newString(tag))
			if initOpts != nil {
				if e := initOpts(insto, arg(args, 1), requested); e != nil {
					return mkundef(), e
				}
			}
			if !requireNew && !rt.constructing() && this.IsObjectType() {
				if has, _ := rt.ordinaryHasInstance(ctorSelf, this); has {
					if o := rt.objPtr(this); o != nil {
						o.defineOwnSymbol(legacySym.handle(), inst, 0)
						return this, nil
					}
				}
			}
			return inst, nil
		})
		ctorSelf = ctor
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
				n, digits, e := rt.intlNumericArg(arg(args, 0))
				if e != nil {
					return mkundef(), e
				}
				return rt.newString(rt.formatNumberOf(opts, li, n, digits)), nil
			}), nil
		})
		po.defineAccessor("format", getter, mkundef(), true, false, attrConfigurable)
		rt.defMethod(po, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			opts, e := rt.requireNumberFormat(this)
			if e != nil {
				return mkundef(), e
			}
			v, digits, e := rt.intlNumericArg(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			return rt.partsArray(numberPartsOf(opts, rt.intlLocaleOf(this), v, digits)), nil
		})
		numberRange := func(rt *Runtime, this Value, args []Value) ([]sourcedPart, *ThrowError) {
			opts, e := rt.requireNumberFormat(this)
			if e != nil {
				return nil, e
			}
			if arg(args, 0).IsUndefined() || arg(args, 1).IsUndefined() {
				return nil, rt.typeError("formatRange requires two numbers")
			}
			lo, loDigits, e := rt.intlNumericArg(arg(args, 0))
			if e != nil {
				return nil, e
			}
			hi, hiDigits, e := rt.intlNumericArg(arg(args, 1))
			if e != nil {
				return nil, e
			}
			if math.IsNaN(lo) || math.IsNaN(hi) {
				return nil, rt.rangeError("Range bounds must not be NaN")
			}
			li := rt.intlLocaleOf(this)
			return rangeParts(numberPartsOf(opts, li, lo, loDigits),
				numberPartsOf(opts, li, hi, hiDigits)), nil
		}
		rt.defMethod(po, "formatRange", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			parts, e := numberRange(rt, this, args)
			if e != nil {
				return mkundef(), e
			}
			return rt.newString(sourcedString(parts)), nil
		})
		rt.defMethod(po, "formatRangeToParts", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			parts, e := numberRange(rt, this, args)
			if e != nil {
				return mkundef(), e
			}
			return rt.sourcedPartsArray(parts), nil
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
			oo.defineOwn("numberingSystem", rt.newString(n.numbering), attrDefault)
			oo.defineOwn("style", rt.newString(n.style), attrDefault)
			str("currency", n.currency)
			str("currencyDisplay", n.currencyDisplay)
			str("currencySign", n.currencySign)
			str("unit", n.unit)
			str("unitDisplay", n.unitDisplay)
			oo.defineOwn("minimumIntegerDigits", mknum(float64(n.digits.minInt)), attrDefault)
			// Both sets are reported when both are in use, which is what a
			// rounding priority other than "auto" means -- including the one
			// compact notation picks for itself.
			if n.digits.maxSig == 0 || n.compactAuto {
				oo.defineOwn("minimumFractionDigits", mknum(float64(n.digits.minFrac)), attrDefault)
				oo.defineOwn("maximumFractionDigits", mknum(float64(n.digits.maxFrac)), attrDefault)
			}
			if n.digits.maxSig > 0 {
				oo.defineOwn("minimumSignificantDigits", mknum(float64(n.digits.minSig)), attrDefault)
				oo.defineOwn("maximumSignificantDigits", mknum(float64(n.digits.maxSig)), attrDefault)
			}
			if n.useGrouping == "" {
				oo.defineOwn("useGrouping", mkbool(false), attrDefault)
			} else {
				oo.defineOwn("useGrouping", rt.newString(n.useGrouping), attrDefault)
			}
			oo.defineOwn("notation", rt.newString(n.notation), attrDefault)
			str("compactDisplay", n.compactDisplay)
			oo.defineOwn("signDisplay", rt.newString(n.signDisplay), attrDefault)
			oo.defineOwn("roundingIncrement", mknum(float64(n.roundingIncr)), attrDefault)
			oo.defineOwn("roundingMode", rt.newString(n.roundingMode), attrDefault)
			oo.defineOwn("roundingPriority", rt.newString(n.roundingPriority), attrDefault)
			oo.defineOwn("trailingZeroDisplay", rt.newString(n.trailingZero), attrDefault)
			return o, nil
		})
	})
	defineService("DateTimeFormat", false, func(inst *object, options Value, requested []string) *ThrowError {
		// Everything is fixed at construction: an unknown zone or an illegal
		// combination of options is reported here rather than on some later
		// format() call in another file.
		d, e := rt.initDateTimeOptions(options, requested)
		if e != nil {
			return e
		}
		inst.setSlot(slotIntlTimeZone, rt.newString(d.timeZone))
		inst.setSlot(slotIntlDateTimeOpts, rt.newString(d.String()))
		return nil
	}, func(po *object) {
		instant := func(rt *Runtime, args []Value) (float64, *ThrowError) {
			// With no argument the spec formats the current time.
			ms := float64(time.Now().UnixMilli())
			if a := arg(args, 0); !a.IsUndefined() {
				n, e := rt.toNumber(a)
				if e != nil {
					return 0, e
				}
				ms = timeClip(n)
			}
			if math.IsNaN(ms) {
				return 0, rt.rangeError("Invalid time value")
			}
			return ms, nil
		}
		getter := rt.newNativeFunc("get format", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			d, e := rt.requireDateTimeFormat(this)
			if e != nil {
				return mkundef(), e
			}
			loc := zoneFor(d.timeZone)
			return rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
				ms, e := instant(rt, args)
				if e != nil {
					return mkundef(), e
				}
				var b strings.Builder
				for _, p := range d.dateTimeParts(msInZone(ms, loc)) {
					b.WriteString(p.val)
				}
				return rt.newString(b.String()), nil
			}), nil
		})
		po.defineAccessor("format", getter, mkundef(), true, false, attrConfigurable)
		rt.defMethod(po, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			d, e := rt.requireDateTimeFormat(this)
			if e != nil {
				return mkundef(), e
			}
			ms, e := instant(rt, args)
			if e != nil {
				return mkundef(), e
			}
			return rt.partsArray(d.dateTimeParts(msInZone(ms, zoneFor(d.timeZone)))), nil
		})
		dateRange := func(rt *Runtime, this Value, args []Value) ([]sourcedPart, *ThrowError) {
			d, e := rt.requireDateTimeFormat(this)
			if e != nil {
				return nil, e
			}
			if arg(args, 0).IsUndefined() || arg(args, 1).IsUndefined() {
				return nil, rt.typeError("formatRange requires two time values")
			}
			a, e := rt.toNumber(arg(args, 0))
			if e != nil {
				return nil, e
			}
			b, e := rt.toNumber(arg(args, 1))
			if e != nil {
				return nil, e
			}
			a, b = timeClip(a), timeClip(b)
			if math.IsNaN(a) || math.IsNaN(b) {
				return nil, rt.rangeError("Invalid time value")
			}
			loc := zoneFor(d.timeZone)
			return rangeParts(d.dateTimeParts(msInZone(a, loc)), d.dateTimeParts(msInZone(b, loc))), nil
		}
		rt.defMethod(po, "formatRange", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			parts, e := dateRange(rt, this, args)
			if e != nil {
				return mkundef(), e
			}
			return rt.newString(sourcedString(parts)), nil
		})
		rt.defMethod(po, "formatRangeToParts", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			parts, e := dateRange(rt, this, args)
			if e != nil {
				return mkundef(), e
			}
			return rt.sourcedPartsArray(parts), nil
		})
		// The key order is the specification's and a test reads it back.
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			d, e := rt.requireDateTimeFormat(this)
			if e != nil {
				return mkundef(), e
			}
			o := rt.newPlainObject()
			oo := rt.objPtr(o)
			oo.defineOwn("locale", rt.newString(d.tag), attrDefault)
			oo.defineOwn("calendar", rt.newString(d.calendar), attrDefault)
			oo.defineOwn("numberingSystem", rt.newString(d.numbering), attrDefault)
			oo.defineOwn("timeZone", rt.newString(d.timeZone), attrDefault)
			// hourCycle and hour12 are reported only by a formatter that shows
			// an hour: they say nothing about a date.
			if _, hasHour := d.comps["hour"]; hasHour || d.timeStyle != "" {
				hc := d.resolvedHourCycle()
				oo.defineOwn("hourCycle", rt.newString(hc), attrDefault)
				oo.defineOwn("hour12", mkbool(hc == "h11" || hc == "h12"), attrDefault)
			}
			if d.dateStyle != "" {
				oo.defineOwn("dateStyle", rt.newString(d.dateStyle), attrDefault)
			}
			if d.timeStyle != "" {
				oo.defineOwn("timeStyle", rt.newString(d.timeStyle), attrDefault)
			}
			for _, c := range dtComponents {
				v, ok := d.comps[c]
				if !ok {
					continue
				}
				if c == "fractionalSecondDigits" {
					oo.defineOwn(c, mknum(float64(d.fracDigits)), attrDefault)
					continue
				}
				oo.defineOwn(c, rt.newString(v), attrDefault)
			}
			return o, nil
		})
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
		// compactDisplay is read before the digit options here, which is the
		// order PluralRules lists and a getter can see.
		compact, _, e := rt.intlStringOption(options, "compactDisplay", []string{"short", "long"})
		if e != nil {
			return e
		}
		p, e := rt.intlDigitOptions(options, 0, 3)
		if e != nil {
			return e
		}
		if e := rt.intlRoundingOptions(&p, options); e != nil {
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
			oo.defineOwn("roundingIncrement", mknum(float64(p.roundingIncr)), attrDefault)
			oo.defineOwn("roundingMode", rt.newString(p.roundingMode), attrDefault)
			oo.defineOwn("roundingPriority", rt.newString(p.priority), attrDefault)
			oo.defineOwn("trailingZeroDisplay", rt.newString(p.trailingZero), attrDefault)
			if p.notation == "compact" {
				oo.defineOwn("compactDisplay", rt.newString(p.compact), attrDefault)
			}
			return o, nil
		})
	})

	defineService("ListFormat", true, func(inst *object, options Value, requested []string) *ThrowError {
		l, e := rt.initListOptions(options, requested)
		if e != nil {
			return e
		}
		inst.setSlot(slotIntlListOpts, rt.newString(l.String()))
		return nil
	}, func(po *object) {
		rt.defMethod(po, "format", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			l, e := rt.requireListFormat(this)
			if e != nil {
				return mkundef(), e
			}
			items, e := rt.listStrings(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			var b strings.Builder
			for _, p := range l.listParts(items) {
				b.WriteString(p.val)
			}
			return rt.newString(b.String()), nil
		})
		rt.defMethod(po, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			l, e := rt.requireListFormat(this)
			if e != nil {
				return mkundef(), e
			}
			items, e := rt.listStrings(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			return rt.partsArray(l.listParts(items)), nil
		})
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			l, e := rt.requireListFormat(this)
			if e != nil {
				return mkundef(), e
			}
			o := rt.newPlainObject()
			oo := rt.objPtr(o)
			oo.defineOwn("locale", rt.newString(l.tag), attrDefault)
			oo.defineOwn("type", rt.newString(l.kind), attrDefault)
			oo.defineOwn("style", rt.newString(l.style), attrDefault)
			return o, nil
		})
	})

	defineService("RelativeTimeFormat", true, func(inst *object, options Value, requested []string) *ThrowError {
		r, e := rt.initRelTimeOptions(options, requested)
		if e != nil {
			return e
		}
		inst.setSlot(slotIntlRelTimeOpts, rt.newString(r.String()))
		return nil
	}, func(po *object) {
		rt.defMethod(po, "format", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			r, e := rt.requireRelTimeFormat(this)
			if e != nil {
				return mkundef(), e
			}
			v, unit, e := rt.relTimeArgs(args)
			if e != nil {
				return mkundef(), e
			}
			var b strings.Builder
			for _, p := range r.relTimeParts(v, unit, rt.intlLocaleOf(this)) {
				b.WriteString(p.val)
			}
			return rt.newString(b.String()), nil
		})
		rt.defMethod(po, "formatToParts", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			r, e := rt.requireRelTimeFormat(this)
			if e != nil {
				return mkundef(), e
			}
			v, unit, e := rt.relTimeArgs(args)
			if e != nil {
				return mkundef(), e
			}
			return rt.unitPartsArray(r.relTimeParts(v, unit, rt.intlLocaleOf(this))), nil
		})
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			r, e := rt.requireRelTimeFormat(this)
			if e != nil {
				return mkundef(), e
			}
			o := rt.newPlainObject()
			oo := rt.objPtr(o)
			oo.defineOwn("locale", rt.newString(r.tag), attrDefault)
			oo.defineOwn("style", rt.newString(r.style), attrDefault)
			oo.defineOwn("numeric", rt.newString(r.numeric), attrDefault)
			oo.defineOwn("numberingSystem", rt.newString(r.numbering), attrDefault)
			return o, nil
		})
	})

	defineService("DisplayNames", true, func(inst *object, options Value, requested []string) *ThrowError {
		d, e := rt.initDisplayOptions(options, requested)
		if e != nil {
			return e
		}
		inst.setSlot(slotIntlDisplayOpts, rt.newString(d.String()))
		return nil
	}, func(po *object) {
		rt.defMethod(po, "of", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			d, e := rt.requireDisplayNames(this)
			if e != nil {
				return mkundef(), e
			}
			s, e := rt.toStringValue(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			code, ok := canonicalDisplayCode(d.kind, rt.strGo(s))
			if !ok {
				return mkundef(), rt.rangeError("Invalid " + d.kind + " code: " + rt.strGo(s))
			}
			// No name data, so every lookup misses and fallback decides.
			if d.fallback == "none" {
				return mkundef(), nil
			}
			return rt.newString(code), nil
		})
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			d, e := rt.requireDisplayNames(this)
			if e != nil {
				return mkundef(), e
			}
			o := rt.newPlainObject()
			oo := rt.objPtr(o)
			oo.defineOwn("locale", rt.newString(d.tag), attrDefault)
			oo.defineOwn("style", rt.newString(d.style), attrDefault)
			oo.defineOwn("type", rt.newString(d.kind), attrDefault)
			oo.defineOwn("fallback", rt.newString(d.fallback), attrDefault)
			if d.kind == "language" {
				oo.defineOwn("languageDisplay", rt.newString(d.langStyle), attrDefault)
			}
			return o, nil
		})
	})

	// Segments and its iterator are ordinary objects with their own
	// prototypes, built once and shared: `segment()` hands back an object
	// whose state is three slots and whose behaviour is all here.
	segIterProto := rt.newObject(rt.iteratorProto)
	rt.setStringTag(segIterProto, "Segmenter String Iterator")
	rt.defMethod(rt.objPtr(segIterProto), "next", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || !o.getSlot(slotSegmentsOpts).IsString() {
			return mkundef(), rt.typeError("not a Segment Iterator")
		}
		opts := parseSegmenterOptions(rt.strGo(o.getSlot(slotSegmentsOpts)))
		input := o.getSlot(slotSegmentsInput)
		units := rt.strUTF16(input)
		pos := int(o.getSlot(slotSegIterPos).Number())
		if pos >= len(units) {
			return rt.genResult(mkundef(), true), nil
		}
		end, wordLike := segmentEnd(units, pos, opts.granularity)
		o.setSlot(slotSegIterPos, mknum(float64(end)))
		return rt.genResult(rt.segmentData(input, units, pos, end, wordLike, opts.granularity), false), nil
	})

	segmentsProto := rt.newObject(rt.objectProto)
	spo := rt.objPtr(segmentsProto)
	rt.defMethod(spo, "containing", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || !o.getSlot(slotSegmentsOpts).IsString() {
			return mkundef(), rt.typeError("not a Segments object")
		}
		opts := parseSegmenterOptions(rt.strGo(o.getSlot(slotSegmentsOpts)))
		input := o.getSlot(slotSegmentsInput)
		units := rt.strUTF16(input)
		n, e := rt.toIntegerOrInfinity(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		if n < 0 || n >= float64(len(units)) {
			return mkundef(), nil
		}
		start, end, wordLike := segmentAt(units, int(n), opts.granularity)
		return rt.segmentData(input, units, start, end, wordLike, opts.granularity), nil
	})
	iterFn := rt.newNativeFunc("[Symbol.iterator]", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || !o.getSlot(slotSegmentsOpts).IsString() {
			return mkundef(), rt.typeError("not a Segments object")
		}
		it := rt.newObject(segIterProto)
		ito := rt.objPtr(it)
		ito.setSlot(slotSegmentsOpts, o.getSlot(slotSegmentsOpts))
		ito.setSlot(slotSegmentsInput, o.getSlot(slotSegmentsInput))
		ito.setSlot(slotSegIterPos, mknum(0))
		return it, nil
	})
	if rt.symIterator != 0 {
		spo.defineOwnSymbol(rt.symIterator.handle(), iterFn, attrWritable|attrConfigurable)
	}

	defineService("Segmenter", true, func(inst *object, options Value, requested []string) *ThrowError {
		sg, e := rt.initSegmenterOptions(options, requested)
		if e != nil {
			return e
		}
		inst.setSlot(slotIntlSegmenterOpts, rt.newString(sg.String()))
		return nil
	}, func(po *object) {
		rt.defMethod(po, "segment", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			sg, e := rt.requireSegmenter(this)
			if e != nil {
				return mkundef(), e
			}
			s, e := rt.toStringValue(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			seg := rt.newObject(segmentsProto)
			so := rt.objPtr(seg)
			so.setSlot(slotSegmentsOpts, rt.newString(sg.String()))
			so.setSlot(slotSegmentsInput, s)
			return seg, nil
		})
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			sg, e := rt.requireSegmenter(this)
			if e != nil {
				return mkundef(), e
			}
			o := rt.newPlainObject()
			oo := rt.objPtr(o)
			oo.defineOwn("locale", rt.newString(sg.tag), attrDefault)
			oo.defineOwn("granularity", rt.newString(sg.granularity), attrDefault)
			return o, nil
		})
	})

	defineService("DurationFormat", true, func(inst *object, options Value, requested []string) *ThrowError {
		d, e := rt.initDurationOptions(options, requested)
		if e != nil {
			return e
		}
		inst.setSlot(slotIntlDurationOpts, rt.newString(d.String()))
		return nil
	}, func(po *object) {
		rt.defMethod(po, "format", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			d, e := rt.requireDurationFormat(this)
			if e != nil {
				return mkundef(), e
			}
			rec, e := rt.durationRecord(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			var b strings.Builder
			for _, p := range rt.durationParts(d, rt.intlLocaleOf(this), rec) {
				b.WriteString(p.val)
			}
			return rt.newString(b.String()), nil
		})
		rt.defMethod(po, "formatToParts", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			d, e := rt.requireDurationFormat(this)
			if e != nil {
				return mkundef(), e
			}
			rec, e := rt.durationRecord(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			return rt.unitPartsArray(rt.durationParts(d, rt.intlLocaleOf(this), rec)), nil
		})
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			d, e := rt.requireDurationFormat(this)
			if e != nil {
				return mkundef(), e
			}
			o := rt.newPlainObject()
			oo := rt.objPtr(o)
			oo.defineOwn("locale", rt.newString(d.tag), attrDefault)
			oo.defineOwn("numberingSystem", rt.newString(d.numbering), attrDefault)
			oo.defineOwn("style", rt.newString(d.style), attrDefault)
			for i, unit := range durationUnits {
				oo.defineOwn(unit, rt.newString(d.unitStyle[i]), attrDefault)
				oo.defineOwn(unit+"Display", rt.newString(d.display[i]), attrDefault)
			}
			if d.fracDigits >= 0 {
				oo.defineOwn("fractionalDigits", mknum(float64(d.fracDigits)), attrDefault)
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
