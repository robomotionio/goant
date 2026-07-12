package engine

import (
	"math"
	"testing"
)

// runNum evaluates src and asserts the completion value is the number want.
func runNum(t *testing.T, src string, want float64) {
	t.Helper()
	rt := New()
	v, err := rt.RunString("test.js", src)
	if err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	if v.Type() != TNum {
		t.Fatalf("run %q: result type %v not number", src, v.Type())
	}
	if v.Number() != want && !(math.IsNaN(want) && math.IsNaN(v.Number())) {
		t.Fatalf("run %q = %v want %v", src, v.Number(), want)
	}
}

func runStr(t *testing.T, src, want string) {
	t.Helper()
	rt := New()
	v, err := rt.RunString("test.js", src)
	if err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	if !v.IsString() {
		t.Fatalf("run %q: result type %v not string", src, v.Type())
	}
	if got := string(rt.strBytes(v)); got != want {
		t.Fatalf("run %q = %q want %q", src, got, want)
	}
}

func runBool(t *testing.T, src string, want bool) {
	t.Helper()
	rt := New()
	v, err := rt.RunString("test.js", src)
	if err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	if v.Type() != TBool || v.Bool() != want {
		t.Fatalf("run %q = %v want %v", src, v, want)
	}
}

func TestExecArithmetic(t *testing.T) {
	runNum(t, "2 + 3 * 4;", 14)
	runNum(t, "(2 + 3) * 4;", 20)
	runNum(t, "2 ** 3 ** 2;", 512) // right-assoc
	runNum(t, "10 % 3;", 1)
	runNum(t, "7 / 2;", 3.5)
	runNum(t, "-5 + 3;", -2)
	runNum(t, "1 + 2 + 3 + 4;", 10)
	runNum(t, "5 - -3;", 8)
}

func TestExecBitwise(t *testing.T) {
	runNum(t, "5 & 3;", 1)
	runNum(t, "5 | 2;", 7)
	runNum(t, "5 ^ 1;", 4)
	runNum(t, "1 << 4;", 16)
	runNum(t, "256 >> 2;", 64)
	runNum(t, "-1 >>> 28;", 15)
	runNum(t, "~5;", -6)
}

func TestExecVariables(t *testing.T) {
	runNum(t, "var x = 10; var y = 20; x + y;", 30)
	runNum(t, "let a = 5; a = a * 2; a;", 10)
	runNum(t, "var x = 1; x += 4; x *= 3; x;", 15)
	runNum(t, "const c = 42; c;", 42)
}

func TestExecStrings(t *testing.T) {
	runStr(t, `"hello" + " " + "world";`, "hello world")
	runStr(t, `"n=" + 42;`, "n=42")
	runStr(t, `1 + " apples";`, "1 apples")
}

func TestExecComparison(t *testing.T) {
	runBool(t, "2 < 3;", true)
	runBool(t, "3 <= 3;", true)
	runBool(t, "5 > 10;", false)
	runBool(t, "1 === 1;", true)
	runBool(t, "1 === 2;", false)
	runBool(t, `"a" < "b";`, true)
	runBool(t, "1 == 1;", true)
	runBool(t, `1 == "1";`, true) // abstract equality coercion
	runBool(t, "null == undefined;", true)
	runBool(t, "(0/0) === (0/0);", false) // NaN !== NaN (produced without the NaN global)
	runBool(t, "1 !== 2;", true)
}

func TestExecLogical(t *testing.T) {
	runNum(t, "1 && 2;", 2)
	runNum(t, "0 || 5;", 5)
	runNum(t, "1 && 0 || 3;", 3)
	runBool(t, "!0;", true)
	runBool(t, "!1;", false)
	runNum(t, "null ?? 7;", 7)
	runNum(t, "5 ?? 7;", 5)
}

func TestExecControlFlow(t *testing.T) {
	runNum(t, "var x = 0; if (1 < 2) { x = 10; } else { x = 20; } x;", 10)
	runNum(t, "var x = 0; if (1 > 2) { x = 10; } else { x = 20; } x;", 20)
	runNum(t, "var x = 5; var y = x > 3 ? 100 : 200; y;", 100)
}

func TestExecLoops(t *testing.T) {
	runNum(t, "var sum = 0; var i = 0; while (i < 5) { sum = sum + i; i = i + 1; } sum;", 10)
	runNum(t, "var n = 1; var i = 0; while (i < 10) { n = n * 2; i = i + 1; } n;", 1024)
	runNum(t, "var x = 0; do { x = x + 1; } while (x < 3); x;", 3)
}

func TestExecFactorial(t *testing.T) {
	// Iterative factorial via while loop.
	runNum(t, `
		var n = 6;
		var result = 1;
		var i = 1;
		while (i <= n) {
			result = result * i;
			i = i + 1;
		}
		result;
	`, 720)
}

func TestExecCompletionValue(t *testing.T) {
	// The script completion value is that of the last evaluated expression.
	runNum(t, "1; 2; 3;", 3)
	runNum(t, "var x = 5; x;", 5)
}
