//go:build amd64

package engine

import "github.com/robomotionio/goant/internal/jitasm"

// Which register does what, on amd64.
//
// R13 carries the ExecContext and R14 the goroutine, both fixed by jitmem. R12
// holds the base of the locals array and R15 the NaN-box threshold. RCX is kept
// aside as a scratch: a variable shift count has to be in CL, whatever the
// machine would rather do, and a spare register is also what lets a 64-bit
// constant be built where one is needed.
//
// What is left is the operand stack, which a template compiler assigns
// positionally rather than allocating: an expression that nests deeper than this
// is simply not compiled. Nine slots rather than ten is not a real loss —
// nothing in a corpus of seven thousand functions is refused for depth.
var jitStackRegs = []jitasm.Reg{
	jitasm.RAX, jitasm.RDX, jitasm.RBX,
	jitasm.RSI, jitasm.RDI, jitasm.R8, jitasm.R9,
	jitasm.R10, jitasm.R11,
}

const (
	jitRegLocals  = jitasm.R12
	jitRegGuard   = jitasm.R15
	jitRegScratch = jitasm.RegShiftCount
	// jitRegReturn carries a returning frame's value to the compiled call site
	// that entered it, beside the exit code in jitRegExit. It is an operand-stack
	// register like any other — the frame it belongs to is over by the time this
	// is read, and the caller's copy of it is in the spill area.
	jitRegReturn = jitasm.RDX
	// jitRegExit is where the trampoline reads the exit code from, and so is
	// also the register the templates reach for when they want one that is dead:
	// it is the first operand-stack slot, which nothing holds at an exit.
	jitRegExit = jitasm.RAX
	jitRegTmp  = jitRegExit
	// The two double registers the arithmetic runs in. A template compiler needs
	// no more: every operator here is binary and consumes both.
	jitRegF0 = jitasm.X0
	jitRegF1 = jitasm.X1
)
