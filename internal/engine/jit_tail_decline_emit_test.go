//go:build amd64 || arm64

package engine

import "testing"

// A tail call whose callee declines the arguments it was handed.
//
// Declining is compiled code saying "nothing has happened, run me in the
// interpreter instead", and it is reported by returning false all the way up to
// whoever entered the frame. After a tail call that is a lie: the frame belongs
// to the callee, and the caller it took over from has already run to its
// return. The caller's caller would run the ORIGINAL function a second time.
//
// It is only visible when the function has a side effect, which is why these
// count their own entries rather than checking a result. Octane's pdfjs is the
// program that found it: Parser.filter tail-calls Parser.makeFilter, makeFilter
// declined, and the PDF stream was read twice — reported as a malformed
// document.
//
// Strict code throughout: a proper tail call is only emitted there.
func TestATailCalleeThatDeclinesDoesNotRerunItsCaller(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"the callee wants a number and is handed a string", `
			"use strict";
			var entries = 0;
			function callee(x) { return x * 2 + 1; }
			function caller(x) { entries++; return callee(x); }
			var warm = 0;
			for (var k = 0; k < 40; k++) warm += caller(k);
			entries = 0;
			caller("7");
			entries;`},
		{"through a method, which is the pdfjs shape", `
			"use strict";
			var entries = 0;
			function P() {}
			P.prototype.inner = function (a, b) { return a * b; };
			P.prototype.outer = function (a, b) { entries++; return this.inner(a, b); };
			var p = new P(), warm = 0;
			for (var k = 0; k < 40; k++) warm += p.outer(k, 2);
			entries = 0;
			p.outer("3", 2);
			entries;`},
		{"and the value it produces is still right", `
			"use strict";
			function callee(x) { return x * 2 + 1; }
			function caller(x) { return callee(x); }
			var warm = 0;
			for (var k = 0; k < 40; k++) warm += caller(k);
			caller("7") + caller(3);`},
	} {
		jitBothWays(t, tc.name, tc.src)
	}
}
