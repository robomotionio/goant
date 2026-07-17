// Package regexpjs is the JS→regex translation layer (PLAN.md Phase 4.3/8): it
// validates ECMAScript regular expressions and retargets them onto
// dlclark/regexp2 (which supports an ECMAScript mode directly). Flags g/y and
// lastIndex handling live at the JS level; i/m/s/u map to regexp2 options.
//
// Position semantics: regexp2 operates on Go runes. For BMP text a rune index
// equals a UTF-16 index; astral-aware indexing is a later refinement.
package regexpjs

import (
	"fmt"
	"strings"

	"github.com/dlclark/regexp2"
)

// Regexp is a compiled JS regular expression.
type Regexp struct {
	re     *regexp2.Regexp
	Source string
	Flags  string

	Global      bool
	IgnoreCase  bool
	Multiline   bool
	DotAll      bool
	Unicode     bool
	UnicodeSets bool
	Sticky      bool

	// groupNames maps the safe internal capture-group name regexp2 sees back to
	// the original ECMAScript name (nil when the pattern has no named groups).
	groupNames map[string]string
	// groupKinds lists the capture groups in ECMAScript definition order (true =
	// named). It is used to reorder regexp2's results, which number all unnamed
	// groups before all named ones, back into left-to-right order. nil when the
	// pattern has no named groups (no reordering needed).
	groupKinds []bool
}

// Group is one capture group in a match (Index is a rune offset; -1 = unmatched).
type Group struct {
	Index  int
	Length int
	Value  string
	Name   string
}

// Match is a successful regex match with its capture groups (group 0 is whole).
type Match struct {
	Index  int
	Groups []Group
}

// Compile validates flags and compiles pattern under ECMAScript semantics.
// translateAnnexBEscapes rewrites the JS identity escapes that regexp2 would
// otherwise misread. `\A \Z \z \G` (regexp2 anchors) become the literal letter,
// and `\c` not followed by an ASCII control letter becomes a literal backslash +
// c (Annex B ExtendedAtom). All other escapes — including `\\` — pass through
// verbatim; the two-rune advance on `\\` keeps a literal backslash from being
// rescanned.
func translateAnnexBEscapes(src string) string {
	rs := []rune(src)
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\\' || i+1 >= len(rs) {
			b.WriteRune(rs[i])
			continue
		}
		switch n := rs[i+1]; n {
		case 'A', 'Z', 'z', 'G':
			b.WriteRune(n)
		case 'p', 'P':
			// In a non-Unicode pattern a bare \p / \P (not a \p{…} property escape) is
			// an identity escape — the literal letter — not the incomplete property
			// escape regexp2 would reject.
			if i+2 < len(rs) && rs[i+2] == '{' {
				b.WriteRune('\\')
				b.WriteRune(n)
			} else {
				b.WriteRune(n)
			}
		case 'c':
			if i+2 < len(rs) && isASCIILetter(rs[i+2]) {
				b.WriteString(`\c`)
			} else {
				b.WriteString(`\\c`)
			}
		default:
			b.WriteRune('\\')
			b.WriteRune(n)
		}
		i++ // consumed the escaped rune too
	}
	return b.String()
}

func isASCIILetter(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func Compile(pattern, flags string) (*Regexp, error) {
	r := &Regexp{Source: pattern, Flags: flags}
	seen := map[rune]bool{}
	opts := regexp2.RegexOptions(regexp2.ECMAScript)
	for _, f := range flags {
		if seen[f] {
			return nil, fmt.Errorf("duplicate flag '%c' in regular expression", f)
		}
		seen[f] = true
		switch f {
		case 'g':
			r.Global = true
		case 'i':
			r.IgnoreCase = true
			opts |= regexp2.IgnoreCase
		case 'm':
			r.Multiline = true
			opts |= regexp2.Multiline
		case 's':
			r.DotAll = true
			opts |= regexp2.Singleline
		case 'u':
			r.Unicode = true
			opts |= regexp2.Unicode
		case 'v':
			// unicodeSets mode: treat like `u` and additionally resolve class set
			// operations (&&, --) ahead of compilation.
			r.Unicode = true
			r.UnicodeSets = true
			opts |= regexp2.Unicode
		case 'y':
			r.Sticky = true
		case 'd':
			// hasIndices — affects match-result shape only, no compile change.
		default:
			return nil, fmt.Errorf("invalid regular expression flag '%c'", f)
		}
	}
	// Inline modifier groups `(?ims-ims:…)` have grammar early errors regexp2's
	// permissive .NET option syntax would not catch; reject the invalid ones here.
	if err := validateModifierGroups(pattern); err != nil {
		return nil, fmt.Errorf("invalid regular expression: %v", err)
	}
	// A quantifier on a lookbehind (any mode) or on a lookahead (Unicode mode) is
	// an early error the .NET engine would otherwise accept.
	if err := validateQuantifiedAssertions(pattern, r.Unicode); err != nil {
		return nil, fmt.Errorf("invalid regular expression: %v", err)
	}
	// Two named capture groups may share a name only across separate alternatives;
	// a same-alternative duplicate is an early error.
	if err := validateDuplicateGroupNames(pattern); err != nil {
		return nil, fmt.Errorf("invalid regular expression: %v", err)
	}
	// An empty pattern matches the empty string; ECMAScript spells it (?:).
	src := pattern
	// Annex B identity escapes: outside Unicode mode, `\A \Z \z \G` are literal
	// letters in JS (regexp2/.NET would read them as anchors), and `\c` not
	// followed by a control letter is a literal backslash + c.
	if !r.Unicode {
		src = translateAnnexBEscapes(src)
	}
	// Rename named groups / backreferences to regexp2-safe internal names (ES
	// allows names regexp2's \w+ grammar rejects, and duplicate names).
	if gs, gm, gk, gerr := translateGroupNames(src); gerr != nil {
		return nil, fmt.Errorf("invalid regular expression: %v", gerr)
	} else {
		src, r.groupNames, r.groupKinds = gs, gm, gk
	}
	// Under the u/v flag, translate ES Unicode property escapes (\p{…}) into
	// explicit code-point classes regexp2 can compile.
	if r.UnicodeSets {
		t, terr := translateVFlagSets(src)
		if terr != nil {
			return nil, fmt.Errorf("invalid regular expression: %v", terr)
		}
		src = t
	}
	// Property escapes may expand to sub-patterns that themselves contain \p (a
	// string property built from \p{Emoji}), so translate repeatedly to a fixpoint.
	for pass := 0; r.Unicode && pass < 4 && (strings.Contains(src, `\p`) || strings.Contains(src, `\P`)); pass++ {
		t, terr := translateUnicodeProps(src, r.UnicodeSets)
		if terr != nil {
			return nil, fmt.Errorf("invalid regular expression: %v", terr)
		}
		if t == src {
			break
		}
		src = t
	}
	// Under the u+i flags, expand special-fold letters (S↔ſ, K↔K, …) into their
	// full orbit so regexp2's IgnoreCase does not miss them.
	if r.Unicode && r.IgnoreCase {
		src = expandCaseFold(src)
	}
	if src == "" {
		src = "(?:)"
	}
	re, err := regexp2.Compile(src, opts)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression: %v", err)
	}
	r.re = re
	return r, nil
}

// Exec runs the regex against input starting at rune index start, returning the
// first match (or nil if none). Sticky matches must begin exactly at start.
func (r *Regexp) Exec(input []rune, start int) (*Match, error) {
	if start < 0 || start > len(input) {
		return nil, nil
	}
	m, err := r.re.FindRunesMatchStartingAt(input, start)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	if r.Sticky && m.Index != start {
		return nil, nil
	}
	groups := m.Groups()
	convert := func(g regexp2.Group) Group {
		name := g.Name
		if orig, ok := r.groupNames[name]; ok {
			name = orig // map the internal name back to the ECMAScript name
		}
		gg := Group{Index: -1, Name: name}
		if len(g.Captures) > 0 {
			gg.Index = g.Index
			gg.Length = g.Length
			gg.Value = g.String()
		}
		return gg
	}
	out := &Match{Index: m.Index, Groups: make([]Group, len(groups))}
	// regexp2 numbers every unnamed group before every named group; when the
	// pattern mixes the two, reorder them into ECMAScript left-to-right order.
	if r.groupKinds != nil && len(r.groupKinds)+1 == len(groups) {
		unnamedCount := 0
		for _, named := range r.groupKinds {
			if !named {
				unnamedCount++
			}
		}
		out.Groups[0] = convert(groups[0])
		unnamedPtr, namedPtr := 1, unnamedCount+1
		for es, named := range r.groupKinds {
			src := unnamedPtr
			if named {
				src, namedPtr = namedPtr, namedPtr+1
			} else {
				unnamedPtr++
			}
			out.Groups[es+1] = convert(groups[src])
		}
		return out, nil
	}
	for i, g := range groups {
		out.Groups[i] = convert(g)
	}
	return out, nil
}

// Test reports whether the regex matches anywhere at/after start.
func (r *Regexp) Test(input []rune, start int) (bool, error) {
	m, err := r.Exec(input, start)
	return m != nil, err
}

// GroupCount returns the number of capture groups (excluding group 0).
func (r *Regexp) GroupCount() int {
	return r.re.GetGroupNumbers()[len(r.re.GetGroupNumbers())-1]
}
