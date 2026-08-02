//go:build amd64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// Branching on a value rather than on a comparison.
//
// This tier could only branch on a comparison it had fused with the branch
// itself, so `if (x)`, `a && b`, `a || b` and `a ? b : c` — every conditional
// whose test is a bare value — refused the whole function. Weighted by frame
// entries that looked small. Weighted by interpreted bytecode instructions it is
// the largest item left: 41 million in a single Richards function, entered a
// hundred and sixty-five times, and 13.4 million across seven in DeltaBlue.
//
// ToBoolean is a switch over the value's type, and most of it is one compare.
// What is emitted is the cases a hot branch actually tests — a Number, the two
// Booleans, undefined, null, and the three object tags — and what is left to the
// runtime is the two that need to look at something other than the Value itself:
// a String, whose truth is its length, and a BigInt, whose truth is its sign.
//
// The order is chosen for the branch predictor rather than for the specification:
// the Number test comes first because it is one unsigned compare that also
// separates every tagged value from every untagged one.

// jitEmitTruthyBranch branches to target when the truth of the value in v
// matches whenTrue.
//
// v is consumed. sp is the depth *including* v, which is what the slow path
// spills; jitRegScratch is clobbered. back says the target is a loop header, so
// the branch to it has to carry the fuel check.
func jitEmitTruthyBranch(a *jitasm.Asm, v jitasm.Reg, whenTrue, back bool, target *jitasm.Label,
	sp int, fixups *[]jitResumeFixup) bool {
	scratch := jitRegScratch
	tagged := a.NewLabel()
	truthy := a.NewLabel()
	falsy := a.NewLabel()
	after := a.NewLabel()

	// A Number, which is every untagged Value. Falsy for both zeroes and for
	// NaN: UCOMISD sets ZF for equality and PF for unordered, so the two
	// branches below are the whole of `d != 0 && !isNaN(d)`.
	a.CmpRegReg(v, jitRegGuard)
	a.Jcc(jitasm.CondA, tagged)
	a.MovqXReg(jitasm.X0, v)
	a.XorpdXX(jitasm.X1, jitasm.X1)
	a.UcomisdXX(jitasm.X0, jitasm.X1)
	a.Jcc(jitasm.CondP, falsy)
	a.Jcc(jitasm.CondE, falsy)
	a.Jmp(truthy)

	// The four values that are their own answer.
	a.Bind(tagged)
	for _, tc := range []struct {
		val Value
		to  *jitasm.Label
	}{
		{mkbool(true), truthy},
		{mkbool(false), falsy},
		{mkundef(), falsy},
		{mknull(), falsy},
	} {
		a.MovRegImm64(scratch, uint64(tc.val))
		a.CmpRegReg(v, scratch)
		a.Jcc(jitasm.CondE, tc.to)
	}

	// An object is truthy whatever is in it. Three tags tested rather than the
	// object family as a mask: these are the ones a condition meets, and a
	// Promise or a generator taking the runtime path costs a call it would
	// otherwise not make in a branch anyone measures.
	a.MovRegReg(scratch, v)
	a.ShrRegImm(scratch, nanboxTypeShift)
	for _, t := range []Type{TObj, TArr, TFunc} {
		a.CmpRegImm32(scratch, uint32(nanboxPrefix>>nanboxTypeShift)|uint32(t))
		a.Jcc(jitasm.CondE, truthy)
	}

	// A String or a BigInt: the answer is in what the Value points at rather
	// than in the Value, so the runtime answers it.
	if !jitCallHelper(a, sp, jitHelperToBoolean, fixups) {
		return false
	}
	a.MovRegMem(scratch, jitasm.RegCtx, jitmem.CtxOffRet)
	a.TestRegReg(scratch, scratch)
	a.Jcc(jitasm.CondNE, truthy)

	// Both arms end unconditionally, so their order is free and neither can fall
	// into the other.
	a.Bind(falsy)
	if whenTrue {
		a.Jmp(after)
	} else if back {
		jitBackEdge(a, target, fixups)
	} else {
		a.Jmp(target)
	}
	a.Bind(truthy)
	if whenTrue {
		if back {
			jitBackEdge(a, target, fixups)
		} else {
			a.Jmp(target)
		}
	} else {
		a.Jmp(after)
	}
	a.Bind(after)
	return true
}
