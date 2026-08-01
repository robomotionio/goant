//go:build arm64

#include "textflag.h"

// func Enter(pc uintptr, ctx *ExecContext) uint64
//
// R10 carries the ExecContext. R28 is deliberately untouched: it holds the
// current goroutine. R27 (the assembler's temporary), R29 (frame pointer) and
// R30 (link register) are equally off limits to generated code, and R18 is the
// platform register that Darwin and Windows both reserve.
//
// The frame is non-zero so the assembler saves R30 across the call; with a zero
// frame the BL below would overwrite the return address into Go.
TEXT ·Enter(SB), NOSPLIT, $16-24
	MOVD	pc+0(FP), R9
	MOVD	ctx+8(FP), R10
	BL	(R9)
	MOVD	R0, ret+16(FP)
	RET
