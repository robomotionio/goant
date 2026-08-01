//go:build amd64

package jitmem

import (
	"encoding/binary"
	"testing"
)

// returnConstantCode is
//
//	MOV RAX, 42
//	RET
var returnConstantCode = []byte{
	0x48, 0xC7, 0xC0, 0x2A, 0x00, 0x00, 0x00, // MOV RAX, 42
	0xC3, // RET
}

// retOnly is the cheapest possible compiled function: it does nothing and
// returns. Entering it measures the trampoline and nothing else.
var retOnly = []byte{0xC3}

// helperProtocolCode hand-assembles the two halves of a compiled function that
// asks the runtime to do something in the middle.
//
// R13 holds the ExecContext, which is what Enter puts there. The first half
// records what it wants and returns; the second half is where Run re-enters
// once the helper has run, and it hands back whatever the helper produced.
//
// base is the address the code will live at, needed because the resume address
// is an absolute immediate. Callers Alloc first, which fixes the address, then
// generate — the block cannot move, which is exactly why Write refuses to grow
// one.
func helperProtocolCode(base uintptr, helperID, arg0 uint64) []byte {
	movImm := func(disp8 byte, imm uint32) []byte {
		b := []byte{0x49, 0xC7, 0x45, disp8, 0, 0, 0, 0} // MOV QWORD [R13+disp8], imm32
		binary.LittleEndian.PutUint32(b[4:], imm)
		return b
	}
	resumeCode := func() []byte {
		c := movImm(CtxOffExit, uint32(ExitReturn))
		c = append(c, 0x49, 0x8B, 0x45, CtxOffRet) // MOV RAX, [R13+Ret]
		return append(c, 0xC3)                     // RET
	}

	req := movImm(CtxOffExit, uint32(ExitHelper))
	req = append(req, movImm(CtxOffHelper, uint32(helperID))...)
	req = append(req, movImm(CtxOffArgs, uint32(arg0))...)
	req = append(req, 0x48, 0xB8, 0, 0, 0, 0, 0, 0, 0, 0) // MOV RAX, imm64
	patch := len(req) - 8
	req = append(req, 0x49, 0x89, 0x45, CtxOffResume) // MOV [R13+Resume], RAX
	req = append(req, 0xC3)                           // RET

	binary.LittleEndian.PutUint64(req[patch:], uint64(base)+uint64(len(req)))
	return append(req, resumeCode()...)
}

// buildHelperBlock allocates, generates against the block's own address, and
// seals. Returns the entry point.
func buildHelperBlock(t testing.TB) (*Block, uintptr) {
	t.Helper()
	// Sized generously; the exact length is whatever the generator produces and
	// a page is the smallest thing Alloc can hand out anyway.
	b, err := Alloc(128)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	t.Cleanup(func() { b.Free() })
	if _, err := b.Write(helperProtocolCode(b.Addr(), 7, 6)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := b.Protect(); err != nil {
		t.Fatalf("Protect: %v", err)
	}
	return b, b.Addr()
}

// TestHelperProtocol drives the full exit-and-re-enter cycle: generated code
// asks for helper 7 with argument 6, Go runs it, generated code resumes and
// returns the answer.
func TestHelperProtocol(t *testing.T) {
	_, pc := buildHelperBlock(t)

	var ctx ExecContext
	var calls int
	got := Run(pc, &ctx, func(c *ExecContext) {
		calls++
		if c.Helper != 7 {
			t.Errorf("helper = %d, want 7", c.Helper)
		}
		if c.Args[0] != 6 {
			t.Errorf("arg0 = %d, want 6", c.Args[0])
		}
		c.Ret = c.Args[0] * 7
	})
	if calls != 1 {
		t.Errorf("helper ran %d times, want 1", calls)
	}
	if got != 42 {
		t.Errorf("Run = %d, want 42", got)
	}
}

//go:noinline
func goHelper(ctx *ExecContext) { ctx.Ret = ctx.Args[0] * 7 }

// BenchmarkGoCall is the number to beat: what a helper call costs when the
// caller is ordinary Go, which is what it costs in C ant where jit_helper_* is
// a plain CALL.
func BenchmarkGoCall(b *testing.B) {
	ctx := &ExecContext{Args: [4]uint64{6}}
	for b.Loop() {
		goHelper(ctx)
	}
}

// BenchmarkEnter is the floor: leaving Go for generated code that does nothing
// and coming straight back. Every helper call pays at least this.
func BenchmarkEnter(b *testing.B) {
	bl, err := Alloc(len(retOnly))
	if err != nil {
		b.Fatal(err)
	}
	defer bl.Free()
	bl.Write(retOnly)
	bl.Protect()
	pc := bl.Addr()
	ctx := &ExecContext{}
	for b.Loop() {
		Enter(pc, ctx)
	}
}

// BenchmarkHelperRoundTrip is the real cost: generated code records a request,
// returns to Go, Go dispatches the helper, and generated code is re-entered.
// This is what a compiled function pays every time it cannot do something
// itself, and it is the number that decides how much the JIT may delegate.
func BenchmarkHelperRoundTrip(b *testing.B) {
	bl, pc := buildHelperBlock(b)
	_ = bl
	ctx := &ExecContext{}
	for b.Loop() {
		Run(pc, ctx, goHelper)
	}
}
