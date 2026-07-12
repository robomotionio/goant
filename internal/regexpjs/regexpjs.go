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

	Global     bool
	IgnoreCase bool
	Multiline  bool
	DotAll     bool
	Unicode     bool
	UnicodeSets bool
	Sticky      bool
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
	// An empty pattern matches the empty string; ECMAScript spells it (?:).
	src := pattern
	// Under the u/v flag, translate ES Unicode property escapes (\p{…}) into
	// explicit code-point classes regexp2 can compile.
	if r.UnicodeSets {
		t, terr := translateVFlagSets(src)
		if terr != nil {
			return nil, fmt.Errorf("invalid regular expression: %v", terr)
		}
		src = t
	}
	if r.Unicode && (strings.Contains(src, `\p`) || strings.Contains(src, `\P`)) {
		t, terr := translateUnicodeProps(src)
		if terr != nil {
			return nil, fmt.Errorf("invalid regular expression: %v", terr)
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
	out := &Match{Index: m.Index, Groups: make([]Group, len(groups))}
	for i, g := range groups {
		gg := Group{Index: -1, Name: g.Name}
		if len(g.Captures) > 0 {
			gg.Index = g.Index
			gg.Length = g.Length
			gg.Value = g.String()
		}
		out.Groups[i] = gg
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
