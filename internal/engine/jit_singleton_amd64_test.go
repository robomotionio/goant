//go:build amd64

package engine

import (
	"fmt"
	"strings"
	"testing"
)

// jitSingletonOperands is every kind of value the emitted comparison has to get
// right, which is every kind there is: the singletons themselves, both sides of
// the Number/tagged split, the two string forms that are equal without sharing a
// handle, an object, and the values that make `==` and `===` disagree.
var jitSingletonOperands = []string{
	"undefined", "null", "true", "false",
	"0", "-0", "1", "NaN", "Infinity", "-Infinity", "1e308*10",
	`""`, `"a"`, `("a"+"")`, `"0"`, `"null"`, `"false"`,
	"({})", "[]", "[0]", "(function(){})", "Symbol.iterator",
	"0n", "1n",
	"new Number(0)", "new String('')", "new Boolean(false)",
}

var jitSingletonRHSs = []string{"undefined", "null", "true", "false"}
var jitSingletonOps = []string{"==", "!=", "===", "!=="}

// TestSingletonComparisonAgreesWithTheInterpreter is the gate on the whole file.
//
// Every operator against every singleton against every kind of operand, run once
// interpreted and once compiled and required to produce the same bits. A
// template that answers exactly is only worth having if it answers correctly,
// and the failure mode here is a wrong Boolean rather than a crash — which no
// conformance run over this engine's own tests would necessarily catch, because
// the comparison sites in test262 are not in functions hot enough to tier up.
func TestSingletonComparisonAgreesWithTheInterpreter(t *testing.T) {
	for _, op := range jitSingletonOps {
		for _, rhs := range jitSingletonRHSs {
			for _, lhs := range jitSingletonOperands {
				// Wrapped in a function called past the tier's threshold, since
				// the comparison has to be inside compiled code to be compiled at
				// all. The result is accumulated as a string so a difference in
				// any iteration survives to the end.
				src := fmt.Sprintf(`
					function cmp(v) { return (v %s %s) ? 1 : 0; }
					var s = 0;
					for (var i = 0; i < 200; i++) s += cmp(%s);
					s;
				`, op, rhs, lhs)
				name := fmt.Sprintf("%s %s %s", lhs, op, rhs)
				jitBothWays(t, name, src)
			}
		}
	}
}

// The fused form takes a different path — the comparison feeds a branch and no
// Boolean is ever materialised — so it needs its own differential.
func TestSingletonComparisonFusedIntoABranch(t *testing.T) {
	for _, op := range jitSingletonOps {
		for _, rhs := range jitSingletonRHSs {
			for _, lhs := range jitSingletonOperands {
				src := fmt.Sprintf(`
					function cmp(v) { if (v %s %s) { return 7; } return 9; }
					var s = 0;
					for (var i = 0; i < 200; i++) s += cmp(%s);
					s;
				`, op, rhs, lhs)
				name := fmt.Sprintf("if (%s %s %s)", lhs, op, rhs)
				jitBothWays(t, name, src)
			}
		}
	}
}

// A comparison whose literal operand is also a branch target cannot use the
// literal: control can arrive from the branch with something else on the stack.
// `(c ? a : null) === null` is the shape that produces it.
func TestSingletonComparisonDeclinesABranchTarget(t *testing.T) {
	for _, lhs := range jitSingletonOperands {
		src := fmt.Sprintf(`
			function cmp(c, v) { return ((c ? v : null) === null) ? 1 : 0; }
			var s = 0;
			for (var i = 0; i < 200; i++) s += cmp(i %% 2, %s);
			s;
		`, lhs)
		jitBothWays(t, "ternary "+lhs, src)
	}
}

// Both operand orders, since only the right-hand literal is recognised. The
// left-hand form must still give the right answer through the helper.
func TestSingletonComparisonOnTheLeft(t *testing.T) {
	for _, op := range jitSingletonOps {
		for _, k := range jitSingletonRHSs {
			for _, other := range jitSingletonOperands {
				src := fmt.Sprintf(`
					function cmp(v) { return (%s %s v) ? 1 : 0; }
					var s = 0;
					for (var i = 0; i < 200; i++) s += cmp(%s);
					s;
				`, k, op, other)
				jitBothWays(t, fmt.Sprintf("%s %s %s", k, op, other), src)
			}
		}
	}
}

// `x == true` and `x == false` are not emitted, because abstract equality
// coerces a Boolean through ToNumber and an object operand then runs
// ToPrimitive — user code, which can throw. This asserts the refusal rather than
// the reason: if the template ever grows to cover them, the throwing case has to
// be handled and this is where that gets noticed.
func TestAbstractEqualityWithABooleanStillCallsOut(t *testing.T) {
	if jitSingletonComparable(OpEq, mkbool(true)) || jitSingletonComparable(OpNe, mkbool(false)) {
		t.Fatal("`x == true` is emitted, but ToNumber on the Boolean and " +
			"ToPrimitive on an object operand can call user code and throw")
	}
	for _, k := range []string{"true", "false"} {
		src := `
			function cmp(v) { return (v == ` + k + `) ? 1 : 0; }
			var s = 0, thrown = 0;
			var o = { valueOf: function () { throw new Error("boom"); } };
			for (var i = 0; i < 200; i++) s += cmp(i % 2 ? 1 : 0);
			try { cmp(o); } catch (e) { thrown = 1; }
			"" + s + ":" + thrown;
		`
		jitBothWays(t, "coercing == "+k, src)
	}
}

// The whole point is that these stop leaving compiled code, and a probe that
// declines gives the same answer as one that hits — so the counter is the only
// thing that can tell them apart.
func TestSingletonComparisonDoesNotCallOut(t *testing.T) {
	savedJIT, savedStats := jitEnabled, jitStats.enabled
	jitEnabled, jitStats.enabled = true, true
	defer func() { jitEnabled, jitStats.enabled = savedJIT, savedStats }()

	before := jitStats.genSlow
	rt := New()
	if _, err := rt.RunString("noexit.js", `
		function isNull(v) { return v == null; }
		function isStrictNull(v) { return v === null; }
		function notFalse(v) { return v !== false; }
		var s = 0;
		for (var i = 0; i < 20000; i++) {
			if (isNull(null)) s++;
			if (isStrictNull(i)) s++;
			if (notFalse(i)) s++;
		}
		s;
	`); err != nil {
		t.Fatal(err)
	}
	if got := jitStats.genSlow - before; got > 1000 {
		t.Errorf("%d operands went to the runtime; the singleton templates are not being reached", got)
	}
}

// A sanity check on the claim the `==` template rests on, stated as code rather
// than left in a comment: TUndef and TNull are adjacent, so the two are one
// unsigned range, and nothing else lands in it.
func TestNullishIsOneUnsignedRange(t *testing.T) {
	if TNull != TUndef+1 {
		t.Fatalf("TUndef=%d TNull=%d are no longer adjacent; the `x == null` "+
			"template subtracts and compares against 1", TUndef, TNull)
	}
	base := uint64(nanboxPrefix>>nanboxTypeShift) + uint64(TUndef)
	for _, v := range []Value{
		mkundef(), mknull(), mkbool(true), mkbool(false), mknum(0), mknum(1),
		mknum(-1), tov(1e308), mkval(TStr, 1), mkval(TObj, 1), mkval(TSymbol, 1),
		mkval(TBigInt, 1), mkval(TArr, 1), mkval(TFunc, 1),
	} {
		inRange := (uint64(v)>>nanboxTypeShift)-base <= 1
		if inRange != v.IsNullish() {
			t.Errorf("%v (%#016x): the emitted range test says %v, IsNullish says %v",
				v.Type(), uint64(v), inRange, v.IsNullish())
		}
	}
}

// Named so a reader looking for the coverage finds it: the operand list has to
// keep containing the cases that separate `==` from `===`.
func TestSingletonOperandsCoverTheInterestingCases(t *testing.T) {
	joined := strings.Join(jitSingletonOperands, " ")
	for _, want := range []string{"NaN", "-0", "0n", `""`, "new Boolean(false)", "Symbol.iterator"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the differential no longer covers %s", want)
		}
	}
}
