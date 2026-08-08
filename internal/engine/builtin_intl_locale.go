package engine

// Intl.Locale: a language tag as an object, with the subtags and the Unicode
// extension keywords readable as properties.
//
// It is not an Intl service constructor -- it formats nothing. What it is, is
// the one place the tag grammar is exposed to a script, which makes it the
// thing that says out loud whether the engine agrees with the specification
// about what a locale is.

import "strings"

// localeBrand reads an Intl.Locale's [[Locale]] slot, which is also its brand.
func (rt *Runtime) localeBrand(v Value) (string, bool) {
	o := rt.objPtr(v)
	if o == nil {
		return "", false
	}
	s := o.getSlot(slotLocaleTag)
	if !s.IsString() {
		return "", false
	}
	return rt.strGo(s), true
}

// requireLocale is RequireInternalSlot([[InitializedLocale]]) for the getters,
// which are on the prototype and so reachable with any receiver at all.
func (rt *Runtime) requireLocale(this Value) (*langTag, *ThrowError) {
	tag, ok := rt.localeBrand(this)
	if !ok {
		return nil, rt.typeError("not an Intl.Locale")
	}
	t, ok := parseLangTag(tag)
	if !ok {
		return nil, rt.typeError("not an Intl.Locale")
	}
	return t, nil
}

// intlStringOption is ECMA-402 GetOption with type string: absent means the
// fallback, and a value outside `values` (when given) is a RangeError.
func (rt *Runtime) intlStringOption(options Value, name string, values []string) (string, bool, *ThrowError) {
	if options.IsUndefined() {
		return "", false, nil
	}
	v, e := rt.getField(options, name)
	if e != nil {
		return "", false, e
	}
	if v.IsUndefined() {
		return "", false, nil
	}
	s, e := rt.toStringValue(v)
	if e != nil {
		return "", false, e
	}
	got := rt.strGo(s)
	if values != nil && !tagContains(values, got) {
		return "", false, rt.rangeError("Invalid value " + got + " for option " + name)
	}
	return got, true, nil
}

// isUnicodeType matches the `type` production the -u- keyword values use:
// one or more alphanumeric subtags of three to eight characters.
func isUnicodeType(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, "-") {
		if len(part) < 3 || len(part) > 8 || !tagAlnum(part) {
			return false
		}
	}
	return true
}

// keywordOption reads one option that lands in the -u- extension: absent leaves
// whatever the tag already carried, and a value that is not a well-formed type
// is a RangeError rather than a silently dropped keyword.
func (rt *Runtime) keywordOption(t *langTag, options Value, name, key string, values []string) *ThrowError {
	got, present, e := rt.intlStringOption(options, name, values)
	if e != nil {
		return e
	}
	if !present {
		return nil
	}
	if values == nil && !isUnicodeType(got) {
		return rt.rangeError("Invalid value " + got + " for option " + name)
	}
	t.setUKeyword(key, asciiLower(got))
	return nil
}

func (rt *Runtime) initIntlLocale(intl *object) {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)

	ctor := rt.newNativeFunc("Locale", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !rt.constructing() {
			return mkundef(), rt.typeError("Constructor Intl.Locale requires 'new'")
		}
		tagArg := arg(args, 0)
		var tag string
		if s, ok := rt.localeBrand(tagArg); ok {
			tag = s
		} else if tagArg.IsString() || tagArg.IsObjectType() {
			s, e := rt.toStringValue(tagArg)
			if e != nil {
				return mkundef(), e
			}
			tag = rt.strGo(s)
		} else {
			return mkundef(), rt.typeError("Intl.Locale: tag must be a string or an object")
		}
		options := arg(args, 1)
		if options.IsNull() {
			return mkundef(), rt.typeError("Options must be an object")
		}

		t, ok := parseLangTag(tag)
		if !ok {
			return mkundef(), rt.rangeError("Incorrect locale information provided: " + tag)
		}

		// ApplyOptionsToTag: the language, script, region and variants options
		// replace the corresponding subtags before anything is canonicalised.
		if v, present, e := rt.intlStringOption(options, "language", nil); e != nil {
			return mkundef(), e
		} else if present {
			if !isLangSubtag(v) {
				return mkundef(), rt.rangeError("Invalid language option: " + v)
			}
			t.lang = asciiLower(v)
		}
		if v, present, e := rt.intlStringOption(options, "script", nil); e != nil {
			return mkundef(), e
		} else if present {
			if !isScriptSubtag(v) {
				return mkundef(), rt.rangeError("Invalid script option: " + v)
			}
			t.script = tagTitle(v)
		}
		if v, present, e := rt.intlStringOption(options, "region", nil); e != nil {
			return mkundef(), e
		} else if present {
			if !isRegionSubtag(v) {
				return mkundef(), rt.rangeError("Invalid region option: " + v)
			}
			t.region = asciiUpper(v)
		}
		if v, present, e := rt.intlStringOption(options, "variants", nil); e != nil {
			return mkundef(), e
		} else if present {
			vs := strings.Split(asciiLower(v), "-")
			seen := map[string]bool{}
			for _, s := range vs {
				if !isVariantSubtag(s) || seen[s] {
					return mkundef(), rt.rangeError("Invalid variants option: " + v)
				}
				seen[s] = true
			}
			t.variants = vs
		}

		if e := rt.keywordOption(t, options, "calendar", "ca", nil); e != nil {
			return mkundef(), e
		}
		if e := rt.keywordOption(t, options, "collation", "co", nil); e != nil {
			return mkundef(), e
		}
		if e := rt.keywordOption(t, options, "hourCycle", "hc", []string{"h11", "h12", "h23", "h24"}); e != nil {
			return mkundef(), e
		}
		if e := rt.keywordOption(t, options, "caseFirst", "kf", []string{"upper", "lower", "false"}); e != nil {
			return mkundef(), e
		}
		if e := rt.keywordOption(t, options, "numberingSystem", "nu", nil); e != nil {
			return mkundef(), e
		}
		// firstDayOfWeek accepts a weekday name or the numbers 1-7, and is
		// stored as the name the -u-fw keyword uses.
		if !options.IsUndefined() {
			v, e := rt.getField(options, "firstDayOfWeek")
			if e != nil {
				return mkundef(), e
			}
			if !v.IsUndefined() {
				s, e := rt.toStringValue(v)
				if e != nil {
					return mkundef(), e
				}
				day := weekdayKeyword(asciiLower(rt.strGo(s)))
				if day == "" {
					return mkundef(), rt.rangeError("Invalid firstDayOfWeek option: " + rt.strGo(s))
				}
				t.setUKeyword("fw", day)
			}
			// numeric is a boolean, spelled in the tag as the presence or
			// absence of a type on -u-kn.
			nv, e := rt.getField(options, "numeric")
			if e != nil {
				return mkundef(), e
			}
			if !nv.IsUndefined() {
				if rt.toBoolean(nv) {
					t.setUKeyword("kn", "true")
				} else {
					t.setUKeyword("kn", "false")
				}
			}
		}

		t.canonicalize()
		inst := rt.newObject(rt.newTargetProto(proto))
		rt.objPtr(inst).setSlot(slotLocaleTag, rt.newString(t.String()))
		return inst, nil
	})

	co := rt.objPtr(ctor)
	co.defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.setStringTag(proto, "Intl.Locale")

	rt.defMethod(po, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		t, e := rt.requireLocale(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(t.String()), nil
	})
	rt.defMethod(po, "maximize", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.localeDerive(this, proto, (*langTag).maximized)
	})
	rt.defMethod(po, "minimize", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.localeDerive(this, proto, (*langTag).minimized)
	})

	getter := func(name string, f func(t *langTag) Value) {
		g := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			t, e := rt.requireLocale(this)
			if e != nil {
				return mkundef(), e
			}
			return f(t), nil
		})
		po.defineAccessor(name, g, mkundef(), true, false, attrConfigurable)
	}
	str := func(s string) Value {
		if s == "" {
			return mkundef()
		}
		return rt.newString(s)
	}
	getter("baseName", func(t *langTag) Value { return rt.newString(t.languageID()) })
	getter("language", func(t *langTag) Value { return rt.newString(t.lang) })
	getter("script", func(t *langTag) Value { return str(t.script) })
	getter("region", func(t *langTag) Value { return str(t.region) })
	getter("variants", func(t *langTag) Value { return str(strings.Join(t.variants, "-")) })
	kw := func(name, key string) {
		getter(name, func(t *langTag) Value {
			v, _ := t.uKeyword(key)
			return str(v)
		})
	}
	kw("calendar", "ca")
	kw("collation", "co")
	kw("hourCycle", "hc")
	kw("caseFirst", "kf")
	kw("numberingSystem", "nu")
	getter("firstDayOfWeek", func(t *langTag) Value {
		v, ok := t.uKeyword("fw")
		if !ok {
			return mkundef()
		}
		return str(v)
	})
	getter("numeric", func(t *langTag) Value {
		v, ok := t.uKeyword("kn")
		return mkbool(ok && v == "true")
	})

	intl.defineOwn("Locale", ctor, attrWritable|attrConfigurable)
}

// localeDerive builds a new Intl.Locale from a transformation of this one,
// which is what maximize and minimize both are.
func (rt *Runtime) localeDerive(this, proto Value, f func(*langTag) (*langTag, bool)) (Value, *ThrowError) {
	t, e := rt.requireLocale(this)
	if e != nil {
		return mkundef(), e
	}
	out, ok := f(t)
	if !ok {
		out = t
	}
	out.canonicalize()
	inst := rt.newObject(proto)
	rt.objPtr(inst).setSlot(slotLocaleTag, rt.newString(out.String()))
	return inst, nil
}

// weekdayKeyword maps the firstDayOfWeek option's accepted spellings onto the
// -u-fw type. The numbers are ISO-8601 weekday numbers, so 1 is Monday.
func weekdayKeyword(s string) string {
	switch s {
	case "mon", "1":
		return "mon"
	case "tue", "2":
		return "tue"
	case "wed", "3":
		return "wed"
	case "thu", "4":
		return "thu"
	case "fri", "5":
		return "fri"
	case "sat", "6":
		return "sat"
	case "sun", "7", "0":
		return "sun"
	}
	return ""
}
