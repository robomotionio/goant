//go:build arm64 && !windows

#include "textflag.h"

// func flushICacheRange(start, end uintptr)
//
// The stride is the architecture's minimum line — sixteen bytes, four words —
// rather than the real one, and that is not an approximation. A stride *wider*
// than the line silently skips lines and produces a JIT that works until it
// mysteriously does not; a narrower one issues the same maintenance operation
// several times for one line, which is correct and costs a few hundred
// instructions on a path taken once per compiled function.
//
// The real width is in CTR_EL0, and reading it is what this used to do. On
// Apple Silicon that read is not permitted from userspace: `MRS x2, CTR_EL0`
// is the first instruction of this function and macOS answers it with SIGILL,
// which is exactly the finding no emulator produces. Apple's own answer is
// sys_icache_invalidate, and that is C — so the way to stay free of cgo is not
// to ask.
//
// DC CVAU / IC IVAU are spelled as their SYS encodings because Go's arm64
// assembler does not accept the mnemonics. DC CVAU, Rt is SYS #3, C7, C11, #1
// and IC IVAU, Rt is SYS #3, C7, C5, #1; the low five bits select Rt, which is
// R6 (0b00110) in both loops below.
#define DC_CVAU_R6	WORD $0xd50b7b26
#define IC_IVAU_R6	WORD $0xd50b7526

TEXT ·flushICacheRange(SB), NOSPLIT|NOFRAME, $0-16
	MOVD	start+0(FP), R0
	MOVD	end+8(FP), R1
	MOVD	$16, R4
	BIC	$15, R0, R6		// round start down to a line boundary
dcloop:
	CMP	R1, R6
	BHS	dcdone
	DC_CVAU_R6
	ADD	R4, R6, R6
	B	dcloop
dcdone:
	DSB	$11			// DSB ISH — the cleans must land before the invalidates

	BIC	$15, R0, R6
icloop:
	CMP	R1, R6
	BHS	icdone
	IC_IVAU_R6
	ADD	R4, R6, R6
	B	icloop
icdone:
	DSB	$11			// DSB ISH
	ISB	$15			// ISB SY — discard anything already fetched
	RET
