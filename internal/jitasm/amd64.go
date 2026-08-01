//go:build amd64

package jitasm

import "encoding/binary"

// Reg is a general-purpose register, numbered as the instruction encoding does.
type Reg uint8

const (
	RAX Reg = iota
	RCX
	RDX
	RBX
	RSP
	RBP
	RSI
	RDI
	R8
	R9
	R10
	R11
	R12
	R13
	R14
	R15
)

// R14 holds the current goroutine and R13 the ExecContext (see jitmem). Neither
// is available to generated code; R13 only for addressing the context.
const (
	RegG   = R14
	RegCtx = R13
)

// XReg is an SSE register. JavaScript numbers are doubles, so these carry the
// arithmetic and the general-purpose registers carry NaN-boxed values.
type XReg uint8

const (
	X0 XReg = iota
	X1
	X2
	X3
	X4
	X5
	X6
	X7
	X8
	X9
	X10
	X11
	X12
	X13
	X14
	X15
)

// Cond is a condition code, numbered as the Jcc opcode's low nibble.
type Cond uint8

const (
	// Integer conditions.
	CondE  Cond = 0x4 // equal
	CondNE Cond = 0x5 // not equal
	CondB  Cond = 0x2 // unsigned below
	CondAE Cond = 0x3 // unsigned above or equal
	CondBE Cond = 0x6 // unsigned below or equal
	CondA  Cond = 0x7 // unsigned above
	CondL  Cond = 0xC // signed less
	CondGE Cond = 0xD // signed greater or equal
	CondLE Cond = 0xE // signed less or equal
	CondG  Cond = 0xF // signed greater
	// Set by UCOMISD when either operand is NaN. Every JavaScript comparison
	// has to route unordered to false, so this is not an edge case.
	CondP  Cond = 0xA // parity set: unordered
	CondNP Cond = 0xB // parity clear: ordered
)

// Label is a branch target. A label may be jumped to before it is bound; Code
// resolves the displacements.
type Label struct {
	bound bool
	off   int
	uses  []int // offsets of rel32 displacement fields awaiting this label
}

// Asm accumulates encoded instructions.
type Asm struct {
	buf    []byte
	labels []*Label
}

// NewAsm returns an assembler with room for a typical compiled function.
func NewAsm() *Asm { return &Asm{buf: make([]byte, 0, 512)} }

// Len is the number of bytes emitted so far, which is also the offset the next
// instruction will start at.
func (a *Asm) Len() int { return len(a.buf) }

// Code resolves outstanding branches and returns the encoded bytes.
//
// It panics on an unbound label that something jumped to: that is a bug in the
// compiler using this package, and discovering it as a wild branch at run time
// would be considerably worse.
func (a *Asm) Code() []byte {
	for _, l := range a.labels {
		if !l.bound {
			if len(l.uses) > 0 {
				panic("jitasm: branch to an unbound label")
			}
			continue
		}
		for _, at := range l.uses {
			binary.LittleEndian.PutUint32(a.buf[at:], uint32(int32(l.off-(at+4))))
		}
		l.uses = nil
	}
	return a.buf
}

// Unresolved reports whether anything branches to a label that was never bound.
//
// Code panics on that, because a wild branch is worse than a crash. A compiler
// that may legitimately stop emitting part way — because it decided not to
// compile the function after all — asks here first and declines quietly.
func (a *Asm) Unresolved() bool {
	for _, l := range a.labels {
		if !l.bound && len(l.uses) > 0 {
			return true
		}
	}
	return false
}

// NewLabel allocates an unbound label.
func (a *Asm) NewLabel() *Label {
	l := &Label{}
	a.labels = append(a.labels, l)
	return l
}

// Offset is where a bound label sits in the emitted code, for a caller that has
// to turn it into an absolute address once the code has somewhere to live.
func (l *Label) Offset() int { return l.off }

// Bind fixes a label at the current offset.
func (a *Asm) Bind(l *Label) {
	l.bound = true
	l.off = len(a.buf)
}

func (a *Asm) emit(b ...byte) { a.buf = append(a.buf, b...) }

func (a *Asm) emit32(v uint32) {
	a.buf = binary.LittleEndian.AppendUint32(a.buf, v)
}

func (a *Asm) emit64(v uint64) {
	a.buf = binary.LittleEndian.AppendUint64(a.buf, v)
}

// rex builds a REX prefix. w selects 64-bit operands; r, x and b are the high
// bits of the reg, index and rm fields.
func rex(w bool, r, x, b uint8) byte {
	v := byte(0x40)
	if w {
		v |= 0x08
	}
	v |= (r & 8) >> 1
	v |= (x & 8) >> 2
	v |= (b & 8) >> 3
	return v
}

func modrm(mod, reg, rm uint8) byte { return mod<<6 | (reg&7)<<3 | rm&7 }

// emitRM encodes a ModRM byte for a register-to-register operation.
func (a *Asm) emitRM(w bool, opcode []byte, reg, rm uint8) {
	if p := rex(w, reg, 0, rm); w || p != 0x40 {
		a.emit(p)
	}
	a.emit(opcode...)
	a.emit(modrm(3, reg, rm))
}

// emitMem encodes a ModRM byte (and SIB, and displacement) for an operation on
// [base+disp].
//
// The two awkward cases are structural, not incidental: rm=100 means "a SIB
// byte follows" so RSP and R12 always need one, and rm=101 with mod=00 means
// RIP-relative so RBP and R13 always need an explicit displacement. R13 is the
// context register, so the second case is on the hot path rather than a corner.
func (a *Asm) emitMem(w bool, opcode []byte, reg, base uint8, disp int32) {
	if p := rex(w, reg, 0, base); w || p != 0x40 {
		a.emit(p)
	}
	a.emit(opcode...)

	var mod uint8
	switch {
	case disp == 0 && base&7 != 5:
		mod = 0
	case disp >= -128 && disp <= 127:
		mod = 1
	default:
		mod = 2
	}
	a.emit(modrm(mod, reg, base))
	if base&7 == 4 { // RSP or R12: SIB with no index
		a.emit(0x24)
	}
	switch mod {
	case 1:
		a.emit(byte(int8(disp)))
	case 2:
		a.emit32(uint32(disp))
	}
}

// ---- moves ----

// MovRegImm64 loads a 64-bit immediate. Used for addresses — pool bases, helper
// identifiers, resume points — which is why it does not shrink to the 32-bit
// form: a compiler that patches an immediate after emitting needs its width to
// be predictable.
func (a *Asm) MovRegImm64(dst Reg, v uint64) {
	a.emit(rex(true, 0, 0, uint8(dst)), 0xB8|uint8(dst)&7)
	a.emit64(v)
}

// MovRegImm64At is MovRegImm64 with the offset of the immediate, for a caller
// that must patch it once an address it did not yet know becomes available.
func (a *Asm) MovRegImm64At(dst Reg, v uint64) (immOff int) {
	a.emit(rex(true, 0, 0, uint8(dst)), 0xB8|uint8(dst)&7)
	off := len(a.buf)
	a.emit64(v)
	return off
}

// PatchUint64 overwrites an immediate emitted earlier.
func (a *Asm) PatchUint64(off int, v uint64) {
	binary.LittleEndian.PutUint64(a.buf[off:], v)
}

func (a *Asm) MovRegReg(dst, src Reg) { a.emitRM(true, []byte{0x89}, uint8(src), uint8(dst)) }
func (a *Asm) MovRegMem(dst, base Reg, disp int32) {
	a.emitMem(true, []byte{0x8B}, uint8(dst), uint8(base), disp)
}
func (a *Asm) MovMemReg(base Reg, disp int32, src Reg) {
	a.emitMem(true, []byte{0x89}, uint8(src), uint8(base), disp)
}

// MovMemImm32 stores a sign-extended 32-bit immediate, which is how the small
// constants of the exit protocol are written into the context.
func (a *Asm) MovMemImm32(base Reg, disp int32, v uint32) {
	a.emitMem(true, []byte{0xC7}, 0, uint8(base), disp)
	a.emit32(v)
}

// ---- integer arithmetic ----
//
// Present because NaN-boxing needs it: unpacking a tag is a shift and a mask,
// and building a Value is an or. The arithmetic JavaScript actually performs is
// in the double instructions below.

func (a *Asm) AddRegReg(dst, src Reg) { a.emitRM(true, []byte{0x01}, uint8(src), uint8(dst)) }
func (a *Asm) SubRegReg(dst, src Reg) { a.emitRM(true, []byte{0x29}, uint8(src), uint8(dst)) }
func (a *Asm) AndRegReg(dst, src Reg) { a.emitRM(true, []byte{0x21}, uint8(src), uint8(dst)) }
func (a *Asm) OrRegReg(dst, src Reg)  { a.emitRM(true, []byte{0x09}, uint8(src), uint8(dst)) }
func (a *Asm) XorRegReg(dst, src Reg) { a.emitRM(true, []byte{0x31}, uint8(src), uint8(dst)) }
func (a *Asm) CmpRegReg(x, y Reg)     { a.emitRM(true, []byte{0x39}, uint8(y), uint8(x)) }
func (a *Asm) TestRegReg(x, y Reg)    { a.emitRM(true, []byte{0x85}, uint8(y), uint8(x)) }

func (a *Asm) ShrRegImm(dst Reg, n uint8) { a.emitRM(true, []byte{0xC1}, 5, uint8(dst)); a.emit(n) }
func (a *Asm) ShlRegImm(dst Reg, n uint8) { a.emitRM(true, []byte{0xC1}, 4, uint8(dst)); a.emit(n) }

// SubRegImm32 subtracts a sign-extended 32-bit immediate and sets the flags,
// which is how a back-edge fuel counter is decremented and tested at once.
func (a *Asm) SubRegImm32(r Reg, v uint32) {
	a.emitRM(true, []byte{0x81}, 5, uint8(r))
	a.emit32(v)
}

// CmpRegImm32 compares against a sign-extended 32-bit immediate.
func (a *Asm) CmpRegImm32(r Reg, v uint32) {
	a.emitRM(true, []byte{0x81}, 7, uint8(r))
	a.emit32(v)
}

// ---- doubles ----

func (a *Asm) emitSSE(prefix byte, opcode []byte, reg, rm uint8) {
	a.emit(prefix)
	if p := rex(false, reg, 0, rm); p != 0x40 {
		a.emit(p)
	}
	a.emit(opcode...)
	a.emit(modrm(3, reg, rm))
}

func (a *Asm) emitSSEMem(prefix byte, opcode []byte, reg, base uint8, disp int32) {
	a.emit(prefix)
	a.emitMemNoRexPrefix(opcode, reg, base, disp)
}

// emitMemNoRexPrefix is emitMem for the SSE forms, whose mandatory prefix has
// to come before REX rather than after it.
func (a *Asm) emitMemNoRexPrefix(opcode []byte, reg, base uint8, disp int32) {
	if p := rex(false, reg, 0, base); p != 0x40 {
		a.emit(p)
	}
	a.emit(opcode...)
	var mod uint8
	switch {
	case disp == 0 && base&7 != 5:
		mod = 0
	case disp >= -128 && disp <= 127:
		mod = 1
	default:
		mod = 2
	}
	a.emit(modrm(mod, reg, base))
	if base&7 == 4 {
		a.emit(0x24)
	}
	switch mod {
	case 1:
		a.emit(byte(int8(disp)))
	case 2:
		a.emit32(uint32(disp))
	}
}

func (a *Asm) MovsdXX(dst, src XReg) { a.emitSSE(0xF2, []byte{0x0F, 0x10}, uint8(dst), uint8(src)) }
func (a *Asm) MovsdXMem(dst XReg, base Reg, disp int32) {
	a.emitSSEMem(0xF2, []byte{0x0F, 0x10}, uint8(dst), uint8(base), disp)
}
func (a *Asm) MovsdMemX(base Reg, disp int32, src XReg) {
	a.emitSSEMem(0xF2, []byte{0x0F, 0x11}, uint8(src), uint8(base), disp)
}

func (a *Asm) AddsdXX(dst, src XReg) { a.emitSSE(0xF2, []byte{0x0F, 0x58}, uint8(dst), uint8(src)) }
func (a *Asm) SubsdXX(dst, src XReg) { a.emitSSE(0xF2, []byte{0x0F, 0x5C}, uint8(dst), uint8(src)) }
func (a *Asm) MulsdXX(dst, src XReg) { a.emitSSE(0xF2, []byte{0x0F, 0x59}, uint8(dst), uint8(src)) }
func (a *Asm) DivsdXX(dst, src XReg) { a.emitSSE(0xF2, []byte{0x0F, 0x5E}, uint8(dst), uint8(src)) }

// UcomisdXX compares two doubles, setting the parity flag if either is NaN.
// Callers branch on CondP first: every JavaScript relational operator is false
// when an operand is NaN, and the ordered condition codes do not say so.
func (a *Asm) UcomisdXX(x, y XReg) {
	a.emit(0x66)
	if p := rex(false, uint8(x), 0, uint8(y)); p != 0x40 {
		a.emit(p)
	}
	a.emit(0x0F, 0x2E, modrm(3, uint8(x), uint8(y)))
}

// MovqXReg moves the raw 64 bits of a general-purpose register into an SSE
// register, and MovqRegX the other way. This is the NaN-box boundary: a Value
// that is a double is already the double's bit pattern, so unboxing is this
// move and no conversion.
func (a *Asm) MovqXReg(dst XReg, src Reg) {
	a.emit(0x66, rex(true, uint8(dst), 0, uint8(src)), 0x0F, 0x6E, modrm(3, uint8(dst), uint8(src)))
}

func (a *Asm) MovqRegX(dst Reg, src XReg) {
	a.emit(0x66, rex(true, uint8(src), 0, uint8(dst)), 0x0F, 0x7E, modrm(3, uint8(src), uint8(dst)))
}

// ---- control flow ----

func (a *Asm) Jmp(l *Label) {
	a.emit(0xE9)
	l.uses = append(l.uses, len(a.buf))
	a.emit32(0)
}

func (a *Asm) Jcc(c Cond, l *Label) {
	a.emit(0x0F, 0x80|byte(c))
	l.uses = append(l.uses, len(a.buf))
	a.emit32(0)
}

func (a *Asm) CallReg(r Reg) { a.emitRM(false, []byte{0xFF}, 2, uint8(r)) }
func (a *Asm) Ret()          { a.emit(0xC3) }

func (a *Asm) Push(r Reg) {
	if r >= R8 {
		a.emit(rex(false, 0, 0, uint8(r)))
	}
	a.emit(0x50 | uint8(r)&7)
}

func (a *Asm) Pop(r Reg) {
	if r >= R8 {
		a.emit(rex(false, 0, 0, uint8(r)))
	}
	a.emit(0x58 | uint8(r)&7)
}
