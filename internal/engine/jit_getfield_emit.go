//go:build amd64 || arm64

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
// no more: an own slot holding something other than an unparsed JSON span.
// Every other shape the cache can describe — an inherited method, a store
// transition — falls to the runtime, which handles it as it always did.
//
// It scans every way, because probing only the first one was measured at a 1.0%
// hit rate on a loop the interpreter caches perfectly. Ways fill in order, so
// way 0 holds the first shape a site ever saw — and a hundred objects from one
// `{x, y}` literal occupy three shapes, 1 and 1 and 98, because the transition
// memo produces a shape of its own for each of the first two and never again.
// Way 0 was therefore filled by the object whose shape does not recur. Scanning
// is what makes the compiled probe answer the question the interpreter answers
// rather than a narrower one; the cost is three instructions per way that does
// not match, and none at all for a site that matches at way 0.

// jitICGetSpareRegs is how many operand-stack registers beyond the receiver's
// the probe needs. A site emitted at a depth that cannot spare them keeps the
// runtime path, which costs nothing but the speed it would have gained.
const jitICGetSpareRegs = 3

// jitEmitICGet emits the probe.
//
// recv holds the receiver on entry, and the property's value on the fall-through
// to done. obj, way and hold are scratch, as is jitRegScratch; all four are
// clobbered on both paths. Control reaches slow with recv still holding the receiver,
// which is what the runtime path needs and the reason the loaded value is
// checked before it is moved there.
//
// wayBase is the address of the site's first cache way and epoch the address of
// the generation counter, both constants — see jitICWayAddr and jitEpochAddr.
// hits is the counter to increment when the probe serves the read, which is a
// different counter for a field and for a global: the two decline for different
// reasons and one figure would hide whichever is doing worse.
func jitEmitICGet(a *jitasm.Asm, recv, obj, way, hold jitasm.Reg, wayBase, epoch, hits uintptr, slow, done *jitasm.Label) {
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
	a.MovRegMem(way, jitasm.RegCtx, jitmem.CtxOffPool)
	jitEmitResolve(a, obj, recv, way)

	// The way recording this object's shape, if the site has one. Shape identity
	// is most of what makes a cache sound: it says the object has the same
	// properties in the same slots as the one the entry was recorded from.
	//
	// At most one way can hold a given shape — a fill reuses the way already
	// holding it — so the first match is the only candidate and there is nothing
	// to keep scanning for. A way that was never filled holds a nil shape, which
	// no live object's shape equals, and a way retired with its site holds a nil
	// shape too because that is what killing a site now writes. So the ways with
	// something in them are exactly the ways the interpreter would consult.
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

	// The way has not been retired. Identity is not sufficient on its own: a
	// shape that is not yet shared is mutated in place, so a delete can move
	// slots underneath a cache while the pointer stays equal. Every mutation
	// that can do that bumps this counter.
	a.MovRegImm64(scratch, uint64(epoch))
	a.Mov32RegMem(scratch, scratch, 0)
	a.Cmp32RegMem(scratch, way, int32(jitOffWayEpoch))
	a.Jcc(jitasm.CondNE, slow)

	// Two pointers that must be nil, tested as one: a toShape means this is a
	// store site's transition entry, and a proxy means the receiver's [[Get]] is
	// a trap rather than a read.
	a.MovRegMem(scratch, way, int32(jitOffWayToShape))
	a.OrsRegMem(scratch, obj, int32(jitOffObjProxy))
	a.Jcc(jitasm.CondNE, slow)

	// A holder means the property was found up the prototype chain, and the slot
	// to read is on it rather than on the receiver.
	//
	// This is not a refinement. A class's methods live on its prototype, so
	// `o.m()` is an inherited read every time — and while this case fell to the
	// runtime, compiling the method call made DeltaBlue 9% *slower*, because the
	// probe declined 55% of the reads that the interpreter's cache was serving
	// and paid a helper round trip for each one.
	//
	// What is cached is the conclusion of a prototype walk, so the guard is the
	// receiver's [[Prototype]] still being the one the entry was filled from. Two
	// objects can share a shape and not a prototype; every object the walk passed
	// through is flagged usedAsProto, so a layout change to any of them bumps the
	// epoch checked above.
	own := a.NewLabel()
	a.MovRegMem(hold, way, int32(jitOffWayHolder))
	a.TestRegReg(hold, hold)
	a.Jcc(jitasm.CondE, own)
	a.MovRegMem(scratch, way, int32(jitOffWayProtoVal))
	a.CmpRegMem(scratch, obj, int32(jitOffObjProto))
	a.Jcc(jitasm.CondNE, slow)
	// Read from the holder from here on — including its inobj limit, which is
	// the holder's shape's and not the receiver's.
	a.MovRegReg(obj, hold)
	a.Bind(own)

	// Where the slot lives, in the object or in its overflow slice.
	a.Mov32RegMem(way, way, int32(jitOffWaySlot))
	jitEmitSlotAddr(a, obj, way, slow)

	// The value, unless it is a span of a JSON document that has not been parsed
	// yet. slotGet carries that check for the same reason this does: the slot
	// holds a sentinel until something reads it, and materialising one is the
	// runtime's job.
	a.MovRegMem(way, way, 0)
	a.MovRegImm64(scratch, uint64(lazyBase))
	a.CmpRegReg(way, scratch)
	a.Jcc(jitasm.CondAE, slow)

	if jitStats.enabled {
		a.MovRegImm64(scratch, uint64(hits))
		a.AddMemImm32(scratch, 0, 1)
	}
	a.MovRegReg(recv, way)
	a.Jmp(done)
}
