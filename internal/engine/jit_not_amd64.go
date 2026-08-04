//go:build amd64

package engine

import "github.com/robomotionio/goant/internal/jitasm"

// jitEmitNotNumber replaces the Number in r with the Boolean `!r`.
//
// Zero and NaN are the falsy Numbers, and UCOMISD sets the zero flag for both —
// equal, or unordered. So one flag is `!x` already, for either signed zero and
// for a NaN, and one comparison answers the whole question.
func jitEmitNotNumber(a *jitasm.Asm, r jitasm.Reg) {
	a.XorRegReg(jitRegScratch, jitRegScratch)
	a.MovqXReg(jitRegF1, jitRegScratch)
	a.MovqXReg(jitRegF0, r)
	a.UcomisdXX(jitRegF0, jitRegF1)
	jitFBoolean(a, jitasm.FCondE, r)
}
