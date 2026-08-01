//go:build arm64

package jitmem

// returnConstantCode is
//
//	MOVZ X0, #42
//	RET
//
// Encoded rather than assembled because the point of the test is to prove that
// bytes this package was handed become instructions the CPU executes; going
// through Go's assembler would test Go's assembler.
var returnConstantCode = []byte{
	0x40, 0x05, 0x80, 0xD2, // MOVZ X0, #42
	0xC0, 0x03, 0x5F, 0xD6, // RET
}

// retOnly returns immediately, so entering it measures the trampoline alone.
var retOnly = []byte{0xC0, 0x03, 0x5F, 0xD6} // RET
