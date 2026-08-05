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

// RegShiftCount is where a variable shift count has to be. The architecture
// insists on CL here and does not on arm64, so the templates name it rather than
// writing RCX and being right by accident on one of the two.
const RegShiftCount = RCX

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
	// Set when a signed operation overflowed. Used to recognise the one input
	// CVTTSD2SI cannot convert, which it reports by returning INT64_MIN.
	CondO Cond = 0x0 // overflow
)

// FCond is a condition on the flags a double comparison leaves.
//
// The same encodings as the unsigned integer conditions, because that is what
// UCOMISD sets — and a separate type all the same, because arm64's FCMP does
// not. There the condition that reads "above" for integers is also true when an
// operand is NaN, so the two families have to be told apart at the call site
// rather than at the encoder. See the arm64 file.
type FCond uint8

const (
	FCondE  FCond = 0x4 // ZF: equal, or unordered — test parity separately
	FCondNE FCond = 0x5
	FCondB  FCond = 0x2 // CF: below and ordered
	FCondBE FCond = 0x6
	FCondA  FCond = 0x7
	FCondAE FCond = 0x3
	// Set by UCOMISD when either operand is NaN. Every JavaScript comparison
	// has to route unordered to false, so this is not an edge case.
	FCondUnordered FCond = 0xA // PF
	FCondOrdered   FCond = 0xB
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

// Overflowed reports whether any branch was further than its instruction can
// reach. Never, here: a rel32 displacement covers two gigabytes and a compiled
// function is thousands of bytes. It exists because arm64's conditional branch
// covers one megabyte, which a function can reach.
func (a *Asm) Overflowed() bool { return false }

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

// emitMemIndex encodes an operation on [base + index*scale + disp].
//
// Always a SIB byte, so the two awkward cases of emitMem shrink to one: rm=100
// selects SIB, which is what is wanted, and base=101 with mod=00 means "no base,
// disp32 follows", so RBP and R13 still need an explicit displacement. RSP
// cannot be an index at all: 100 in the index field means "no index", and only
// REX.X rescues it — which is why R12, whose low bits are also 100, is perfectly
// usable as one. That asymmetry is worth a panic rather than a silent
// misencoding, because dropping an index reads the base and gets a real value.
func (a *Asm) emitMemIndex(w bool, opcode []byte, reg, base, index uint8, scale uint8, disp int32) {
	if index == uint8(RSP) {
		panic("jitasm: RSP cannot be a scaled index")
	}
	var ss uint8
	switch scale {
	case 1:
		ss = 0
	case 2:
		ss = 1
	case 4:
		ss = 2
	case 8:
		ss = 3
	default:
		panic("jitasm: scale must be 1, 2, 4 or 8")
	}
	if p := rex(w, reg, index, base); w || p != 0x40 {
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
	a.emit(modrm(mod, reg, 4)) // rm=100: a SIB byte follows
	a.emit(ss<<6 | (index&7)<<3 | base&7)
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

// MovMem8Imm stores a single byte.
//
// Go lays a run of bool fields out one byte apart, so a wider store aimed at one
// of them writes the ones after it as well. This is the only instruction that
// can set such a field without knowing what it neighbours.
func (a *Asm) MovMem8Imm(base Reg, disp int32, v uint8) {
	a.emitMem(false, []byte{0xC6}, 0, uint8(base), disp)
	a.emit(v)
}

// Mov32RegMem loads the low 32 bits of a memory operand, zero-extended.
//
// The engine stores a slot number and a cache epoch as uint32, and reading one
// with a 64-bit load would pull in whatever field follows it.
func (a *Asm) Mov32RegMem(dst, base Reg, disp int32) {
	a.emitMem(false, []byte{0x8B}, uint8(dst), uint8(base), disp)
}

// The narrow stores a typed array needs: the low 8, 16 or 32 bits of a register,
// leaving what surrounds them alone. Storing an integer element IS this
// truncation — ToInt8 and friends are the value modulo the width, which for an
// exact integer already in the register is exactly its low bits.
//
// No REX.W on any of them: the width is the opcode's, and asking for 64 bits
// would write over the neighbouring elements. The byte form does take an
// unconditional REX, for the reason SetccReg documents at length — without one,
// encodings 4 to 7 name AH..BH rather than SPL..DIL, so storing the low byte of
// RSI would silently store the high byte of RCX instead.

// MovMem8Reg stores the low byte of src.
func (a *Asm) MovMem8Reg(base Reg, disp int32, src Reg) {
	a.emit(rex(false, uint8(src), 0, uint8(base)))
	a.emitMemNoRex([]byte{0x88}, uint8(src), uint8(base), disp)
}

// MovMem16Reg stores the low two bytes of src.
func (a *Asm) MovMem16Reg(base Reg, disp int32, src Reg) {
	a.emit(0x66)
	a.emitMem(false, []byte{0x89}, uint8(src), uint8(base), disp)
}

// MovMem32Reg stores the low four bytes of src.
func (a *Asm) MovMem32Reg(base Reg, disp int32, src Reg) {
	a.emitMem(false, []byte{0x89}, uint8(src), uint8(base), disp)
}

// MovRegMemIndex loads from base+index*scale+disp, which is what reading an
// element of an array whose index is not known until run time takes.
func (a *Asm) MovRegMemIndex(dst, base, index Reg, scale uint8, disp int32) {
	a.emitMemIndex(true, []byte{0x8B}, uint8(dst), uint8(base), uint8(index), scale, disp)
}

// MovMemIndexReg stores src at base+index*scale+disp — MovRegMemIndex written
// backwards, and what assigning to a property whose slot is not known until run
// time takes.
func (a *Asm) MovMemIndexReg(base, index Reg, scale uint8, disp int32, src Reg) {
	a.emitMemIndex(true, []byte{0x89}, uint8(src), uint8(base), uint8(index), scale, disp)
}

// MovzxRegMem8 loads one byte and zero-extends it, for the uint8 fields of a
// shape.
func (a *Asm) MovzxRegMem8(dst, base Reg, disp int32) {
	a.emitMem(false, []byte{0x0F, 0xB6}, uint8(dst), uint8(base), disp)
}

// The narrow loads a typed array needs. An Int8Array element is a signed byte
// and a Uint16Array element an unsigned halfword, and the difference is the
// instruction rather than anything after it: sign extension is what makes -1
// read back as -1 rather than 255, and CVTSI2SD converts whatever the whole
// register holds.
//
// The signed forms take REX.W so the extension fills all 64 bits, because the
// conversion that follows reads the register as a 64-bit integer. The unsigned
// forms do not need it: writing the low 32 bits of a register clears the top
// half, which is the zero extension.

// MovsxRegMem8 loads one byte and sign-extends it.
func (a *Asm) MovsxRegMem8(dst, base Reg, disp int32) {
	a.emitMem(true, []byte{0x0F, 0xBE}, uint8(dst), uint8(base), disp)
}

// MovzxRegMem16 loads two bytes and zero-extends them.
func (a *Asm) MovzxRegMem16(dst, base Reg, disp int32) {
	a.emitMem(false, []byte{0x0F, 0xB7}, uint8(dst), uint8(base), disp)
}

// MovsxRegMem16 loads two bytes and sign-extends them.
func (a *Asm) MovsxRegMem16(dst, base Reg, disp int32) {
	a.emitMem(true, []byte{0x0F, 0xBF}, uint8(dst), uint8(base), disp)
}

// MovsxRegMem32 loads four bytes and sign-extends them, which Mov32RegMem
// followed by MovsxdRegReg also does in one instruction more.
func (a *Asm) MovsxRegMem32(dst, base Reg, disp int32) {
	a.emitMem(true, []byte{0x63}, uint8(dst), uint8(base), disp)
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

// AddRegImm32 adds a sign-extended 32-bit immediate and sets the flags.
func (a *Asm) AddRegImm32(r Reg, v uint32) {
	a.emitRM(true, []byte{0x81}, 0, uint8(r))
	a.emit32(v)
}

// AndRegImm32 masks against a sign-extended 32-bit immediate.
// AndsRegImm32 is AndRegImm32 for the callers that go on to branch on the
// result. Here they are the same instruction; on arm64 they are not, because
// the ordinary bitwise operations leave the flags alone.
func (a *Asm) AndsRegImm32(r Reg, v uint32) { a.AndRegImm32(r, v) }

func (a *Asm) AndRegImm32(r Reg, v uint32) {
	a.emitRM(true, []byte{0x81}, 4, uint8(r))
	a.emit32(v)
}

// AddRegMem adds the memory operand at base+disp, which is how a base pointer is
// folded in without needing a register to hold it first.
func (a *Asm) AddRegMem(dst, base Reg, disp int32) {
	a.emitMem(true, []byte{0x03}, uint8(dst), uint8(base), disp)
}

// AddMemImm32 adds a sign-extended immediate to a 64-bit memory operand, which
// is how compiled code bumps a counter without spending a second register.
func (a *Asm) AddMemImm32(base Reg, disp int32, v uint32) {
	a.emitMem(true, []byte{0x81}, 0, uint8(base), disp)
	a.emit32(v)
}

// OrRegMem, CmpRegMem and Cmp32RegMem take their second operand from memory.
//
// A guard sequence is a run of compares against fields of structures that are
// read once and never held, so folding the load into the compare is not a
// peephole — it is the difference between needing a spare register and not.
// OrsRegMem is OrRegMem for a caller that branches on the result — testing two
// pointers for nil at once, which is what both property probes do. Here it is
// the same instruction; on arm64 the bitwise operations leave the flags alone.
func (a *Asm) OrsRegMem(dst, base Reg, disp int32) { a.OrRegMem(dst, base, disp) }

func (a *Asm) OrRegMem(dst, base Reg, disp int32) {
	a.emitMem(true, []byte{0x0B}, uint8(dst), uint8(base), disp)
}

func (a *Asm) CmpRegMem(dst, base Reg, disp int32) {
	a.emitMem(true, []byte{0x3B}, uint8(dst), uint8(base), disp)
}

func (a *Asm) Cmp32RegMem(dst, base Reg, disp int32) {
	a.emitMem(false, []byte{0x3B}, uint8(dst), uint8(base), disp)
}

// LeaRegMemIndex computes base+index*scale+disp without dereferencing it, and
// without touching the flags — which is what turns a three-instruction address
// calculation into one.
func (a *Asm) LeaRegMemIndex(dst, base, index Reg, scale uint8, disp int32) {
	a.emitMemIndex(true, []byte{0x8D}, uint8(dst), uint8(base), uint8(index), scale, disp)
}

// LeaRegMem is base+disp as a value rather than an address to read, and like
// LeaRegMemIndex it leaves the flags alone. What wants it is a field that is an
// array rather than a word: the locals a compiled call writes its arguments
// into are inside the callee's context, so the callee needs their address and
// not their contents.
func (a *Asm) LeaRegMem(dst, base Reg, disp int32) {
	a.emitMem(true, []byte{0x8D}, uint8(dst), uint8(base), disp)
}

func (a *Asm) XchgRegReg(x, y Reg) { a.emitRM(true, []byte{0x87}, uint8(x), uint8(y)) }

// ImulRegImm32 multiplies by a sign-extended 32-bit immediate.
//
// Needed because a pool cell is not a power of two in size, so turning an index
// into a byte offset is a multiply rather than a shift.
func (a *Asm) ImulRegImm32(dst, src Reg, v uint32) {
	a.emitRM(true, []byte{0x69}, uint8(dst), uint8(src))
	a.emit32(v)
}

// Lea32RegMem computes base+disp and keeps the low 32 bits, zero-extended.
//
// The address is never dereferenced. It is here because LEA is the one
// instruction that adds and truncates without touching the flags, which is what
// a caller wants when it has just branched on them.
func (a *Asm) Lea32RegMem(dst, base Reg, disp int32) {
	a.emitMem(false, []byte{0x8D}, uint8(dst), uint8(base), disp)
}

// ---- 32-bit integer operations ----
//
// JavaScript's bitwise operators are defined on 32 bits: both operands go
// through ToInt32, the operation happens there, and the result comes back as a
// double. These are the middle step, so they are deliberately the 32-bit forms —
// dropping REX.W is not an optimisation but the semantics. Writing a 32-bit
// register also clears the upper half, which is what makes the unsigned result
// of `>>>` a zero-extension for free.
//
// The shift forms take their count in CL and need no masking: x86 masks a 32-bit
// shift count to five bits, which is exactly the `& 31` the specification asks
// for.

func (a *Asm) And32RegReg(dst, src Reg) { a.emitRM(false, []byte{0x21}, uint8(src), uint8(dst)) }
func (a *Asm) Or32RegReg(dst, src Reg)  { a.emitRM(false, []byte{0x09}, uint8(src), uint8(dst)) }
func (a *Asm) Xor32RegReg(dst, src Reg) { a.emitRM(false, []byte{0x31}, uint8(src), uint8(dst)) }
func (a *Asm) Mov32RegReg(dst, src Reg) { a.emitRM(false, []byte{0x89}, uint8(src), uint8(dst)) }
func (a *Asm) Not32Reg(r Reg)           { a.emitRM(false, []byte{0xF7}, 2, uint8(r)) }

func (a *Asm) Shl32RegCL(r Reg) { a.emitRM(false, []byte{0xD3}, 4, uint8(r)) }
func (a *Asm) Shr32RegCL(r Reg) { a.emitRM(false, []byte{0xD3}, 5, uint8(r)) }
func (a *Asm) Sar32RegCL(r Reg) { a.emitRM(false, []byte{0xD3}, 7, uint8(r)) }

// MovsxdRegReg sign-extends a 32-bit register into a 64-bit one, which is how a
// signed ToInt32 result becomes something CVTSI2SD will turn back into the right
// double.
func (a *Asm) MovsxdRegReg(dst, src Reg) { a.emitRM(true, []byte{0x63}, uint8(dst), uint8(src)) }

// SetccReg writes 1 or 0 into the low byte of a register.
//
// The REX prefix is emitted unconditionally. Without one, encodings 4 to 7 name
// AH..BH rather than SPL..DIL, so a byte operation on RSI would silently write
// to the high half of RBP instead.
// SetfccReg is SetccReg for the flags a double comparison left. See FCond.
//
// Spelled out rather than routed through emitRM, and that is the whole point:
// emitRM omits a REX prefix it does not need, and this instruction needs one it
// does not need. The two operand-stack slots that are RSI and RDI encode as 6
// and 7, which without a REX name DH and BH — so `SETcc SIL` silently became
// `SETcc DH`, writing a comparison's answer into the high byte of another
// operand and leaving the slot that should hold it untouched.
//
// It cost Octane's TypeScript benchmark, which reported "Parse errors" for a
// day while test262 and mjsunit stayed green: the corruption only happens at an
// operand depth of three or four, which small functions never reach.
func (a *Asm) SetfccReg(c FCond, r Reg) {
	a.emit(rex(false, 0, 0, uint8(r)))
	a.emit(0x0F, 0x90|byte(c), modrm(3, 0, uint8(r)))
}

func (a *Asm) SetccReg(c Cond, r Reg) {
	a.emit(rex(false, 0, 0, uint8(r)))
	a.emit(0x0F, 0x90|byte(c), modrm(3, 0, uint8(r)))
}

// MovzxRegReg8 widens that byte to the whole register.
func (a *Asm) MovzxRegReg8(dst, src Reg) {
	a.emit(rex(false, uint8(dst), 0, uint8(src)))
	a.emit(0x0F, 0xB6, modrm(3, uint8(dst), uint8(src)))
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
	a.emitMemNoRex(opcode, reg, base, disp)
}

// emitMemNoRex is the ModRM, SIB and displacement of a memory operand, for the
// callers that have already decided what REX prefix they need — the byte store,
// which needs one the encoder would have judged unnecessary.
func (a *Asm) emitMemNoRex(opcode []byte, reg, base uint8, disp int32) {
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

// Cvtss2sdXMem loads four bytes as a single-precision float and widens them to
// the double every JavaScript number is. One instruction, because a Float32Array
// element is never a double in memory and always one in the language.
func (a *Asm) Cvtss2sdXMem(dst XReg, base Reg, disp int32) {
	a.emitSSEMem(0xF3, []byte{0x0F, 0x5A}, uint8(dst), uint8(base), disp)
}

// Cvtsd2ssXX narrows a double to single precision, and MovssMemX stores the
// four bytes of one. The pair is Cvtss2sdXMem written backwards, for a store
// into a Float32Array.
func (a *Asm) Cvtsd2ssXX(dst, src XReg) {
	a.emitSSE(0xF2, []byte{0x0F, 0x5A}, uint8(dst), uint8(src))
}

func (a *Asm) MovssMemX(base Reg, disp int32, src XReg) {
	a.emitSSEMem(0xF3, []byte{0x0F, 0x11}, uint8(src), uint8(base), disp)
}

// XorpdXX clears a register when both operands are the same one, which is how a
// zero to compare a double against is produced without a constant.
func (a *Asm) XorpdXX(dst, src XReg) { a.emitSSE(0x66, []byte{0x0F, 0x57}, uint8(dst), uint8(src)) }

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

// Cvttsd2siRegX truncates a double towards zero into a 64-bit integer.
//
// When the result will not fit — the double is too large, or is a NaN or an
// infinity — it returns INT64_MIN rather than faulting. That value is what
// callers test for, because it is the only signal that the conversion did not
// happen; it is also a legitimate result for exactly one input, which is why the
// test has to be arranged so that treating it as a failure is still correct.
func (a *Asm) Cvttsd2siRegX(dst Reg, src XReg) {
	a.emit(0xF2, rex(true, uint8(dst), 0, uint8(src)), 0x0F, 0x2C, modrm(3, uint8(dst), uint8(src)))
}

// Cvtsi2sdXReg converts a signed 64-bit integer back into a double.
func (a *Asm) Cvtsi2sdXReg(dst XReg, src Reg) {
	a.emit(0xF2, rex(true, uint8(dst), 0, uint8(src)), 0x0F, 0x2A, modrm(3, uint8(dst), uint8(src)))
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

// Jfcc branches on the flags a double comparison left. See FCond.
func (a *Asm) Jfcc(c FCond, l *Label) {
	a.emit(0x0F, 0x80|byte(c))
	l.uses = append(l.uses, len(a.buf))
	a.emit32(0)
}

func (a *Asm) CallReg(r Reg) { a.emitRM(false, []byte{0xFF}, 2, uint8(r)) }
func (a *Asm) Ret()          { a.emit(0xC3) }

// SaveLink and RestoreLink bracket a call through CallReg, preserving whatever
// the architecture puts the caller's own return address in.
//
// Nothing here: a CALL pushes the return address and the matching RET pops it,
// so a nested call disturbs nothing the caller was holding. On arm64 the return
// address is a register and a nested call overwrites it, which is why the pair
// exists at all.
func (a *Asm) SaveLink()    {}
func (a *Asm) RestoreLink() {}

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
