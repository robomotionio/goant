//go:build amd64 || arm64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// Writing `a[i] = v` into a fast array, in machine code.
//
// The half of the element store that is about JavaScript people write. The
// TypedArray store landed first and crypto did not move by one count —
// 72,085,742 `putelem` exits before and 72,085,742 after, 81.5% of its total
// both times — because crypto's arrays are ordinary ones. So is anything that
// fills an array in a loop.
//
// It is harder than the view's store was easy, and precisely where that one had
// nothing:
//
//   - A frozen array's elements are non-writable, so the store can be REJECTED.
//     A view's never is.
//   - The value is a Value, so what is written may be a handle rather than a
//     number, and the invocation-dirty pair has to be maintained. (No write
//     barrier, though: the collector traces object.arr when it reaches the
//     object, and a Value is an integer to Go's own collector.)
//   - A store past the end grows the storage and moves `length`, a store into a
//     hole is not a store over a value, and an index shadowed by a named
//     property defined with attributes is a different operation entirely.
//
// THE LAST GROUP IS WHY THIS CHAIN IS NARROWER THAN THE INTERPRETER'S.
//
// setElementR writes whenever the slot is live in the storage, and then bumps
// `length` if the index reached past it. This requires `idx < length` as well,
// so the length never changes and none of it needs emitting. What that gives up
// is a store to a live slot BETWEEN length and the end of the storage — a stale
// element past the logical end — which goes to the runtime.
//
// Being narrower than the runtime is the safe direction, and it is the only
// direction that is safe: every guard here can send a store to setElementR,
// which will do the whole job correctly, whereas a guard that let one through
// wrongly would write to the array and move on. It also happens to make the
// chain shorter than the read's, because a bound that cannot move needs no
// maintenance.

// jitEmitPutElem emits the store.
//
// recv holds the array, key the index and val the value. obj and idx are
// scratch, as is jitRegScratch. Nothing is left on the operand stack. Control
// reaches slow with recv, key and val untouched, which is what the runtime path
// needs.
func jitEmitPutElem(a *jitasm.Asm, recv, key, val, obj, idx jitasm.Reg, slow, done *jitasm.Label) {
	scratch := jitRegScratch

	// A fast array, and its object — the read's opening, unchanged.
	jitEmitTagCheck(a, recv, TArr, slow)

	// A write into an array older than this invocation is state the next run
	// inherits. Emitted here because it is the last point where both scratch
	// registers are free, and because the runtime notes on the ATTEMPT too:
	// setElementR notes before it knows whether the index is in range, so a
	// store that fails a guard below and lands in the runtime is noted there
	// rather than being missed.
	jitEmitNoteSharedMutation(a, recv, obj, idx)

	a.MovRegMem(idx, jitasm.RegCtx, jitmem.CtxOffPool)
	jitEmitResolve(a, obj, recv, idx)

	// Frozen means the elements are non-writable and the store is rejected —
	// silently in sloppy mode, as a TypeError in strict. Both are the runtime's
	// to decide, so this only has to recognise it. Checked before the key is
	// decoded because scratch is free until then.
	a.MovzxRegMem8(scratch, obj, int32(jitOffObjFrozen))
	a.TestRegReg(scratch, scratch)
	a.Jcc(jitasm.CondNE, slow)

	// The key is a Number, and an integer. The read's test, unchanged.
	a.CmpRegReg(key, jitRegGuard)
	a.Jcc(jitasm.CondA, slow)
	a.MovqXReg(jitRegF0, key)
	a.Cvttsd2siRegX(idx, jitRegF0)
	a.Cvtsi2sdXReg(jitRegF1, idx)
	a.MovqRegX(scratch, jitRegF1)
	a.CmpRegReg(scratch, key)
	a.Jcc(jitasm.CondNE, slow)

	// Below the array's length AND below what its storage holds. Both, for the
	// read's reason — an array's length may run past its elements — and for one
	// more: requiring the index below the length is what makes this a store that
	// cannot change the length, which is the whole of why nothing here has to
	// maintain one.
	a.Mov32RegMem(scratch, obj, int32(jitOffObjArrLen))
	a.CmpRegReg(idx, scratch)
	a.Jcc(jitasm.CondAE, slow)
	a.CmpRegMem(idx, obj, int32(jitOffObjArrCap))
	a.Jcc(jitasm.CondAE, slow)

	// The address of the slot. obj stops being the object here — everything
	// after this needs the element's address and nothing needs the object.
	a.MovRegMem(obj, obj, int32(jitOffObjArr))
	a.LeaRegMemIndex(obj, obj, idx, 8, 0)

	// What is already there decides whether this is a store at all. A hole has
	// to reach the prototype chain, where an inherited setter or a non-writable
	// property may intercept it; an unparsed JSON span has to be materialised
	// before it can be replaced. Both are the runtime's, exactly as they are for
	// the read.
	a.MovRegMem(scratch, obj, 0)
	a.MovRegImm64(idx, uint64(tEmpty))
	a.CmpRegReg(scratch, idx)
	a.Jcc(jitasm.CondE, slow)
	a.MovRegImm64(idx, uint64(lazyBase))
	a.CmpRegReg(scratch, idx)
	a.Jcc(jitasm.CondAE, slow)

	a.MovMemReg(obj, 0, val)

	if jitStats.enabled {
		a.MovRegImm64(scratch, uint64(jitElemPutHitAddr()))
		a.AddMemImm32(scratch, 0, 1)
	}
	a.Jmp(done)
}
