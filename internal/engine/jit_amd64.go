//go:build amd64

package engine

import (
	"runtime"
	"unsafe"

	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// The first tier compiles straight-line Number arithmetic and nothing else.
//
// That is narrow on purpose. Everything it does emit is side-effect-free — no
// stores, no calls, no branches — which makes bailing out trivially correct:
// the interpreter re-runs the function from the top and no state has to be
// reconstructed. Deoptimisation that has to rebuild a frame is the hardest part
// of a JavaScript JIT, and a tier that never needs it is the right place to
// prove the rest of the machinery works.
//
// Registers. R13 carries the ExecContext and R14 the goroutine, both fixed by
// jitmem. R12 holds the base of the locals array and R15 the NaN-box threshold,
// which is compared against on every value that enters the compiled code. What
// is left is the operand stack: a template compiler assigns it positionally
// rather than allocating, so a function whose expressions nest deeper than this
// simply is not compiled.
var jitStackRegs = []jitasm.Reg{
	jitasm.RAX, jitasm.RCX, jitasm.RDX, jitasm.RBX,
	jitasm.RSI, jitasm.RDI, jitasm.R8, jitasm.R9,
	jitasm.R10, jitasm.R11,
}

const (
	jitRegLocals = jitasm.R12
	jitRegGuard  = jitasm.R15
)

// jitCode is a compiled function and the block its code lives in.
type jitCode struct {
	block *jitmem.Block
	entry uintptr
}

// jitCompile compiles fn, or reports that it will not.
//
// Refusing is the common answer and costs nothing: the caller keeps
// interpreting. Every reason to refuse is a shape this tier does not model, not
// an error.
func jitCompile(fn *svFunc) *jitCode {
	a := jitasm.NewAsm()
	bail := a.NewLabel()

	// Prologue. The locals base arrives in Args[0]; the threshold is a constant
	// worth a register because every guard and every NaN canonicalisation
	// compares against it.
	a.MovRegMem(jitRegLocals, jitasm.RegCtx, jitmem.CtxOffArgs)
	a.MovRegImm64(jitRegGuard, uint64(nanboxPrefix))

	sp := 0
	returned := false
	code := fn.code
	ip := fn.startIP

	// A non-arrow function body opens by binding `this` to a local. Skipping it
	// is sound rather than merely convenient: the only way compiled code could
	// observe the slot is by reading it, and a slot holding `this` is not a
	// Number, so the load guard would bail and the interpreter would run the
	// prologue itself.
	if ip+3 < len(code) && Opcode(code[ip]) == OpThis && Opcode(code[ip+1]) == OpPutLocal {
		ip += 4
	}

	for ip < len(code) && !returned {
		switch op := Opcode(code[ip]); op {
		case OpConstI8:
			if sp >= len(jitStackRegs) {
				return nil
			}
			a.MovRegImm64(jitStackRegs[sp], uint64(tov(float64(int8(code[ip+1])))))
			sp++
			ip += 2

		case OpConst:
			idx := readU32(code, ip+1)
			if int(idx) >= len(fn.constants) || sp >= len(jitStackRegs) {
				return nil
			}
			c := fn.constants[idx]
			if !c.IsNumber() {
				// A String or an object constant would mean the generic
				// operators, which is the next tier's problem.
				return nil
			}
			a.MovRegImm64(jitStackRegs[sp], uint64(c))
			sp++
			ip += 5

		case OpGetLocal:
			i := readU16(code, ip+1)
			if int(i) >= fn.maxLocals || sp >= len(jitStackRegs) {
				return nil
			}
			r := jitStackRegs[sp]
			a.MovRegMem(r, jitRegLocals, int32(i)*8)
			// Anything above the threshold is tagged: a String, an object, a
			// binding still in its temporal dead zone. All of them mean the
			// interpreter, and guarding here rather than at each operator keeps
			// the arithmetic itself unconditional.
			a.CmpRegReg(r, jitRegGuard)
			a.Jcc(jitasm.CondA, bail)
			sp++
			ip += 3

		case OpAdd, OpSub, OpMul, OpDiv:
			if sp < 2 {
				return nil
			}
			x, y := jitStackRegs[sp-2], jitStackRegs[sp-1]
			a.MovqXReg(jitasm.X0, x)
			a.MovqXReg(jitasm.X1, y)
			switch op {
			case OpAdd:
				a.AddsdXX(jitasm.X0, jitasm.X1)
			case OpSub:
				a.SubsdXX(jitasm.X0, jitasm.X1)
			case OpMul:
				a.MulsdXX(jitasm.X0, jitasm.X1)
			case OpDiv:
				a.DivsdXX(jitasm.X0, jitasm.X1)
			}
			a.MovqRegX(x, jitasm.X0)
			jitCanonicalizeNaN(a, x)
			sp--
			ip++

		case OpReturn:
			if sp != 1 {
				return nil
			}
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffRet, jitStackRegs[0])
			a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitReturn))
			a.MovRegImm64(jitasm.RAX, jitmem.ExitReturn)
			a.Ret()
			returned = true
			ip++

		default:
			return nil
		}
	}
	if !returned {
		// Falling off the end means an implicit `return undefined`, which is not
		// a Number and so is not this tier's business.
		return nil
	}

	a.Bind(bail)
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitDeopt))
	a.MovRegImm64(jitasm.RAX, jitmem.ExitDeopt)
	a.Ret()

	buf := a.Code()
	block, err := jitmem.Alloc(len(buf))
	if err != nil {
		return nil
	}
	if _, err := block.Write(buf); err != nil {
		block.Free()
		return nil
	}
	if err := block.Protect(); err != nil {
		block.Free()
		return nil
	}
	return &jitCode{block: block, entry: block.Addr()}
}

// jitCanonicalizeNaN folds a NaN result into the one bit pattern the NaN box
// can hold.
//
// Not optional, and not obvious. x86 produces 0xFFF8000000000000 as its default
// quiet NaN — for 0/0 among others — and that is numerically above the tag
// threshold, so storing it raw would hand the rest of the engine a value that
// reads as a tagged object rather than as a number. tov canonicalises for the
// same reason; this is the compiled form of the same rule, and it costs a
// compare and a branch that is never taken in practice.
func jitCanonicalizeNaN(a *jitasm.Asm, r jitasm.Reg) {
	ok := a.NewLabel()
	a.CmpRegReg(r, jitRegGuard)
	a.Jcc(jitasm.CondBE, ok)
	a.MovRegImm64(r, uint64(canonicalNaN))
	a.Bind(ok)
}

// jitRun enters compiled code with the frame's locals and reports whether it
// produced an answer. A false return means the code bailed and the caller must
// interpret instead; because this tier emits nothing with a side effect, that
// is always safe.
func (c *jitCode) jitRun(locals []Value) (Value, bool) {
	if len(locals) == 0 {
		return mkundef(), false
	}
	ctx := jitmem.ExecContext{Args: [4]uint64{uint64(uintptr(unsafe.Pointer(&locals[0])))}}
	jitmem.Enter(c.entry, &ctx)
	// The locals slice reaches compiled code as an integer, so nothing in the
	// call graph keeps it reachable for the collector.
	runtime.KeepAlive(locals)
	if ctx.Exit != jitmem.ExitReturn {
		return mkundef(), false
	}
	return Value(ctx.Ret), true
}

// free releases the code block. A jitCode must outlive every entry into it.
func (c *jitCode) free() {
	if c.block != nil {
		c.block.Free()
		c.block = nil
	}
}
