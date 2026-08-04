//go:build arm64

package jitasm

// The arm64 encoder.
//
// Same method set as the amd64 one, deliberately: the templates that use it are
// the tier's whole compiler, and a second copy of them is a second place for
// every future opcode to be got wrong. What differs is behind these names.
//
// Three differences shape everything here.
//
// Instructions are four bytes and fixed-width, so there is no ModRM to build and
// no variable-length problem — but also no memory operand on arithmetic. Where
// amd64 writes `cmp rax, [rcx+8]`, this loads and then compares, which needs a
// register the caller did not provide. X16 and X17 are it: the ARM procedure
// standard reserves them for exactly this (they are IP0 and IP1, clobbered by
// any veneer the linker inserts), so nothing else may hold a value across one of
// these calls and nothing here has to ask.
//
// Immediates are small. An ADD takes twelve bits, a load's offset takes twelve
// scaled by the access size, and a 64-bit constant takes four instructions. So
// the same method emits one instruction or three depending on its argument,
// which is why MovRegImm64At exists separately: a resume address is patched into
// code that has already been emitted, so its encoding has to be the same length
// whatever value it starts with.
//
// Conditions are the third. amd64 uses the unsigned condition codes for double
// comparisons because UCOMISD sets CF and ZF; arm64's FCMP sets a different
// combination, and the condition that reads "above" for integers reads "above or
// unordered" after it. NaN then compares greater than everything, which is the
// one answer JavaScript never wants. So the two are separate types here —
// Cond and FCond — and the amd64 file defines both as well, mapping them onto
// the same encodings it always used.

import "encoding/binary"

// Reg is a general-purpose register, numbered as the instruction encoding does.
type Reg uint8

const (
	X0 Reg = iota
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
	X16
	X17
	X18
	X19
	X20
	X21
	X22
	X23
	X24
	X25
	X26
	X27
	X28
	X29
	X30
	// ZR reads as zero and discards what is written to it, and in the load and
	// store encodings the same number means the stack pointer instead. Which one
	// an instruction means is a property of the instruction, so both names exist.
	ZR Reg = 31
	SP Reg = 31
)

// RegCtx carries the ExecContext and RegG the goroutine, both fixed by jitmem's
// entry trampoline. X18 is the platform register Darwin and Windows reserve, X27
// is the Go assembler's temporary, X29 is the frame pointer and X30 the link
// register: none of them is generated code's to use.
const (
	RegG   = X28
	RegCtx = X10
)

// scratch0 and scratch1 are this package's, for the sequences that need a
// register the caller did not name — a memory operand on an arithmetic
// instruction, or an immediate too large for the instruction's field. IP0 and
// IP1 in the ARM procedure standard, which reserves them for the same reason.
const (
	scratch0 = X16
	scratch1 = X17
)

// XReg is a floating-point register. JavaScript numbers are doubles, so these
// carry the arithmetic and the general-purpose registers carry NaN-boxed values.
type XReg uint8

const (
	D0 XReg = iota
	D1
	D2
	D3
	D4
	D5
	D6
	D7
	D8
	D9
	D10
	D11
	D12
	D13
	D14
	D15
)

// Cond is an integer condition, numbered as the B.cond encoding does.
type Cond uint8

const (
	CondE  Cond = 0x0 // EQ: equal
	CondNE Cond = 0x1 // NE: not equal
	CondAE Cond = 0x2 // HS: unsigned above or equal
	CondB  Cond = 0x3 // LO: unsigned below
	CondA  Cond = 0x8 // HI: unsigned above
	CondBE Cond = 0x9 // LS: unsigned below or equal
	CondGE Cond = 0xA // GE: signed greater or equal
	CondL  Cond = 0xB // LT: signed less
	CondG  Cond = 0xC // GT: signed greater
	CondLE Cond = 0xD // LE: signed less or equal
	// CondO is set when a signed operation overflowed, which is how the one
	// input FCVTZS cannot convert is recognised.
	CondO Cond = 0x6 // VS: overflow
)

// FCond is a condition on the flags a double comparison leaves.
//
// A separate type because the mapping is not the integer one. FCMP sets C and V
// on unordered, so HI — the integer "above" — is true when one operand is NaN,
// and every JavaScript comparison has to answer false there. These four are the
// combinations that do.
type FCond uint8

const (
	FCondE  FCond = 0x0 // EQ: equal and ordered
	FCondNE FCond = 0x1 // NE: not equal, or unordered
	FCondB  FCond = 0x4 // MI: less than and ordered
	FCondBE FCond = 0x9 // LS: less or equal and ordered
	FCondA  FCond = 0xC // GT: greater than and ordered
	FCondAE FCond = 0xA // GE: greater or equal and ordered
	// FCondUnordered is true when either operand was NaN, which is what an
	// equality comparison has to test separately: EQ is already false for it.
	FCondUnordered FCond = 0x6 // VS
	FCondOrdered   FCond = 0x7 // VC
)

// Label is a branch target. A label may be jumped to before it is bound; Code
// resolves the displacements.
type Label struct {
	bound bool
	off   int
	uses  []labelUse
}

// labelUse is one branch waiting on a label: where its instruction word is, and
// which field the displacement goes in — a conditional branch reaches ±1MB and
// an unconditional one ±128MB, and they are not the same bits.
type labelUse struct {
	off  int
	wide bool
}

// Asm accumulates encoded instructions.
type Asm struct {
	buf      []byte
	labels   []*Label
	overflow bool
}

// Overflowed reports whether any branch was further than its instruction can
// reach, which makes what Code returned unusable. Meaningful only after Code.
func (a *Asm) Overflowed() bool { return a.overflow }

// NewAsm returns an assembler with room for a typical compiled function.
func NewAsm() *Asm { return &Asm{buf: make([]byte, 0, 512)} }

// Len is the number of bytes emitted so far, which is also the offset the next
// instruction will start at.
func (a *Asm) Len() int { return len(a.buf) }

// Code returns the encoded instructions with every branch resolved.
//
// A displacement that does not fit is recorded rather than raised: a conditional
// branch reaches a megabyte here and an unconditional one a hundred and
// twenty-eight, so overflowing either means a function larger than this tier
// should have taken, and refusing it is the answer. The caller asks Overflowed
// afterwards — the bytes it gets back in that case are not code and are not
// meant to be run.
func (a *Asm) Code() []byte {
	for _, l := range a.labels {
		if !l.bound {
			continue
		}
		for _, u := range l.uses {
			delta := (l.off - u.off) / 4
			w := binary.LittleEndian.Uint32(a.buf[u.off:])
			if u.wide {
				if delta < -(1<<25) || delta >= 1<<25 {
					a.overflow = true
				}
				w = w&0xFC000000 | uint32(delta)&0x03FFFFFF
			} else {
				if delta < -(1<<18) || delta >= 1<<18 {
					a.overflow = true
				}
				w = w&0xFF00001F | uint32(delta)&0x7FFFF<<5
			}
			binary.LittleEndian.PutUint32(a.buf[u.off:], w)
		}
	}
	return a.buf
}

// Unresolved reports whether any label was jumped to and never bound, which is
// a function the emitter gave up on partway through rather than an error here.
func (a *Asm) Unresolved() bool {
	for _, l := range a.labels {
		if !l.bound && len(l.uses) > 0 {
			return true
		}
	}
	return false
}

// NewLabel returns an unbound label.
func (a *Asm) NewLabel() *Label {
	l := &Label{}
	a.labels = append(a.labels, l)
	return l
}

// Bind fixes a label at the current offset.
func (a *Asm) Bind(l *Label) {
	l.bound, l.off = true, len(a.buf)
}

// Offset is where a bound label sits in Code's output.
func (l *Label) Offset() int { return l.off }

// word appends one instruction.
func (a *Asm) word(w uint32) {
	a.buf = binary.LittleEndian.AppendUint32(a.buf, w)
}

// ---- moves ----

// MovRegImm64 materialises a 64-bit constant, in as few instructions as the
// value allows.
func (a *Asm) MovRegImm64(dst Reg, v uint64) {
	// A single MOVZ or MOVN covers the common shapes: a small positive number,
	// and the all-ones-above pattern a NaN box prefix has.
	for hw := 0; hw < 4; hw++ {
		if v&^(uint64(0xFFFF)<<(16*hw)) == 0 {
			a.word(0xD2800000 | uint32(hw)<<21 | uint32(v>>(16*hw)&0xFFFF)<<5 | uint32(dst))
			return
		}
		if ^v&^(uint64(0xFFFF)<<(16*hw)) == 0 {
			a.word(0x92800000 | uint32(hw)<<21 | uint32(^v>>(16*hw)&0xFFFF)<<5 | uint32(dst))
			return
		}
	}
	a.movImmFull(dst, v)
}

// MovRegImm64At materialises a constant in a fixed sixteen bytes and reports
// where they start, for a value patched in after the code has an address.
func (a *Asm) MovRegImm64At(dst Reg, v uint64) (immOff int) {
	off := len(a.buf)
	a.movImmFull(dst, v)
	return off
}

// PatchUint64 rewrites the constant a MovRegImm64At emitted at off.
func (a *Asm) PatchUint64(off int, v uint64) {
	saved := a.buf
	a.buf = a.buf[:off]
	dst := Reg(binary.LittleEndian.Uint32(saved[off:]) & 0x1F)
	a.movImmFull(dst, v)
	a.buf = saved
}

// movImmFull is the four-instruction form, one sixteen-bit slice at a time. The
// same length whatever the value, which is what makes it patchable.
func (a *Asm) movImmFull(dst Reg, v uint64) {
	a.word(0xD2800000 | uint32(v&0xFFFF)<<5 | uint32(dst))
	for hw := 1; hw < 4; hw++ {
		a.word(0xF2800000 | uint32(hw)<<21 | uint32(v>>(16*hw)&0xFFFF)<<5 | uint32(dst))
	}
}

// MovRegReg copies a register.
func (a *Asm) MovRegReg(dst, src Reg) {
	a.word(0xAA0003E0 | uint32(src)<<16 | uint32(dst))
}

// MovRegMem loads eight bytes from base+disp.
func (a *Asm) MovRegMem(dst, base Reg, disp int32) {
	a.loadStore(0xF9400000, 0xF8400000, 3, dst, base, disp)
}

// MovMemReg stores eight bytes to base+disp.
func (a *Asm) MovMemReg(base Reg, disp int32, src Reg) {
	a.loadStore(0xF9000000, 0xF8000000, 3, src, base, disp)
}

// Mov32RegMem loads four bytes and zero-extends.
func (a *Asm) Mov32RegMem(dst, base Reg, disp int32) {
	a.loadStore(0xB9400000, 0xB8400000, 2, dst, base, disp)
}

// MovzxRegMem8 loads one byte and zero-extends.
func (a *Asm) MovzxRegMem8(dst, base Reg, disp int32) {
	a.loadStore(0x39400000, 0x38400000, 0, dst, base, disp)
}

// MovMemImm32 stores a sign-extended 32-bit constant as eight bytes.
func (a *Asm) MovMemImm32(base Reg, disp int32, v uint32) {
	a.MovRegImm64(scratch0, uint64(int64(int32(v))))
	a.MovMemReg(base, disp, scratch0)
}

// MovMem8Imm stores one byte.
func (a *Asm) MovMem8Imm(base Reg, disp int32, v uint8) {
	a.MovRegImm64(scratch0, uint64(v))
	a.loadStore(0x39000000, 0x38000000, 0, scratch0, base, disp)
}

// MovRegMemIndex loads eight bytes from base+index*scale.
func (a *Asm) MovRegMemIndex(dst, base, index Reg, scale uint8, disp int32) {
	a.indexAddr(base, index, scale, disp)
	a.MovRegMem(dst, scratch1, 0)
}

// MovMemIndexReg stores eight bytes to base+index*scale.
func (a *Asm) MovMemIndexReg(base, index Reg, scale uint8, disp int32, src Reg) {
	a.indexAddr(base, index, scale, disp)
	a.MovMemReg(scratch1, 0, src)
}

// indexAddr puts base+index*scale+disp in scratch1.
//
// Scaled addressing is an addressing mode on arm64 too, but only for a shift
// that matches the access size and only with no displacement. Computing the
// address is one instruction more and covers every case, and the sites that use
// it are element accesses that already cost a bounds check.
func (a *Asm) indexAddr(base, index Reg, scale uint8, disp int32) {
	shift := uint32(0)
	switch scale {
	case 1:
	case 2:
		shift = 1
	case 4:
		shift = 2
	case 8:
		shift = 3
	default:
		panic("jitasm: unsupported index scale")
	}
	// ADD Xs1, Xbase, Xindex, LSL #shift
	a.word(0x8B000000 | uint32(index)<<16 | shift<<10 | uint32(base)<<5 | uint32(scratch1))
	if disp != 0 {
		a.addImm(scratch1, scratch1, disp)
	}
}

// LeaRegMem is base+disp as a value rather than an address to read.
func (a *Asm) LeaRegMem(dst, base Reg, disp int32) {
	a.addImm(dst, base, disp)
}

// LeaRegMemIndex is base+index*scale+disp, without touching the flags.
func (a *Asm) LeaRegMemIndex(dst, base, index Reg, scale uint8, disp int32) {
	a.indexAddr(base, index, scale, disp)
	a.MovRegReg(dst, scratch1)
}

// loadStore emits a load or store, picking the scaled unsigned-offset form when
// the displacement fits it and falling back to computing the address.
func (a *Asm) loadStore(scaled, unscaled uint32, log2Size uint, data, base Reg, disp int32) {
	if disp >= 0 && disp&int32((1<<log2Size)-1) == 0 && disp>>log2Size < 1<<12 {
		a.word(scaled | uint32(disp)>>log2Size<<10 | uint32(base)<<5 | uint32(data))
		return
	}
	if disp >= -256 && disp < 256 {
		a.word(unscaled | uint32(disp)&0x1FF<<12 | uint32(base)<<5 | uint32(data))
		return
	}
	a.addImm(scratch0, base, disp)
	a.word(scaled | uint32(scratch0)<<5 | uint32(data))
}

// addImm adds a signed constant, in one instruction where it fits.
func (a *Asm) addImm(dst, src Reg, v int32) {
	op := uint32(0x91000000) // ADD immediate
	u := uint32(v)
	if v < 0 {
		op = 0xD1000000 // SUB immediate
		u = uint32(-v)
	}
	switch {
	case u < 1<<12:
		a.word(op | u<<10 | uint32(src)<<5 | uint32(dst))
	case u&0xFFF == 0 && u>>12 < 1<<12:
		a.word(op | 1<<22 | u>>12<<10 | uint32(src)<<5 | uint32(dst))
	default:
		a.MovRegImm64(scratch0, uint64(int64(v)))
		a.word(0x8B000000 | uint32(scratch0)<<16 | uint32(src)<<5 | uint32(dst))
	}
}

// ---- integer arithmetic ----

func (a *Asm) AddRegReg(dst, src Reg) { a.dataReg(0x8B000000, dst, dst, src) }
func (a *Asm) SubRegReg(dst, src Reg) { a.dataReg(0xCB000000, dst, dst, src) }
func (a *Asm) AndRegReg(dst, src Reg) { a.dataReg(0x8A000000, dst, dst, src) }
func (a *Asm) OrRegReg(dst, src Reg)  { a.dataReg(0xAA000000, dst, dst, src) }
func (a *Asm) XorRegReg(dst, src Reg) { a.dataReg(0xCA000000, dst, dst, src) }

// CmpRegReg sets the flags from x - y.
func (a *Asm) CmpRegReg(x, y Reg) { a.dataReg(0xEB000000, ZR, x, y) }

// TestRegReg sets the flags from x & y.
func (a *Asm) TestRegReg(x, y Reg) { a.dataReg(0xEA000000, ZR, x, y) }

// dataReg emits a three-register data-processing instruction.
func (a *Asm) dataReg(op uint32, dst, lhs, rhs Reg) {
	a.word(op | uint32(rhs)<<16 | uint32(lhs)<<5 | uint32(dst))
}

func (a *Asm) AddRegImm32(r Reg, v uint32) { a.addImm(r, r, int32(v)) }
func (a *Asm) SubRegImm32(r Reg, v uint32) { a.addImm(r, r, -int32(v)) }

// CmpRegImm32 sets the flags from r minus a sign-extended constant.
func (a *Asm) CmpRegImm32(r Reg, v uint32) {
	if u := uint32(int32(v)); int32(v) >= 0 && u < 1<<12 {
		a.word(0xF1000000 | u<<10 | uint32(r)<<5 | uint32(ZR))
		return
	}
	a.MovRegImm64(scratch0, uint64(int64(int32(v))))
	a.CmpRegReg(r, scratch0)
}

// AndRegImm32 masks by a constant.
func (a *Asm) AndRegImm32(r Reg, v uint32) {
	a.MovRegImm64(scratch0, uint64(v))
	a.AndRegReg(r, scratch0)
}

// AndsRegImm32 is AndRegImm32 for a caller that branches on the result. The
// ordinary bitwise instructions here do not touch the flags, which on amd64 they
// cannot help doing — so a template that masks and then tests needs to say so.
func (a *Asm) AndsRegImm32(r Reg, v uint32) {
	a.MovRegImm64(scratch0, uint64(v))
	a.dataReg(0xEA000000, r, r, scratch0) // ANDS
}

// AddRegMem, OrRegMem, CmpRegMem and Cmp32RegMem are the memory-operand forms
// amd64 has and this architecture does not: the value is loaded first, into the
// register this package reserves for it.
func (a *Asm) AddRegMem(dst, base Reg, disp int32) {
	a.MovRegMem(scratch0, base, disp)
	a.AddRegReg(dst, scratch0)
}

func (a *Asm) OrRegMem(dst, base Reg, disp int32) {
	a.MovRegMem(scratch0, base, disp)
	a.OrRegReg(dst, scratch0)
}

// OrsRegMem is OrRegMem for a caller that branches on the result. ORR leaves the
// flags alone here and ORs cannot take a memory operand, so the test has to be
// made rather than inherited.
func (a *Asm) OrsRegMem(dst, base Reg, disp int32) {
	a.MovRegMem(scratch0, base, disp)
	a.OrRegReg(dst, scratch0)
	a.TestRegReg(dst, dst)
}

func (a *Asm) CmpRegMem(dst, base Reg, disp int32) {
	a.MovRegMem(scratch0, base, disp)
	a.CmpRegReg(dst, scratch0)
}

func (a *Asm) Cmp32RegMem(dst, base Reg, disp int32) {
	a.Mov32RegMem(scratch0, base, disp)
	a.word(0x6B000000 | uint32(scratch0)<<16 | uint32(dst)<<5 | uint32(ZR))
}

// AddMemImm32 adds a sign-extended constant to eight bytes in memory.
func (a *Asm) AddMemImm32(base Reg, disp int32, v uint32) {
	a.MovRegMem(scratch1, base, disp)
	a.MovRegImm64(scratch0, uint64(int64(int32(v))))
	a.AddRegReg(scratch1, scratch0)
	a.MovMemReg(base, disp, scratch1)
}

// ImulRegImm32 multiplies by a sign-extended 32-bit constant.
func (a *Asm) ImulRegImm32(dst, src Reg, v uint32) {
	a.MovRegImm64(scratch0, uint64(int64(int32(v))))
	// MADD Xd, Xn, Xm, XZR
	a.word(0x9B000000 | uint32(scratch0)<<16 | uint32(ZR)<<10 | uint32(src)<<5 | uint32(dst))
}

// MovsxdRegReg sign-extends the low 32 bits.
func (a *Asm) MovsxdRegReg(dst, src Reg) {
	// SBFM Xd, Xn, #0, #31
	a.word(0x93400000 | 31<<10 | uint32(src)<<5 | uint32(dst))
}

// MovzxRegReg8 zero-extends the low byte.
func (a *Asm) MovzxRegReg8(dst, src Reg) {
	// UBFM Wd, Wn, #0, #7
	a.word(0x53000000 | 7<<10 | uint32(src)<<5 | uint32(dst))
}

// ShlRegImm and ShrRegImm are the 64-bit shifts by a constant, which arm64
// spells as bitfield moves.
func (a *Asm) ShlRegImm(dst Reg, n uint8) {
	s := uint32(n) & 63
	a.word(0xD3400000 | (64-s)&63<<16 | (63-s)<<10 | uint32(dst)<<5 | uint32(dst))
}

func (a *Asm) ShrRegImm(dst Reg, n uint8) {
	s := uint32(n) & 63
	a.word(0xD3400000 | s<<16 | 63<<10 | uint32(dst)<<5 | uint32(dst))
}

// Shl32RegCL, Shr32RegCL and Sar32RegCL shift a 32-bit value by the count in
// the register amd64 is obliged to use. There is no such obligation here, so
// the count register is named rather than implied — see RegShiftCount.
func (a *Asm) Shl32RegCL(r Reg) { a.shiftReg(0x1AC02000, r, RegShiftCount) }
func (a *Asm) Shr32RegCL(r Reg) { a.shiftReg(0x1AC02400, r, RegShiftCount) }
func (a *Asm) Sar32RegCL(r Reg) { a.shiftReg(0x1AC02800, r, RegShiftCount) }

// RegShiftCount is where a variable shift count lives. Nothing about this
// architecture requires a particular register; the name exists because amd64
// requires CL, and one name means the templates do not have to know which.
const RegShiftCount = X21

func (a *Asm) shiftReg(op uint32, dst, count Reg) {
	a.word(op | uint32(count)<<16 | uint32(dst)<<5 | uint32(dst))
}

// The 32-bit register-to-register forms, for the bitwise operators JavaScript
// defines on int32.
func (a *Asm) And32RegReg(dst, src Reg) { a.dataReg(0x0A000000, dst, dst, src) }
func (a *Asm) Or32RegReg(dst, src Reg)  { a.dataReg(0x2A000000, dst, dst, src) }
func (a *Asm) Xor32RegReg(dst, src Reg) { a.dataReg(0x4A000000, dst, dst, src) }
func (a *Asm) Mov32RegReg(dst, src Reg) { a.dataReg(0x2A0003E0, dst, ZR, src) }
func (a *Asm) Not32Reg(r Reg)           { a.dataReg(0x2A2003E0, r, ZR, r) }

func (a *Asm) Lea32RegMem(dst, base Reg, disp int32) {
	if disp >= 0 && disp < 1<<12 {
		a.word(0x11000000 | uint32(disp)<<10 | uint32(base)<<5 | uint32(dst))
		return
	}
	a.addImm(dst, base, disp)
	a.Mov32RegReg(dst, dst)
}

// SetccReg materialises a condition as 0 or 1.
func (a *Asm) SetccReg(c Cond, r Reg) {
	a.cset(uint32(c), r)
}

// SetfccReg is SetccReg for the flags a double comparison left. See FCond.
func (a *Asm) SetfccReg(c FCond, r Reg) {
	a.cset(uint32(c), r)
}

// cset is CSINC Xd, XZR, XZR, invert(cond) — one where the condition holds and
// zero where it does not.
func (a *Asm) cset(c uint32, r Reg) {
	a.word(0x9A800400 | uint32(ZR)<<16 | (c^1)<<12 | uint32(ZR)<<5 | uint32(r))
}

// ---- doubles ----

func (a *Asm) MovsdXX(dst, src XReg) { a.word(0x1E604000 | uint32(src)<<5 | uint32(dst)) }
func (a *Asm) AddsdXX(dst, src XReg) { a.fpReg(0x1E602800, dst, dst, src) }
func (a *Asm) SubsdXX(dst, src XReg) { a.fpReg(0x1E603800, dst, dst, src) }
func (a *Asm) MulsdXX(dst, src XReg) { a.fpReg(0x1E600800, dst, dst, src) }
func (a *Asm) DivsdXX(dst, src XReg) { a.fpReg(0x1E601800, dst, dst, src) }
func (a *Asm) UcomisdXX(x, y XReg)   { a.word(0x1E602000 | uint32(y)<<16 | uint32(x)<<5) }

// XorpdXX is amd64's way of writing zero into a double register, which is what
// every use of it here is. Anything else has no meaning on this architecture,
// where the general-purpose file has the bitwise operators and this one does
// not.
func (a *Asm) XorpdXX(dst, src XReg) {
	if dst != src {
		panic("jitasm: XORPD is only the zeroing idiom here")
	}
	a.word(0x9E670000 | uint32(ZR)<<5 | uint32(dst)) // FMOV Dd, XZR
}

func (a *Asm) fpReg(op uint32, dst, lhs, rhs XReg) {
	a.word(op | uint32(rhs)<<16 | uint32(lhs)<<5 | uint32(dst))
}

func (a *Asm) MovsdXMem(dst XReg, base Reg, disp int32) {
	a.loadStore(0xFD400000, 0xFC400000, 3, Reg(dst), base, disp)
}

func (a *Asm) MovsdMemX(base Reg, disp int32, src XReg) {
	a.loadStore(0xFD000000, 0xFC000000, 3, Reg(src), base, disp)
}

// MovqXReg and MovqRegX move the bits of a value between the two register
// files, which is what a NaN box is: the same eight bytes read as a double and
// as a tagged integer.
func (a *Asm) MovqXReg(dst XReg, src Reg) {
	a.word(0x9E670000 | uint32(src)<<5 | uint32(dst))
}

func (a *Asm) MovqRegX(dst Reg, src XReg) {
	a.word(0x9E660000 | uint32(src)<<5 | uint32(dst))
}

// Cvttsd2siRegX truncates a double towards zero.
func (a *Asm) Cvttsd2siRegX(dst Reg, src XReg) {
	a.word(0x9E780000 | uint32(src)<<5 | uint32(dst))
}

// Cvtsi2sdXReg converts a signed integer to a double.
func (a *Asm) Cvtsi2sdXReg(dst XReg, src Reg) {
	a.word(0x9E620000 | uint32(src)<<5 | uint32(dst))
}

// ---- control flow ----

func (a *Asm) Jmp(l *Label) {
	l.uses = append(l.uses, labelUse{off: len(a.buf), wide: true})
	a.word(0x14000000)
}

func (a *Asm) Jcc(c Cond, l *Label) {
	l.uses = append(l.uses, labelUse{off: len(a.buf)})
	a.word(0x54000000 | uint32(c))
}

// Jfcc branches on the flags a double comparison left. See FCond.
func (a *Asm) Jfcc(c FCond, l *Label) {
	l.uses = append(l.uses, labelUse{off: len(a.buf)})
	a.word(0x54000000 | uint32(c))
}

func (a *Asm) CallReg(r Reg) { a.word(0xD63F0000 | uint32(r)<<5) }
func (a *Asm) Ret()          { a.word(0xD65F03C0) }

// SaveLink and RestoreLink bracket a call through CallReg. BLR puts the return
// address in X30 and RET jumps to it, so a nested call destroys the address its
// caller was going to return by — here that is the way back into Go, and losing
// it is not a wrong answer but a jump into whatever the callee left behind.
func (a *Asm) SaveLink()    { a.Push(X30) }
func (a *Asm) RestoreLink() { a.Pop(X30) }

// Push and Pop keep the stack pointer sixteen-byte aligned, which the
// architecture requires of every access made through it.
func (a *Asm) Push(r Reg) {
	// STR Xr, [SP, #-16]!
	a.word(0xF81F0FE0 | uint32(r))
}

func (a *Asm) Pop(r Reg) {
	// LDR Xr, [SP], #16
	a.word(0xF84107E0 | uint32(r))
}
