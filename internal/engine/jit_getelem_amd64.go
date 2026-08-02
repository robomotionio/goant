//go:build amd64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// Reading `a[i]` from a fast array, in machine code.
//
// The last property access this tier could not do, and the one the two
// benchmarks it still makes slower are entirely built from. Crypto refuses 2.81
// million frame entries for GET_ELEM and NavierStokes every one of its
// remaining functions; both are array mathematics, and `GET_UPVAL` was built
// first on the theory that it was NavierStokes' blocker and moved neither.
//
// It is a different guard chain from the named-property probe rather than
// another use of it. There is no shape and no cache site: an array's elements
// live in a slice of their own, and what has to be established is that the
// receiver is a fast array, that the key is an integer index rather than an
// arbitrary Number or a String, and that the slot holds a value rather than a
// hole. Everything else — a TypedArray, a String, an index defined with
// attributes, an index past the end that has to walk the prototype chain —
// falls to the runtime.

// jitICElemSpareRegs is how many operand-stack registers beyond the receiver's
// and the key's the probe needs.
const jitICElemSpareRegs = 2

// jitEmitGetElem emits the read.
//
// recv holds the array and key the index; recv holds the result on the
// fall-through to done. obj and idx are scratch, as is jitRegScratch. Control
// reaches slow with recv and key untouched, which is what the runtime path
// needs.
func jitEmitGetElem(a *jitasm.Asm, recv, key, obj, idx jitasm.Reg, slow, done *jitasm.Label) {
	scratch := jitRegScratch

	// A fast array, and its object. The tag check is what makes resolving the
	// handle safe, exactly as it is for a named read, and it is the cheapest
	// rejection here so it comes first.
	//
	// The resolve happens before the key is decoded because it needs a scratch
	// register of its own for the pool, and idx is the only one free — the
	// destination cannot be jitRegScratch, which jitEmitResolve clobbers as it
	// goes.
	jitEmitTagCheck(a, recv, TArr, slow)
	a.MovRegMem(idx, jitasm.RegCtx, jitmem.CtxOffPool)
	jitEmitResolve(a, obj, recv, idx)

	// The key is a Number. An untagged Value is a double, so this is the same
	// single unsigned compare every arithmetic operator makes.
	a.CmpRegReg(key, jitRegGuard)
	a.Jcc(jitasm.CondA, slow)

	// And an integer, which is what an array index is. Converting to an integer
	// and back and requiring the same bits is the whole test: it rejects a
	// fraction, both infinities, NaN — whose conversion fails and comes back as
	// something else entirely — and negative zero, whose bits differ from the
	// positive zero it converts back to. A negative index fails the unsigned
	// bound below rather than here.
	a.MovqXReg(jitasm.X0, key)
	a.Cvttsd2siRegX(idx, jitasm.X0)
	a.Cvtsi2sdXReg(jitasm.X1, idx)
	a.MovqRegX(scratch, jitasm.X1)
	a.CmpRegReg(scratch, key)
	a.Jcc(jitasm.CondNE, slow)

	// Below the array's length, and below what its storage actually holds. Both
	// are needed and neither implies the other: an array's length may run past
	// its element slice, and the elements past the end of the length are stale
	// rather than absent. Unsigned throughout, so a negative index — which
	// converted to a large positive integer above — fails here.
	a.Mov32RegMem(scratch, obj, int32(jitOffObjArrLen))
	a.CmpRegReg(idx, scratch)
	a.Jcc(jitasm.CondAE, slow)
	a.CmpRegMem(idx, obj, int32(jitOffObjArrCap))
	a.Jcc(jitasm.CondAE, slow)

	// The element. A hole is the empty sentinel and has to reach the prototype
	// chain; an unparsed span has to be materialised. Both are the runtime's.
	a.MovRegMem(scratch, obj, int32(jitOffObjArr))
	a.MovRegMemIndex(idx, scratch, idx, 8, 0)
	a.MovRegImm64(scratch, uint64(tEmpty))
	a.CmpRegReg(idx, scratch)
	a.Jcc(jitasm.CondE, slow)
	a.MovRegImm64(scratch, uint64(lazyBase))
	a.CmpRegReg(idx, scratch)
	a.Jcc(jitasm.CondAE, slow)

	if jitStats.enabled {
		a.MovRegImm64(scratch, uint64(jitElemHitAddr()))
		a.AddMemImm32(scratch, 0, 1)
	}
	a.MovRegReg(recv, idx)
	a.Jmp(done)
}
