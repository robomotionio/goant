package engine

// Intl.Collator, over golang.org/x/text/collate.
//
// The comparison used to be a byte-order fallback, which gets the easy cases
// right and the interesting ones wrong: it reported ö (U+00F6) and ö (o + a
// combining diaeresis) as different strings, when they are the same string
// under any normalisation and every collator in the world sorts them together.
// x/text/collate implements the Unicode Collation Algorithm over CLDR's
// tailorings, which is the thing that has to be right here.

import (
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// collatorOptions is a Collator's resolved state, stored on the instance as a
// string for the same reason PluralRules stores its own: slots hold Values.
type collatorOptions struct {
	tag         string
	usage       string // "sort" or "search"
	sensitivity string // "base", "accent", "case", "variant"
	ignorePunct bool
	collation   string // the -u-co- type, or "default"
	numeric     bool
	caseFirst   string // "upper", "lower", "false"
}

func defaultCollatorOptions() collatorOptions {
	return collatorOptions{tag: defaultLocale, usage: "sort", sensitivity: "variant",
		collation: "default", caseFirst: "false"}
}

func (c collatorOptions) String() string {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	return strings.Join([]string{c.usage, c.sensitivity, b(c.ignorePunct), c.collation,
		b(c.numeric), c.caseFirst, c.tag}, ",")
}

func parseCollatorOptions(s string) collatorOptions {
	f := strings.Split(s, ",")
	if len(f) != 7 {
		return defaultCollatorOptions()
	}
	return collatorOptions{usage: f[0], sensitivity: f[1], ignorePunct: f[2] == "1",
		collation: f[3], numeric: f[4] == "1", caseFirst: f[5], tag: f[6]}
}

// collator builds the x/text collator this instance denotes. Sensitivity is
// the interesting mapping: it says which differences count, and the library
// spells the same idea as which differences to ignore.
func (c collatorOptions) collator() *collate.Collator {
	var opts []collate.Option
	switch c.sensitivity {
	case "base":
		opts = append(opts, collate.Loose)
	case "accent":
		opts = append(opts, collate.IgnoreCase)
	case "case":
		opts = append(opts, collate.IgnoreDiacritics)
	}
	if c.numeric {
		opts = append(opts, collate.Numeric)
	}
	// caseFirst has no x/text option: the library orders case by the locale's
	// own tailoring and does not expose an override. It is reported through
	// resolvedOptions and does not change the comparison, which is a smaller
	// lie than reordering by hand would be.
	t, err := language.Parse(c.tag)
	if err != nil {
		t = language.Und
	}
	return collate.New(t, opts...)
}

func (c collatorOptions) compare(a, b string) int {
	if c.ignorePunct {
		a, b = stripPunctuation(a), stripPunctuation(b)
	}
	return c.collator().CompareString(a, b)
}

// stripPunctuation drops the characters ignorePunctuation is about. CLDR calls
// them "variable" and defines the set by collation weight; this is the ASCII
// and general-punctuation approximation, which covers what a script that sets
// the option is actually asking for.
func stripPunctuation(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80 && strings.ContainsRune(" !\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", r):
			continue
		case r >= 0x2000 && r <= 0x206F: // general punctuation
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// requireCollator is RequireInternalSlot([[InitializedCollator]]).
func (rt *Runtime) requireCollator(this Value) (collatorOptions, *ThrowError) {
	this = rt.unwrapLegacyIntl(this, slotIntlCollatorOpts)
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlCollatorOpts); v.IsString() {
			return parseCollatorOptions(rt.strGo(v)), nil
		}
	}
	return collatorOptions{}, rt.typeError("not an Intl.Collator")
}

// unwrapLegacyIntl follows %IntlLegacyConstructedSymbol% when it is there:
// an object the constructor was CALLED on carries the real formatter under it,
// and every method has to look through.
//
// The read is an ordinary [[Get]] rather than an own-property lookup, because
// the specification says so and a Proxy can see the difference -- a test wraps
// the object and asserts the symbol went through its trap.
func (rt *Runtime) unwrapLegacyIntl(this Value, brand internalSlot) Value {
	o := rt.objPtr(this)
	if o == nil || rt.intlLegacySym == 0 {
		return this
	}
	if o.getSlot(brand).IsString() {
		return this // already the formatter itself
	}
	v, e := rt.getFieldSymbol(this, rt.intlLegacySym.handle())
	if e == nil && v.IsObjectType() {
		return v
	}
	return this
}

// initCollatorOptions reads the options bag in the order the specification
// lists it, which is observable: a getter on any of these records when it ran.
func (rt *Runtime) initCollatorOptions(options Value, requested []string) (collatorOptions, *ThrowError) {
	c := defaultCollatorOptions()
	if len(requested) > 0 {
		c.tag = requested[0]
	}
	usage, ok, e := rt.intlStringOption(options, "usage", []string{"sort", "search"})
	if e != nil {
		return c, e
	}
	if ok {
		c.usage = usage
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return c, e
	}
	collation, ok, e := rt.intlStringOption(options, "collation", nil)
	if e != nil {
		return c, e
	}
	if ok {
		if !isUnicodeType(collation) {
			return c, rt.rangeError("Invalid value " + collation + " for option collation")
		}
		if isCollationType(asciiLower(collation)) {
			c.collation = asciiLower(collation)
		}
	}
	numeric, e := rt.intlBoolOption(options, "numeric")
	if e != nil {
		return c, e
	}
	caseFirst, ok, e := rt.intlStringOption(options, "caseFirst", []string{"upper", "lower", "false"})
	if e != nil {
		return c, e
	}
	if ok {
		c.caseFirst = caseFirst
	}
	sens, ok, e := rt.intlStringOption(options, "sensitivity",
		[]string{"base", "accent", "case", "variant"})
	if e != nil {
		return c, e
	}
	if ok {
		c.sensitivity = sens
	}
	ignore, e := rt.intlBoolOption(options, "ignorePunctuation")
	if e != nil {
		return c, e
	}

	// The tag's own -u- keywords are the fallback for the three options that
	// have one, and the options win where both are given. The resolved locale
	// is then rebuilt to carry exactly the keywords that survived that: an
	// option that overrode a keyword removes it, an unsupported value removes
	// it, and everything else in the extension -- attributes, keywords this
	// service knows nothing about -- goes with them. Two tags that resolve the
	// same way must resolve to the same locale string.
	t, tok := parseLangTag(c.tag)
	if !tok {
		t = &langTag{lang: c.tag}
	}
	kept := &langTag{lang: t.lang, script: t.script, region: t.region, variants: t.variants}
	// ResolveLocale keeps the keyword when the value it settled on is the one
	// the extension asked for -- including when an option asked for the same
	// thing. Only an option that CHANGES the value removes it.
	if v, has := t.uKeyword("co"); has && isCollationType(v) {
		if collation == "" || asciiLower(collation) == v {
			c.collation = v
			kept.setUKeyword("co", v)
		}
	}
	if v, has := t.uKeyword("kn"); has {
		fromTag := v == "" || v == "true"
		if numeric == nil || *numeric == fromTag {
			c.numeric = fromTag
			kept.setUKeyword("kn", boolKeyword(fromTag))
		}
	}
	if numeric != nil {
		c.numeric = *numeric
	}
	if v, has := t.uKeyword("kf"); has && tagContains([]string{"upper", "lower", "false"}, v) {
		if caseFirst == "" || caseFirst == v {
			c.caseFirst = v
			kept.setUKeyword("kf", v)
		}
	}
	c.tag = kept.String()

	// ignorePunctuation defaults to true for the languages whose scripts write
	// no spaces between words, where punctuation is not a separator; CLDR lists
	// Thai among them and it is the one test262 checks.
	if strings.HasPrefix(c.tag, "th") {
		c.ignorePunct = true
	}
	if ignore != nil {
		c.ignorePunct = *ignore
	}
	return c, nil
}

func boolKeyword(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// intlBoolOption is GetOption with type boolean: nil when the option was
// absent, which is a different thing from present and false.
func (rt *Runtime) intlBoolOption(options Value, name string) (*bool, *ThrowError) {
	if options.IsUndefined() {
		return nil, nil
	}
	v, e := rt.getField(options, name)
	if e != nil {
		return nil, e
	}
	if v.IsUndefined() {
		return nil, nil
	}
	b := rt.toBoolean(v)
	return &b, nil
}

// collatorForCompare is the Collator that String.prototype.localeCompare uses.
// It is built per call, which is what the specification says to do -- the
// method takes its own locales and options arguments and a cached one could
// not honour them.
func (rt *Runtime) collatorForCompare(locales, options Value) (collatorOptions, *ThrowError) {
	requested, e := rt.canonicalizeLocaleList(locales)
	if e != nil {
		return collatorOptions{}, e
	}
	return rt.initCollatorOptions(options, requested)
}
