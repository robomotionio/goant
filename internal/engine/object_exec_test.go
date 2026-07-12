package engine

import "testing"

func TestExecObjectLiteral(t *testing.T) {
	runNum(t, "var o = {a: 1, b: 2}; o.a + o.b;", 3)
	runNum(t, "var o = {x: 10}; o.x;", 10)
	runNum(t, "var o = {}; o.y = 42; o.y;", 42)
	runStr(t, `var o = {name: "goant"}; o.name;`, "goant")
}

func TestExecObjectComputedKey(t *testing.T) {
	runNum(t, `var k = "foo"; var o = {[k]: 99}; o.foo;`, 99)
	runNum(t, `var o = {}; var key = "z"; o[key] = 7; o.z;`, 7)
}

func TestExecObjectNested(t *testing.T) {
	runNum(t, "var o = {inner: {value: 5}}; o.inner.value;", 5)
	runNum(t, "var o = {a: {b: {c: 42}}}; o.a.b.c;", 42)
}

func TestExecArrayLiteral(t *testing.T) {
	runNum(t, "var a = [10, 20, 30]; a[0] + a[1] + a[2];", 60)
	runNum(t, "var a = [1, 2, 3]; a.length;", 3)
	runNum(t, "var a = []; a[0] = 5; a[1] = 10; a[0] + a[1];", 15)
	runNum(t, "var a = [1, 2, 3]; a[5] = 6; a.length;", 6)
}

func TestExecArrayElementAssign(t *testing.T) {
	runNum(t, "var a = [1, 2, 3]; a[1] = 99; a[1];", 99)
	runNum(t, "var m = [[1, 2], [3, 4]]; m[1][0];", 3)
}

func TestExecMethodCall(t *testing.T) {
	// `this` inside a method refers to the receiver.
	runNum(t, `
		var counter = {
			value: 0,
			inc: function() { this.value = this.value + 1; return this.value; }
		};
		counter.inc();
		counter.inc();
		counter.inc();
	`, 3)
}

func TestExecMethodThis(t *testing.T) {
	runNum(t, `
		var obj = {
			x: 10,
			getX: function() { return this.x; }
		};
		obj.getX();
	`, 10)
	// Method shorthand.
	runNum(t, `
		var obj = { n: 5, double() { return this.n * 2; } };
		obj.double();
	`, 10)
}

func TestExecStringIndexAndLength(t *testing.T) {
	runStr(t, `"hello"[0];`, "h")
	runStr(t, `"hello"[4];`, "o")
	runNum(t, `"hello".length;`, 5)
	runNum(t, `"héllo".length;`, 5) // UTF-16 length
	runNum(t, `"𝕏".length;`, 2)     // astral char is 2 UTF-16 units
}

func TestExecComputedMethodCall(t *testing.T) {
	runNum(t, `
		var o = { add: function(a, b) { return a + b; } };
		var name = "add";
		o[name](3, 4);
	`, 7)
}
