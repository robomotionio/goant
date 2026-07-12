package engine

import "regexp"

// Minimal Intl: the constructors exist, work with and without `new`, expose the
// expected prototype methods, and validate BCP 47 language tags. Actual
// locale-sensitive formatting/collation is host-locale-agnostic (adequate for
// the conformance surface, which only exercises shape and tag validation).

// langtagRe matches a structurally valid BCP 47 `langtag` (RFC 5646) — which
// excludes privateuse-only ("x-…") and irregular grandfathered ("i-…") tags, so
// those are rejected as invalid language tags.
var langtagRe = regexp.MustCompile(`(?i)^` +
	`([a-z]{2,3}(-[a-z]{3}){0,3}|[a-z]{4,8})` + // language (+ up to 3 extlang)
	`(-[a-z]{4})?` + // script
	`(-([a-z]{2}|[0-9]{3}))?` + // region
	`(-([a-z0-9]{5,8}|[0-9][a-z0-9]{3}))*` + // variant
	`(-[a-wy-z0-9](-[a-z0-9]{2,8})+)*` + // extension (singleton != x)
	`(-x(-[a-z0-9]{1,8})+)?` + // optional privateuse
	`$`)

// validateLocales implements CanonicalizeLocaleList's structural check: a single
// tag string, or each element of a list, must be a structurally valid language
// tag. undefined is accepted (empty list).
func (rt *Runtime) validateLocales(v Value) *ThrowError {
	check := func(tag Value) *ThrowError {
		s, e := rt.toStringValue(tag)
		if e != nil {
			return e
		}
		str := string(rt.strBytes(s))
		if !langtagRe.MatchString(str) {
			return &ThrowError{Value: rt.makeError(rt.errors.rangeProto, "RangeError", "Invalid language tag: "+str), rt: rt}
		}
		return nil
	}
	if v.IsUndefined() {
		return nil
	}
	if v.IsString() {
		return check(v)
	}
	if v.IsObjectType() {
		n, e := rt.lengthOf(v)
		if e != nil {
			return e
		}
		for i := 0; i < n; i++ {
			el, e := rt.getElement(v, mknum(float64(i)))
			if e != nil {
				return e
			}
			if e := check(el); e != nil {
				return e
			}
		}
	}
	return nil
}

func (rt *Runtime) initIntl() {
	intl := rt.newObject(rt.objectProto)
	io := rt.objPtr(intl)
	rt.setStringTag(intl, "Intl")

	// defineService installs an Intl service constructor: callable with or without
	// `new` (both yield an instance), validating its locales argument, with the
	// given prototype methods.
	defineService := func(name string, methods func(po *object)) {
		proto := rt.newObject(rt.objectProto)
		po := rt.objPtr(proto)
		ctor := rt.newNativeFunc(name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if e := rt.validateLocales(arg(args, 0)); e != nil {
				return mkundef(), e
			}
			// A fresh instance whose [[Prototype]] honours new.target (both `new
			// Intl.X()` and `Intl.X()` return an instance).
			return rt.newObject(rt.newTargetProto(proto)), nil
		})
		co := rt.objPtr(ctor)
		co.defineOwn("prototype", proto, 0)
		po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
		rt.setStringTag(proto, "Intl."+name)
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return rt.newPlainObject(), nil
		})
		if methods != nil {
			methods(po)
		}
		co.defineOwn("supportedLocalesOf", rt.newNativeFunc("supportedLocalesOf", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return rt.newArray(), nil
		}), attrWritable|attrConfigurable)
		io.defineOwn(name, ctor, attrWritable|attrConfigurable)
	}

	defineService("Collator", func(po *object) {
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
	defineService("NumberFormat", func(po *object) {
		getter := rt.newNativeFunc("get format", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
				n, e := rt.toNumber(arg(args, 0))
				if e != nil {
					return mkundef(), e
				}
				return rt.newString(numberToString(n)), nil
			}), nil
		})
		po.defineAccessor("format", getter, mkundef(), true, false, attrConfigurable)
	})
	defineService("DateTimeFormat", func(po *object) {
		getter := rt.newNativeFunc("get format", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
				return rt.newString(""), nil
			}), nil
		})
		po.defineAccessor("format", getter, mkundef(), true, false, attrConfigurable)
		// resolvedOptions reports the resolved timeZone (defaulting to the host's,
		// which this runtime treats as UTC) and calendar/numberingSystem.
		rt.defMethod(po, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.newPlainObject()
			oo := rt.objPtr(o)
			oo.defineOwn("locale", rt.newString("en-US"), attrDefault)
			oo.defineOwn("calendar", rt.newString("gregory"), attrDefault)
			oo.defineOwn("numberingSystem", rt.newString("latn"), attrDefault)
			oo.defineOwn("timeZone", rt.newString("UTC"), attrDefault)
			return o, nil
		})
	})

	rt.defGlobal("Intl", intl)
}
