//go:build amd64

package engine

import "testing"

// A branch inside an operand stack deeper than the register window used to
// corrupt the slots below it. The refill that brings a slot back into a
// register is emitted after the template, so on a conditional branch it sat on
// the fall-through path alone — and the taken edge carried on with the
// condition still in the register the window said held an operand.
//
// The shape it takes in real code is an array literal with a conditional
// element past the ninth, which is how it was found: test262's
// Error.prototype.stack setter test builds one, and every label in it came out
// as the first character of a different label.
//
// Each case runs the construction in a loop so the function tiers, and reduces
// the array to a number — jitBothWays compares the two runs' Values, and a
// string is a handle that differs between two runtimes.
func TestBranchInsideADeepOperandStack(t *testing.T) {
	const drive = `
		var acc = 0;
		for (var k = 0; k < 40; k++) { var a = f(k & 1); for (var i = 0; i < a.length; i++) acc = acc * 3 + (+a[i] || 0); acc = acc % 1000003; }
		acc;`
	for _, tc := range []struct{ name, src string }{
		{"ternary at the tenth element", `
			function f(c) { return [1,2,3,4,5,6,7,8,9, c?0:10]; }` + drive},
		{"ternary at the eleventh", `
			function f(c) { return [1,2,3,4,5,6,7,8,9,10, c?0:11]; }` + drive},
		{"several past the window", `
			function f(c) { return [1,2,3,4,5,6,7,8, c?0:9, c?0:10, c?0:11]; }` + drive},
		{"a comparison, which fuses with its branch", `
			function f(n) { return [1,2,3,4,5,6,7,8,9, n < 2 ? 0 : 10, n > 9 ? 0 : 11]; }` + drive},
		{"nullish, which branches without a Boolean", `
			function f(c) { var u = c ? undefined : 7; return [1,2,3,4,5,6,7,8,9, u ?? 10, u ?? 11]; }` + drive},
		{"a conditional argument past the ninth", `
			function g(){ var s = 0; for (var i = 0; i < arguments.length; i++) s += arguments[i]; return s; }
			function f(c) { return [g(1,2,3,4,5,6,7,8,9, c?0:10, c?0:11)]; }` + drive},
		{"a logical operator, which branches on its left", `
			function f(c) { return [1,2,3,4,5,6,7,8,9, c && 10, c || 11]; }` + drive},
	} {
		jitBothWays(t, tc.name, tc.src)
	}
}
