package engine

import (
	"unsafe"

	"github.com/robomotionio/goant/internal/jitmem"
)

// jitSlotAt is operand slot i of a compiled frame.
//
// One load, and deliberately not a slice index: building a slice header per
// call out was worth between three and eleven percent across Octane, because a
// call out is about five nanoseconds all in and a helper that reads two
// operands was paying for two headers to get at them.
//
// The bound is the emitter's rather than a check here. Compiled code writes
// StackN itself, from a depth it knew when it emitted the store, and the array
// was sized from the same walk — see jitMaxOperandDepth, which the emitter
// re-checks after every instruction.
func jitSlotAt(ctx *jitmem.ExecContext, i int) Value {
	return *(*Value)(ctx.SlotPtr(i))
}

// jitFrameStack is the live part of a compiled frame's operand stack.
//
// For the collector and for the argument window a call hands its callee, where
// a slice is what is wanted and one more is not worth counting. Nothing is
// copied: this is the same array, and it stays alive because the context holds
// either it or the struct it sits inside.
//
// Not in the amd64 file because the collector reads it and the collector is
// every platform's.
func jitFrameStack(ctx *jitmem.ExecContext) []Value {
	s := ctx.Slots()
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice((*Value)(unsafe.Pointer(&s[0])), len(s))
}
