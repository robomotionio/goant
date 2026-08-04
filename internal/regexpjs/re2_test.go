package regexpjs

import (
	"fmt"
	"strings"
	"testing"
)

// The fast path's contract is that it answers exactly what regexp2 would have,
// so the test is a differential one: every pattern below is run both ways
// against every subject, and any difference in the match offset or in any
// capture is a failure. Patterns the translation declines are still run — the
// point of listing them is to notice if one starts being accepted, which is when
// its semantics need a second look.

var re2Patterns = []struct {
	pat, flags string
	fast       bool // expected to have a translation
}{
	// The Octane RegExp corpus, which is what the fast path exists for.
	{`^ba`, "", true},
	{`(((\w+)://)([^/:]*)(:(\d+))?)?([^#?]*)(\?([^#]*))?(#(.*))?`, "", true},
	{`^\s*|\s*$`, "g", true},
	{`\bQBZPbageby_cynprubyqre\b`, "g", true},
	{`,`, "", true},
	{`^[\s\xa0]+|[\s\xa0]+$`, "g", true},
	{`(\d*)(\D*)`, "g", true},
	{`(^|\s)lhv\-h(\s|$)`, "", true},
	{`\?[\w\W]*(sevraqvq|punaaryvq|tebhcvq)=([^\&\?#]*)`, "i", true},
	{`^\s*(\S*(\s+\S+)*)\s*$`, "", true},
	{`(-[a-z])`, "i", true},
	{`^(?:(?:[^:/?#]+):)?(?://(?:[^/?#]*))?([^?#]*)(?:\?([^#]*))?(?:#(.*))?`, "", true},
	{`^(([^:/?#]+):)?(//([^/?#]*))?([^?#]*)(\?([^#]*))?(#(.*))?$`, "", true},
	{`(\$\{4\})|(\$4\b)`, "g", true},
	{`\{0\}`, "g", true},
	{`%2R`, "gi", true},
	{`\b[a-z]`, "g", true},
	{`\bhfucjrn\s*=\s*([^;]*)`, "i", true},
	{`-\D`, "g", true},
	{`[<>]`, "g", true},
	{`^(mu-(PA|GJ)|wn|xb)$`, "", true},
	{`(^|[^\\])\"\\/Qngr\((-?[0-9]+)\)\\/\"`, "g", true},
	{`^[^<]*(<(.|\s)+>)[^>]*$|^#(\w+)$`, "", true},
	{`(?:^|\s+)ba(?:\s+|$)`, "", true},
	{`\s?;\s?`, "", true},
	{`\s*([+>~\s])\s*([a-zA-Z#.*:\[])`, "g", true},
	{`^([>+~])\s*(\w*)`, "i", true},
	{`^>\s*((?:[\w\u0128-\uffff*_-]|\\.)+)`, "", true},
	{`^([#.]?)((?:[\w\u0128-\uffff*_-]|\\.)*)`, "", true},
	{`\t`, "g", true},
	{`TNQP=([^;]*)`, "i", true},
	{`uggcf?://([^/]+\.)?snprobbx\.pbz/`, "", true},
	{`v/g.tvs#(.*)`, "i", true},
	{`^(:)([\w-]+)\("?'?(.*?(\(.*?\))?[^(]*?)"?'?\)`, "", true},
	{`%\w?$`, "", true},
	{`^(\w+|\*)$`, "", true},
	{`\W`, "g", true},
	{`/\xc4/t`, "", true},
	{`##yv16##`, "gi", true},
	{`(\\\"|\x00-|\x1f|\x7f-|\x9f|\u00ad|\u0600-|\u0604|\u070f|\u17b4|\u17b5|\u200c-|\u200f|\u2028-|\u202f|\u2060-|\u206f|\ufeff|\ufff0-|\uffff)`, "g", true},

	// Constructs the translation has to refuse.
	{`^(\[) *@?([\w-]+) *([!*$^~=]*) *('?"?)(.*?)\4 *\]`, "", false}, // backreference
	{`(?=a)b`, "", false},           // lookahead
	{`(?<=a)b`, "", false},          // lookbehind
	{`(?<name>a)`, "", false},       // named group
	{`(a*)*`, "", false},            // nullable body with a capture
	{`(a?)+b`, "", false},           // ditto
	{`(?:(a)|)*`, "", false},        // ditto, nested
	{`^a$`, "m", false},             // multiline
	{`\u{1F600}`, "u", false},       // unicode mode
	{`\p{L}`, "u", false},           // property escape
	{`a{1001}`, "", false},          // over RE2's repeat limit
	{`\k<x>(?<x>a)`, "", false},     // named backreference
	{`caf\u00e9`, "i", true},        // non-ASCII, but only ever reachable through an escape
	{`[\u0100-\u3000]`, "i", false}, // range spanning a fold into ASCII
	{`\1(a)`, "", false},            // octal/backreference
	{`(?i:a)b`, "", false},          // inline modifier group

	// Accepted, and worth pinning: these are the shapes most likely to drift.
	{`(?:a*)*b`, "", true},   // nullable body, but nothing to capture
	{`((a)|b)+`, "", true},   // stale capture from an earlier iteration
	{`[]`, "", true},         // matches nothing
	{`[^]`, "", true},        // matches anything
	{`a{2,4}?`, "", true},    // lazy counted repeat
	{`[a-]`, "", true},       // trailing dash is a literal
	{`[-a]`, "", true},       // leading dash is a literal
	{`[\b]`, "", true},       // backspace
	{`a{`, "", true},         // Annex B literal brace
	{`\$\.\*`, "", true},     // identity escapes
	{`[\s\S]`, "", true},     // the "any character" idiom
	{`\S`, "", true},         // vertical tab must not match
	{`.`, "s", true},         // dotAll
	{`.`, "", true},          // and not
	{`[^\n]`, "", true},      // negated class
	{`^`, "", true},          // bare anchors
	{`$`, "", true},          //
	{`(a)(b)?(c)`, "", true}, // an unset group in the middle
	{`\cA`, "", true},        // control escape
	{`\x41\u0042`, "", true}, // hex escapes
	{`[\d-]`, "", true},      // a dash after a class escape is a literal
	{`(a)|(b)`, "", true},    // alternation with disjoint captures
	{`\0`, "", true},         // NUL
	{`a|`, "", true},         // empty alternative
	{`[\w.-]+@[\w.-]+`, "", true},

	// The suffix program, which has to make `^` fail everywhere but position 0.
	{`^abc`, "g", true},
	{`(?:^|x)ab`, "g", true},
	{`^|a`, "g", true},
	{`\Babc`, "g", true},  // a word boundary has no suffix program at all
	{`a{1000}`, "", true}, // exactly at the repeat limit
	{`a{0,1000}`, "", true},
	{`a{0,1001}`, "", false},

	// Required literals: present only where every match must contain the run.
	{`x(abc)+y`, "", true},
	{`abc|def`, "", true},
	{`(abc)?d`, "", true},
}

var re2Subjects = []string{
	"", "a", "b", "ab", "abc", "  a b  ", "\v\t\n x", "528.9", "pyvpx",
	"uggc://jjj.snprobbx.pbz/ybtva.cuc", "vachggrkg QBZPbageby_cynprubyqre",
	"Zbmvyyn/5.0 (Jvaqbjf; H; Jvaqbjf AG 5.1; ra-HF) NccyrJroXvg/528.9",
	"onpxtebhaq-pbybe", "qvi .so_zrah", "#Ybtva_cnffjbeq", "*", ".pybfr",
	"a,b,,c", "  ", "\t\t", "aaaa", "zh-TW", "%2f%2F", "x@y.z", "a{", "[]",
	"\\\"\\/Qngr(-12)\\/\"", "<n uers=\"#\">k</n>", "a\rb", "a\nb",
}

func TestFastPathAgreesWithRegexp2(t *testing.T) {
	for _, p := range re2Patterns {
		re, err := Compile(p.pat, p.flags)
		if err != nil {
			t.Errorf("/%s/%s: compile: %v", p.pat, p.flags, err)
			continue
		}
		if re.HasFast() != p.fast {
			t.Errorf("/%s/%s: fast path = %v, want %v", p.pat, p.flags, re.HasFast(), p.fast)
		}
		if !re.HasFast() {
			continue
		}
		for _, s := range re2Subjects {
			if !isASCII(s) {
				t.Fatalf("subject %q is not ASCII", s)
			}
			units := []rune(s)
			for start := 0; start <= len(s); start++ {
				got, handled := re.ExecASCII(s, start)
				if !handled {
					continue
				}
				want, err := re.Exec(units, start)
				if err != nil {
					t.Fatalf("/%s/%s on %q: regexp2: %v", p.pat, p.flags, s, err)
				}
				if !sameMatch(got, want) {
					t.Errorf("/%s/%s on %q from %d:\n fast %s\n slow %s",
						p.pat, p.flags, s, start, showMatch(got), showMatch(want))
					break
				}
			}
		}
	}
}

// TestRequiredLiteralChangesNothing checks the prescan on its own terms. The
// differential test above cannot see a wrong required literal, because both
// matchers consult the same one — so here the same patterns run with it removed
// and the answers have to be identical.
func TestRequiredLiteralChangesNothing(t *testing.T) {
	tested := 0
	for _, p := range re2Patterns {
		re, err := Compile(p.pat, p.flags)
		if err != nil {
			t.Errorf("/%s/%s: compile: %v", p.pat, p.flags, err)
			continue
		}
		re.ensureFast()
		if re.reqLit == nil {
			continue
		}
		tested++
		bare, err := Compile(p.pat, p.flags)
		if err != nil {
			t.Fatal(err)
		}
		bare.ensureFast()
		bare.reqLit, bare.reqLitStr = nil, ""
		for _, s := range re2Subjects {
			units := []rune(s)
			for start := 0; start <= len(s); start++ {
				got, err1 := re.Exec(units, start)
				want, err2 := bare.Exec(units, start)
				if err1 != nil || err2 != nil {
					t.Fatalf("/%s/%s: %v %v", p.pat, p.flags, err1, err2)
				}
				if !sameMatch(got, want) {
					t.Errorf("/%s/%s on %q from %d: literal %q dropped a match:\n with %s\n without %s",
						p.pat, p.flags, s, start, re.reqLitStr, showMatch(got), showMatch(want))
					break
				}
			}
		}
	}
	if tested == 0 {
		t.Fatal("no pattern in the corpus produced a required literal")
	}
	t.Logf("%d patterns have a required literal", tested)
}

// TestFastPathOffsetsAreCodeUnits pins the reason the fast path insists on an
// ASCII subject: with anything else, Go's byte offsets and ECMAScript's
// code-unit offsets are different numbers.
func TestFastPathOffsetsAreCodeUnits(t *testing.T) {
	re, err := Compile(`b`, "")
	if err != nil {
		t.Fatal(err)
	}
	if !re.HasFast() {
		t.Fatal("expected a fast path for /b/")
	}
	m, err := re.Exec([]rune("é b"), 0)
	if err != nil || m == nil {
		t.Fatalf("m=%v err=%v", m, err)
	}
	if m.Index != 2 {
		t.Errorf("code-unit index = %d, want 2", m.Index)
	}
	// The same subject in bytes would put it at 3, which is why the caller has to
	// check strIsASCII before handing the string over.
	if got, _ := re.ExecASCII("é b", 0); got == nil || got.Index != 3 {
		t.Errorf("byte index = %v, want 3", got)
	}
}

func sameMatch(a, b *Match) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	if a.Index != b.Index || len(a.Groups) != len(b.Groups) {
		return false
	}
	for i := range a.Groups {
		if a.Groups[i].Index != b.Groups[i].Index ||
			a.Groups[i].Length != b.Groups[i].Length ||
			a.Groups[i].Value != b.Groups[i].Value {
			return false
		}
	}
	return true
}

func showMatch(m *Match) string {
	if m == nil {
		return "<no match>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "@%d", m.Index)
	for _, g := range m.Groups {
		if g.Index < 0 {
			b.WriteString(" -")
			continue
		}
		fmt.Fprintf(&b, " %d:%q", g.Index, g.Value)
	}
	return b.String()
}

// TestRequiredLiteralSelection pins which patterns get a literal. A literal that
// is not actually required would make the prescan skip real matches; one that is
// missed only costs speed.
func TestRequiredLiteralSelection(t *testing.T) {
	for _, tc := range []struct{ pat, flags, want string }{
		{`x(abc)+y`, "", "abc"},    // the plus body runs at least once
		{`abc|def`, "", ""},        // either branch could be the one that matched
		{`(abc)?d`, "", ""},        // "d" alone is too short to be worth a pass
		{`(abc)?defg`, "", "defg"}, // the mandatory tail is
		{`\d+-\d+`, "", ""},        // no literal run at all
		{`a{2,4}bcd`, "", "bcd"},   // counted repeats do not confuse the walk
		{`(^|[^\\])"\\/Qngr\(`, "", `"\/Qngr(`},
		{`Qngr`, "i", ""}, // folded literals are not fixed strings
	} {
		re, err := Compile(tc.pat, tc.flags)
		if err != nil {
			t.Errorf("/%s/%s: %v", tc.pat, tc.flags, err)
			continue
		}
		re.ensureFast()
		if re.reqLitStr != tc.want {
			t.Errorf("/%s/%s: required literal %q, want %q", tc.pat, tc.flags, re.reqLitStr, tc.want)
		}
	}
}
