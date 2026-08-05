//go:build amd64 || arm64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
)

// Writing `a[i] = v` into a TypedArray, in machine code.
//
// Until now this tier emitted NOTHING for an element store — not for a view and
// not for a fast array either. The comment on OpPutElem said why: everything the
// read establishes has to hold, and one thing more. For a fast array that is
// still true. For a view it is not, and the difference is what makes this the
// half of the work that is worth the least explanation and the most:
//
//   - There is no writability or extensibility to check. An integer-indexed
//     store into a view is never rejected and never throws: an index outside the
//     window is a silent no-op, in strict mode as well as sloppy, so the emitted
//     path and the runtime path have the same answer for the caller — nothing.
//   - There is no invocation-dirty bookkeeping. That exists so a handle written
//     into an object older than the invocation stops the pools being truncated
//     underneath it; this writes BYTES into storage Go already owns, and creates
//     no reference at all. Which is why noteSharedMutation is nowhere in the
//     runtime's typed-array store either.
//   - There is no write barrier, for the same reason. The collector traces
//     Values, and an element here is not one.
//
// So the store is the read's guard chain with the load turned around, and the
// only new question is the value.
//
// THE VALUE, AND WHY THE GUARD IS "AN EXACT INTEGER" RATHER THAN A CONVERSION.
//
// The spec's answer for an integer view is the value modulo the element width,
// which for an exact integer already in a register is precisely its low bits —
// so a truncating store IS ToInt32, ToUint8 and the rest, with no masking. What
// it is not is an answer for a fraction, a NaN, an infinity, or a magnitude past
// what an int64 holds; those need rounding and special cases, and they go to the
// runtime.
//
// Requiring an exact integer, by converting and converting back, is also what
// makes the two backends agree. amd64's CVTTSD2SI answers INT64_MIN for
// everything it cannot convert; arm64's FCVTZS saturates instead, so +Infinity
// comes back as INT64_MAX — whose low 32 bits are -1 where the spec says 0. The
// round-trip rejects every input the two disagree about, which is the same set,
// so neither backend needs to know about the other.

// jitEmitPutElemTyped emits the store.
//
// recv holds the view, key the index and val the value. obj and idx are scratch,
// as is jitRegScratch. Nothing is left on the operand stack: a store consumes
// its three operands and produces no value. Control reaches slow with recv, key
// and val untouched, which is what the runtime path needs.
//
// Reports whether it emitted anything.
func jitEmitPutElemTyped(a *jitasm.Asm, kind taKind, recv, key, val, obj, idx jitasm.Reg, slow, done *jitasm.Label) bool {
	if !jitStoreKindEmittable(kind) {
		return false
	}
	scratch := jitRegScratch

	// The window, the key, and the address — the read's chain exactly, which
	// leaves the address in jitRegScratch. It is moved out of there because
	// everything after this needs jitRegScratch itself: the value's round trip
	// through the double registers has nowhere else to land.
	//
	// idx is where it goes. Both idx and obj are finished with by then — the
	// address no longer depends on either — and obj is the register the
	// converted value needs.
	jitEmitTypedWindow(a, kind, recv, key, obj, idx, scratch, slow)
	a.MovRegReg(idx, scratch)

	// The value is a Number. An untagged Value is a double, the same single
	// unsigned compare the key took.
	a.CmpRegReg(val, jitRegGuard)
	a.Jcc(jitasm.CondA, slow)

	switch kind {
	case taFloat64:
		// Already the bit pattern to store, and every one of them is a valid
		// element — including the NaN the read has to guard against, because
		// the collision is with the BOX and nothing here is boxing.
		a.MovMemReg(idx, 0, val)
	case taFloat32:
		a.MovqXReg(jitRegF0, val)
		a.Cvtsd2ssXX(jitRegF0, jitRegF0)
		a.MovssMemX(idx, 0, jitRegF0)
	default:
		// An exact integer, established the way the key was.
		a.MovqXReg(jitRegF0, val)
		a.Cvttsd2siRegX(obj, jitRegF0)
		a.Cvtsi2sdXReg(jitRegF1, obj)
		a.MovqRegX(scratch, jitRegF1)
		a.CmpRegReg(scratch, val)
		a.Jcc(jitasm.CondNE, slow)
		switch taKinds[kind].size {
		case 1:
			a.MovMem8Reg(idx, 0, obj)
		case 2:
			a.MovMem16Reg(idx, 0, obj)
		case 4:
			a.MovMem32Reg(idx, 0, obj)
		}
	}

	if jitStats.enabled {
		a.MovRegImm64(scratch, uint64(jitElemPutHitAddr()))
		a.AddMemImm32(scratch, 0, 1)
	}
	a.Jmp(done)
	return true
}

// jitStoreKindEmittable is jitElemKindEmittable minus Uint8Clamped, whose store
// clamps and rounds half to even rather than truncating — a different function
// from every other kind's, and one no workload measured here performs.
func jitStoreKindEmittable(kind taKind) bool {
	return kind != taUint8Clamped && jitElemKindEmittable(kind)
}
