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

		// The remaining options are read in this order, and the order is
		// observable: constructor-getter-order.js installs a getter on each and
		// records the sequence.
		if e := rt.keywordOption(t, options, "calendar", "ca", nil); e != nil {
			return mkundef(), e
		}
		if e := rt.keywordOption(t, options, "collation", "co", nil); e != nil {
			return mkundef(), e
		}
		// firstDayOfWeek is an ordinary keyword type, except that the numbers
		// 0 through 7 spell the weekday names -u-fw uses.
		if got, present, e := rt.intlStringOption(options, "firstDayOfWeek", nil); e != nil {
			return mkundef(), e
		} else if present {
			day := asciiLower(got)
			if d := weekdayKeyword(day); d != "" {
				day = d
			}
			if !isUnicodeType(day) {
				return mkundef(), rt.rangeError("Invalid value " + got + " for option firstDayOfWeek")
			}
			t.setUKeyword("fw", day)
		}
		if e := rt.keywordOption(t, options, "hourCycle", "hc", []string{"h11", "h12", "h23", "h24"}); e != nil {
			return mkundef(), e
		}
		if e := rt.keywordOption(t, options, "caseFirst", "kf", []string{"upper", "lower", "false"}); e != nil {
			return mkundef(), e
		}
		// numeric is a boolean, spelled in the tag as the presence or absence
		// of a type on -u-kn.
		if !options.IsUndefined() {
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
		if e := rt.keywordOption(t, options, "numberingSystem", "nu", nil); e != nil {
			return mkundef(), e
		}

		t.canonicalize()
		np, e := rt.newTargetProtoE(proto)
		if e != nil {
			return mkundef(), e
		}
		inst := rt.newObject(np)
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
	// The Locale-info methods. What each of them can honestly say is limited by
	// the CLDR tables this engine does not carry yet: the calendar and
	// numbering system are the only ones implemented anywhere in it, and the
	// week and text data below is the rule plus the handful of exceptions that
	// matter, not the whole per-locale table.
	list := func(name string, f func(t *langTag) []string) {
		rt.defMethod(po, name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			t, e := rt.requireLocale(this)
			if e != nil {
				return mkundef(), e
			}
			return rt.newArrayOfStrings(f(t)), nil
		})
	}
	// A tag that names one of these outright is not asking what the locale
	// prefers -- it has already chosen, so the list is that one value alone.
	preferred := func(t *langTag, key string, rest []string) []string {
		if v, ok := t.uKeyword(key); ok && v != "" {
			return []string{v}
		}
		return rest
	}
	list("getCalendars", func(t *langTag) []string { return preferred(t, "ca", []string{"gregory"}) })
	list("getCollations", func(t *langTag) []string { return preferred(t, "co", []string{"emoji", "eor"}) })
	list("getNumberingSystems", func(t *langTag) []string { return preferred(t, "nu", []string{"latn"}) })
	list("getHourCycles", func(t *langTag) []string {
		return preferred(t, "hc", []string{hourCycleForLocale(t)})
	})
	rt.defMethod(po, "getTimeZones", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		t, e := rt.requireLocale(this)
		if e != nil {
			return mkundef(), e
		}
		// Undefined for a locale with no region, which is the specification's
		// answer rather than an empty list: "the zones of nowhere" is not a
		// question with an answer.
		if t.region == "" {
			return mkundef(), nil
		}
		return rt.newArrayOfStrings(zonesForRegion(t.region)), nil
	})
	rt.defMethod(po, "getTextInfo", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		t, e := rt.requireLocale(this)
		if e != nil {
			return mkundef(), e
		}
		o := rt.newPlainObject()
		rt.objPtr(o).defineOwn("direction", rt.newString(textDirection(t.lang)), attrDefault)
		return o, nil
	})
	rt.defMethod(po, "getWeekInfo", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		t, e := rt.requireLocale(this)
		if e != nil {
			return mkundef(), e
		}
		first, weekend := weekInfo(effectiveRegion(t))
		// -u-fw names the first day outright, whatever the region would say.
		if v, has := t.uKeyword("fw"); has {
			if d := weekdayNumber(v); d != 0 {
				first = d
			}
		}
		o := rt.newPlainObject()
		oo := rt.objPtr(o)
		oo.defineOwn("firstDay", mknum(float64(first)), attrDefault)
		arr := rt.newArray()
		ao := rt.objPtr(arr)
		for i, d := range weekend {
			rt.arraySet(ao, uint32(i), mknum(float64(d)))
		}
		oo.defineOwn("weekend", arr, attrDefault)
		return o, nil
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
	// A keyword getter answers undefined when the keyword is absent and its
	// value when it is there, which for a keyword written bare is "".
	kw := func(name, key string) {
		getter(name, func(t *langTag) Value {
			v, ok := t.uKeyword(key)
			if !ok {
				return mkundef()
			}
			return rt.newString(v)
		})
	}
	kw("calendar", "ca")
	kw("collation", "co")
	kw("hourCycle", "hc")
	kw("caseFirst", "kf")
	kw("numberingSystem", "nu")
	kw("firstDayOfWeek", "fw")
	// numeric is the one boolean keyword: "-u-kn" and "-u-kn-true" are true,
	// "-u-kn-false" is false, and no keyword at all is false.
	getter("numeric", func(t *langTag) Value {
		v, ok := t.uKeyword("kn")
		return mkbool(ok && (v == "" || v == "true"))
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

// effectiveRegion is the region a locale's calendar and week data comes from,
// in RegionPreference's priority order: the -u-rg keyword overrides everything,
// then the tag's own region, then a -u-sd subdivision's first two letters --
// a subdivision is only consulted when the tag did not name a region at all,
// because "the state of Inuvik in Japan" is a contradiction and the country
// wins it. Failing all of those it is what likely subtags infers, and failing
// that the world.
func effectiveRegion(t *langTag) string {
	if v, has := t.uKeyword("rg"); has && len(v) >= 2 {
		return asciiUpper(v[:2])
	}
	if t.region != "" {
		return t.region
	}
	if v, has := t.uKeyword("sd"); has && len(v) >= 2 {
		return asciiUpper(v[:2])
	}
	if m, ok := t.maximized(); ok && m.region != "" {
		return m.region
	}
	return "001"
}

// weekdayNumber maps a -u-fw type to its ISO weekday number, 0 for a value
// that is not a weekday at all.
func weekdayNumber(s string) int {
	for i, d := range []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"} {
		if s == d {
			return i + 1
		}
	}
	return 0
}

// hourCycleForRegion is the clock a region reads. Twelve hours is the
// English-speaking world and a scattering of others; twenty-four is the rest,
// which is why the twelve-hour list is the one written out.
func hourCycleForRegion(region string) string {
	switch region {
	case "US", "GB", "CA", "AU", "NZ", "IE", "IN", "PH", "PK", "BD", "EG",
		"MX", "CO", "SA", "MY", "NG", "KE", "ZA", "GR", "KR", "JP":
		return "h12"
	}
	return "h23"
}

// hourCycleForLocale reads the language before the region, because the clock
// travels with the language rather than the country: Quebec writes 14:00 and
// Ontario writes 2 PM, and both of them are CA.
func hourCycleForLocale(t *langTag) string {
	switch t.lang {
	case "fr", "de", "ru", "pl", "cs", "sk", "hu", "ro", "bg", "uk", "lt",
		"lv", "et", "sl", "hr", "sr", "ku", "fa", "vi", "id", "tr", "nl",
		"sv", "nb", "no", "da", "fi", "is", "ca", "eu", "gl", "sq", "mk",
		"be", "hy", "ka", "kk", "uz", "az", "mn", "th", "zh", "ja":
		return "h23"
	}
	return hourCycleForRegion(effectiveRegion(t))
}

// textDirection is the writing direction of a language. The right-to-left
// scripts are a closed and short list; everything else is left to right.
func textDirection(lang string) string {
	switch lang {
	case "ar", "arc", "az", "ckb", "dv", "fa", "he", "iw", "ku", "ps", "sd",
		"ug", "ur", "yi", "ji":
		return "rtl"
	}
	return "ltr"
}

// weekInfo is CLDR's week data: the first day of the week, which days are the
// weekend, and how many days of a week must fall in a year for it to count as
// that year's first. The values are ISO weekday numbers, so Monday is 1 and
// Sunday is 7. Monday-start with a Saturday-Sunday weekend is the rule; the
// regions listed are the exceptions that a European or American script is
// likely to meet.
func weekInfo(region string) (first int, weekend []int) {
	switch region {
	case "US", "CA", "JP", "IL", "PH", "BR", "MX", "KR", "TW", "ZA", "HK", "MO":
		first = 7
	case "AE", "AF", "BH", "DZ", "EG", "IQ", "IR", "JO", "KW", "LY", "OM",
		"QA", "SA", "SD", "SY", "YE":
		first = 6
	case "MV":
		first = 5
	default:
		first = 1
	}
	switch region {
	case "AE", "BH", "DZ", "EG", "IL", "IQ", "JO", "KW", "LY", "OM", "QA",
		"SA", "SD", "SY", "YE":
		weekend = []int{5, 6}
	case "AF", "IR":
		weekend = []int{5}
	case "IN", "NP":
		weekend = []int{7}
	default:
		weekend = []int{6, 7}
	}
	return
}

// zonesForRegion is the IANA zones of an ISO 3166 region, from the database's
// own zone.tab (see tools/gencldr). An identifier names a continent and a city
// rather than a country, so this cannot be read off the names -- Europe/Zurich
// is Swiss and Europe/Busingen is German, and nothing in either string says so.
func zonesForRegion(region string) []string {
	zones, ok := cldrRegionZones()[region]
	if !ok {
		return nil
	}
	return strings.Split(zones, " ")
}
