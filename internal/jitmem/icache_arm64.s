//go:build arm64 && !windows

#include "textflag.h"

// func flushICacheRange(start, end uintptr)
//
// Line sizes come from CTR_EL0 rather than being assumed. Cores disagree —
// 64 bytes is common, Apple's are not uniformly so — and a stride wider than
// the real line silently skips lines, which produces a JIT that works until it
// mysteriously does not.
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
	MRS	CTR_EL0, R2

	// DminLine (bits 19:16) is log2 of the D-cache line size in words.
	LSR	$16, R2, R3
	AND	$15, R3, R3
	MOVD	$4, R4
	LSL	R3, R4, R4
	SUB	$1, R4, R5
	BIC	R5, R0, R6		// round start down to a line boundary
dcloop:
	CMP	R1, R6
	BHS	dcdone
	DC_CVAU_R6
	ADD	R4, R6, R6
	B	dcloop
dcdone:
	DSB	$11			// DSB ISH — the cleans must land before the invalidates

	// IminLine (bits 3:0) is log2 of the I-cache line size in words, and is
	// not required to match DminLine.
	AND	$15, R2, R3
	MOVD	$4, R4
	LSL	R3, R4, R4
	SUB	$1, R4, R5
	BIC	R5, R0, R6
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
