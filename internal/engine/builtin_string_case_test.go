package engine

import (
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// The ASCII fast path is only allowed to exist if it agrees with the Unicode
// machinery it skips. That is the claim: for a string whose every byte is
// ASCII, the Unicode default case mapping and the ASCII one are the same
// function. This checks it rather than asserting it — over every ASCII byte,
// which is the whole domain the fast path claims, so the check is exhaustive
// rather than a sample.
func TestTheASCIIFastPathAgreesWithUnicode(t *testing.T) {
	// Every single ASCII byte on its own.
	for c := 0; c < 0x80; c++ {
		b := []byte{byte(c)}
		if got, want := string(jsToUpperCase(b)), cases.Upper(language.Und).String(string(b)); got != want {
			t.Errorf("upper(%q): fast path %q, Unicode %q", b, got, want)
		}
		if got, want := string(jsToLowerCase(b)), cases.Lower(language.Und).String(string(b)); got != want {
			t.Errorf("lower(%q): fast path %q, Unicode %q", b, got, want)
		}
	}

	// And every ASCII pair, because a contextual rule would show up between two
	// characters and not in either alone.
	for a := 0; a < 0x80; a++ {
		for b := 0; b < 0x80; b++ {
			in := []byte{byte(a), byte(b)}
			if got, want := string(jsToUpperCase(in)), cases.Upper(language.Und).String(string(in)); got != want {
				t.Fatalf("upper(%q): fast path %q, Unicode %q", in, got, want)
			}
			if got, want := string(jsToLowerCase(in)), cases.Lower(language.Und).String(string(in)); got != want {
				t.Fatalf("lower(%q): fast path %q, Unicode %q", in, got, want)
			}
		}
	}
}

// The cases the fast path must decline. Each of these is a mapping ASCII rules
// would get wrong, which is why the Unicode path has to stay.
func TestNonASCIIStillTakesTheUnicodePath(t *testing.T) {
	cases_ := []struct{ name, in, upper, lower string }{
		{"sharp s expands", "straße", "STRASSE", "straße"},
		{"ligature expands", "ﬀ", "FF", "ﬀ"},
		{"dotted capital I", "İ", "İ", "i̇"},
		{"final sigma", "\u039F\u0394\u039F\u03A3", "\u039F\u0394\u039F\u03A3", "\u03BF\u03B4\u03BF\u03C2"},
		{"medial sigma", "\u039F\u0394\u039F\u03A3\u03A3", "\u039F\u0394\u039F\u03A3\u03A3", "\u03BF\u03B4\u03BF\u03C3\u03C2"},
		{"latin-1", "café", "CAFÉ", "café"},
		{"mixed ascii and not", "aÉb", "AÉB", "aéb"},
	}
	for _, c := range cases_ {
		if got := string(jsToUpperCase([]byte(c.in))); got != c.upper {
			t.Errorf("%s: upper(%q) = %q, want %q", c.name, c.in, got, c.upper)
		}
		if got := string(jsToLowerCase([]byte(c.in))); got != c.lower {
			t.Errorf("%s: lower(%q) = %q, want %q", c.name, c.in, got, c.lower)
		}
	}
}

// A lone surrogate makes the bytes invalid UTF-8, and that path must still
// leave them alone rather than replacing them with U+FFFD.
func TestLoneSurrogatesSurviveCasing(t *testing.T) {
	b := []byte{'a', 0xED, 0xA0, 0x80, 'b'} // "a" + WTF-8 D800 + "b"
	if utf8.Valid(b) {
		t.Fatal("fixture is not the invalid-UTF-8 case it claims to be")
	}
	up := jsToUpperCase(b)
	if !strings.HasPrefix(string(up), "A") || !strings.HasSuffix(string(up), "B") {
		t.Fatalf("upper did not case the ASCII around the surrogate: %q", up)
	}
	if !strings.Contains(string(up), string([]byte{0xED, 0xA0, 0x80})) {
		t.Fatalf("the lone surrogate did not survive: %q", up)
	}
}

// Both return a buffer the caller owns, because newStringBytes takes ownership
// of what it is handed. Returning the receiver's own bytes when there is
// nothing to convert would let a later write through one string be seen by the
// other.
func TestCasingNeverAliasesItsInput(t *testing.T) {
	for _, in := range []string{"ALREADY UPPER", "already lower", "", "1234!"} {
		b := []byte(in)
		up, lo := jsToUpperCase(b), jsToLowerCase(b)
		if len(b) > 0 {
			if &up[0] == &b[0] {
				t.Fatalf("upper(%q) returned the input buffer", in)
			}
			if &lo[0] == &b[0] {
				t.Fatalf("lower(%q) returned the input buffer", in)
			}
		}
		// And mutating the result must not disturb the source.
		for i := range up {
			up[i] = '.'
		}
		if string(b) != in {
			t.Fatalf("writing to the result changed the input: %q became %q", in, b)
		}
	}
}
