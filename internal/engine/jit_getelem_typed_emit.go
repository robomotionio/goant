//go:build amd64 || arm64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// Reading `a[i]` from a TypedArray, in machine code.
//
// The first thing here compiled from what the program did rather than from its
// bytecode. The site's element kind is a byte the interpreter recorded — see
// elemfeedback.go for why that is a byte and not a dispatch — and it decides
// one instruction of the chain below: which load.
//
// It is a different chain from the fast-array one and not an extension of it.
// A fast array holds Values in a slice of its own and the guards are about
// holes; a view holds bytes in a buffer three objects away and the guards are
// about the window still being over them. The two do not share a receiver
// either, which is why a site gets one or the other: the fast-array chain
// served 6,059 of zlib's 2.13 billion element reads, because it opens by
// checking for tag TArr and every one of those receivers was a view.
//
// What the runtime decides and this only checks:
//
//   - jitKind is non-zero exactly for a view whose elements are doubles at a
//     fixed offset — not length-tracking, not BigInt, not Float16. One byte
//     compare rejects all of those together, and a view built by some future
//     path that skips newTAView is refused rather than read wrongly, because
//     the eligible value is the non-zero one.
//   - bufPtr is the buffer object resolved once. Holding it is safe for the
//     same reason object.clPtr is: a pool cell never moves, and the handle in
//     buf keeps it reachable.
//
// What it must check itself, and cannot skip:
//
//   - The window is still over the bytes. A detached buffer keeps the view's
//     length and loses its bytes, and a resizable one can shrink under a view
//     that is not tracking it, in which case the WHOLE view is out of bounds
//     rather than the tail of it. Both are "byteOffset + length*size is past
//     the end of abuf", which is the one compare below — bound on the byte
//     slice, never on the view's own length.

// jitEmitTypedWindow emits everything both element chains share: that the
// receiver is a view of this site's kind, that its window is still over the
// buffer's bytes, and that the key is an integer index inside it.
//
// On the fall-through, addr holds the byte address of the element and obj the
// view; idx has been consumed. Control reaches slow with recv, key and anything
// else the caller holds untouched.
//
// Written once and used twice rather than copied, because the two chains have to
// agree about the bound: a read that checked the window and a write that checked
// the view's own length would differ only on a detached buffer, which is the one
// case that matters and the one no ordinary test reaches.
//
// note asks for the invocation-dirty pair to be maintained, which a write owes
// and a read does not. It is emitted here rather than by the caller because
// straight after the tag check is the only point where the receiver is known to
// be an object AND all three scratch registers are still free.
func jitEmitTypedWindow(a *jitasm.Asm, kind taKind, recv, key, obj, idx, addr jitasm.Reg, note bool, slow *jitasm.Label) {
	// addr and idx are both live across the LEA at the end, so naming the same
	// register twice scales the buffer pointer by the element size instead of
	// the index. That is a fault at a wild address rather than a wrong answer,
	// and it is a bug in the caller — so it is raised here, where the caller is,
	// rather than discovered later. It has happened once already.
	if addr == idx || addr == obj || idx == obj {
		panic("jit: typed element chain needs three distinct scratch registers")
	}
	size := taKinds[kind].size
	scratch := jitRegScratch

	// A TypedArray, and its object. Same opening as the fast-array chain and
	// for the same reason: the tag check is what makes resolving the handle
	// safe, and it is the cheapest rejection here.
	jitEmitTagCheck(a, recv, TTypedArray, slow)

	// A write into a view older than this invocation is state the next run
	// inherits. Noted on the attempt rather than on the store, which is what the
	// runtime does too — setElementR notes before it knows whether the index is
	// in range — so the two tiers cannot disagree about a write that misses.
	if note {
		jitEmitNoteSharedMutation(a, recv, obj, idx)
	}

	a.MovRegMem(idx, jitasm.RegCtx, jitmem.CtxOffPool)
	jitEmitResolve(a, obj, recv, idx)

	// The view. Tested for nil before anything is read through it: every
	// construction site sets it beside the tag, but a fault in generated code
	// is a crash a long way from its cause, and this is two instructions.
	a.MovRegMem(obj, obj, int32(jitOffObjTA))
	a.TestRegReg(obj, obj)
	a.Jcc(jitasm.CondE, slow)

	// The kind this site was compiled for, which also rejects every view whose
	// elements are not doubles at a fixed offset. See jitKind.
	a.MovzxRegMem8(scratch, obj, int32(jitOffTAJITKind))
	a.CmpRegImm32(scratch, uint32(kind)+1)
	a.Jcc(jitasm.CondNE, slow)

	// The window is still over the bytes: byteOffset + length*size must be at
	// or below len(abuf). Done before the key is decoded because idx is free
	// until then and the buffer's length has to land somewhere.
	a.MovRegMem(idx, obj, int32(jitOffTABufPtr))
	a.MovRegMem(idx, idx, int32(jitOffObjABufLen))
	a.MovRegMem(scratch, obj, int32(jitOffTALength))
	if s := jitLog2(size); s != 0 {
		a.ShlRegImm(scratch, s)
	}
	a.AddRegMem(scratch, obj, int32(jitOffTAByteOffset))
	a.CmpRegReg(scratch, idx)
	a.Jcc(jitasm.CondA, slow)

	// The key is a Number, and an integer. Both tests are the fast-array
	// chain's, unchanged: an untagged Value is a double, and converting to an
	// integer and back and requiring the same bits rejects a fraction, both
	// infinities, NaN and negative zero.
	a.CmpRegReg(key, jitRegGuard)
	a.Jcc(jitasm.CondA, slow)
	a.MovqXReg(jitRegF0, key)
	a.Cvttsd2siRegX(idx, jitRegF0)
	a.Cvtsi2sdXReg(jitRegF1, idx)
	a.MovqRegX(scratch, jitRegF1)
	a.CmpRegReg(scratch, key)
	a.Jcc(jitasm.CondNE, slow)

	// Below the view's length. Unsigned, so a negative index — which converted
	// to a large positive integer above — fails here. There is no second bound
	// to check the way a fast array needs one: the window test above already
	// established that every element up to length is inside the bytes.
	a.CmpRegMem(idx, obj, int32(jitOffTALength))
	a.Jcc(jitasm.CondAE, slow)

	// The address: the buffer's bytes, plus the view's offset into them, plus
	// the index scaled by the element. bufPtr is loaded a second time rather
	// than kept in a register, because there is no register to keep it in and
	// it is the line the length came from.
	a.MovRegMem(addr, obj, int32(jitOffTABufPtr))
	a.MovRegMem(addr, addr, int32(jitOffObjABuf))
	a.AddRegMem(addr, obj, int32(jitOffTAByteOffset))
	a.LeaRegMemIndex(addr, addr, idx, uint8(size), 0)
}

// jitEmitGetElemTyped emits the read for one element kind.
//
// recv holds the view and key the index; recv holds the result on the
// fall-through to done. obj and idx are scratch, as is jitRegScratch. Control
// reaches slow with recv and key untouched, which is what the runtime path
// needs — so nothing is written to recv until every guard has passed.
//
// Reports whether it emitted anything: a kind with no single load is refused
// here as well as by the runtime's jitKind, so that the two cannot disagree.
func jitEmitGetElemTyped(a *jitasm.Asm, kind taKind, recv, key, obj, idx jitasm.Reg, slow, done *jitasm.Label) bool {
	if !jitElemKindEmittable(kind) {
		return false
	}
	scratch := jitRegScratch
	jitEmitTypedWindow(a, kind, recv, key, obj, idx, scratch, false, slow)

	// The element, into idx rather than recv: the float kinds have one guard
	// left after the load, and reaching slow with the receiver overwritten
	// would hand the runtime path the wrong operand.
	switch kind {
	case taInt8:
		a.MovsxRegMem8(idx, scratch, 0)
	case taUint8, taUint8Clamped:
		a.MovzxRegMem8(idx, scratch, 0)
	case taInt16:
		a.MovsxRegMem16(idx, scratch, 0)
	case taUint16:
		a.MovzxRegMem16(idx, scratch, 0)
	case taInt32:
		a.MovsxRegMem32(idx, scratch, 0)
	case taUint32:
		// Zero-extended into the whole register, so the conversion below reads
		// it as the unsigned value it is rather than as a negative int32.
		a.Mov32RegMem(idx, scratch, 0)
	case taFloat32:
		a.Cvtss2sdXMem(jitRegF0, scratch, 0)
		a.MovqRegX(idx, jitRegF0)
	case taFloat64:
		a.MovRegMem(idx, scratch, 0)
	}

	switch kind {
	case taFloat32, taFloat64:
		// A float element is already a double and needs no conversion — but it
		// may be a NaN whose bits collide with the box. Every tagged Value
		// sorts above the prefix and every double at or below it, so one
		// unsigned compare is exactly "these bits would read as a tagged
		// value", and the runtime path canonicalises it in tov.
		a.CmpRegReg(idx, jitRegGuard)
		a.Jcc(jitasm.CondA, slow)
	default:
		a.Cvtsi2sdXReg(jitRegF0, idx)
		a.MovqRegX(idx, jitRegF0)
	}

	if jitStats.enabled {
		a.MovRegImm64(scratch, uint64(jitElemHitAddr()))
		a.AddMemImm32(scratch, 0, 1)
	}
	a.MovRegReg(recv, idx)
	a.Jmp(done)
	return true
}

// jitElemKindEmittable reports whether a kind has a single load that produces a
// double. Float16 has no widening instruction on either backend and the two
// BigInt kinds allocate; both are also zero in jitKind, and the two must agree.
func jitElemKindEmittable(kind taKind) bool {
	switch kind {
	case taInt8, taUint8, taUint8Clamped, taInt16, taUint16,
		taInt32, taUint32, taFloat32, taFloat64:
		return true
	}
	return false
}

// jitLog2 is the shift that multiplies by an element size.
func jitLog2(size int) uint8 {
	switch size {
	case 2:
		return 1
	case 4:
		return 2
	case 8:
		return 3
	}
	return 0
}
