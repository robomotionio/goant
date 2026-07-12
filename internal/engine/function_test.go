package engine

import "testing"

func TestFuncBasicCall(t *testing.T) {
	runNum(t, "function add(a, b) { return a + b; } add(2, 3);", 5)
	runNum(t, "function sq(x) { return x * x; } sq(7);", 49)
	runNum(t, "var f = function(x) { return x + 1; }; f(10);", 11)
}

func TestFuncArrow(t *testing.T) {
	runNum(t, "var double = x => x * 2; double(21);", 42)
	runNum(t, "var add = (a, b) => a + b; add(4, 5);", 9)
	runNum(t, "var f = (x) => { return x - 1; }; f(10);", 9)
}

func TestFuncRecursion(t *testing.T) {
	// Recursive factorial.
	runNum(t, `
		function fact(n) {
			if (n <= 1) return 1;
			return n * fact(n - 1);
		}
		fact(6);
	`, 720)
	// Recursive fibonacci.
	runNum(t, `
		function fib(n) {
			if (n < 2) return n;
			return fib(n - 1) + fib(n - 2);
		}
		fib(10);
	`, 55)
}

func TestFuncHoisting(t *testing.T) {
	// Call precedes declaration textually — hoisting must make it work.
	runNum(t, `
		var r = early(5);
		function early(x) { return x * 10; }
		r;
	`, 50)
}

func TestFuncClosureCapture(t *testing.T) {
	// A closure captures an outer variable by reference.
	runNum(t, `
		function makeCounter() {
			var count = 0;
			return function() { count = count + 1; return count; };
		}
		var c = makeCounter();
		c();
		c();
		c();
	`, 3)
}

func TestFuncClosureSharedUpvalue(t *testing.T) {
	// Two closures sharing the same captured variable see each other's writes.
	runNum(t, `
		function make() {
			var x = 10;
			var get = function() { return x; };
			var inc = function() { x = x + 5; };
			inc();
			inc();
			return get();
		}
		make();
	`, 20)
}

func TestFuncNestedClosures(t *testing.T) {
	// Upvalue captured through two function levels (non-local capture).
	runNum(t, `
		function outer(a) {
			return function middle(b) {
				return function inner(c) {
					return a + b + c;
				};
			};
		}
		outer(100)(20)(3);
	`, 123)
}

func TestFuncHigherOrder(t *testing.T) {
	runNum(t, `
		function apply(f, x) { return f(x); }
		function inc(n) { return n + 1; }
		apply(inc, 41);
	`, 42)
}

func TestFuncStackOverflowGuard(t *testing.T) {
	// Unbounded recursion should raise a RangeError, not crash the host.
	rt := New()
	_, err := rt.RunString("t.js", "function r() { return r(); } r();")
	if err == nil {
		t.Fatal("expected stack overflow error")
	}
}
