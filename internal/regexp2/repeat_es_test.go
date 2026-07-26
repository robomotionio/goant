package regexp2

import "testing"

// The fork's reason for existing: ECMAScript discards an optional iteration
// that matched the empty string and backtracks for a longer one, where .NET
// ends the loop and keeps it (see FORK.md).
//
// Every expectation below was taken from V8 (node 25) rather than written by
// hand — these are exactly the cases where ECMAScript, .NET and PCRE disagree,
// so a plausible-looking guess is worth nothing.
func TestNullableQuantifierMatchesECMAScript(t *testing.T) {
	// want[0] is the whole match; want[i] is capture i, with "" meaning unset.
	cases := []struct {
		pattern string
		input   string
		want    []string
	}{
		{`(a?b??)*`, "ab", []string{"ab", "b"}},
		{`(x?y??)*`, "xy", []string{"xy", "y"}},
		{`(a?)*`, "", []string{"", ""}},
		{`(a)*`, "aaa", []string{"aaa", "a"}},
		{`(a*)*`, "aaa", []string{"aaa", "aaa"}},
		{`(a??)*`, "aaa", []string{"aaa", "a"}},
		{`(a|)*`, "aaa", []string{"aaa", "a"}},
		{`(ab|a|)*`, "abab", []string{"abab", "ab"}},
		{`(a*)*b`, "aab", []string{"aab", "aa"}},
		{`^(a*)*$`, "aaa", []string{"aaa", "aaa"}},
		{`(.*(.)?)*`, "abcd", []string{"abcd", "abcd", ""}},
		{`((?:a?)*)*c`, "aac", []string{"aac", "aa"}},
		{`([^ab]*?)*`, "baba", []string{"", ""}},
		{`()*`, "", []string{"", ""}},
		{`(z*?)*`, "zz", []string{"zz", "z"}},
	}
	for _, tc := range cases {
		re, err := Compile(tc.pattern, ECMAScript)
		if err != nil {
			t.Errorf("%s: compile: %v", tc.pattern, err)
			continue
		}
		m, err := re.FindStringMatch(tc.input)
		if err != nil {
			t.Errorf("%s on %q: %v", tc.pattern, tc.input, err)
			continue
		}
		if m == nil {
			t.Errorf("%s on %q: no match, want %q", tc.pattern, tc.input, tc.want[0])
			continue
		}
		for i, want := range tc.want {
			got := ""
			if g := m.GroupByNumber(i); g != nil {
				got = g.String()
			}
			if got != want {
				t.Errorf("%s on %q: group %d = %q, want %q", tc.pattern, tc.input, i, got, want)
			}
		}
	}
}
