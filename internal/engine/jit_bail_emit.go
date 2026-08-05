//go:build amd64 || arm64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// jitEmitBail emits the sequence that stops this frame and hands it to the
// interpreter. See jitbail.go for what a bail is and why it is cheap.
//
// It is the first half of jitCallHelper and none of the second: the same window
// spill, the same StackN, and then a RET that nothing resumes. What replaces the
// resume address is the bytecode offset — the frame is not coming back, so what
// it owes is a place in the program rather than a place in the code.
//
// sp is the depth *before* the instruction at `at` runs, so the operand stack
// this publishes is the one that instruction expects to find. Everything below
// the register window is already in the spill array, which is where it lives
// between instructions rather than only across a call, so the loop covers the
// window alone.
//
// Both stores are 32-bit into 64-bit fields, which is what every other write to
// StackN does and rests on the same fact: Go allocates the context zeroed and
// never writes the upper half of either field, so it stays zero for the life of
// the context.
func jitEmitBail(a *jitasm.Asm, sp, at int, deep bool) {
	base, off := jitStackBase(a, deep)
	for i := jitWindowBase(sp); i < sp; i++ {
		a.MovMemReg(base, off+int32(8*i), jitSlot(i))
	}
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffStackN, uint32(sp))
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffBailIP, uint32(at))
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitBail))
	a.MovRegImm64(jitRegExit, jitmem.ExitBail)
	a.Ret()
}
