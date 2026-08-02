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

// TestScaledIndexEveryRegister is the SIB test.
//
// Every combination of base and index, because the encoding has three ways to go
// wrong that a single pair would not show: R12 as a base collides with the
// "SIB follows" escape in the rm field, R13 as a base collides with the
// "no base" escape in the SIB byte, and either register as an index needs REX.X
// rather than REX.B — an encoder that sets the wrong one addresses RAX instead
// of R8 and reads whatever happens to be there.
func TestScaledIndexEveryRegister(t *testing.T) {
	scratch := make([]uint64, 64)
	for i := range scratch {
		scratch[i] = uint64(i)*0x1010101 + 1
	}
	addr := uint64(uintptr(unsafe.Pointer(&scratch[0])))

	for _, base := range allocatable {
		for _, index := range allocatable {
			if index == base {
				continue
			}
			for _, disp := range []int32{0, 8, 200} {
				const idx = 5
				a := NewAsm()
				a.MovRegMem(base, RegCtx, jitmem.CtxOffArgs)
				a.MovRegMem(index, RegCtx, jitmem.CtxOffArgs+8)
				a.MovRegMemIndex(RAX, base, index, 8, disp)
				a.Ret()

				ctx := jitmem.ExecContext{Args: [4]uint64{addr, idx}}
				want := scratch[idx+disp/8]
				got := run(t, a.Code(), &ctx)
				runtime.KeepAlive(scratch)
				if got != want {
					t.Errorf("base %d index %d disp %d: got %#x, want %#x",
						base, index, disp, got, want)
				}
			}
		}
	}
}

// TestScaledIndexScales checks that each scale factor is what it says, since
// getting one wrong reads a real value from the wrong element rather than
// failing.
func TestScaledIndexScales(t *testing.T) {
	scratch := make([]byte, 64)
	for i := range scratch {
		scratch[i] = byte(i)
	}
	addr := uint64(uintptr(unsafe.Pointer(&scratch[0])))

	for _, scale := range []uint8{1, 2, 4, 8} {
		a := NewAsm()
		a.MovRegMem(RSI, RegCtx, jitmem.CtxOffArgs)
		a.MovRegMem(RDI, RegCtx, jitmem.CtxOffArgs+8)
		a.MovRegMemIndex(RAX, RSI, RDI, scale, 0)
		a.Ret()

		ctx := jitmem.ExecContext{Args: [4]uint64{addr, 3}}
		got := byte(run(t, a.Code(), &ctx))
		runtime.KeepAlive(scratch)
		if want := scratch[3*int(scale)]; got != want {
			t.Errorf("scale %d: read element %d, want %d", scale, got, want)
		}
	}
}

// TestLeaScaledIndexKeepsFlags is why LEA is used for address arithmetic in a
// guard sequence: the compare before it has already set the flags a branch after
// it depends on.
func TestLeaScaledIndexKeepsFlags(t *testing.T) {
	a := NewAsm()
	a.MovRegImm64(RAX, 1)
	a.MovRegImm64(RCX, 1)
	a.CmpRegReg(RAX, RCX) // equal
	a.MovRegImm64(RSI, 0x1000)
	a.MovRegImm64(RDI, 2)
	a.LeaRegMemIndex(RDX, RSI, RDI, 8, 16)
	a.MovRegImm64(RAX, 0)
	equal := a.NewLabel()
	a.Jcc(CondE, equal)
	a.Ret() // RAX = 0: LEA destroyed the flags
	a.Bind(equal)
	a.MovRegReg(RAX, RDX)
	a.Ret()

	var ctx jitmem.ExecContext
	if got := run(t, a.Code(), &ctx); got != 0x1000+2*8+16 {
		t.Errorf("lea produced %#x (0 means the flags did not survive)", got)
	}
}

// TestMemoryOperandForms covers the compares and the or that read their second
// operand from memory, including the 32-bit forms that must not pull in the
// field that follows.
func TestMemoryOperandForms(t *testing.T) {
	scratch := []uint64{7, 0xFFFFFFFF00000007, 0}
	addr := uint64(uintptr(unsafe.Pointer(&scratch[0])))

	cases := []struct {
		name string
		emit func(*Asm)
		want uint64
	}{
		// cmp rax, [mem] over equal and unequal operands.
		{"cmp-eq", func(a *Asm) {
			a.MovRegImm64(RAX, 7)
			a.CmpRegMem(RAX, RSI, 0)
		}, 1},
		{"cmp-ne", func(a *Asm) {
			a.MovRegImm64(RAX, 8)
			a.CmpRegMem(RAX, RSI, 0)
		}, 0},
		// The 32-bit compare must ignore the high half, which is what makes it
		// the right instruction for a uint32 field with another beside it.
		{"cmp32-eq", func(a *Asm) {
			a.MovRegImm64(RAX, 7)
			a.Cmp32RegMem(RAX, RSI, 8)
		}, 1},
		{"cmp64-differs", func(a *Asm) {
			a.MovRegImm64(RAX, 7)
			a.CmpRegMem(RAX, RSI, 8)
		}, 0},
		// or rax, [mem] sets ZF from the result, which is how several pointer
		// fields are tested for nil at once.
		{"or-zero", func(a *Asm) {
			a.XorRegReg(RAX, RAX)
			a.OrRegMem(RAX, RSI, 16)
		}, 1},
		{"or-nonzero", func(a *Asm) {
			a.XorRegReg(RAX, RAX)
			a.OrRegMem(RAX, RSI, 0)
		}, 0},
	}
	for _, tc := range cases {
		a := NewAsm()
		a.MovRegMem(RSI, RegCtx, jitmem.CtxOffArgs)
		tc.emit(a)
		set := a.NewLabel()
		a.Jcc(CondE, set)
		a.MovRegImm64(RAX, 0)
		a.Ret()
		a.Bind(set)
		a.MovRegImm64(RAX, 1)
		a.Ret()

		ctx := jitmem.ExecContext{Args: [4]uint64{addr}}
		got := run(t, a.Code(), &ctx)
		runtime.KeepAlive(scratch)
		if got != tc.want {
			t.Errorf("%s: zero flag %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestByteAndWordLoads checks the narrow loads zero-extend rather than merging
// into what the register already held.
func TestByteAndWordLoads(t *testing.T) {
	scratch := []uint64{0xAABBCCDDEEFF0102}
	addr := uint64(uintptr(unsafe.Pointer(&scratch[0])))

	for _, tc := range []struct {
		name string
		emit func(*Asm)
		want uint64
	}{
		{"movzx8", func(a *Asm) { a.MovzxRegMem8(RAX, RSI, 0) }, 0x02},
		{"movzx8-disp", func(a *Asm) { a.MovzxRegMem8(RAX, RSI, 1) }, 0x01},
		{"mov32", func(a *Asm) { a.Mov32RegMem(RAX, RSI, 0) }, 0xEEFF0102},
		{"mov32-disp", func(a *Asm) { a.Mov32RegMem(RAX, RSI, 4) }, 0xAABBCCDD},
	} {
		a := NewAsm()
		a.MovRegImm64(RAX, ^uint64(0)) // must be overwritten, not merged into
		a.MovRegMem(RSI, RegCtx, jitmem.CtxOffArgs)
		tc.emit(a)
		a.Ret()

		ctx := jitmem.ExecContext{Args: [4]uint64{addr}}
		got := run(t, a.Code(), &ctx)
		runtime.KeepAlive(scratch)
		if got != tc.want {
			t.Errorf("%s: got %#x, want %#x", tc.name, got, tc.want)
		}
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
		{"mov rax, [rsi+rdi*8]", func(a *Asm) { a.MovRegMemIndex(RAX, RSI, RDI, 8, 0) },
			[]byte{0x48, 0x8B, 0x04, 0xFE}},
		{"mov rax, [r13+r12*8+16]", func(a *Asm) { a.MovRegMemIndex(RAX, R13, R12, 8, 16) },
			[]byte{0x4B, 0x8B, 0x44, 0xE5, 0x10}},
		{"lea rdx, [rsi+rdi*1+8]", func(a *Asm) { a.LeaRegMemIndex(RDX, RSI, RDI, 1, 8) },
			[]byte{0x48, 0x8D, 0x54, 0x3E, 0x08}},
		{"cmp rax, [rsi+8]", func(a *Asm) { a.CmpRegMem(RAX, RSI, 8) },
			[]byte{0x48, 0x3B, 0x46, 0x08}},
		{"cmp eax, [rsi+8]", func(a *Asm) { a.Cmp32RegMem(RAX, RSI, 8) },
			[]byte{0x3B, 0x46, 0x08}},
		{"or rax, [rsi+8]", func(a *Asm) { a.OrRegMem(RAX, RSI, 8) },
			[]byte{0x48, 0x0B, 0x46, 0x08}},
		{"movzx eax, byte [rsi+8]", func(a *Asm) { a.MovzxRegMem8(RAX, RSI, 8) },
			[]byte{0x0F, 0xB6, 0x46, 0x08}},
		{"mov eax, [rsi+8]", func(a *Asm) { a.Mov32RegMem(RAX, RSI, 8) },
			[]byte{0x8B, 0x46, 0x08}},
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
