package engine

import (
	"unsafe"

	"github.com/robomotionio/goant/internal/jitmem"
)

// jitFrameStack is a compiled frame's operand stack as a slice of Values.
//
// The context holds it as an address because generated code indexes it by a
// compile-time offset and cannot follow a slice header. Nothing is copied: this
// is the same array, and it stays alive because the context holds the slice it
// was cut from.
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
