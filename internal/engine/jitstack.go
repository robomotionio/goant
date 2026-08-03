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

// jitFrame is the vmFrame a compiled frame published on entry.
//
// Compiled code keeps its `with` chain there rather than in the context,
// because that is where an interpreted frame keeps it and where the collector
// already looks — see markFrames. Nil only if something entered compiled code
// without publishing a frame, which nothing does.
func (rt *Runtime) jitFrame() *vmFrame {
	if rt.frameDepth < 0 || rt.frameDepth >= len(rt.frames) {
		return nil
	}
	return &rt.frames[rt.frameDepth]
}

// jitHasWith reports whether the body builds a `with` chain of its own.
func jitHasWith(fn *svFunc) bool {
	code := fn.code
	for ip := 0; ip < len(code); {
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		if size <= 0 || ip+size > len(code) {
			return false
		}
		if op == OpEnterWith {
			return true
		}
		ip += size
	}
	return false
}

// jitCaptureUpvalue is the compiled frame's version of the interpreter's
// captureUpvalue: the cell for a local slot, created on first use and shared
// with every other capture of the same slot in this frame.
//
// A direct eval needs it because code inside the eval can close over one of
// this function's locals, and that closure must see later writes through the
// same cell. The map and the dropFrameLocals are the ones the compiled CLOSURE
// already uses — see jitHelperClosure for why the frame gives up its buffer at
// the first capture.
func jitCaptureUpvalue(rt *Runtime, locals []Value) func(int) *upvalue {
	return func(slot int) *upvalue {
		if slot < 0 || slot >= len(locals) {
			return &upvalue{location: new(Value)}
		}
		open := rt.jitOpenUpvals[rt.frameDepth]
		if open == nil {
			open = map[int]*upvalue{}
			if rt.jitOpenUpvals == nil {
				rt.jitOpenUpvals = map[int]map[int]*upvalue{}
			}
			rt.jitOpenUpvals[rt.frameDepth] = open
			rt.dropFrameLocals(rt.frameDepth)
		}
		if u, ok := open[slot]; ok {
			return u
		}
		u := &upvalue{location: &locals[slot]}
		open[slot] = u
		return u
	}
}
