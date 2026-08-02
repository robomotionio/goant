//go:build amd64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// The inline-cache hit for a named property read, in machine code.
//
// This is the operation the tier exists to make fast. Everything else it
// compiles — the arithmetic, the branches, the loop — is work the interpreter
// also does quickly; a property read is where the interpreter spends its time,
// because `o.x` in a shaped object is a cache probe wrapped in a bytecode
// dispatch wrapped in a bounds check.
//
// Delegating the probe to a helper was measured and is worse than not compiling
// the read at all: GET_FIELD through the exit-and-re-enter protocol took a loop
// from 1133ms to 1244ms. The round trip costs more than the lookup it saves. So
// the hit has to be emitted, and what the runtime keeps is the miss.
//
// What is emitted is exactly icWay.hit restricted to the case it can serve, and
// no more: one way, an own slot, in the object rather than its overflow, holding
// something other than an unparsed JSON span. Every other shape the cache can
// describe — an inherited method, a store transition, a slot past the inline
// limit — falls to the runtime, which handles it as it always did. Widening the
// probe to scan all eight ways is a later question, and one to answer with a
// measurement rather than by reasoning about how many shapes a site sees.

// jitICGetSpareRegs is how many operand-stack registers beyond the receiver's
// the probe needs. A site emitted at a depth that cannot spare them keeps the
// runtime path, which costs nothing but the speed it would have gained.
const jitICGetSpareRegs = 2

// jitEmitICGet emits the probe.
//
// recv holds the receiver on entry, and the property's value on the fall-through
// to done. obj and tmp are scratch, as is jitRegScratch; all three are clobbered
// on both paths. Control reaches slow with recv still holding the receiver,
// which is what the runtime path needs and the reason the loaded value is
// checked before it is moved there.
//
// way is the address of the site's first cache way and epoch the address of the
// generation counter, both constants — see jitICWayAddr and jitEpochAddr.
func jitEmitICGet(a *jitasm.Asm, recv, obj, tmp jitasm.Reg, way, epoch uintptr, slow, done *jitasm.Label) {
	scratch := jitRegScratch

	// A plain object. Restricting to one tag rather than testing the whole
	// object family is the first cut: arrays, functions and generators reach
	// their fields through the runtime for now, and every ordinary receiver —
	// an object literal, a class instance — is this tag.
	//
	// The check is not an optimisation. The handle is the low bits of the
	// payload, so a double resolved as an object would index the chunk vector
	// with its own mantissa.
	jitEmitTagCheck(a, recv, TObj, slow)

	// The receiver as an object. The pool comes from the context because two
	// Runtimes have two of them.
	a.MovRegMem(tmp, jitasm.RegCtx, jitmem.CtxOffPool)
	jitEmitResolve(a, obj, recv, tmp)

	// The cached shape is still this object's. Shape identity is most of what
	// makes a cache sound: it says the object has the same properties in the
	// same slots as the one the entry was recorded from.
	a.MovRegImm64(tmp, uint64(way))
	a.MovRegMem(scratch, obj, int32(jitOffObjShape))
	a.CmpRegMem(scratch, tmp, int32(jitOffWayShape))
	a.Jcc(jitasm.CondNE, slow)

	// The way has not been retired. Identity is not sufficient on its own: a
	// shape that is not yet shared is mutated in place, so a delete can move
	// slots underneath a cache while the pointer stays equal. Every mutation
	// that can do that bumps this counter.
	a.MovRegImm64(scratch, uint64(epoch))
	a.Mov32RegMem(scratch, scratch, 0)
	a.Cmp32RegMem(scratch, tmp, int32(jitOffWayEpoch))
	a.Jcc(jitasm.CondNE, slow)

	// An own data slot, on a receiver whose [[Get]] is a slot read. Three
	// pointers that must all be nil, tested as one: a holder means the property
	// was found up the prototype chain and the chain would have to be guarded, a
	// toShape means this is a store site's transition entry, and a proxy means
	// the receiver's [[Get]] is a trap rather than a read.
	a.MovRegMem(scratch, tmp, int32(jitOffWayHolder))
	a.OrRegMem(scratch, tmp, int32(jitOffWayToShape))
	a.OrRegMem(scratch, obj, int32(jitOffObjProxy))
	a.Jcc(jitasm.CondNE, slow)

	// The slot is in the object rather than its overflow slice. Both bounds are
	// needed and neither implies the other: the inline area is a fixed size, and
	// a shape may declare a smaller limit than that. Together they are the
	// clamped limit slotGet computes, and they also reject the sentinel a site
	// records for a shape it has decided it cannot cache — it is the largest
	// uint32, so it fails the first compare.
	a.Mov32RegMem(tmp, tmp, int32(jitOffWaySlot))
	a.CmpRegImm32(tmp, uint32(jitInobjSlots))
	a.Jcc(jitasm.CondAE, slow)
	a.MovRegMem(scratch, obj, int32(jitOffObjShape))
	a.MovzxRegMem8(scratch, scratch, int32(jitOffShapeInobjLimit))
	a.CmpRegReg(tmp, scratch)
	a.Jcc(jitasm.CondAE, slow)

	// The value, unless it is a span of a JSON document that has not been parsed
	// yet. slotGet carries that check for the same reason this does: the slot
	// holds a sentinel until something reads it, and materialising one is the
	// runtime's job.
	a.MovRegMemIndex(tmp, obj, tmp, 8, int32(jitOffObjInobj))
	a.MovRegImm64(scratch, uint64(lazyBase))
	a.CmpRegReg(tmp, scratch)
	a.Jcc(jitasm.CondAE, slow)

	if jitStats.enabled {
		a.MovRegImm64(scratch, uint64(jitICHitAddr()))
		a.AddMemImm32(scratch, 0, 1)
	}
	a.MovRegReg(recv, tmp)
	a.Jmp(done)
}
