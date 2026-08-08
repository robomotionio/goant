package engine

// Intl.Segmenter.
//
// The boundaries are an approximation of UAX #29, not the whole of it: the
// grapheme rules here keep surrogate pairs, combining marks, variation
// selectors, ZWJ sequences and regional-indicator pairs together, which is
// what a script iterating "user-perceived characters" is asking for, and they
// do not implement the Hangul or Indic conjunct rules. Word boundaries are the
// letters-and-digits reading rather than the full property table.
//
// Indices are UTF-16 code units, because that is what a JavaScript string
// index is and what `containing` is handed.

import (
	"strings"
	"unicode"
)

type segmenterOptions struct {
	tag         string
	granularity string // "grapheme", "word", "sentence"
}

func (s segmenterOptions) String() string { return s.tag + "\t" + s.granularity }

func parseSegmenterOptions(s string) segmenterOptions {
	f := strings.Split(s, "\t")
	if len(f) != 2 {
		return segmenterOptions{tag: defaultLocale, granularity: "grapheme"}
	}
	return segmenterOptions{tag: f[0], granularity: f[1]}
}

// codePointAt decodes the code point beginning at unit i, reporting how many
// code units it took. A lone surrogate is a code point of its own, which is
// what a JavaScript string permits.
func codePointAt(units []rune, i int) (rune, int) {
	c := units[i]
	if c >= 0xD800 && c <= 0xDBFF && i+1 < len(units) {
		if d := units[i+1]; d >= 0xDC00 && d <= 0xDFFF {
			return 0x10000 + (c-0xD800)<<10 + (d - 0xDC00), 2
		}
	}
	return c, 1
}

func isExtender(r rune) bool {
	switch {
	case r == 0x200D, r == 0x200C: // ZWJ, ZWNJ
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // variation selectors
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // emoji skin-tone modifiers
		return true
	case r >= 0xE0100 && r <= 0xE01EF: // variation selectors supplement
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc)
}

func isRegionalIndicator(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// The Hangul jamo classes of UAX #29. A syllable is L* V+ T* and counts as one
// grapheme however many code points spell it.
func isHangulLead(r rune) bool  { return r >= 0x1100 && r <= 0x115F }
func isHangulVowel(r rune) bool { return r >= 0x1160 && r <= 0x11A7 }
func isHangulTrail(r rune) bool { return r >= 0x11A8 && r <= 0x11FF }

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' ||
		unicode.In(r, unicode.Mn, unicode.Mc)
}

// segmentEnd returns the index one past the segment that starts at i, and
// whether that segment is word-like (only meaningful at word granularity).
func segmentEnd(units []rune, i int, granularity string) (int, bool) {
	switch granularity {
	case "word":
		r, w := codePointAt(units, i)
		if isWordChar(r) {
			j, prev := i+w, r
			for j < len(units) {
				r2, w2 := codePointAt(units, j)
				if isWordChar(r2) {
					j, prev = j+w2, r2
					continue
				}
				// An apostrophe inside a word keeps it one word, and a point
				// or a comma does the same BETWEEN DIGITS: "don't" and "1.23"
				// are each one thing, and "a,b" is two. One at the end of a
				// run belongs to neither side.
				joins := r2 == '\'' || r2 == 0x2019
				if (r2 == '.' || r2 == ',') && unicode.IsDigit(prev) {
					joins = true
				}
				if joins {
					if k := j + w2; k < len(units) {
						if r3, _ := codePointAt(units, k); isWordChar(r3) {
							j, prev = j+w2, r2
							continue
						}
					}
				}
				break
			}
			return j, true
		}
		if unicode.IsSpace(r) {
			j := i + w
			for j < len(units) {
				r2, w2 := codePointAt(units, j)
				if !unicode.IsSpace(r2) {
					break
				}
				j += w2
			}
			return j, false
		}
		return i + w, false

	case "sentence":
		j := i
		for j < len(units) {
			r, w := codePointAt(units, j)
			j += w
			if r == '.' || r == '!' || r == '?' || r == 0x2026 {
				// The trailing whitespace belongs to the sentence that ended.
				for j < len(units) {
					r2, w2 := codePointAt(units, j)
					if !unicode.IsSpace(r2) {
						break
					}
					j += w2
					if r2 == '\n' {
						break
					}
				}
				return j, false
			}
		}
		return j, false

	default: // grapheme
		r, w := codePointAt(units, i)
		j := i + w
		if r == '\r' && j < len(units) && units[j] == '\n' {
			return j + 1, false
		}
		if isRegionalIndicator(r) && j < len(units) {
			if r2, w2 := codePointAt(units, j); isRegionalIndicator(r2) {
				j += w2
			}
		}
		// A Hangul syllable written as jamo is one grapheme: a leading
		// consonant, then vowels, then trailing consonants, in that order.
		if isHangulLead(r) {
			for j < len(units) {
				r2, w2 := codePointAt(units, j)
				if !isHangulVowel(r2) && !isHangulTrail(r2) {
					break
				}
				j += w2
			}
		} else if isHangulVowel(r) {
			for j < len(units) {
				r2, w2 := codePointAt(units, j)
				if !isHangulVowel(r2) && !isHangulTrail(r2) {
					break
				}
				j += w2
			}
		}
		for j < len(units) {
			r2, w2 := codePointAt(units, j)
			if !isExtender(r2) {
				break
			}
			j += w2
			// A ZWJ joins whatever follows it, however unrelated it looks:
			// that is how a family emoji is one grapheme.
			if r2 == 0x200D && j < len(units) {
				_, w3 := codePointAt(units, j)
				j += w3
			}
		}
		return j, false
	}
}

// segmentAt finds the segment containing the code-unit index n, by walking the
// boundaries from the start. Walking is O(n) per call, which is what an
// implementation without a cached boundary list can do and is not what
// `containing` is usually used for anyway.
func segmentAt(units []rune, n int, granularity string) (start, end int, wordLike bool) {
	i := 0
	for i < len(units) {
		e, w := segmentEnd(units, i, granularity)
		if n < e {
			return i, e, w
		}
		i = e
	}
	return len(units), len(units), false
}

func (rt *Runtime) requireSegmenter(this Value) (segmenterOptions, *ThrowError) {
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlSegmenterOpts); v.IsString() {
			return parseSegmenterOptions(rt.strGo(v)), nil
		}
	}
	return segmenterOptions{}, rt.typeError("not an Intl.Segmenter")
}

func (rt *Runtime) initSegmenterOptions(options Value, requested []string) (segmenterOptions, *ThrowError) {
	s := segmenterOptions{tag: defaultLocale, granularity: "grapheme"}
	if len(requested) > 0 {
		if t, ok := parseLangTag(requested[0]); ok {
			s.tag = t.languageID()
		} else {
			s.tag = requested[0]
		}
	}
	if _, _, e := rt.intlStringOption(options, "localeMatcher", []string{"lookup", "best fit"}); e != nil {
		return s, e
	}
	g, ok, e := rt.intlStringOption(options, "granularity",
		[]string{"grapheme", "word", "sentence"})
	if e != nil {
		return s, e
	}
	if ok {
		s.granularity = g
	}
	return s, nil
}

// segmentData is the object both the iterator and `containing` hand back.
func (rt *Runtime) segmentData(input Value, units []rune, start, end int, wordLike bool, granularity string) Value {
	o := rt.newPlainObject()
	oo := rt.objPtr(o)
	oo.defineOwn("segment", rt.newString(utf16RunesToString(units[start:end])), attrDefault)
	oo.defineOwn("index", mknum(float64(start)), attrDefault)
	oo.defineOwn("input", input, attrDefault)
	if granularity == "word" {
		oo.defineOwn("isWordLike", mkbool(wordLike), attrDefault)
	}
	return o
}
