//go:build amd64 || arm64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
)

// Comparing against `null`, `undefined`, `true` or `false`, in machine code.
//
// The generic equality operators guard on both operands being Numbers and hand
// everything else to the runtime, which is correct and, on real programs, the
// common case rather than the rare one. Counting exits by helper says how
// common:
//
//	richards      14,693,005 Equals exits
//	earley-boyer  11,611,708
//
// against fourteen `== null` / `!= null` sites in richards and a hundred and
// thirty-eight `=== null` / `!== false` / `=== true` sites in earley-boyer. A
// comparison against a literal is the shape this code is actually made of, and
// every one of them was leaving compiled code for a round trip that costs more
// than the surrounding arithmetic saves.
//
// Against a literal the answer needs no guard at all, because it is exact:
//
//   - `x === undefined|null|true|false` is bit equality with that singleton, and
//     nothing else in the value space has those bit patterns. One compare.
//   - `x == undefined|null` is `x` being one of the two. TUndef and TNull are
//     adjacent, so that is a subtract and an unsigned compare.
//
// Neither can throw and neither needs the operand's type known, so this replaces
// the guard-and-call rather than sitting in front of it.
//
// `x == true` and `x == false` are deliberately not here. Abstract equality
// coerces a Boolean through ToNumber and compares again, which for an object
// operand runs ToPrimitive and can call user code — a helper's job, not a
// template's.

// jitSingletonRHS reports the singleton the top of the operand stack holds at
// `at`, when that is knowable from the instruction before it.
//
// prevOp and prevIP are the previous instruction. The only thing that has to be
// established beyond its opcode is that control cannot arrive at `at` any other
// way: if `at` is a branch target then some predecessor jumped here and the top
// of the stack is whatever that path left, so the constant is not a constant.
//
// Deliberately local. The alternative — tracking a known-constant kind per stack
// slot through the whole emitter, beside the Number-ness one — has to be
// maintained at every site that writes a slot, and a slot wrongly believed
// constant is a miscompilation rather than a refusal. Reading the previous
// opcode cannot be wrong.
func jitSingletonRHS(labels map[int]*jitasm.Label, prevOp Opcode, prevIP, at int) (Value, bool) {
	if prevIP < 0 || prevIP >= at {
		return 0, false
	}
	if _, isTarget := labels[at]; isTarget {
		return 0, false
	}
	switch prevOp {
	case OpUndef:
		return mkundef(), true
	case OpNull:
		return mknull(), true
	case OpTrue:
		return mkbool(true), true
	case OpFalse:
		return mkbool(false), true
	}
	return 0, false
}

// jitSingletonComparable reports whether this operator and this singleton have
// an exact answer that can be emitted.
func jitSingletonComparable(op Opcode, k Value) bool {
	switch op {
	case OpSeq, OpSne:
		return true // bit equality, for all four
	case OpEq, OpNe:
		return k.IsNullish() // the Booleans coerce; see the note above
	}
	return false
}

// jitEmitSingletonEquals leaves the comparison's result in dst as a Boolean.
//
// x holds the value being compared and may be the same register as dst, which is
// what the call site wants: the result replaces the deeper of the two operands.
// jitRegScratch is clobbered.
func jitEmitSingletonEquals(a *jitasm.Asm, op Opcode, k Value, x, dst jitasm.Reg) {
	negate := op == OpNe || op == OpSne
	var c jitasm.Cond

	switch op {
	case OpSeq, OpSne:
		// Bit equality against the singleton. Every one of the four has a payload
		// fixed by its constructor — undefined and null carry zero, the Booleans
		// carry 0 and 1 — so no other Value can collide with them, and a Number
		// cannot because it is untagged.
		a.MovRegImm64(jitRegScratch, uint64(k))
		a.CmpRegReg(x, jitRegScratch)
		c = jitasm.CondE
		if negate {
			c = jitasm.CondNE
		}

	default:
		// `x == null` and `x == undefined`, which are the same question: is x one
		// of the two. Everything else abstract equality could do with a nullish
		// operand ends at "not equal" — the Boolean and object arms both require
		// the other side to be a type this one is not, and neither ToPrimitive nor
		// any other user code is reachable, so there is no throw to model.
		//
		// TUndef is 7 and TNull is 8, so the two are one unsigned range. The
		// subtract also disposes of Numbers: an untagged Value shifts to
		// something below the prefix and wraps to a very large number.
		jitEmitNullishFlags(a, x)
		c = jitasm.CondBE
		if negate {
			c = jitasm.CondA
		}
	}

	// The Boolean itself. SETcc gives 0 or 1 and a Boolean Value is the false
	// pattern with the payload or-ed in, which is the same construction
	// jitEqualsValue uses for the Number case.
	a.SetccReg(c, dst)
	a.MovzxRegReg8(dst, dst)
	a.MovRegImm64(jitRegScratch, uint64(mkfalse()))
	a.OrRegReg(dst, jitRegScratch)
}

// jitEmitNullishFlags sets the flags so that CondBE means "x is null or
// undefined". jitRegScratch is clobbered; x is not.
//
// TUndef is 7 and TNull is 8, so the two are one unsigned range. The subtract
// disposes of Numbers as a side effect: an untagged Value shifts to something
// below the prefix and wraps to a very large number.
func jitEmitNullishFlags(a *jitasm.Asm, x jitasm.Reg) {
	a.MovRegReg(jitRegScratch, x)
	a.ShrRegImm(jitRegScratch, nanboxTypeShift)
	a.SubRegImm32(jitRegScratch, uint32(nanboxPrefix>>nanboxTypeShift)+uint32(TUndef))
	a.CmpRegImm32(jitRegScratch, 1)
}

// jitEmitNullishTest leaves `x is null or undefined` in dst as a Boolean, which
// is the whole of IS_UNDEF_OR_NULL. dst may be x.
func jitEmitNullishTest(a *jitasm.Asm, x, dst jitasm.Reg) {
	jitEmitNullishFlags(a, x)
	a.SetccReg(jitasm.CondBE, dst)
	a.MovzxRegReg8(dst, dst)
	a.MovRegImm64(jitRegScratch, uint64(mkfalse()))
	a.OrRegReg(dst, jitRegScratch)
}
