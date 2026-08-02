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
// dst, src and pool must be distinct, and jitRegScratch is clobbered. No bounds
// check is emitted, which is safe only downstream of jitEmitTagCheck — a Value
// that passed it carries a handle the engine issued, and every issued handle has
// a chunk.
func jitEmitResolve(a *jitasm.Asm, dst, src, pool jitasm.Reg) {
	// The handle is the low 32 bits of the payload. Taking them with a 32-bit
	// move drops the tag as a side effect, so no payload mask is needed.
	a.Mov32RegReg(dst, src)

	// The chunk: chunks[h >> shift], reached through the slice header rather
	// than a baked-in vector, because appending a chunk reallocates the vector
	// while the header's own address never moves.
	a.MovRegReg(jitRegScratch, dst)
	a.ShrRegImm(jitRegScratch, jitPoolChunkShift)
	a.ShlRegImm(jitRegScratch, 3) // the vector holds pointers
	a.AddRegMem(jitRegScratch, pool, int32(jitOffPoolChunks))
	a.MovRegMem(jitRegScratch, jitRegScratch, 0)

	// The cell within it: (h & mask) * sizeof(cell). A cell is not a power of
	// two in size, so the scale is a multiply rather than a shift.
	a.AndRegImm32(dst, jitPoolChunkMask)
	a.ImulRegImm32(dst, dst, uint32(jitSizeofPoolCell))
	a.AddRegReg(dst, jitRegScratch)
	if jitOffCellElem != 0 {
		a.AddRegImm32(dst, uint32(jitOffCellElem))
	}
}
