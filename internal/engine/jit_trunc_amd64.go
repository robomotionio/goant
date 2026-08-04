//go:build amd64

package engine

import "github.com/robomotionio/goant/internal/jitasm"

// jitEmitTruncGuard sends a double the conversion could not represent to slow,
// and leaves the rest truncated to 32 bits in dst.
//
// CVTTSD2SI reports every failure the same way: it returns INT64_MIN, for a
// value too large, for a value too small and for a NaN alike. Subtracting one
// overflows for INT64_MIN and for nothing else, and the LEA puts the one back
// and truncates in the same instruction.
func jitEmitTruncGuard(a *jitasm.Asm, dst jitasm.Reg, slow *jitasm.Label) {
	a.SubRegImm32(dst, 1)
	a.Jcc(jitasm.CondO, slow)
	a.Lea32RegMem(dst, dst, 1)
}
