package engine

import "testing"

// String.prototype.split("") divides a string into UTF-16 code units. The
// linear rewrite of that loop has an ASCII path and a non-ASCII path, and the
// non-ASCII one has to take a surrogate pair apart by hand, so the boundary
// cases are pinned here rather than left to the general conformance run.
func TestSplitEmptySeparator(t *testing.T) {
	runStr(t, `JSON.stringify("abc".split(""))`, `["a","b","c"]`)
	runStr(t, `JSON.stringify("".split(""))`, `[]`)

	// Non-ASCII BMP: one element per character.
	runStr(t, `JSON.stringify("héllo".split(""))`, `["h","é","l","l","o"]`)

	// An astral character is two code units, so it comes apart into its
	// surrogate halves — split("") divides code units, not code points.
	runNum(t, `"a\u{1D54F}b".split("").length`, 4)
	runBool(t, `"a\u{1D54F}b".split("")[1] === "\uD835"`, true)
	runBool(t, `"a\u{1D54F}b".split("")[2] === "\uDD4F"`, true)

	// The limit applies to code units too, so it can cut a pair in half.
	runStr(t, `JSON.stringify("abcdef".split("", 3))`, `["a","b","c"]`)
	runNum(t, `"a\u{1D54F}b".split("", 2).length`, 2)
	runBool(t, `"a\u{1D54F}b".split("", 2)[1] === "\uD835"`, true)
	runStr(t, `JSON.stringify("abc".split("", 0))`, `[]`)

	// A string that is ASCII apart from one character must still take the
	// non-ASCII path correctly — the two paths differ, so the boundary matters.
	runStr(t, `JSON.stringify("abécd".split(""))`, `["a","b","é","c","d"]`)
}

// The rewrite exists to make this linear. A quadratic implementation does not
// fail this test, it fails to finish it: the previous code took 20 seconds on
// this input, so a regression shows up as a test timeout rather than a wrong
// answer.
func TestSplitEmptySeparatorIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a 200k-element array")
	}
	runNum(t, `"A".repeat(200000).split("").length`, 200000)
	runNum(t, `"é".repeat(50000).split("").length`, 50000)
}
