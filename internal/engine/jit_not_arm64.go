//go:build arm64

package engine

import "github.com/robomotionio/goant/internal/jitasm"

// jitEmitNotNumber replaces the Number in r with the Boolean `!r`.
//
// Zero and NaN are the falsy Numbers, and here they are two questions. FCMP
// leaves the zero flag set for equality alone: unordered clears it and sets the
// overflow flag instead, so the single test amd64 makes reports `!NaN` as false.
// Which is what the emulator caught, in `!(a - b)` with a NaN either side.
func jitEmitNotNumber(a *jitasm.Asm, r jitasm.Reg) {
	falsy := a.NewLabel()
	done := a.NewLabel()

	a.MovqXReg(jitRegF0, r)
	// Against itself: unordered only for a NaN, which is the whole of the test
	// the zero flag does not carry.
	a.UcomisdXX(jitRegF0, jitRegF0)
	a.Jfcc(jitasm.FCondUnordered, falsy)
	a.XorRegReg(jitRegScratch, jitRegScratch)
	a.MovqXReg(jitRegF1, jitRegScratch)
	a.UcomisdXX(jitRegF0, jitRegF1)
	a.Jfcc(jitasm.FCondE, falsy)

	a.MovRegImm64(r, uint64(mkfalse()))
	a.Jmp(done)
	a.Bind(falsy)
	a.MovRegImm64(r, uint64(mktrue()))
	a.Bind(done)
}
