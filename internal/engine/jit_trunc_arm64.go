//go:build arm64

package engine

import "github.com/robomotionio/goant/internal/jitasm"

// jitEmitTruncGuard sends a double the conversion could not represent to slow,
// and leaves the rest truncated to 32 bits in dst.
//
// FCVTZS saturates rather than reporting: a value above the range gives
// INT64_MAX, one below gives INT64_MIN, and a NaN gives zero. So the two
// saturation values are the failures, and they cannot be anything else — a
// double whose truncation is exactly INT64_MIN or INT64_MAX is by definition at
// or past the edge, and ToInt32 of it is not the truncation.
//
// A NaN needs no guard at all here: ToInt32(NaN) is zero, which is what the
// conversion already produced.
func jitEmitTruncGuard(a *jitasm.Asm, dst jitasm.Reg, slow *jitasm.Label) {
	a.MovRegImm64(jitRegScratch, 1<<63)
	a.CmpRegReg(dst, jitRegScratch)
	a.Jcc(jitasm.CondE, slow)
	a.MovRegImm64(jitRegScratch, 1<<63-1)
	a.CmpRegReg(dst, jitRegScratch)
	a.Jcc(jitasm.CondE, slow)
	a.Mov32RegReg(dst, dst)
}
