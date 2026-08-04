//go:build amd64 || arm64

package engine

import (
	"fmt"
	"strings"
	"testing"
)

// The operand stack is nine registers over an array in the context, and a
// function whose stack goes deeper slides that window: a push past nine writes
// out the slot nine below, and a pop brings it back.
//
// Everything here is about the boundary. The register a slot lives in is its
// index modulo nine, so a mistake does not produce a crash or a refusal — it
// produces the wrong operand, silently, in a function that compiled. The bug
// this found was exactly that shape: a ten-element array literal came back
// holding its own callee, because the slot re-entering the window was the same
// slot the call had just put its result in.
func TestTheOperandWindowSlides(t *testing.T) {
	cases := []struct{ name, src string }{}

	// An array literal of n elements holds n operands at once. Nine is the last
	// depth that fits in registers, so these walk across the line.
	for _, n := range []int{8, 9, 10, 11, 12, 16, 24, 31, 32} {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprint(i * 3)
		}
		cases = append(cases, struct{ name, src string }{
			fmt.Sprintf("array-literal-%d", n),
			`function f(k){ var a = [` + strings.Join(parts, ",") + `]; a[0] = k; return a; }
			 var s = 0; for (var i = 0; i < 3000; i++) { var r = f(i); s = r.length * 100000 + r[0] + r[` +
				fmt.Sprint(n-1) + `]; } s;`,
		})
	}

	// An object literal is the same depth in a different shape, and its values
	// are consumed by DEFINE_FIELD rather than all at once.
	for _, n := range []int{9, 12, 20} {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf("p%d: %d", i, i*7)
		}
		cases = append(cases, struct{ name, src string }{
			fmt.Sprintf("object-literal-%d", n),
			`function f(k){ return {` + strings.Join(parts, ",") + `, last: k}; }
			 var s = 0; for (var i = 0; i < 3000; i++) { var o = f(i); s = o.p0 + o.p` +
				fmt.Sprint(n-1) + ` + o.last; } s;`,
		})
	}

	cases = append(cases,
		// A call with many arguments: the callee and every argument are live at
		// once, and the result lands in the callee's slot — the case that was
		// wrong, because that slot is also the one coming back into the window.
		struct{ name, src string }{"call-with-twelve-arguments", `
			function g(a,b,c,d,e,f2,g2,h,i2,j,k,l){ return a+b+c+d+e+f2+g2+h+i2+j+k+l; }
			function f(n){ return g(n,1,2,3,4,5,6,7,8,9,10,11); }
			var s = 0; for (var i = 0; i < 3000; i++) s = f(i); s;`},
		struct{ name, src string }{"method-call-with-twelve-arguments", `
			var o = { g: function(a,b,c,d,e,f2,g2,h,i2,j,k,l){ return a+b+c+d+e+f2+g2+h+i2+j+k+l; } };
			function f(n){ return o.g(n,1,2,3,4,5,6,7,8,9,10,11); }
			var s = 0; for (var i = 0; i < 3000; i++) s = f(i); s;`},
		struct{ name, src string }{"new-with-twelve-arguments", `
			function C(a,b,c,d,e,f2,g2,h,i2,j,k,l){ this.v = a+b+c+d+e+f2+g2+h+i2+j+k+l; }
			function f(n){ return new C(n,1,2,3,4,5,6,7,8,9,10,11).v; }
			var s = 0; for (var i = 0; i < 3000; i++) s = f(i); s;`},
		// Arithmetic across the boundary: the operands are deep, so the guards
		// and the unboxing read registers the window has moved.
		struct{ name, src string }{"deep-arithmetic", `
			function f(a){ return [1,2,3,4,5,6,7,8, a*2 + a/4 - (a%3), 10, 11]; }
			var s = 0; for (var i = 1; i < 3000; i++) { var r = f(i); s = r[8] + r[10]; } s;`},
		// A helper call while the stack is deep: the spill covers the window
		// and the slots below it are already in the array they live in.
		struct{ name, src string }{"helper-call-while-deep", `
			function f(a, o){ return [1,2,3,4,5,6,7,8,9, o.x, a in o, typeof o]; }
			var s = ""; for (var i = 0; i < 3000; i++) { var r = f("x", {x: i}); s = "" + r[9] + r[10] + r[11] + r.length; } s;`},
		// A throw unwinding out of a deep stack, and a property read deep
		// enough that its inline cache has no spare registers left.
		struct{ name, src string }{"throw-from-a-deep-stack", `
			function f(a, o){ return [1,2,3,4,5,6,7,8,9,10, o.x]; }
			var s = 0; for (var i = 0; i < 3000; i++) s = f(0, {x:i})[10];
			var m = ""; try { f(0, null); } catch (e) { m = e.name; } "" + s + ":" + m;`},
		// Strings, so a wrong slot shows up as wrong text rather than as a
		// number that happens to be close.
		struct{ name, src string }{"deep-string-concatenation", `
			function f(a){ return ["a","b","c","d","e","f","g","h","i", a, "k"].join("-"); }
			var s = ""; for (var i = 0; i < 3000; i++) s = f("Z" + i); s;`},
		// Nested literals: the inner one pushes and pops while the outer one is
		// already past the boundary.
		struct{ name, src string }{"nested-literals", `
			function f(a){ return [1,2,3,4,5,6,7,8, [9, 10, a, 12], 13, 14]; }
			var s = 0; for (var i = 0; i < 3000; i++) { var r = f(i); s = r[8][2] + r[10] + r.length; } s;`},
	)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// And they have to compile: run against a nine-slot stack these all pass by
// refusing, which is what they did before the window existed.
func TestDeepStacksCompile(t *testing.T) {
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	for _, tc := range []struct {
		src   string
		depth int
	}{
		{"function f(k){ return [1,2,3,4,5,6,7,8,9,10,k]; }", 11},
		{"function f(k){ return {a:1,b:2,c:3,d:4,e:5,f:6,g:7,h:8,i:9,j:k}; }", 10},
		{"function f(k,g){ return g(k,1,2,3,4,5,6,7,8,9,10,11); }", 13},
		{"function f(k){ return [1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,k]; }", 31},
	} {
		var why string
		fn := jitFn(t, tc.src)
		c := jitCompile(fn, &why)
		if c == nil {
			t.Errorf("refused %q: %s", tc.src, why)
			continue
		}
		c.free()
	}

	// And there is no shallow ceiling above it. The deepest function in the
	// Octane corpus is a source file that is one array literal — seventeen
	// thousand operands live at once — and it compiles because the array the
	// window slides over is allocated per frame at the depth the function needs
	// rather than being a fixed part of every context.
	deep := make([]string, 20000)
	for i := range deep {
		deep[i] = fmt.Sprint(i % 97)
	}
	src := "function f(k){ var a = [" + strings.Join(deep, ",") + ", k]; return a.length + a[0] + a[20000]; }"
	fn := jitFn(t, src)
	var why string
	c := jitCompile(fn, &why)
	if c == nil {
		t.Fatalf("refused a 20,000-deep literal: %s", why)
	}
	defer c.free()
	if c.slots < 20000 {
		t.Errorf("recorded %d operand slots for a 20,000-element literal", c.slots)
	}
}
