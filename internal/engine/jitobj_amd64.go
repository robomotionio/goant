//go:build amd64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
)

// Reaching an object from compiled code.
//
// Everything the tier still refuses — property reads and writes, globals,
// elements, calls — starts here, because every one of them has to get from a
// Value to the object it names before it can do anything else. A Value carries a
// 32-bit handle rather than a pointer, which is what keeps Go's collector out of
// the engine's heap entirely, and the price is that resolving one is two
// dependent loads instead of none.
//
// The sequence is `chunks[h >> shift][h & mask]`, and its offsets come from
// jitlayout.go rather than from anything written here.

// jitEmitTagCheck branches to notObject unless the Value in r is one of the
// types that names a pool cell.
//
// This has to come before any resolution, and not only because the wrong type
// gives the wrong answer: the handle is the low bits of the payload, so a double
// interpreted as an object would index the chunk vector with its own mantissa.
// The guard is what makes every later step's bounds safe rather than lucky.
func jitEmitTagCheck(a *jitasm.Asm, r jitasm.Reg, want Type, notObject *jitasm.Label) {
	// A tagged Value is prefix | type<<shift | payload, so shifting the type
	// field down and comparing is one test for "tagged, and this type" — an
	// untagged double lands somewhere else in the shifted space and fails.
	a.MovRegReg(jitRegScratch, r)
	a.ShrRegImm(jitRegScratch, nanboxTypeShift)
	a.CmpRegImm32(jitRegScratch, uint32(nanboxPrefix>>nanboxTypeShift)|uint32(want))
	a.Jcc(jitasm.CondNE, notObject)
}

// jitEmitResolve turns the handle in the low bits of the Value in src into the
// address of its object in dst.
//
// pool is a register holding the address of the object pool, which is passed in
// rather than baked into the code: two Runtimes have two pools, and code
// compiled against one must never read the other's.
//
// dst, src and pool must be distinct; pool and jitRegScratch are both clobbered.
// No bounds check is emitted, which is safe only downstream of jitEmitTagCheck —
// a Value that passed it carries a handle the engine issued, and every issued
// handle has a chunk.
func jitEmitResolve(a *jitasm.Asm, dst, src, pool jitasm.Reg) {
	// The chunk vector, read through the slice header rather than baked in,
	// because appending a chunk reallocates it while the header's own address
	// never moves.
	a.MovRegMem(pool, pool, int32(jitOffPoolChunks))

	// The handle is the low 32 bits of the payload. Taking them with a 32-bit
	// move drops the tag as a side effect, so no payload mask is needed.
	a.Mov32RegReg(dst, src)

	// The chunk: chunks[h >> shift].
	a.MovRegReg(jitRegScratch, dst)
	a.ShrRegImm(jitRegScratch, jitPoolChunkShift)
	a.MovRegMemIndex(jitRegScratch, pool, jitRegScratch, 8, 0)

	// The cell within it: (h & mask) * sizeof(cell). A cell is not a power of
	// two in size, so the scale is a multiply rather than an addressing mode,
	// and only the last addition folds into an LEA.
	a.AndRegImm32(dst, jitPoolChunkMask)
	a.ImulRegImm32(dst, dst, uint32(jitSizeofPoolCell))
	a.LeaRegMemIndex(dst, jitRegScratch, dst, 1, int32(jitOffCellElem))
}

// jitEmitSlotAddr replaces the shape slot number in slot with the address of
// that slot's Value.
//
// An object keeps its first four properties in itself and the rest in a slice,
// and until this existed the compiled probes served only the first four. That
// is a narrower rule than it sounds: four is ANT_INOBJ_MAX_SLOTS, so a class
// instance with five fields has a fifth property no compiled read could reach,
// and the global object — which carries every builtin before a script's own
// names — has none of them in the object at all. A global read that only served
// inline slots was measured at a 0% hit rate, which is what sent this here
// rather than leaving it as an optimisation.
//
// obj and slot must be distinct, and jitRegScratch is clobbered. Control reaches
// slow when the slot is past what the object actually holds — which is where the
// sentinel a site records for an uncacheable shape ends up, since it is the
// largest uint32 and no overflow slice is that long.
func jitEmitSlotAddr(a *jitasm.Asm, obj, slot jitasm.Reg, slow *jitasm.Label) {
	scratch := jitRegScratch
	overflow := a.NewLabel()
	have := a.NewLabel()

	// The shape's limit, which is what decides where a slot lives. It is a
	// property of the shape rather than a constant: a shape may declare fewer
	// inline slots than the object has room for.
	a.MovRegMem(scratch, obj, int32(jitOffObjShape))
	a.MovzxRegMem8(scratch, scratch, int32(jitOffShapeInobjLimit))
	a.CmpRegReg(slot, scratch)
	a.Jcc(jitasm.CondAE, overflow)

	a.LeaRegMemIndex(slot, obj, slot, 8, int32(jitOffObjInobj))
	a.Jmp(have)

	a.Bind(overflow)
	// The index within the slice, bounds-checked against its length rather than
	// its capacity. A slot the shape declares can still be past the end of the
	// slice — slotSet grows it on demand — and growing one is the runtime's job.
	a.SubRegReg(slot, scratch)
	a.CmpRegMem(slot, obj, int32(jitOffObjOverflowLen))
	a.Jcc(jitasm.CondAE, slow)
	a.MovRegMem(scratch, obj, int32(jitOffObjOverflow))
	a.LeaRegMemIndex(slot, scratch, slot, 8, 0)

	a.Bind(have)
}
