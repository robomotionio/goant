//go:build arm64

package jitasm

import (
	"math"
	"testing"
	"unsafe"

	"github.com/robomotionio/goant/internal/jitmem"
)

// run assembles, makes executable and enters the code, returning what it left
// in X0 along with the context it operated on.
//
// Testing an encoder by executing its output rather than by comparing against
// expected bytes is the difference between checking the encoding this package
// believes in and checking the one the processor does. Under `go test -exec
// qemu-aarch64` that processor is emulated, which is enough: qemu decodes the
// architecture rather than the encoder's opinion of it, so a field in the wrong
// place fails there exactly as it would on hardware.
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

// allocatable is every register generated code may use. X10 carries the
// context, X16 and X17 are this package's own, X18 is the platform register,
// X27 the Go assembler's temporary, X28 the goroutine, X29 the frame pointer,
// X30 the link register, and 31 is the stack pointer.
var allocatable = []Reg{
	X0, X1, X2, X3, X4, X5, X6, X7, X8, X9,
	X11, X12, X13, X14, X15, X19, X20, X21, X22, X23, X24, X25, X26,
}

// TestMovRegImm64EveryRegister covers the register field, which is the same
// five bits in every instruction here and so is worth proving once across the
// whole file rather than per instruction.
func TestMovRegImm64EveryRegister(t *testing.T) {
	const want = 0x123456789ABCDEF0
	for _, r := range allocatable {
		a := NewAsm()
		a.MovRegImm64(r, want)
		a.MovRegReg(X0, r)
		a.Ret()
		if got := run(t, a.Code(), &jitmem.ExecContext{}); got != want {
			t.Errorf("through X%d: got %#x, want %#x", r, got, want)
		}
	}
}

// TestMovRegImm64Shapes covers the shortcuts. A constant that fits one MOVZ or
// one MOVN is one instruction and everything else is four, so the values that
// take each path have to produce the same answer.
func TestMovRegImm64Shapes(t *testing.T) {
	for _, want := range []uint64{
		0, 1, 0xFFFF, 0x10000, 0xFFFF0000, 0xFFFF00000000, 0xFFFF000000000000,
		^uint64(0), ^uint64(0xFFFF), 0xFFF0000000000000, // the NaN-box prefix
		0x7FF8000000000000, 0x123456789ABCDEF0,
	} {
		a := NewAsm()
		a.MovRegImm64(X0, want)
		a.Ret()
		if got := run(t, a.Code(), &jitmem.ExecContext{}); got != want {
			t.Errorf("MovRegImm64(%#x): got %#x", want, got)
		}
	}
}

// TestMovRegImm64AtIsFixedWidth is what patching a resume address depends on:
// the value is not known when the instruction is emitted, so its encoding may
// not depend on the value.
func TestMovRegImm64AtIsFixedWidth(t *testing.T) {
	a := NewAsm()
	off := a.MovRegImm64At(X0, 0)
	if n := a.Len() - off; n != 16 {
		t.Fatalf("MovRegImm64At emitted %d bytes, want 16", n)
	}
	a.Ret()
	const want uint64 = 0xDEADBEEFCAFEF00D
	a.PatchUint64(off, want)
	if got := run(t, a.Code(), &jitmem.ExecContext{}); got != want {
		t.Errorf("after patching: got %#x, want %#x", got, want)
	}
}

// TestLoadsAndStores covers the displacement forms: the scaled one an offset
// into the context uses, the unscaled negative one, and the fallback that has to
// compute the address.
func TestLoadsAndStores(t *testing.T) {
	for _, disp := range []int32{0, 8, 64, 4096, 32760, 32768, 100000} {
		buf := make([]uint64, 1+int(disp)/8+2)
		base := uintptr(uintptrOfSlice(buf))
		const want = 0x0102030405060708
		a := NewAsm()
		a.MovRegImm64(X1, uint64(base))
		a.MovRegImm64(X2, want)
		a.MovMemReg(X1, disp, X2)
		a.MovRegMem(X0, X1, disp)
		a.Ret()
		if got := run(t, a.Code(), &jitmem.ExecContext{}); got != want {
			t.Errorf("store then load at +%d: got %#x", disp, got)
		}
		if buf[disp/8] != want {
			t.Errorf("store at +%d landed elsewhere", disp)
		}
	}
}

// TestNarrowLoads covers the widths a Value's tag and an object's flags are read
// at, both of which zero-extend rather than sign-extend.
func TestNarrowLoads(t *testing.T) {
	buf := []uint64{0xFFFFFFFFFFFFFF81}
	base := uintptrOfSlice(buf)
	a := NewAsm()
	a.MovRegImm64(X1, uint64(base))
	a.Mov32RegMem(X0, X1, 0)
	a.MovzxRegMem8(X2, X1, 0)
	a.MovRegImm64(X3, 32)
	a.shiftReg(0x9AC02000, X2, X3) // LSLV X2, X2, X3
	a.OrRegReg(X0, X2)
	a.Ret()
	if got := run(t, a.Code(), &jitmem.ExecContext{}); got != 0x81FFFFFF81 {
		t.Errorf("got %#x, want %#x", got, 0x81FFFFFF81)
	}
}

// TestArithmetic covers the integer operations the templates emit, each against
// what Go computes for the same operands.
func TestArithmetic(t *testing.T) {
	var x, y uint64 = 0x00FF00FF00FF00FF, 0x0F0F0F0F0F0F0F0F
	for _, tc := range []struct {
		name string
		emit func(*Asm)
		want uint64
	}{
		{"add", func(a *Asm) { a.AddRegReg(X0, X1) }, x + y},
		{"sub", func(a *Asm) { a.SubRegReg(X0, X1) }, x - y},
		{"and", func(a *Asm) { a.AndRegReg(X0, X1) }, x & y},
		{"or", func(a *Asm) { a.OrRegReg(X0, X1) }, x | y},
		{"xor", func(a *Asm) { a.XorRegReg(X0, X1) }, x ^ y},
		{"shl", func(a *Asm) { a.ShlRegImm(X0, 5) }, x << 5},
		{"shr", func(a *Asm) { a.ShrRegImm(X0, 5) }, x >> 5},
		{"add-imm", func(a *Asm) { a.AddRegImm32(X0, 4095) }, x + 4095},
		{"add-imm-large", func(a *Asm) { a.AddRegImm32(X0, 100000) }, x + 100000},
		{"sub-imm", func(a *Asm) { a.SubRegImm32(X0, 4095) }, x - 4095},
		{"and-imm", func(a *Asm) { a.AndRegImm32(X0, 0xF0F0) }, x & 0xF0F0},
		{"mul-imm", func(a *Asm) { a.ImulRegImm32(X0, X0, 40) }, x * 40},
		{"sxtw", func(a *Asm) { a.MovsxdRegReg(X0, X1) }, uint64(int64(int32(uint32(y))))},
		{"lea", func(a *Asm) { a.LeaRegMem(X0, X1, 24) }, y + 24},
	} {
		a := NewAsm()
		a.MovRegImm64(X0, x)
		a.MovRegImm64(X1, y)
		tc.emit(a)
		a.Ret()
		if got := run(t, a.Code(), &jitmem.ExecContext{}); got != tc.want {
			t.Errorf("%s: got %#x, want %#x", tc.name, got, tc.want)
		}
	}
}

// TestConditions covers every condition the templates branch on, in both
// directions, because a condition encoded as its own inverse passes half of any
// test that only checks one.
func TestConditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		c      Cond
		x, y   uint64
		expect bool
	}{
		{"eq-true", CondE, 7, 7, true},
		{"eq-false", CondE, 7, 8, false},
		{"ne-true", CondNE, 7, 8, true},
		{"ne-false", CondNE, 7, 7, false},
		{"below-true", CondB, 1, 2, true},
		{"below-false", CondB, 2, 1, false},
		{"below-unsigned", CondB, 2, ^uint64(0), true},
		{"above-true", CondA, 2, 1, true},
		{"above-false", CondA, 1, 2, false},
		{"above-unsigned", CondA, ^uint64(0), 2, true},
		{"ae-true", CondAE, 2, 2, true},
		{"ae-false", CondAE, 1, 2, false},
		{"be-true", CondBE, 2, 2, true},
		{"be-false", CondBE, 3, 2, false},
		{"lt-signed", CondL, ^uint64(0), 1, true},
		{"ge-signed", CondGE, 1, ^uint64(0), true},
		{"gt-signed", CondG, 1, ^uint64(0), true},
		{"le-signed", CondLE, ^uint64(0), 1, true},
	} {
		a := NewAsm()
		taken := a.NewLabel()
		a.MovRegImm64(X1, tc.x)
		a.MovRegImm64(X2, tc.y)
		a.MovRegImm64(X0, 0)
		a.CmpRegReg(X1, X2)
		a.Jcc(tc.c, taken)
		a.Ret()
		a.Bind(taken)
		a.MovRegImm64(X0, 1)
		a.Ret()
		want := uint64(0)
		if tc.expect {
			want = 1
		}
		if got := run(t, a.Code(), &jitmem.ExecContext{}); got != want {
			t.Errorf("%s: got %d, want %d", tc.name, got, want)
		}
	}
}

// TestDoubleConditions is the reason FCond exists. The integer conditions read
// the same flags, and after a double comparison two of them answer true for NaN
// — which is the one answer no JavaScript comparison may give.
func TestDoubleConditions(t *testing.T) {
	nan := math.NaN()
	for _, tc := range []struct {
		name   string
		c      FCond
		x, y   float64
		expect bool
	}{
		{"lt-true", FCondB, 1, 2, true},
		{"lt-false", FCondB, 2, 1, false},
		{"lt-nan", FCondB, nan, 1, false},
		{"gt-true", FCondA, 2, 1, true},
		{"gt-false", FCondA, 1, 2, false},
		{"gt-nan", FCondA, nan, 1, false},
		{"le-true", FCondBE, 2, 2, true},
		{"le-nan", FCondBE, nan, 1, false},
		{"ge-true", FCondAE, 2, 2, true},
		{"ge-nan", FCondAE, 1, nan, false},
		{"eq-true", FCondE, 2, 2, true},
		{"eq-nan", FCondE, nan, nan, false},
		{"unordered", FCondUnordered, nan, 1, true},
		{"ordered", FCondOrdered, 1, 1, true},
	} {
		a := NewAsm()
		taken := a.NewLabel()
		a.MovRegImm64(X1, math.Float64bits(tc.x))
		a.MovRegImm64(X2, math.Float64bits(tc.y))
		a.MovqXReg(D1, X1)
		a.MovqXReg(D2, X2)
		a.MovRegImm64(X0, 0)
		a.UcomisdXX(D1, D2)
		a.Jfcc(tc.c, taken)
		a.Ret()
		a.Bind(taken)
		a.MovRegImm64(X0, 1)
		a.Ret()
		want := uint64(0)
		if tc.expect {
			want = 1
		}
		if got := run(t, a.Code(), &jitmem.ExecContext{}); got != want {
			t.Errorf("%s: got %d, want %d", tc.name, got, want)
		}
	}
}

// TestDoubleArithmetic covers the four operators and the two conversions, in the
// register file JavaScript's numbers actually live in.
func TestDoubleArithmetic(t *testing.T) {
	const x, y = 7.5, 2.5
	for _, tc := range []struct {
		name string
		emit func(*Asm)
		want float64
	}{
		{"add", func(a *Asm) { a.AddsdXX(D1, D2) }, x + y},
		{"sub", func(a *Asm) { a.SubsdXX(D1, D2) }, x - y},
		{"mul", func(a *Asm) { a.MulsdXX(D1, D2) }, x * y},
		{"div", func(a *Asm) { a.DivsdXX(D1, D2) }, x / y},
		{"move", func(a *Asm) { a.MovsdXX(D1, D2) }, y},
		{"zero", func(a *Asm) { a.XorpdXX(D1, D1) }, 0},
	} {
		a := NewAsm()
		a.MovRegImm64(X1, math.Float64bits(x))
		a.MovRegImm64(X2, math.Float64bits(y))
		a.MovqXReg(D1, X1)
		a.MovqXReg(D2, X2)
		tc.emit(a)
		a.MovqRegX(X0, D1)
		a.Ret()
		got := math.Float64frombits(run(t, a.Code(), &jitmem.ExecContext{}))
		if got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDoubleConversions(t *testing.T) {
	a := NewAsm()
	a.MovRegImm64(X1, math.Float64bits(-3.75))
	a.MovqXReg(D1, X1)
	a.Cvttsd2siRegX(X0, D1) // truncates towards zero: -3
	a.Cvtsi2sdXReg(D2, X0)
	a.MovqRegX(X2, D2)
	a.AddRegReg(X0, X2)
	a.Ret()
	want := uint64(^uint64(2)) + math.Float64bits(-3) // -3 plus the double's bits
	if got := run(t, a.Code(), &jitmem.ExecContext{}); got != want {
		t.Errorf("got %#x, want %#x", got, want)
	}
}

// TestDoubleMemory covers the spill and reload a compiled frame does around
// every call out.
func TestDoubleMemory(t *testing.T) {
	buf := make([]uint64, 4)
	a := NewAsm()
	a.MovRegImm64(X1, uint64(uintptrOfSlice(buf)))
	a.MovRegImm64(X2, math.Float64bits(1.25))
	a.MovqXReg(D1, X2)
	a.MovsdMemX(X1, 16, D1)
	a.MovsdXMem(D3, X1, 16)
	a.MovqRegX(X0, D3)
	a.Ret()
	if got := run(t, a.Code(), &jitmem.ExecContext{}); got != math.Float64bits(1.25) {
		t.Errorf("got %#x", got)
	}
	if buf[2] != math.Float64bits(1.25) {
		t.Errorf("the store landed at the wrong offset")
	}
}

// TestMemoryOperands covers the forms this architecture does not have, which are
// synthesised through the reserved registers — and so are the forms most likely
// to clobber something the caller was holding.
func TestMemoryOperands(t *testing.T) {
	buf := []uint64{5, 40, 0}
	base := uintptrOfSlice(buf)
	a := NewAsm()
	a.MovRegImm64(X1, uint64(base))
	a.MovRegImm64(X0, 2)
	a.AddRegMem(X0, X1, 0)   // 2 + 5
	a.OrRegMem(X0, X1, 8)    // | 40
	a.AddMemImm32(X1, 16, 7) // buf[2] += 7
	a.MovRegMem(X2, X1, 16)
	a.AddRegReg(X0, X2)
	a.Ret()
	if got := run(t, a.Code(), &jitmem.ExecContext{}); got != (2+5|40)+7 {
		t.Errorf("got %d, want %d", got, (2+5|40)+7)
	}
	if buf[2] != 7 {
		t.Errorf("AddMemImm32 wrote %d", buf[2])
	}
}

// TestIndexedMemory covers `a[i]`, whose address is a register times a scale.
func TestIndexedMemory(t *testing.T) {
	buf := []uint64{10, 20, 30, 40}
	a := NewAsm()
	a.MovRegImm64(X1, uint64(uintptrOfSlice(buf)))
	a.MovRegImm64(X2, 2)
	a.MovRegMemIndex(X0, X1, X2, 8, 0)
	a.MovRegImm64(X3, 99)
	a.MovMemIndexReg(X1, X2, 8, 0, X3)
	a.Ret()
	if got := run(t, a.Code(), &jitmem.ExecContext{}); got != 30 {
		t.Errorf("indexed load got %d, want 30", got)
	}
	if buf[2] != 99 {
		t.Errorf("indexed store wrote %d", buf[2])
	}
}

// TestCallAndStack covers the two things the compiled call needs: a call through
// a register, and a stack that survives it.
func TestCallAndStack(t *testing.T) {
	callee := NewAsm()
	callee.MovRegImm64(X0, 1234)
	callee.Ret()
	cb, err := jitmem.Alloc(callee.Len())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cb.Free() })
	if _, err := cb.Write(callee.Code()); err != nil {
		t.Fatal(err)
	}
	if err := cb.Protect(); err != nil {
		t.Fatal(err)
	}

	a := NewAsm()
	a.Push(X30) // the link register, which the call below overwrites
	a.MovRegImm64(X1, 7)
	a.Push(X1)
	a.MovRegImm64(X9, uint64(cb.Addr()))
	a.CallReg(X9)
	a.Pop(X1)
	a.AddRegReg(X0, X1)
	a.Pop(X30)
	a.Ret()
	if got := run(t, a.Code(), &jitmem.ExecContext{}); got != 1241 {
		t.Errorf("got %d, want 1241", got)
	}
}

// TestNarrowOperations covers the 32-bit forms, which are what JavaScript's
// bitwise operators are defined on: every one of them truncates to int32, so a
// 64-bit instruction in their place is right until the operand is large.
func TestNarrowOperations(t *testing.T) {
	var x, y uint64 = 0x1234_5678_9ABC_DEF0, 0x0F0F_0F0F_0F0F_0F0F
	for _, tc := range []struct {
		name string
		emit func(*Asm)
		want uint64
	}{
		{"and32", func(a *Asm) { a.And32RegReg(X0, X1) }, uint64(uint32(x) & uint32(y))},
		{"or32", func(a *Asm) { a.Or32RegReg(X0, X1) }, uint64(uint32(x) | uint32(y))},
		{"xor32", func(a *Asm) { a.Xor32RegReg(X0, X1) }, uint64(uint32(x) ^ uint32(y))},
		{"mov32", func(a *Asm) { a.Mov32RegReg(X0, X1) }, uint64(uint32(y))},
		{"not32", func(a *Asm) { a.Not32Reg(X0) }, uint64(^uint32(x))},
		{"lea32", func(a *Asm) { a.Lea32RegMem(X0, X1, 3) }, uint64(uint32(y) + 3)},
		{"shl-var", func(a *Asm) {
			a.MovRegImm64(RegShiftCount, 4)
			a.Shl32RegCL(X0)
		}, uint64(uint32(x) << 4)},
		{"shr-var", func(a *Asm) {
			a.MovRegImm64(RegShiftCount, 4)
			a.Shr32RegCL(X0)
		}, uint64(uint32(x) >> 4)},
		{"sar-var", func(a *Asm) {
			a.MovRegImm64(RegShiftCount, 4)
			a.Sar32RegCL(X0)
		}, uint64(uint32(int32(uint32(x)) >> 4))},
	} {
		a := NewAsm()
		a.MovRegImm64(X0, x)
		a.MovRegImm64(X1, y)
		tc.emit(a)
		a.Ret()
		if got := run(t, a.Code(), &jitmem.ExecContext{}); got != tc.want {
			t.Errorf("%s: got %#x, want %#x", tc.name, got, tc.want)
		}
	}
}

// TestSetcc covers materialising a condition, which is what a comparison
// operator leaves on the operand stack.
func TestSetcc(t *testing.T) {
	for _, tc := range []struct {
		c    Cond
		x, y uint64
		want uint64
	}{
		{CondE, 3, 3, 1},
		{CondE, 3, 4, 0},
		{CondB, 3, 4, 1},
		{CondB, 4, 3, 0},
	} {
		a := NewAsm()
		a.MovRegImm64(X1, tc.x)
		a.MovRegImm64(X2, tc.y)
		a.CmpRegReg(X1, X2)
		a.SetccReg(tc.c, X0)
		a.MovzxRegReg8(X0, X0)
		a.Ret()
		if got := run(t, a.Code(), &jitmem.ExecContext{}); got != tc.want {
			t.Errorf("Setcc(%d, %d, %d): got %d, want %d", tc.c, tc.x, tc.y, got, tc.want)
		}
	}
}

// TestContextRegister is the ABI: the trampoline puts the context in X10 and
// generated code addresses every one of its fields off it.
func TestContextRegister(t *testing.T) {
	ctx := &jitmem.ExecContext{}
	a := NewAsm()
	a.MovRegImm64(X1, 0xABCD)
	a.MovMemReg(RegCtx, jitmem.CtxOffRet, X1)
	a.MovMemImm32(RegCtx, jitmem.CtxOffStackN, 3)
	a.MovRegMem(X0, RegCtx, jitmem.CtxOffRet)
	a.Ret()
	if got := run(t, a.Code(), ctx); got != 0xABCD {
		t.Errorf("got %#x", got)
	}
	if ctx.Ret != 0xABCD || ctx.StackN != 3 {
		t.Errorf("context holds Ret=%#x StackN=%d", ctx.Ret, ctx.StackN)
	}
}

// TestBranchDistances covers the displacement fields, which are different widths
// for the two branch instructions and are the one thing here a short test cannot
// reach by accident.
func TestBranchDistances(t *testing.T) {
	a := NewAsm()
	far := a.NewLabel()
	back := a.NewLabel()
	a.MovRegImm64(X0, 0)
	a.Bind(back)
	a.AddRegImm32(X0, 1)
	a.CmpRegImm32(X0, 3)
	a.Jcc(CondB, back)
	a.Jmp(far)
	a.MovRegImm64(X0, 999) // skipped
	a.Bind(far)
	a.Ret()
	if got := run(t, a.Code(), &jitmem.ExecContext{}); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

// uintptrOfSlice is the address of a slice's first element, for the tests that
// hand generated code a real buffer to work on.
func uintptrOfSlice(s []uint64) uintptr {
	return uintptr(unsafe.Pointer(&s[0]))
}
