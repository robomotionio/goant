//go:build amd64 || arm64

package engine

import (
	"math"
	"strings"
	"testing"
)

// A comparison materialised as a value, at every operand depth.
//
// A comparison that feeds a branch never becomes a value; one that does goes
// through SETcc, and on amd64 that instruction names a *byte* register. Two of
// the operand-stack slots are RSI and RDI, whose byte forms need a REX prefix
// that means nothing else — without it the encoding names DH and BH instead,
// and the answer lands in the high byte of a different operand while the slot
// that should hold it keeps whatever it had.
//
// Nothing crashes, and nothing shallow is wrong: the slots involved are the
// fourth and fifth, which a small function never reaches. test262 and mjsunit
// were both green with this in; Octane's TypeScript benchmark reported "Parse
// errors". So the depth is the test, and this walks it.
func TestAComparisonIsRightAtEveryOperandDepth(t *testing.T) {
	inputs := []struct{ a, b float64 }{
		{1, 2}, {2, 1}, {3, 3}, {-1, 1}, {0, -0},
		{math.NaN(), 1}, {1, math.NaN()}, {math.NaN(), math.NaN()},
		{math.Inf(1), math.Inf(1)}, {math.Inf(-1), 0},
	}

	for _, op := range []string{"<", "<=", ">", ">=", "==", "!=", "===", "!=="} {
		for depth := 0; depth <= 8; depth++ {
			// `a+(a+(a+( ... (a OP b) ... )))`, which leaves `depth` operands
			// live when the comparison is evaluated. The additions are there to
			// occupy slots and nothing else.
			src := "function f(a,b){ return " +
				strings.Repeat("a+(", depth) + "(a " + op + " b)" + strings.Repeat(")", depth) +
				"; }"

			rt, fn := jitFnRT(t, src)
			c := jitCompile(fn, nil)
			if c == nil {
				// A depth past the operand window is refused, which is correct
				// and not what this is testing.
				continue
			}
			for _, in := range inputs {
				av, bv := tov(in.a), tov(in.b)
				locals := make([]Value, fn.maxLocals)
				locals[0], locals[1] = av, bv
				got, ok := jitRunT(t, rt, c, fn, locals)
				if !ok {
					t.Errorf("%s(%v,%v): bailed on two Numbers", src, in.a, in.b)
					continue
				}
				want := interpret(t, src, av, bv)
				if uint64(got) != uint64(want) {
					t.Errorf("depth %d, %q: f(%v,%v) = %#016x (%v), interpreter gives %#016x (%v)",
						depth, op, in.a, in.b, uint64(got), got, uint64(want), want)
				}
			}
			c.free()
		}
	}
}

// The same question asked of the value the comparison produces rather than of
// what it is added to: a boolean the caller sees, from as deep as the operand
// window goes.
func TestABooleanSurvivesTheOperandStack(t *testing.T) {
	for depth := 0; depth <= 8; depth++ {
		// The comparison is evaluated with `depth` operands live and its answer
		// is what comes back, so a slot corrupted instead of written shows up as
		// the wrong Value rather than as the wrong number.
		src := "function f(a,b){ var t = " +
			strings.Repeat("a+(", depth) + "0" + strings.Repeat(")", depth) +
			"; var u = (a < b); return u === true ? t + 1 : t - 1; }"

		rt, fn := jitFnRT(t, src)
		c := jitCompile(fn, nil)
		if c == nil {
			continue
		}
		for _, in := range []struct{ a, b float64 }{{1, 2}, {2, 1}, {math.NaN(), 1}} {
			av, bv := tov(in.a), tov(in.b)
			locals := make([]Value, fn.maxLocals)
			locals[0], locals[1] = av, bv
			got, ok := jitRunT(t, rt, c, fn, locals)
			if !ok {
				t.Errorf("depth %d: bailed on two Numbers", depth)
				continue
			}
			want := interpret(t, src, av, bv)
			if uint64(got) != uint64(want) {
				t.Errorf("depth %d: f(%v,%v) = %#016x, interpreter gives %#016x",
					depth, in.a, in.b, uint64(got), uint64(want))
			}
		}
		c.free()
	}
}
