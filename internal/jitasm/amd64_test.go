//go:build amd64

package jitasm

import (
	"math"
	"runtime"
	"testing"
	"unsafe"

	"github.com/robomotionio/goant/internal/jitmem"
)

// run assembles, makes executable and enters the code, returning what it left
// in RAX along with the context it operated on.
//
// Testing an encoder by executing its output rather than by comparing against
// expected bytes is the difference between checking the encoding this package
// believes in and checking the one the processor does. Golden bytes are still
// worth having for the cases where a wrong encoding happens to run — see
// TestEncodings — but the CPU is the authority.
func run(t testing.TB, code []byte, ctx *jitmem.ExecContext) uint64 {
	t.Helper()
	b, err := jitmem.Alloc(len(code))
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	t.Cleanup(func() { b.Free() })
	if _, err := b.Write(code); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := b.Protect(); err != nil {
		t.Fatalf("Protect: %v", err)
	}
	return jitmem.Enter(b.Addr(), ctx)
}

// allocatable is every register a compiled function may use: RSP and RBP carry
// the stack, R13 the context, R14 the goroutine.
var allocatable = []Reg{RAX, RCX, RDX, RBX, RSI, RDI, R8, R9, R10, R11, R12, R15}

// TestMovRegImm64EveryRegister is the REX prefix test. An encoder that drops
// REX.B works perfectly for the low eight registers and silently writes to the
// wrong one for the high eight, which is the kind of bug that surfaces as
// corrupted values a long way from its cause.
func TestMovRegImm64EveryRegister(t *testing.T) {
	const want = 0x123456789ABCDEF0
	for _, r := range allocatable {
		a := NewAsm()
		a.MovRegImm64(r, want)
		a.MovRegReg(RAX, r)
		a.Ret()
		var ctx jitmem.ExecContext
		if got := run(t, a.Code(), &ctx); got != want {
			t.Errorf("register %d: got %#x, want %#x", r, got, want)
		}
	}
}

// TestMemoryThroughContext exercises the addressing mode every compiled
// function uses constantly. R13 is the awkward one: its low three bits are 101,
// which with a zero displacement would encode RIP-relative addressing instead,
// so it always needs an explicit displacement byte.
func TestMemoryThroughContext(t *testing.T) {
	a := NewAsm()
	a.MovRegMem(RAX, RegCtx, jitmem.CtxOffArgs)
	a.MovRegMem(RCX, RegCtx, jitmem.CtxOffArgs+8)
	a.AddRegReg(RAX, RCX)
	a.MovMemReg(RegCtx, jitmem.CtxOffRet, RAX)
	a.Ret()

	ctx := jitmem.ExecContext{Args: [4]uint64{3, 4}}
	if got := run(t, a.Code(), &ctx); got != 7 {
		t.Errorf("returned %d, want 7", got)
	}
	if ctx.Ret != 7 {
		t.Errorf("ctx.Ret = %d, want 7", ctx.Ret)
	}
}

// TestMemoryEveryBase covers the bases that need special encoding: R12, whose
// low bits mean "a SIB byte follows", and R13, whose mean "RIP-relative" unless
// a displacement is forced. Displacement 200 exercises the 32-bit form; 0 and 8
// the compact ones.
func TestMemoryEveryBase(t *testing.T) {
	scratch := make([]uint64, 64)
	for i := range scratch {
		scratch[i] = uint64(i)*0x1010101 + 1
	}
	addr := uint64(uintptr(unsafe.Pointer(&scratch[0])))

	for _, base := range []Reg{RAX, RCX, RBX, RSI, RDI, R8, R11, R12, R15} {
		for _, disp := range []int32{0, 8, 200} {
			a := NewAsm()
			a.MovRegMem(base, RegCtx, jitmem.CtxOffArgs) // base = &scratch[0]
			a.MovRegMem(RAX, base, disp)
			a.Ret()

			ctx := jitmem.ExecContext{Args: [4]uint64{addr}}
			want := scratch[disp/8]
			got := run(t, a.Code(), &ctx)
			runtime.KeepAlive(scratch)
			if got != want {
				t.Errorf("base %d disp %d: got %#x, want %#x", base, disp, got, want)
			}
		}
	}
}

// TestDoubleArithmetic runs the operations that carry actual JavaScript
// arithmetic. Values arrive as raw bits because a goant Value that is a Number
// is already the double's bit pattern — unboxing is a register move, not a
// conversion.
func TestDoubleArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name string
		emit func(*Asm)
		x, y float64
		want float64
	}{
		{"add", (*Asm).AddsdX0X1, 1.5, 2.5, 4},
		{"sub", (*Asm).SubsdX0X1, 1.5, 2.5, -1},
		{"mul", (*Asm).MulsdX0X1, 1.5, 2.5, 3.75},
		{"div", (*Asm).DivsdX0X1, 7.5, 2.5, 3},
	} {
		a := NewAsm()
		a.MovsdXMem(X0, RegCtx, jitmem.CtxOffArgs)
		a.MovsdXMem(X1, RegCtx, jitmem.CtxOffArgs+8)
		tc.emit(a)
		a.MovsdMemX(RegCtx, jitmem.CtxOffRet, X0)
		a.MovqRegX(RAX, X0)
		a.Ret()

		ctx := jitmem.ExecContext{Args: [4]uint64{
			math.Float64bits(tc.x), math.Float64bits(tc.y),
		}}
		got := math.Float64frombits(run(t, a.Code(), &ctx))
		if got != tc.want {
			t.Errorf("%s(%v, %v) = %v, want %v", tc.name, tc.x, tc.y, got, tc.want)
		}
		if math.Float64frombits(ctx.Ret) != tc.want {
			t.Errorf("%s stored %v", tc.name, math.Float64frombits(ctx.Ret))
		}
	}
}

// Helpers so the table above can name an operation. X0 and X1 are fixed because
// a template compiler picks its registers rather than allocating them.
func (a *Asm) AddsdX0X1() { a.AddsdXX(X0, X1) }
func (a *Asm) SubsdX0X1() { a.SubsdXX(X0, X1) }
func (a *Asm) MulsdX0X1() { a.MulsdXX(X0, X1) }
func (a *Asm) DivsdX0X1() { a.DivsdXX(X0, X1) }

// TestNaNBoxRoundTrip checks that moving a value between a general-purpose and
// an SSE register preserves every bit, including the payloads of NaNs. goant
// smuggles its type tags inside NaN patterns, so an instruction that quieted a
// signalling NaN in passing would corrupt values rather than lose precision.
func TestNaNBoxRoundTrip(t *testing.T) {
	for _, bits := range []uint64{
		0, math.Float64bits(1.5), math.Float64bits(math.Inf(-1)),
		0x7FF8000000000000, // canonical quiet NaN
		0xFFF9000000000001, // a tagged value, in goant's NaN-box space
		0xFFFFFFFFFFFFFFFF,
	} {
		a := NewAsm()
		a.MovRegMem(RAX, RegCtx, jitmem.CtxOffArgs)
		a.MovqXReg(X3, RAX)
		a.MovRegImm64(RAX, 0) // clobber, so a no-op move cannot pass
		a.MovqRegX(RAX, X3)
		a.Ret()

		ctx := jitmem.ExecContext{Args: [4]uint64{bits}}
		if got := run(t, a.Code(), &ctx); got != bits {
			t.Errorf("round trip of %#016x gave %#016x", bits, got)
		}
	}
}

// TestBranches covers a forward branch taken and not taken, and a backward
// branch, which is the shape every JavaScript loop compiles to.
func TestBranches(t *testing.T) {
	t.Run("not taken", func(t *testing.T) {
		a := NewAsm()
		skip := a.NewLabel()
		a.MovRegImm64(RAX, 1)
		a.CmpRegImm32(RAX, 1)
		a.Jcc(CondNE, skip)
		a.MovRegImm64(RAX, 42)
		a.Bind(skip)
		a.Ret()
		var ctx jitmem.ExecContext
		if got := run(t, a.Code(), &ctx); got != 42 {
			t.Errorf("got %d, want 42 (branch should not have been taken)", got)
		}
	})

	t.Run("taken", func(t *testing.T) {
		a := NewAsm()
		skip := a.NewLabel()
		a.MovRegImm64(RAX, 2)
		a.CmpRegImm32(RAX, 1)
		a.Jcc(CondNE, skip)
		a.MovRegImm64(RAX, 42)
		a.Bind(skip)
		a.Ret()
		var ctx jitmem.ExecContext
		if got := run(t, a.Code(), &ctx); got != 2 {
			t.Errorf("got %d, want 2 (branch should have been taken)", got)
		}
	})

	t.Run("loop", func(t *testing.T) {
		// RAX = 0; RCX = 10; do { RAX += RCX } while (--RCX != 0)  →  55
		a := NewAsm()
		top := a.NewLabel()
		a.MovRegImm64(RAX, 0)
		a.MovRegImm64(RCX, 10)
		a.MovRegImm64(RDX, 1)
		a.Bind(top)
		a.AddRegReg(RAX, RCX)
		a.SubRegReg(RCX, RDX)
		a.CmpRegImm32(RCX, 0)
		a.Jcc(CondNE, top)
		a.Ret()
		var ctx jitmem.ExecContext
		if got := run(t, a.Code(), &ctx); got != 55 {
			t.Errorf("got %d, want 55", got)
		}
	})
}

// TestUnorderedCompare is the reason UcomisdXX exists in this form. Every
// JavaScript relational operator yields false when either operand is NaN, and
// the ordered condition codes do not encode that — a compiler that emits only
// CondB for `<` reports NaN < 1 as true.
func TestUnorderedCompare(t *testing.T) {
	// Returns 1 if the operands are unordered, 0 otherwise.
	a := NewAsm()
	ordered := a.NewLabel()
	a.MovsdXMem(X0, RegCtx, jitmem.CtxOffArgs)
	a.MovsdXMem(X1, RegCtx, jitmem.CtxOffArgs+8)
	a.MovRegImm64(RAX, 1)
	a.UcomisdXX(X0, X1)
	a.Jcc(CondP, ordered)
	a.MovRegImm64(RAX, 0)
	a.Bind(ordered)
	a.Ret()
	code := a.Code()

	for _, tc := range []struct {
		x, y float64
		want uint64
	}{
		{1, 2, 0},
		{2, 1, 0},
		{1, 1, 0},
		{math.NaN(), 1, 1},
		{1, math.NaN(), 1},
		{math.NaN(), math.NaN(), 1},
	} {
		ctx := jitmem.ExecContext{Args: [4]uint64{
			math.Float64bits(tc.x), math.Float64bits(tc.y),
		}}
		if got := run(t, code, &ctx); got != tc.want {
			t.Errorf("unordered(%v, %v) = %d, want %d", tc.x, tc.y, got, tc.want)
		}
	}
}

// TestShifts covers the tag manipulation NaN-boxing needs: a Value's type is
// bits 51..47, so reading one is a shift and a mask.
func TestShifts(t *testing.T) {
	a := NewAsm()
	a.MovRegMem(RAX, RegCtx, jitmem.CtxOffArgs)
	a.ShrRegImm(RAX, 47)
	a.MovRegImm64(RCX, 0x1F)
	a.AndRegReg(RAX, RCX)
	a.Ret()

	// A tagged Value carrying type 9 in bits 51..47.
	const v = 0xFFF0000000000000 | (9 << 47) | 0x1234
	ctx := jitmem.ExecContext{Args: [4]uint64{v}}
	if got := run(t, a.Code(), &ctx); got != 9 {
		t.Errorf("tag = %d, want 9", got)
	}
}

// TestEncodings pins a handful of sequences byte for byte. Execution catches
// most mistakes, but not one that encodes a different instruction which happens
// to produce the same answer for the values a test used.
func TestEncodings(t *testing.T) {
	for _, tc := range []struct {
		name string
		emit func(*Asm)
		want []byte
	}{
		{"mov rax, 42", func(a *Asm) { a.MovRegImm64(RAX, 42) },
			[]byte{0x48, 0xB8, 42, 0, 0, 0, 0, 0, 0, 0}},
		{"mov r15, 1", func(a *Asm) { a.MovRegImm64(R15, 1) },
			[]byte{0x49, 0xBF, 1, 0, 0, 0, 0, 0, 0, 0}},
		{"mov rax, rcx", func(a *Asm) { a.MovRegReg(RAX, RCX) },
			[]byte{0x48, 0x89, 0xC8}},
		{"mov rax, [r13+16]", func(a *Asm) { a.MovRegMem(RAX, R13, 16) },
			[]byte{0x49, 0x8B, 0x45, 0x10}},
		{"mov [r12+0], rax", func(a *Asm) { a.MovMemReg(R12, 0, RAX) },
			[]byte{0x49, 0x89, 0x04, 0x24}},
		{"add rax, rcx", func(a *Asm) { a.AddRegReg(RAX, RCX) },
			[]byte{0x48, 0x01, 0xC8}},
		{"ret", (*Asm).Ret, []byte{0xC3}},
	} {
		a := NewAsm()
		tc.emit(a)
		got := a.Code()
		if len(got) != len(tc.want) {
			t.Errorf("%s: got % x, want % x", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: got % x, want % x", tc.name, got, tc.want)
				break
			}
		}
	}
}

// TestUnboundLabelPanics keeps a compiler bug from becoming a wild branch.
func TestUnboundLabelPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Code did not panic on a branch to an unbound label")
		}
	}()
	a := NewAsm()
	a.Jmp(a.NewLabel())
	a.Code()
}
