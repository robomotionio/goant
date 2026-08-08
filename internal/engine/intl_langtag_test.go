package engine

import "testing"

// The canonicalisation table from test262's canonicalized-tags.js, plus the
// alias shapes that took the most work to get right: a language keyed on a
// region ("sgn-GR"), one keyed on a variant ("zh-guoyu"), one keyed on a
// variant with no language of its own ("und-arevela", which must leave "hy"
// alone), and a replacement that must not overrule a subtag already written
// down ("sh-Cyrl" is "sr-Cyrl", not "sr-Latn").
func TestCanonicalizeLanguageTag(t *testing.T) {
	for in, want := range map[string]string{
		"de":                          "de",
		"DE-de":                       "de-DE",
		"cmn":                         "zh",
		"CMN-hANS":                    "zh-Hans",
		"cmn-hans-cn":                 "zh-Hans-CN",
		"es-419":                      "es-419",
		"es-419-u-nu-latn":            "es-419-u-nu-latn",
		"cmn-hans-cn-u-ca-t-ca-x-t-u": "zh-Hans-CN-t-ca-u-ca-x-t-u",
		"de-gregory-u-ca-gregory":     "de-gregory-u-ca-gregory",
		"sgn-GR":                      "gss",
		"ji":                          "yi",
		"de-DD":                       "de-DE",
		"in":                          "id",
		"sr-cyrl-ekavsk":              "sr-Cyrl-ekavsk",
		"en-ca-newfound":              "en-CA-newfound",
		"sl-rozaj-biske-1994":         "sl-1994-biske-rozaj",
		"da-u-attr":                   "da-u-attr",
		"da-u-attr-co-search":         "da-u-attr-co-search",
		"sh":                          "sr-Latn",
		"sh-Cyrl":                     "sr-Cyrl",
		"art-lojban":                  "jbo",
		"zh-guoyu":                    "zh",
		"mo":                          "ro",
		"hy-arevela":                  "hy",
		"ja-Latn-hepburn-heploc":      "ja-Latn-alalc97",
		// Keyword values have aliases of their own.
		"und-u-ca-ethiopic-amete-alem": "und-u-ca-ethioaa",
		"und-u-ks-primary":             "und-u-ks-level1",
		"und-u-kb-yes":                 "und-u-kb",
		"und-u-tz-cnckg":               "und-u-tz-cnsha",
		"en-u-ca-islamicc":             "en-u-ca-islamic-civil",
	} {
		got, ok := canonicalizeLangTag(in)
		if !ok {
			t.Errorf("canonicalizeLangTag(%q) rejected a valid tag", in)
			continue
		}
		if got != want {
			t.Errorf("canonicalizeLangTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// test262's getInvalidLanguageTags, which is where the old regular expression
// was wrong: it matched RFC 5646's `langtag` and ECMA-402 wants UTS 35's
// `unicode_locale_id`, under which extlang, grandfathered and privateuse-only
// tags are not tags at all.
func TestInvalidLanguageTagsAreRejected(t *testing.T) {
	for _, tag := range []string{
		"", "i", "x", "u", "419", "u-nu-latn-cu-bob", "hans-cmn-cn",
		"cmn-hans-cn-u-u", "cmn-hans-cn-t-u-ca-u", "de-gregory-gregory",
		"*", "de-*", "中文", "en-ß", "ıd", "es-Latn-latn", "pl-PL-pl",
		"u-ca-gregory", "de-1996-1996", "pt-u-ca-gregory-u-nu-latn",
		"no-nyn", "i-klingon", "zh-hak-CN", "sgn-ils", "x-foo",
		"x-en-US-12345", "x-12345-12345-en-US", "x-en-u-foo", "x-u-foo",
		"de_DE", "DE_de", "cmn_Hans", "es_419", "i_klingon", "enochian_enochian",
		"en\x00", " en", "en ", "it-IT-Latn",
		"de-u", "de-u-", "de-u-ca-", "de-u-ca-gregory-", "si-x",
		// A keyword key is `alphanum alpha`, so the second character must be
		// a letter.
		"en-u-c0", "en-u-00",
	} {
		if got, ok := canonicalizeLangTag(tag); ok {
			t.Errorf("canonicalizeLangTag(%q) accepted it as %q", tag, got)
		}
	}
}

// Canonicalisation has to be a fixed point: the output of it is by definition
// already canonical, so running it again must change nothing. Checked over
// every key of every alias table and of the likely-subtags table, which is
// several thousand tags and every shape the data contains -- a rule that
// rewrites a subtag into something the next pass rewrites again would show up
// here and nowhere else.
func TestCanonicalisationIsAFixedPoint(t *testing.T) {
	tables := []map[string]string{
		cldrLanguageAlias(), cldrScriptAlias(), cldrRegionAlias(),
		cldrVariantAlias(), cldrLikely(),
	}
	checked := 0
	for _, tbl := range tables {
		for key := range tbl {
			once, ok := canonicalizeLangTag(key)
			if !ok {
				continue // not a tag on its own; a region or script alias key
			}
			twice, ok := canonicalizeLangTag(once)
			if !ok {
				t.Errorf("canonicalizeLangTag(%q) = %q, which does not parse", key, once)
				continue
			}
			if once != twice {
				t.Errorf("canonicalizeLangTag(%q) = %q, then %q: not a fixed point", key, once, twice)
			}
			checked++
		}
	}
	if checked < 1000 {
		t.Errorf("only %d tags checked; the tables are not loading", checked)
	}
}

// maximize and minimize are each other's inverse in the only sense that is
// defined: minimizing and maximizing again must land on the same
// language-script-region as maximizing directly.
func TestMinimizeRoundTrips(t *testing.T) {
	for _, tag := range []string{
		"en", "en-US", "en-Latn-US", "zh-TW", "zh-Hant-TW", "sr-Cyrl",
		"de-DE", "pt-BR", "ar-EG", "und-Arab", "en-fonipa",
	} {
		full, ok := parseLangTag(tag)
		if !ok {
			t.Fatalf("parseLangTag(%q) failed", tag)
		}
		max, ok := full.maximized()
		if !ok {
			t.Errorf("%q does not maximize", tag)
			continue
		}
		min, ok := full.minimized()
		if !ok {
			t.Errorf("%q does not minimize", tag)
			continue
		}
		back, ok := min.maximized()
		if !ok {
			t.Errorf("%q minimizes to %q, which does not maximize", tag, min.String())
			continue
		}
		want := (&langTag{lang: max.lang, script: max.script, region: max.region}).String()
		got := (&langTag{lang: back.lang, script: back.script, region: back.region}).String()
		if got != want {
			t.Errorf("%q: maximize is %q but minimize-then-maximize is %q", tag, want, got)
		}
	}
}
