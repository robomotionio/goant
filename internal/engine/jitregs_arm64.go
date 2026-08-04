//go:build arm64

package engine

import "github.com/robomotionio/goant/internal/jitasm"

// Which register does what, on arm64. The roles are the amd64 file's; only the
// registers differ.
//
// X10 carries the ExecContext and X28 the goroutine, both fixed by jitmem. X16
// and X17 belong to the encoder, which needs them for the memory operands this
// architecture does not have. X18 is the platform register, X27 the Go
// assembler's temporary, X29 the frame pointer and X30 the link register.
//
// The operand window is nine registers here too. There are more to spare, but
// the depth is a property of the compiler above this — the slot-to-register map
// and every analysis that follows it — and a second number to keep in agreement
// buys nothing until something is measured that wants it.
var jitStackRegs = []jitasm.Reg{
	jitasm.X0, jitasm.X1, jitasm.X2,
	jitasm.X3, jitasm.X4, jitasm.X5, jitasm.X6,
	jitasm.X7, jitasm.X8,
}

const (
	jitRegLocals  = jitasm.X19
	jitRegGuard   = jitasm.X20
	jitRegScratch = jitasm.RegShiftCount
	jitRegReturn  = jitasm.X1
	jitRegExit    = jitasm.X0
	jitRegTmp     = jitRegExit
	jitRegF0      = jitasm.D0
	jitRegF1      = jitasm.D1
)
