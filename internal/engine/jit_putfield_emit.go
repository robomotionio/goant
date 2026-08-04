//go:build amd64 || arm64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// The inline-cache hit for a named property store, in machine code.
//
// The read's mirror, and deliberately built out of the same pieces: the tag
// check, the handle resolve, the scan over every way, the epoch, the three
// pointers that must be nil, the slot bounds. A store that is a slot write on
// the way in is a slot write on the way out, so any divergence between the two
// probes would be a divergence in what they believe about the same cache.
//
// It is the largest single thing the tier was missing. GET_FIELD compiled and
// PUT_FIELD did not, and a function that reads a field almost always writes one:
// 597 functions in the benchmark corpus had PUT_FIELD as their only unsupported
// opcode, seven times as many as the next.
//
// Two things the read does not have to think about:
//
//   - The invocation-dirty flag. A store to an object older than the running
//     invocation is what tells a host pooling Runtimes that this one cannot be
//     reused, and the interpreter's cached-store path records it explicitly for
//     the same reason this does — the fast path skips [[Set]], which is where it
//     would otherwise be noticed.
//   - Which fills it may serve. A store site's ways come from icFillPut, which
//     records only an own, writable, non-accessor data slot; and from
//     icFillPutTransition, which records a store that creates the property and
//     is what toShape marks. This serves the first and declines the second, so
//     everything about extensibility and about installing a new shape stays in
//     the runtime.
//
// What is emitted is the interpreter's fast path for the case toShape is nil,
// instruction for instruction: note the mutation, write the slot.

// jitICPutSpareRegs is how many operand-stack registers beyond the receiver's
// and the value's the probe needs.
const jitICPutSpareRegs = 2

// jitEmitICPut emits the probe.
//
// recv holds the receiver and val the value being stored; neither is written,
// on either path, which is what lets the slow path hand both to the runtime out
// of the spill area it was going to write them to anyway. obj and way are
// scratch, as is jitRegScratch.
//
// Control reaches done with the store done, or slow with nothing having
// happened.
func jitEmitICPut(a *jitasm.Asm, recv, val, obj, way jitasm.Reg, wayBase, epoch uintptr, slow, done *jitasm.Label) {
	scratch := jitRegScratch

	// A plain object, and its address. Both are the read's, for the read's
	// reasons — see jitEmitICGet.
	jitEmitTagCheck(a, recv, TObj, slow)
	a.MovRegMem(way, jitasm.RegCtx, jitmem.CtxOffPool)
	jitEmitResolve(a, obj, recv, way)

	// The way holding this shape, scanning all of them.
	found := a.NewLabel()
	a.MovRegImm64(way, uint64(wayBase))
	a.MovRegMem(scratch, obj, int32(jitOffObjShape))
	for i := 0; i < icWays; i++ {
		if i > 0 {
			a.AddRegImm32(way, uint32(jitSizeofICWay))
		}
		a.CmpRegMem(scratch, way, int32(jitOffWayShape))
		a.Jcc(jitasm.CondE, found)
	}
	a.Jmp(slow)
	a.Bind(found)

	// Not retired since it was filled.
	a.MovRegImm64(scratch, uint64(epoch))
	a.Mov32RegMem(scratch, scratch, 0)
	a.Cmp32RegMem(scratch, way, int32(jitOffWayEpoch))
	a.Jcc(jitasm.CondNE, slow)

	// An own slot on an ordinary receiver, tested as one word. holder is nil for
	// every store fill and checked anyway; toShape is what marks the transition
	// entry this path does not serve; proxy is a receiver whose [[Set]] is a
	// trap.
	a.MovRegMem(scratch, way, int32(jitOffWayHolder))
	a.OrRegMem(scratch, way, int32(jitOffWayToShape))
	a.OrsRegMem(scratch, obj, int32(jitOffObjProxy))
	a.Jcc(jitasm.CondNE, slow)

	// Where the slot lives, which also rejects the sentinel a site records for a
	// shape it has decided it cannot cache, and a slot the shape declares that
	// the overflow slice has not been grown to yet.
	a.Mov32RegMem(way, way, int32(jitOffWaySlot))
	jitEmitSlotAddr(a, obj, way, slow)

	// The store. No Go write barrier is needed and none is emitted: a Value is a
	// NaN-boxed integer over non-moving pools, so as far as Go's collector is
	// concerned nothing here is a pointer. That is the same property that lets
	// the spill area hold operands across a call out.
	//
	// Nor is the slot's previous contents worth looking at. A slot may hold the
	// sentinel for a span of a JSON document that has not been parsed yet, which
	// a read has to materialise — but a store overwrites it, and the value it
	// would have produced is exactly what is being thrown away. slotSet ignores
	// it for the same reason.
	a.MovMemReg(way, 0, val)

	// obj and way are dead from here, which is what the dirty check borrows.
	jitEmitNoteSharedMutation(a, recv, obj, way)

	if jitStats.enabled {
		a.MovRegImm64(scratch, uint64(jitICPutHitAddr()))
		a.AddMemImm32(scratch, 0, 1)
	}
	a.Jmp(done)
}

// jitEmitNoteSharedMutation is noteSharedMutation for a store that has just
// happened, emitted rather than called.
//
// A host that pools Runtimes needs to know whether a run modified anything that
// predates it, because that state is what the next run would inherit. The
// runtime answers it with a handle comparison against a watermark taken when the
// invocation began: below it is shared, at or above it is this run's own.
//
// recv is the receiver, whose handle is the low half of its Value — it has
// passed the tag check, so the low 32 bits are a handle the engine issued. t0
// and t1 are scratch and jitRegScratch is clobbered; nothing else is touched,
// and the flags are not live across it.
//
// Four instructions when no invocation is running, which is the case for every
// embedding that does not pool: load the runtime, load the watermark, test,
// branch. That is what it costs to keep this correct rather than gated.
func jitEmitNoteSharedMutation(a *jitasm.Asm, recv, t0, t1 jitasm.Reg) {
	scratch := jitRegScratch
	clean := a.NewLabel()

	a.MovRegMem(scratch, jitasm.RegCtx, jitmem.CtxOffHost)
	a.Mov32RegMem(t0, scratch, int32(jitOffRTWatermark))
	a.TestRegReg(t0, t0)
	a.Jcc(jitasm.CondE, clean) // no invocation is watching

	a.MovzxRegMem8(t1, scratch, int32(jitOffRTDirty))
	a.TestRegReg(t1, t1)
	a.Jcc(jitasm.CondNE, clean) // already known to be dirty

	// The handle, zero-extended out of the Value, against the watermark. A
	// 32-bit move is the whole payload extraction: the tag is in the high half.
	a.Mov32RegReg(t1, recv)
	a.CmpRegReg(t1, t0)
	a.Jcc(jitasm.CondAE, clean) // this run's own object

	a.MovMem8Imm(scratch, int32(jitOffRTDirty), 1)
	a.Bind(clean)
}
