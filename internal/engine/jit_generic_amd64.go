//go:build amd64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// The operators, for operands whose type is not known.
//
// Everything above this file rests on knowing that a value is a Number before an
// instruction is emitted for it. That is what makes the arithmetic a single SSE
// instruction with no guard, and it is worth keeping — but it is also why a
// property read could be served in three nanoseconds and still be useless. A
// field holds anything, so `sum += o.a` had no template at all and the whole
// function went back to the interpreter, cache and all.
//
// The way out is not deoptimisation. Deoptimisation is for a guard that fails
// after the frame has been changed, and there is nothing like that here: the
// operands are still in their registers, untouched, and the runtime has an
// implementation of every one of these operators already. So the guard tests
// both operands for the tag bits, takes the SSE path when neither has them, and
// otherwise calls out to exactly what the interpreter would have called. Two
// compares and a branch that is not taken is the whole cost of the fast path.
//
// What the runtime is handed matters as much as that it is called. The operands
// are already on the compiled operand stack, and calling out spills that stack
// into the context where the collector can see it — so the helper reads them
// from there rather than being passed them, and a valueOf that runs a garbage
// collection finds both operands rooted.

// jitEmitNumberPair falls through when x and y are both Numbers and branches to
// slow when either is not.
//
// An untagged Value *is* an IEEE double, so IsNumber is one unsigned compare
// against the threshold R15 holds — the same test the prologue makes of a
// parameter, for the same reason.
//
// Falling through counts, which is what makes it possible to say afterwards
// whether these operators were reached at all — a guard that always sends the
// operands to the runtime and one that never does look exactly alike from the
// outside. The increment is emitted only when the counters are on, because a
// counter nobody reads is still a store to a shared line.
func jitEmitNumberPair(a *jitasm.Asm, x, y jitasm.Reg, slow *jitasm.Label) {
	a.CmpRegReg(x, jitRegGuard)
	a.Jcc(jitasm.CondA, slow)
	a.CmpRegReg(y, jitRegGuard)
	a.Jcc(jitasm.CondA, slow)
	if jitStats.enabled {
		// Before the operator rather than after it: a fused comparison branches
		// away, and an increment after that would only count one of the arms.
		// Nothing here depends on the flags this destroys.
		a.MovRegImm64(jitRegScratch, uint64(jitGenericFastAddr()))
		a.AddMemImm32(jitRegScratch, 0, 1)
	}
}

// jitCallBinary emits the call out for a generic binary operator.
//
// The operands are not passed. jitCallHelper spills the live operand stack into
// the context and records its depth, which puts them at the top of Spill and
// roots them for as long as the helper runs — so the depth and the opcode are
// all the helper needs, and they go in the one argument slot the protocol leaves
// untraced.
// jitEmitNumber is jitEmitNumberPair for the operators that take one operand.
func jitEmitNumber(a *jitasm.Asm, x jitasm.Reg, slow *jitasm.Label) {
	a.CmpRegReg(x, jitRegGuard)
	a.Jcc(jitasm.CondA, slow)
	if jitStats.enabled {
		a.MovRegImm64(jitRegScratch, uint64(jitGenericFastAddr()))
		a.AddMemImm32(jitRegScratch, 0, 1)
	}
}

// jitCallUnary is jitCallBinary for one operand: the opcode goes in the
// untraced argument slot and the operand comes off the top of the spill area.
func jitCallUnary(a *jitasm.Asm, sp int, op Opcode, fixups *[]jitResumeFixup, deep bool) bool {
	if sp < 1 || sp > jitMaxDepth {
		return false
	}
	a.MovRegImm64(jitRegScratch, uint64(op))
	a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
	return jitCallHelper(a, sp, jitHelperUnary, fixups, deep)
}

func jitCallBinary(a *jitasm.Asm, sp int, op Opcode, helper uint32, fixups *[]jitResumeFixup, deep bool) bool {
	if sp < 2 || sp > jitMaxDepth {
		return false
	}
	a.MovRegImm64(jitRegScratch, uint64(op)|uint64(uint32(sp))<<32)
	a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
	return jitCallHelper(a, sp, helper, fixups, deep)
}

// jitBinaryOperands recovers a generic binary operator's operands from the
// spilled operand stack, and the operator itself.
//
// Reports false for a depth that cannot be right, which can only mean the
// emitter and this disagree — better to throw than to read a slot that holds
// something else.
func jitBinaryOperands(ctx *jitmem.ExecContext) (Opcode, Value, Value, bool) {
	op := Opcode(uint32(ctx.Args[3]))
	sp := int(uint32(ctx.Args[3] >> 32))
	if sp < 2 || sp > jitMaxDepth || uint64(sp) != ctx.StackN {
		return 0, 0, 0, false
	}
	stack := jitFrameStack(ctx)
	return op, stack[sp-2], stack[sp-1], true
}

// jitRelationalValue materialises a relational comparison of two Numbers as a
// Boolean in r, for the cases where no branch consumes it.
//
// The unordered arm is not an optimisation of the ordered one. UCOMISD reports a
// NaN operand by setting carry and zero as well as parity, so `<` and `<=` come
// out true and `>` and `>=` come out false, and all four are wrong: every
// relational operator is false when either side is a NaN. The correction has to
// happen before the tag is or-ed in, because that OR destroys the parity flag.
func jitRelationalValue(a *jitasm.Asm, op Opcode, r jitasm.Reg) {
	var c jitasm.FCond
	switch op {
	case OpLt:
		c = jitasm.FCondB
	case OpLe:
		c = jitasm.FCondBE
	case OpGt:
		c = jitasm.FCondA
	default: // OpGe
		c = jitasm.FCondAE
	}
	unordered := a.NewLabel()
	done := a.NewLabel()

	a.SetfccReg(c, r)    // neither SETcc nor MOVZX disturbs the flags, so
	a.MovzxRegReg8(r, r) // parity is still the one UCOMISD set
	a.Jfcc(jitasm.FCondUnordered, unordered)
	a.MovRegImm64(jitRegScratch, uint64(mkfalse()))
	a.OrRegReg(r, jitRegScratch)
	a.Jmp(done)

	a.Bind(unordered)
	a.MovRegImm64(r, uint64(mkbool(false)))
	a.Bind(done)
}

// jitBoolBranch branches to target when the Boolean in r has the sense the
// branch is taken on.
//
// The runtime's comparison operators return a Boolean and nothing else, so this
// is a compare against one of the two bit patterns rather than a truthiness
// test.
func jitBoolBranch(a *jitasm.Asm, whenTrue bool, r jitasm.Reg, target *jitasm.Label) {
	a.MovRegImm64(jitRegScratch, uint64(mkbool(whenTrue)))
	a.CmpRegReg(r, jitRegScratch)
	a.Jcc(jitasm.CondE, target)
}
