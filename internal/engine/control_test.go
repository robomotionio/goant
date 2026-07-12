package engine

import "testing"

func TestExecForLoop(t *testing.T) {
	runNum(t, "var s = 0; for (var i = 0; i < 5; i = i + 1) { s = s + i; } s;", 10)
	runNum(t, "var p = 1; for (var i = 1; i <= 5; i++) { p = p * i; } p;", 120)
	runNum(t, "var n = 0; for (var i = 10; i > 0; i--) { n = n + 1; } n;", 10)
}

func TestExecBreakContinue(t *testing.T) {
	runNum(t, "var s = 0; for (var i = 0; i < 100; i++) { if (i === 5) break; s = s + i; } s;", 10)
	runNum(t, "var s = 0; for (var i = 0; i < 10; i++) { if (i % 2 === 0) continue; s = s + i; } s;", 25)
	runNum(t, "var i = 0; while (true) { i = i + 1; if (i >= 7) break; } i;", 7)
}

func TestExecNestedLoops(t *testing.T) {
	runNum(t, `
		var count = 0;
		for (var i = 0; i < 3; i++) {
			for (var j = 0; j < 3; j++) {
				count = count + 1;
			}
		}
		count;
	`, 9)
	// break only exits the inner loop.
	runNum(t, `
		var count = 0;
		for (var i = 0; i < 3; i++) {
			for (var j = 0; j < 10; j++) {
				if (j === 2) break;
				count = count + 1;
			}
		}
		count;
	`, 6)
}

func TestExecSwitch(t *testing.T) {
	runNum(t, `
		var x = 2;
		var r = 0;
		switch (x) {
			case 1: r = 10; break;
			case 2: r = 20; break;
			case 3: r = 30; break;
			default: r = 99;
		}
		r;
	`, 20)
	// default when no match
	runNum(t, `
		var x = 5;
		var r = 0;
		switch (x) {
			case 1: r = 1; break;
			default: r = 100;
		}
		r;
	`, 100)
	// fall-through
	runNum(t, `
		var r = 0;
		switch (1) {
			case 1: r = r + 1;
			case 2: r = r + 10;
			case 3: r = r + 100; break;
			case 4: r = r + 1000;
		}
		r;
	`, 111)
}

func TestExecThrowCatch(t *testing.T) {
	runNum(t, `
		var r = 0;
		try {
			throw 42;
		} catch (e) {
			r = e;
		}
		r;
	`, 42)
	// catch recovers and execution continues
	runStr(t, `
		var msg = "ok";
		try {
			throw "boom";
		} catch (e) {
			msg = e;
		}
		msg;
	`, "boom")
}

func TestExecThrowAcrossCall(t *testing.T) {
	// An exception thrown in a callee is caught by the caller's handler.
	runNum(t, `
		function boom() { throw 7; }
		var r = 0;
		try {
			boom();
		} catch (e) {
			r = e;
		}
		r;
	`, 7)
}

func TestExecTryFinally(t *testing.T) {
	// finally runs after a normal try body.
	runNum(t, `
		var r = 0;
		try {
			r = 1;
		} finally {
			r = r + 10;
		}
		r;
	`, 11)
}

func TestExecTypeof(t *testing.T) {
	runStr(t, "typeof 42;", "number")
	runStr(t, `typeof "s";`, "string")
	runStr(t, "typeof true;", "boolean")
	runStr(t, "typeof undefined;", "undefined")
	runStr(t, "typeof null;", "object")
	runStr(t, "typeof {};", "object")
	runStr(t, "typeof function(){};", "function")
	runStr(t, "typeof undeclaredGlobalXYZ;", "undefined") // typeof never throws
}

func TestExecInOperator(t *testing.T) {
	runBool(t, `var o = {a: 1}; "a" in o;`, true)
	runBool(t, `var o = {a: 1}; "b" in o;`, false)
	runBool(t, `var a = [1, 2, 3]; 0 in a;`, true)
	runBool(t, `var a = [1, 2, 3]; 5 in a;`, false)
}

func TestExecDelete(t *testing.T) {
	runBool(t, `var o = {a: 1}; delete o.a;`, true)
	runNum(t, `var o = {a: 1, b: 2}; delete o.a; ("a" in o) ? 1 : 0;`, 0)
}

func TestExecInstanceof(t *testing.T) {
	// x instanceof F where F.prototype is in x's chain.
	runBool(t, `
		function Animal() {}
		var a = {};
		a instanceof Animal;
	`, false)
}
