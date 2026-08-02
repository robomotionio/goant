//go:build amd64

package engine

import (
	"encoding/binary"
	"runtime"
	"unsafe"

	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// The first tier compiles numeric functions: locals, numeric constants, the four
// arithmetic operators, comparisons, branches, and loops. It refuses everything
// else.
//
// What makes it tractable is an invariant rather than a restriction on shape.
// Parameters are checked once, on entry, before anything has been written; every
// value the compiled code then produces is the result of double arithmetic, and
// so is a Number by construction; and every local it reads is either one of
// those checked parameters or a local it assigned itself. Nothing inside the
// body can therefore fail a type check, which means no guard is needed after
// entry, and the only way out is the one at the top — before a single store has
// happened.
//
// That is what makes deoptimisation trivial here. Bailing means the interpreter
// runs the function from the beginning, and there is no state to reconstruct
// because none was changed. Rebuilding an interpreter frame from a half-executed
// compiled one is the hardest part of a JavaScript JIT, and a tier that cannot
// need it is the right place to prove the rest of the machinery.
//
// Registers. R13 carries the ExecContext and R14 the goroutine, both fixed by
// jitmem. R12 holds the base of the locals array and R15 the NaN-box threshold.
// RCX is kept aside as a scratch: a variable shift count has to be in CL,
// whatever the machine would rather do, and a spare register is also what lets a
// 64-bit constant be built where one is needed.
//
// What is left is the operand stack, which a template compiler assigns
// positionally rather than allocating: an expression that nests deeper than this
// is simply not compiled. Nine slots rather than ten is not a real loss —
// nothing in a corpus of seven thousand functions is refused for depth.
var jitStackRegs = []jitasm.Reg{
	jitasm.RAX, jitasm.RDX, jitasm.RBX,
	jitasm.RSI, jitasm.RDI, jitasm.R8, jitasm.R9,
	jitasm.R10, jitasm.R11,
}

const (
	jitRegLocals  = jitasm.R12
	jitRegGuard   = jitasm.R15
	jitRegScratch = jitasm.RCX
)

// jitFuel is how many loop iterations compiled code runs before returning to Go.
//
// Generated code contains no safepoint the Go runtime can recognise, so a
// compiled loop that never came back would keep the collector and the scheduler
// waiting for as long as it ran. Counting iterations and leaving is portable in
// a way that reading Go's internal g fields is not, and it costs a decrement and
// a not-taken branch per iteration.
//
// The exit is a resumption, not a bailout: at a back edge the operand stack is
// empty and every live value is already in the locals array, so re-entering
// needs nothing but the address to re-enter at.
const jitFuel = 20000

// refuse records why a function was declined and returns the nil that says so.
func refuse(why *string, reason string) *jitCode {
	if why != nil {
		*why = reason
	}
	return nil
}

// jitCode is a compiled function and the block its code lives in.
type jitCode struct {
	block *jitmem.Block
	entry uintptr
}

// jitResumeFixup records a resume address that could not be written until the
// code had somewhere to live.
type jitResumeFixup struct {
	immOff int
	label  *jitasm.Label
}

// jitCompile compiles fn, or reports that it will not.
//
// Refusing is the common answer and costs nothing: the caller keeps
// interpreting. Every reason to refuse is a shape this tier does not model,
// never an error.
// why, when not nil, receives the reason a function was refused. Only the
// unsupported-opcode case is named individually, because that is the one that
// says what to build next; everything else is a shape this tier declines and is
// reported as such.
func jitCompile(fn *svFunc, why *string) *jitCode {
	if why != nil {
		*why = "shape"
	}
	code := fn.code
	ip := fn.startIP

	// A non-arrow body opens by binding `this` to a local. Skipping it is sound
	// rather than convenient: the slot is left out of the assigned set below, so
	// any read of it refuses to compile, and on the interpreted path the
	// prologue runs as usual.
	if ip+3 < len(code) && Opcode(code[ip]) == OpThis && Opcode(code[ip+1]) == OpPutLocal {
		ip += 4
	}
	start := ip

	// Check for an opcode with no template before anything else, so that the
	// refusal names it. The analyses below decline the same functions, but they
	// decline them as "undecodable", which says nothing about what to build next.
	if bad, ok := jitUnsupported(fn, start); !ok {
		return refuse(why, "op:"+bad)
	}

	targets, ok := jitScanTargets(fn, start)
	if !ok {
		return nil
	}
	blocks, ok := jitAnalyze(fn, start, targets)
	if !ok {
		return refuse(why, "undecodable")
	}
	numeric, ok := jitNumericLocals(fn, blocks)
	if !ok {
		return refuse(why, "undecodable")
	}

	a := jitasm.NewAsm()
	bail := a.NewLabel()
	labels := make(map[int]*jitasm.Label, len(targets))
	for t := range targets {
		labels[t] = a.NewLabel()
	}
	var fixups []jitResumeFixup

	// Entry. The locals base arrives in Args[0] and the fuel in Args[1]; the
	// threshold is worth a register because every parameter check and every NaN
	// canonicalisation compares against it.
	a.MovRegMem(jitRegLocals, jitasm.RegCtx, jitmem.CtxOffArgs)
	a.MovRegImm64(jitRegGuard, uint64(nanboxPrefix))
	for i := 0; i < fn.paramCount && i < fn.maxLocals; i++ {
		// Anything above the threshold is tagged — a String, an object, a
		// binding still in its temporal dead zone — and all of them mean the
		// interpreter. Checking here, once, is what buys an unguarded body.
		a.MovRegMem(jitasm.RAX, jitRegLocals, int32(i)*8)
		a.CmpRegReg(jitasm.RAX, jitRegGuard)
		a.Jcc(jitasm.CondA, bail)
	}

	sp := 0
	returned := false
	// cur tracks which locals are known assigned at this point. It is seeded
	// from the block's entry set and advanced by the stores within the block, so
	// a read is checked against what holds where it actually is rather than
	// against a whole-function summary.
	cur := make([]bool, fn.maxLocals)
	copy(cur, blocks[start].in)
	// kind[i] records whether operand-stack slot i is known to hold a Number.
	// Arithmetic needs that guarantee; storing and returning do not, which is
	// what lets undefined, null and the Booleans travel through compiled code
	// without any of it having to guard.
	kind := make([]bool, len(jitStackRegs)+1)

	for ip < len(code) {
		if b, isLeader := blocks[ip]; isLeader {
			copy(cur, b.in)
		}
		if l, isTarget := labels[ip]; isTarget {
			// Every branch target is reached with an empty operand stack in the
			// code goant's compiler emits. Requiring it rather than tracking a
			// per-block depth keeps the register assignment positional, and a
			// function that violates it is refused rather than mis-compiled.
			if sp != 0 {
				return refuse(why, "stack-at-target")
			}
			a.Bind(l)
			returned = false
		}
		if returned {
			// Unreachable trailer after a return, most often RETURN_UNDEF.
			break
		}

		switch op := Opcode(code[ip]); op {
		case OpConstI8:
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegImm64(jitStackRegs[sp], uint64(tov(float64(int8(code[ip+1])))))
			kind[sp] = true
			sp++
			ip += 2

		case OpUndef, OpNull, OpTrue, OpFalse:
			// Immediates with no heap reference, so nothing has to stay alive
			// for the code to remain valid. A String or object constant would,
			// which is why OpConst still refuses one.
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			var v Value
			switch op {
			case OpUndef:
				v = mkundef()
			case OpNull:
				v = mknull()
			case OpTrue:
				v = mkbool(true)
			default:
				v = mkbool(false)
			}
			a.MovRegImm64(jitStackRegs[sp], uint64(v))
			kind[sp] = false
			sp++
			ip++

		case OpConst:
			idx := readU32(code, ip+1)
			if int(idx) >= len(fn.constants) || sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			c := fn.constants[idx]
			if !c.IsNumber() {
				// A String or an object constant would mean the generic
				// operators, which is the next tier's problem.
				return refuse(why, "non-numeric-constant")
			}
			a.MovRegImm64(jitStackRegs[sp], uint64(c))
			kind[sp] = true
			sp++
			ip += 5

		case OpGetLocal:
			i := int(readU16(code, ip+1))
			if i >= fn.maxLocals || sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			if !cur[i] {
				// Not assigned on every path that reaches here, so the slot may
				// hold undefined, or a binding still in its dead zone. Either
				// way the interpreter is the one that knows what to do.
				return refuse(why, "local-not-assigned")
			}
			a.MovRegMem(jitStackRegs[sp], jitRegLocals, int32(i)*8)
			kind[sp] = numeric[i]
			sp++
			ip += 3

		case OpPutLocal, OpSetLocal:
			i := int(readU16(code, ip+1))
			if i >= fn.maxLocals || sp < 1 {
				return refuse(why, "stack-underflow")
			}
			a.MovMemReg(jitRegLocals, int32(i)*8, jitStackRegs[sp-1])
			// Everything this tier can put on the operand stack is a Number, so
			// storing one is what makes the slot readable from here on.
			cur[i] = true
			if op == OpPutLocal {
				sp-- // PUT_LOCAL consumes the value; SET_LOCAL leaves it
			}
			ip += 3

		case OpPop:
			if sp < 1 {
				return nil
			}
			sp--
			ip++

		case OpAdd, OpSub, OpMul, OpDiv:
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			if !kind[sp-1] || !kind[sp-2] {
				// An operand may not be a Number, and the generic operators are
				// the next tier's problem: `+` on a String concatenates, and on
				// an object calls valueOf.
				return refuse(why, "non-numeric-operand")
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
			kind[sp-2] = true
			sp--
			ip++

		case OpLt, OpLe, OpGt, OpGe:
			// A comparison produces a Boolean, which this tier has no way to
			// represent. It only compiles one when the very next instruction
			// consumes it as a branch, so the Boolean never exists.
			next := ip + 1
			if next >= len(code) || sp < 2 {
				return refuse(why, "shape")
			}
			if !kind[sp-1] || !kind[sp-2] {
				return refuse(why, "non-numeric-operand")
			}
			l, whenTrue, after, ok := jitFuse(code, labels, next)
			if !ok {
				return refuse(why, "compare-not-branched")
			}
			a.MovqXReg(jitasm.X0, jitStackRegs[sp-2])
			a.MovqXReg(jitasm.X1, jitStackRegs[sp-1])
			a.UcomisdXX(jitasm.X0, jitasm.X1)
			jitCompareBranch(a, op, whenTrue, l)
			sp -= 2
			if sp != 0 {
				return nil
			}
			ip = after

		case OpJmp:
			target := int(readU32(code, ip+1))
			l, known := labels[target]
			if !known || sp != 0 {
				return nil
			}
			if target <= ip {
				jitBackEdge(a, l, &fixups)
			} else {
				a.Jmp(l)
			}
			ip += 5

		case OpDup:
			if sp < 1 || sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegReg(jitStackRegs[sp], jitStackRegs[sp-1])
			kind[sp] = kind[sp-1]
			sp++
			ip++

		case OpInsert2:
			// obj a -> a obj a. Three moves rather than a rotate, because the
			// value being duplicated has to survive its own slot being written.
			if sp < 2 || sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			obj, av := jitStackRegs[sp-2], jitStackRegs[sp-1]
			a.MovRegReg(jitStackRegs[sp], av)
			a.MovRegReg(av, obj)
			a.MovRegReg(obj, jitStackRegs[sp])
			kind[sp] = kind[sp-1]
			kind[sp-1] = kind[sp-2]
			kind[sp-2] = kind[sp]
			sp++
			ip++

		case OpBand, OpBor, OpBxor, OpShl, OpShr, OpUshr:
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			if !kind[sp-1] || !kind[sp-2] {
				// A String or an object operand would mean ToPrimitive, and a
				// BigInt a different operator entirely.
				return refuse(why, "non-numeric-operand")
			}
			if !jitBitwise(a, op, jitStackRegs[sp-2], jitStackRegs[sp-1], sp, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			kind[sp-2] = true
			sp--
			ip++

		case OpBnot:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !kind[sp-1] {
				return refuse(why, "non-numeric-operand")
			}
			r := jitStackRegs[sp-1]
			if !jitToInt32(a, r, r, sp, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.Not32Reg(r)
			jitFromInt32(a, r, true)
			kind[sp-1] = true
			ip++

		case OpNeg:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !kind[sp-1] {
				return refuse(why, "non-numeric-operand")
			}
			// Negation is the sign bit and nothing else, which is also why it
			// needs canonicalising: flipping the sign of the canonical NaN puts
			// it above the tag threshold.
			a.MovRegImm64(jitRegScratch, 1<<63)
			a.XorRegReg(jitStackRegs[sp-1], jitRegScratch)
			jitCanonicalizeNaN(a, jitStackRegs[sp-1])
			ip++

		case OpInc, OpDec:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !kind[sp-1] {
				// ToNumeric on anything else, and a BigInt would increment as
				// one rather than coercing.
				return refuse(why, "non-numeric-operand")
			}
			r := jitStackRegs[sp-1]
			a.MovRegImm64(jitRegScratch, uint64(tov(1)))
			a.MovqXReg(jitasm.X1, jitRegScratch)
			a.MovqXReg(jitasm.X0, r)
			if op == OpInc {
				a.AddsdXX(jitasm.X0, jitasm.X1)
			} else {
				a.SubsdXX(jitasm.X0, jitasm.X1)
			}
			a.MovqRegX(r, jitasm.X0)
			jitCanonicalizeNaN(a, r)
			ip++

		case OpNot:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !kind[sp-1] {
				// ToBoolean of anything else is a different question: the empty
				// string is false, and every object is true.
				return refuse(why, "non-numeric-operand")
			}
			// A Number is falsy when it is zero or a NaN, and UCOMISD sets the
			// zero flag for both — equal, or unordered. So the flag is `!x`
			// already, for either signed zero.
			a.XorRegReg(jitRegScratch, jitRegScratch)
			a.MovqXReg(jitasm.X1, jitRegScratch)
			a.MovqXReg(jitasm.X0, jitStackRegs[sp-1])
			a.UcomisdXX(jitasm.X0, jitasm.X1)
			jitBoolean(a, jitasm.CondE, jitStackRegs[sp-1])
			kind[sp-1] = false
			ip++

		case OpEq, OpNe, OpSeq, OpSne:
			// Between two Numbers, abstract and strict equality are the same
			// comparison, so all four differ only in polarity.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			if !kind[sp-1] || !kind[sp-2] {
				return refuse(why, "non-numeric-operand")
			}
			negate := op == OpNe || op == OpSne
			a.MovqXReg(jitasm.X0, jitStackRegs[sp-2])
			a.MovqXReg(jitasm.X1, jitStackRegs[sp-1])
			a.UcomisdXX(jitasm.X0, jitasm.X1)

			if l, whenTrue, after, ok := jitFuse(code, labels, ip+1); ok {
				jitEqualsBranch(a, negate, whenTrue, l)
				sp -= 2
				if sp != 0 {
					return nil
				}
				ip = after
				continue
			}
			// Not consumed by a branch, so the Boolean has to exist. Emitting it
			// is what lets `a === b` be stored, returned, or passed on.
			jitEqualsValue(a, negate, jitStackRegs[sp-2])
			kind[sp-2] = false
			sp--
			ip++

		case OpGetField:
			// The lookup itself is the runtime's: a shape probe, a prototype
			// walk, possibly a getter that is more JavaScript. What compiled
			// code contributes is everything around it. Emitting the
			// inline-cache hit here instead would be the next real gain, and it
			// is what stops this tier being merely correct.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			idx := readU32(code, ip+1)
			if int(idx) >= len(fn.constNames) || idx > 0x7FFFFFFF {
				return refuse(why, "shape")
			}
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+16, jitStackRegs[sp-1])
			a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffArgs+24, idx)
			if !jitCallHelper(a, sp, jitHelperGetField, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-1], jitasm.RegCtx, jitmem.CtxOffRet)
			// A field holds anything, so the result is not a Number as far as
			// this tier knows. Arithmetic on it is refused rather than guarded.
			kind[sp-1] = false
			ip += 7

		case OpReturnUndef:
			if sp != 0 {
				return refuse(why, "return-stack")
			}
			a.MovRegImm64(jitasm.RAX, uint64(mkundef()))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffRet, jitasm.RAX)
			a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitReturn))
			a.MovRegImm64(jitasm.RAX, jitmem.ExitReturn)
			a.Ret()
			returned = true
			ip++

		case OpReturn:
			if sp != 1 {
				return nil
			}
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffRet, jitStackRegs[0])
			a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitReturn))
			a.MovRegImm64(jitasm.RAX, jitmem.ExitReturn)
			a.Ret()
			sp = 0
			returned = true
			ip++

		default:
			if why != nil {
				*why = "op:" + opTable[op].Name
			}
			return nil
		}
	}
	if !returned {
		// Falling off the end is an implicit `return undefined`, which is not a
		// Number and so is not this tier's business.
		return refuse(why, "falls-off-end")
	}

	a.Bind(bail)
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitDeopt))
	a.MovRegImm64(jitasm.RAX, jitmem.ExitDeopt)
	a.Ret()

	// Emission stops at the first unreachable trailer, so a branch target beyond
	// it never got bound. That is a function this tier does not understand the
	// shape of rather than an error, and declining is the answer — jitasm would
	// otherwise panic, correctly, rather than emit a branch to nowhere.
	if a.Unresolved() {
		return refuse(why, "unreachable-target")
	}

	buf := a.Code()
	block, err := jitmem.Alloc(len(buf))
	if err != nil {
		return nil
	}
	// Resume addresses are absolute and the code had no address until now.
	// Patching rather than regenerating is safe because MovRegImm64 always emits
	// the same ten bytes, so nothing has moved.
	base := uint64(block.Addr())
	for _, f := range fixups {
		binary.LittleEndian.PutUint64(buf[f.immOff:], base+uint64(f.label.Offset()))
	}
	if _, err := block.Write(buf); err != nil {
		block.Free()
		return nil
	}
	if err := block.Protect(); err != nil {
		block.Free()
		return nil
	}
	if why != nil {
		*why = ""
	}
	return &jitCode{block: block, entry: block.Addr()}
}

// jitUnsupported reports the first opcode in the body that this tier has no
// template for.
func jitUnsupported(fn *svFunc, start int) (string, bool) {
	code := fn.code
	for ip := start; ip < len(code); {
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		if size <= 0 || ip+size > len(code) {
			return "undecodable", false
		}
		switch op {
		case OpConst, OpConstI8, OpUndef, OpNull, OpTrue, OpFalse,
			OpGetLocal, OpPutLocal, OpSetLocal, OpPop, OpDup, OpInsert2,
			OpAdd, OpSub, OpMul, OpDiv,
			OpBand, OpBor, OpBxor, OpShl, OpShr, OpUshr, OpBnot,
			OpNeg, OpInc, OpDec, OpNot,
			OpLt, OpLe, OpGt, OpGe, OpEq, OpNe, OpSeq, OpSne,
			OpJmp, OpJmpFalse, OpJmpTrue, OpGetField,
			OpReturn, OpReturnUndef, OpThis:
		default:
			return opTable[op].Name, false
		}
		ip += size
	}
	return "", true
}

// jitScanTargets collects every branch target, which the emitter needs before it
// starts so that a forward branch has a label to name.
//
// It refuses a target that is not on an instruction boundary. That cannot arise
// from goant's compiler, but a mis-decoded stream would otherwise be compiled
// into a wild branch rather than declined.
func jitScanTargets(fn *svFunc, start int) (map[int]bool, bool) {
	code := fn.code
	targets := map[int]bool{}
	boundary := map[int]bool{}
	for ip := start; ip < len(code); {
		boundary[ip] = true
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		if size <= 0 || ip+size > len(code) {
			return nil, false
		}
		switch op {
		case OpJmp, OpJmpFalse, OpJmpTrue:
			targets[int(readU32(code, ip+1))] = true
		}
		ip += size
	}
	for t := range targets {
		if !boundary[t] {
			return nil, false
		}
	}
	return targets, true
}

// jitCompareBranch emits the branch for a fused compare.
//
// The parity flag comes first in both directions. UCOMISD sets it when either
// operand is NaN, and every JavaScript relational operator is false then — which
// the ordered condition codes do not encode, so a compiler that emitted only
// "below" for `<` would report NaN < 1 as true.
func jitCompareBranch(a *jitasm.Asm, op Opcode, whenTrue bool, target *jitasm.Label) {
	if whenTrue {
		skip := a.NewLabel()
		a.Jcc(jitasm.CondP, skip)
		switch op {
		case OpLt:
			a.Jcc(jitasm.CondB, target)
		case OpLe:
			a.Jcc(jitasm.CondBE, target)
		case OpGt:
			a.Jcc(jitasm.CondA, target)
		case OpGe:
			a.Jcc(jitasm.CondAE, target)
		}
		a.Bind(skip)
		return
	}
	a.Jcc(jitasm.CondP, target) // unordered: the comparison is false
	switch op {
	case OpLt:
		a.Jcc(jitasm.CondAE, target)
	case OpLe:
		a.Jcc(jitasm.CondA, target)
	case OpGt:
		a.Jcc(jitasm.CondBE, target)
	case OpGe:
		a.Jcc(jitasm.CondB, target)
	}
}

// jitBackEdge emits a loop's backward jump with the fuel check in front of it.
//
// The operand stack is empty here and every live value is in the locals array,
// so leaving is free of consequence: the resume point re-establishes the two
// pinned registers and carries on. Only the registers need restoring because
// nothing else lives in one across an iteration.
func jitBackEdge(a *jitasm.Asm, top *jitasm.Label, fixups *[]jitResumeFixup) {
	cont := a.NewLabel()
	resume := a.NewLabel()

	a.MovRegMem(jitasm.RAX, jitasm.RegCtx, jitmem.CtxOffArgs+8)
	a.SubRegImm32(jitasm.RAX, 1)
	a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+8, jitasm.RAX) // MOV leaves the flags alone
	a.Jcc(jitasm.CondNE, cont)

	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitPreempt))
	immOff := a.MovRegImm64At(jitasm.RAX, 0)
	*fixups = append(*fixups, jitResumeFixup{immOff: immOff, label: resume})
	a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffResume, jitasm.RAX)
	a.MovRegImm64(jitasm.RAX, jitmem.ExitPreempt)
	a.Ret()

	a.Bind(resume)
	a.MovRegMem(jitRegLocals, jitasm.RegCtx, jitmem.CtxOffArgs)
	a.MovRegImm64(jitRegGuard, uint64(nanboxPrefix))

	a.Bind(cont)
	a.Jmp(top)
}

// Helper identifiers. Compiled code cannot call a Go function, so it records
// which one it wants and returns; jitHelper runs it and execution resumes.
const (
	jitHelperGetField = 1
	jitHelperToInt32  = 2
)

// jitCallHelper emits a call out to the runtime.
//
// The operand stack lives in registers, and returning to Go loses every one of
// them, so the live slots go to the context first and come back on the way in.
// A template compiler knows its depth at every point, which is what makes this
// a fixed sequence rather than a scan.
//
// Reports false when the stack is deeper than the context can hold, which is a
// refusal rather than an error.
func jitCallHelper(a *jitasm.Asm, sp int, helper uint32, fixups *[]jitResumeFixup) bool {
	if sp > jitmem.SpillSlots {
		return false
	}
	resume := a.NewLabel()

	for i := 0; i < sp; i++ {
		a.MovMemReg(jitasm.RegCtx, int32(jitmem.CtxOffSpill+8*i), jitStackRegs[i])
	}
	// SpillN is what tells the collector how much of Spill to trace. Writing it
	// before the exit rather than after is the whole point: between the RET
	// below and the helper returning, this context is the only reference to
	// those values.
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffSpillN, uint32(sp))
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffHelper, helper)
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitHelper))
	immOff := a.MovRegImm64At(jitasm.RAX, 0)
	*fixups = append(*fixups, jitResumeFixup{immOff: immOff, label: resume})
	a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffResume, jitasm.RAX)
	a.MovRegImm64(jitasm.RAX, jitmem.ExitHelper)
	a.Ret()

	a.Bind(resume)
	a.MovRegMem(jitRegLocals, jitasm.RegCtx, jitmem.CtxOffArgs)
	a.MovRegImm64(jitRegGuard, uint64(nanboxPrefix))
	for i := 0; i < sp; i++ {
		a.MovRegMem(jitStackRegs[i], jitasm.RegCtx, int32(jitmem.CtxOffSpill+8*i))
	}
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffSpillN, 0)
	return true
}

// jitCanonicalizeNaN folds a NaN result into the one bit pattern the NaN box can
// hold.
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
// produced the answer.
//
// A false return means the code declined the arguments it was given and the
// caller must interpret instead. That is always safe: the only exit that reports
// it is the entry check, which runs before compiled code has written anything.
//
// A throw is not a decline. It comes from a helper, by which point the frame has
// run, and this tier has no exception handlers of its own — TRY_PUSH is refused
// — so the only thing left to do is what the interpreter would: leave.
func (c *jitCode) jitRun(rt *Runtime, fn *svFunc, locals []Value) (Value, *ThrowError, bool) {
	if len(locals) == 0 {
		return mkundef(), nil, false
	}
	ctx := &jitmem.ExecContext{Args: [4]uint64{
		uint64(uintptr(unsafe.Pointer(&locals[0]))),
		jitFuel,
	}}
	// Rooted for as long as compiled code can be suspended in a helper holding
	// values nothing else refers to. See markRoots.
	rt.jitFrames = append(rt.jitFrames, ctx)
	defer func() { rt.jitFrames = rt.jitFrames[:len(rt.jitFrames)-1] }()

	pc := c.entry
	for {
		jitmem.Enter(pc, ctx)
		// The locals slice reaches compiled code as an integer, so nothing in
		// the call graph keeps it reachable for the collector.
		runtime.KeepAlive(locals)
		switch ctx.Exit {
		case jitmem.ExitReturn:
			return Value(ctx.Ret), nil, true
		case jitmem.ExitPreempt:
			// Being here is the safepoint: this is ordinary Go, so the runtime
			// can collect and preempt before the loop is re-entered.
			ctx.Args[1] = jitFuel
			pc = ctx.Resume
		case jitmem.ExitHelper:
			if e := jitHelper(rt, fn, ctx); e != nil {
				return mkundef(), e, true
			}
			pc = ctx.Resume
		default:
			return mkundef(), nil, false
		}
	}
}

// jitHelper runs what compiled code asked for.
func jitHelper(rt *Runtime, fn *svFunc, ctx *jitmem.ExecContext) *ThrowError {
	switch ctx.Helper {
	case jitHelperGetField:
		v, e := rt.getField(Value(ctx.Args[2]), fn.constNames[ctx.Args[3]])
		if e != nil {
			return e
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperToInt32:
		// Reached only for a finite magnitude of 2^63 or more, which CVTTSD2SI
		// cannot report. Zero-extended, because compiled code keeps every
		// intermediate integer that way — see jitToInt32.
		ctx.Ret = uint64(uint32(toInt32(Value(ctx.Args[2]).Number())))
		return nil
	}
	return rt.typeError("unknown JIT helper")
}

// free releases the code block. A jitCode must outlive every entry into it.
func (c *jitCode) free() {
	if c.block != nil {
		c.block.Free()
		c.block = nil
	}
}
