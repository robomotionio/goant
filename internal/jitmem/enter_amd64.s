//go:build amd64

#include "textflag.h"

// func Enter(pc uintptr, ctx *ExecContext) uint64
//
// R13 carries the ExecContext. R14 is deliberately untouched: it holds the
// current goroutine, and Go's signal handling finds g through it — generated
// code that clobbers R14 turns the next SIGURG into a crash rather than a
// preemption.
//
// NOSPLIT because generated code runs on this goroutine's stack and must not
// trip morestack, which would try to unwind from a PC it cannot attribute to
// any Go function.
TEXT ·Enter(SB), NOSPLIT, $0-24
	MOVQ	pc+0(FP), AX
	MOVQ	ctx+8(FP), R13
	CALL	AX
	MOVQ	AX, ret+16(FP)
	RET
