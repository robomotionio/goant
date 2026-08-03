//go:build amd64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// Templates for the operators that work on numbers alone: the bitwise family,
// negation, the increments, and equality between two Numbers.
//
// They are here rather than in the emitter's switch because they share one
// awkward thing — JavaScript's bitwise operators are not defined on doubles.
// Every one of them runs ToInt32 on both operands, does the work in 32 bits, and
// converts back, and that middle step is where the interesting cases live.

// jitToInt32 emits ToInt32 of the Number in src, leaving the result in dst
// zero-extended to 64 bits. dst and src may be the same register.
//
// Zero-extension is not incidental. Between here and the conversion back to a
// double, the value sits in a register that a helper call would spill into the
// context, where the collector reads it as a Value. Anything at or above
// 0xFFF0000000000000 reads as a tagged one, so a sign-extended -1 would be
// scanned as an object handle pointing at nothing. Kept zero-extended, an
// intermediate is always a tiny double and always ignored.
//
// CVTTSD2SI does the whole conversion for any double it can represent, and
// truncating the 64-bit result to 32 bits is exactly the specification's modulo
// 2^32. It reports the inputs it cannot convert — NaN, the infinities, and
// anything of magnitude 2^63 or more — by returning INT64_MIN, and for all but
// the last of those INT64_MIN's low half is the right answer anyway: ToInt32 of
// NaN and of either infinity is zero. So only finite magnitudes at or above 2^63
// need the slow path, and testing for INT64_MIN sends a handful of harmless
// values there too, which costs nothing but time nobody spends.
//
// Reports false if the operand stack is deeper than a helper call can spill,
// which is a refusal rather than an error.
func jitToInt32(a *jitasm.Asm, dst, src jitasm.Reg, sp int, fixups *[]jitResumeFixup) bool {
	if sp > jitMaxDepth {
		return false
	}
	slow := a.NewLabel()
	done := a.NewLabel()

	// Stashed before the conversion because the conversion may overwrite it:
	// callers pass dst == src, and the slow path still needs the original.
	a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+16, src)
	a.MovqXReg(jitasm.X0, src)
	a.Cvttsd2siRegX(dst, jitasm.X0)
	// SUB overflows for INT64_MIN and for nothing else. LEA puts the one back
	// and truncates to 32 bits in the same instruction.
	a.SubRegImm32(dst, 1)
	a.Jcc(jitasm.CondO, slow)
	a.Lea32RegMem(dst, dst, 1)
	a.Jmp(done)

	a.Bind(slow)
	if !jitCallHelper(a, sp, jitHelperToInt32, fixups) {
		return false
	}
	a.MovRegMem(dst, jitasm.RegCtx, jitmem.CtxOffRet)
	a.Bind(done)
	return true
}

// jitFromInt32 emits the conversion back: the 32-bit result in r becomes the
// double that JavaScript says a bitwise operator produces.
//
// signed distinguishes the one operator whose result is not an int32. `>>>`
// yields a uint32, which is why its result is left zero-extended and every other
// one is widened with its sign.
func jitFromInt32(a *jitasm.Asm, r jitasm.Reg, signed bool) {
	if signed {
		a.MovsxdRegReg(r, r)
	}
	a.Cvtsi2sdXReg(jitasm.X0, r)
	a.MovqRegX(r, jitasm.X0)
	// No canonicalisation: an integer converted to a double is never a NaN.
}

// jitBitwise emits one of the six binary bitwise operators over two Numbers.
func jitBitwise(a *jitasm.Asm, op Opcode, x, y jitasm.Reg, sp int, fixups *[]jitResumeFixup) bool {
	// The right operand is converted first so that the left is still boxed, and
	// so recognisable to the collector, for as long as possible. Order is
	// otherwise unobservable here: both operands are already Numbers, so neither
	// conversion can run a valueOf or throw.
	if !jitToInt32(a, y, y, sp, fixups) {
		return false
	}
	if !jitToInt32(a, x, x, sp, fixups) {
		return false
	}

	switch op {
	case OpBand:
		a.And32RegReg(x, y)
	case OpBor:
		a.Or32RegReg(x, y)
	case OpBxor:
		a.Xor32RegReg(x, y)
	case OpShl, OpShr, OpUshr:
		// x86 masks a 32-bit shift count to five bits, which is the `& 31` the
		// specification asks for, so the count needs no masking of its own. It
		// does need to be in CL, which is why that register is not a stack slot.
		a.Mov32RegReg(jitRegScratch, y)
		switch op {
		case OpShl:
			a.Shl32RegCL(x)
		case OpShr:
			a.Sar32RegCL(x) // signed: `>>` keeps the sign
		default:
			a.Shr32RegCL(x) // unsigned: `>>>` does not
		}
	}
	jitFromInt32(a, x, op != OpUshr)
	return true
}

// jitBoolean materialises the flags as a JavaScript Boolean in r.
//
// mkbool differs between true and false in one bit, so widening the byte SETcc
// writes and or-ing in the tag builds either.
func jitBoolean(a *jitasm.Asm, c jitasm.Cond, r jitasm.Reg) {
	a.SetccReg(c, r)
	a.MovzxRegReg8(r, r)
	a.MovRegImm64(jitRegScratch, uint64(mkfalse()))
	a.OrRegReg(r, jitRegScratch)
}

// jitFuse looks past a comparison for the branch that consumes its result.
//
// A comparison produces a Boolean, which this tier has no representation for
// among its stack kinds, so it is only compiled when something immediately
// branches on it — then the Boolean never has to exist. `!` in between changes
// nothing about that: it is another operator this tier cannot represent the
// result of, and folding it into the sense of the branch removes it as surely as
// the comparison. Which is what makes `if (!(a === b))` compile.
//
// Reports the label to branch to, whether the branch is taken when the
// comparison is true, and where emission continues.
func jitFuse(code []byte, labels map[int]*jitasm.Label, at int) (*jitasm.Label, bool, int, bool) {
	polarity := true // whether the value at `at` still has the comparison's sense
	for {
		if at >= len(code) {
			return nil, false, 0, false
		}
		if _, isTarget := labels[at]; isTarget {
			// Something branches into the middle of the sequence, so the value
			// does not reliably come from the comparison.
			return nil, false, 0, false
		}
		switch Opcode(code[at]) {
		case OpNot:
			polarity = !polarity
			at++
		case OpJmpTrue, OpJmpFalse:
			target := int(readU32(code, at+1))
			l, known := labels[target]
			if !known || target <= at {
				// Unknown, or a backward branch: a do-while, whose target is
				// reached with a stack shape this tier does not model.
				return nil, false, 0, false
			}
			if Opcode(code[at]) == OpJmpFalse {
				polarity = !polarity
			}
			return l, polarity, at + 5, true
		default:
			return nil, false, 0, false
		}
	}
}

// jitEqualsValue materialises the result of an equality test as a Boolean in r,
// for the cases where no branch consumes it.
//
// The unordered case needs its own arm in both directions. UCOMISD reports a NaN
// operand by setting the zero flag as well as parity, so SETE answers true and
// SETNE answers false, and both are the wrong way round: nothing equals a NaN.
// The correction has to happen before the tag is or-ed in, because that OR is
// what destroys the parity flag it depends on.
func jitEqualsValue(a *jitasm.Asm, negate bool, r jitasm.Reg) {
	c := jitasm.CondE
	if negate {
		c = jitasm.CondNE
	}
	unordered := a.NewLabel()
	done := a.NewLabel()

	a.SetccReg(c, r)     // neither SETcc nor MOVZX disturbs the flags, so
	a.MovzxRegReg8(r, r) // parity is still the one UCOMISD set
	a.Jcc(jitasm.CondP, unordered)
	a.MovRegImm64(jitRegScratch, uint64(mkfalse()))
	a.OrRegReg(r, jitRegScratch)
	a.Jmp(done)

	a.Bind(unordered)
	a.MovRegImm64(r, uint64(mkbool(negate)))
	a.Bind(done)
}

// jitEqualsBranch emits the branch for a fused equality test.
//
// UCOMISD reports equality as ZF, but sets ZF for unordered operands too, so a
// test that only looked at ZF would report NaN === NaN as true. The parity flag
// separates them and has to be consulted first in both directions.
//
// Both strict and abstract equality reduce to this when both operands are
// Numbers, which they are wherever this is emitted.
func jitEqualsBranch(a *jitasm.Asm, negate, whenTrue bool, target *jitasm.Label) {
	if (!negate) == whenTrue {
		skip := a.NewLabel()
		a.Jcc(jitasm.CondP, skip) // unordered is not equal
		a.Jcc(jitasm.CondE, target)
		a.Bind(skip)
		return
	}
	a.Jcc(jitasm.CondP, target) // unordered: not equal, so the branch is taken
	a.Jcc(jitasm.CondNE, target)
}
