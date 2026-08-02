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
// Wherever an arithmetic instruction is emitted bare, its operands are known to
// be Numbers already: either they were produced by other arithmetic, or they
// came from a local the prologue checked. Nothing on that path can fail a type
// check, so no guard is needed after entry, and the only way out is the one at
// the top — before a single store has happened.
//
// The prologue checks the parameters the body needs to be Numbers and no others,
// which is what lets an object be a parameter at all: the check is "untagged or
// leave". An unchecked parameter is not trusted, it is simply never handed to an
// instruction that would care — see jitNumberDemand.
//
// That is what makes deoptimisation trivial here. Bailing means the interpreter
// runs the function from the beginning, and there is no state to reconstruct
// because none was changed. Rebuilding an interpreter frame from a half-executed
// compiled one is the hardest part of a JavaScript JIT, and a tier that cannot
// need it is the right place to prove the rest of the machinery.
//
// Where a type is not known — a field's value, most of all — the same
// instructions are emitted behind a guard, with a call out to the runtime's own
// operator for whatever the guard turns away. That is not deoptimisation and
// does not weaken any of the above: the guard runs before the operands have been
// touched, and a helper is a call rather than an exit from the frame. See
// jit_generic_amd64.go.
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
//
// osr maps a loop header's bytecode offset to the address of an entry stub for
// it, which is what lets a loop already running in the interpreter move into
// compiled code without waiting to be called again.
type jitCode struct {
	block *jitmem.Block
	entry uintptr
	osr   map[int]uintptr
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
		// Overwritten by every path that declines for a stated reason. Anything
		// still carrying this got out of here without saying why, which is a
		// gap in the diagnostic rather than a shape of function — the last one
		// hid 31.9 million interpreted instructions in two DeltaBlue functions.
		*why = "unstated-refusal"
	}
	code := fn.code
	ip := fn.startIP

	// The prologue that binds `this` to a local used to be stepped over, because
	// compiled code was handed a frame's locals and the receiver was not among
	// them. It is now, so the prologue is compiled like anything else and a
	// method can be one of the functions this tier takes.
	start := ip

	// Check for an opcode with no template before anything else, so that the
	// refusal names it. The analyses below decline the same functions, but they
	// decline them as "undecodable", which says nothing about what to build next.
	if bad, ok := jitUnsupported(fn, start); !ok {
		return refuse(why, "op:"+bad)
	}

	targets, ok := jitScanTargets(fn, start)
	if !ok {
		return refuse(why, "branch-into-instruction")
	}
	blocks, ok := jitAnalyze(fn, start, targets)
	if !ok {
		return refuse(why, "undecodable")
	}
	// The two analyses below walk the same stack discipline the emitter does,
	// and the only way either fails once every opcode has a template is that the
	// walk itself does not hold: a block reached with operands still on the
	// stack, which they model as empty. Named apart from "undecodable" because
	// that is a bytecode the tier could not read, and this is a bytecode it read
	// perfectly and cannot follow — a different piece of work.
	depths, ok := jitBlockDepths(fn, start, blocks)
	if !ok {
		return refuse(why, "stack-across-blocks")
	}
	demand, ok := jitNumberDemand(fn, blocks, depths)
	if !ok {
		return refuse(why, "stack-across-blocks")
	}
	if fn.jit.unchecked {
		// This function has turned its arguments away often enough to stop
		// betting on them. Demanding nothing means the prologue accepts every
		// frame; the arithmetic that would have relied on a checked parameter
		// emits its guard instead, which is what the generic operators are for.
		for i := range demand {
			demand[i] = false
		}
	}
	numeric, ok := jitNumericLocals(fn, blocks, depths, demand)
	if !ok {
		return refuse(why, "stack-across-blocks")
	}
	// The cache array has to exist before anything is emitted, because a
	// compiled property read addresses its site by absolute address. It is
	// allocated once per function and never grown, which is what makes that
	// address a constant.
	ics := frameICs(fn)

	a := jitasm.NewAsm()
	bail := a.NewLabel()
	prologue := a.NewLabel()
	body := a.NewLabel()
	labels := make(map[int]*jitasm.Label, len(targets))
	for t := range targets {
		labels[t] = a.NewLabel()
	}
	var fixups []jitResumeFixup
	a.Bind(body)

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
	// The locals whose Number-ness the body relies on, and the loop headers it
	// could be entered at. Both are only known once the body has been walked,
	// which is why the entry stubs are emitted after it.
	readsNumeric := make([]bool, fn.maxLocals)
	loopHeaders := map[int]bool{}

	for ip < len(code) {
		if b, isLeader := blocks[ip]; isLeader {
			copy(cur, b.in)
		}
		if l, isTarget := labels[ip]; isTarget {
			// A branch target may be reached with operands still live — `a && b`
			// jumps with one — and jitBlockDepths says how many, having required
			// every predecessor to agree. The register assignment is positional,
			// so operands that arrive at the same depth are already in the
			// registers the code after the label expects and nothing has to move.
			//
			// This is also the check on that prediction: falling through with a
			// depth the analysis did not predict refuses the function rather than
			// emitting code that reads the wrong register.
			want := depths[ip]
			if !returned && sp != want {
				return refuse(why, "stack-at-target")
			}
			sp = want
			// Whatever those operands are, this block cannot know: they came from
			// somewhere else. Not a Number is the only sound answer, and it is
			// what both analyses seed as well.
			for i := 0; i < sp; i++ {
				kind[i] = false
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
			// A String or a regexp constant is a handle, and baking one into
			// code is safe for exactly the reason reading it from the pool is:
			// the pool does not move, and the collector marks fn.constants for
			// as long as fn is reachable — which is longer than the code, since
			// fn owns it. A constant pool belongs to the runtime that built it,
			// and so does the function, so the two can never be crossed.
			a.MovRegImm64(jitStackRegs[sp], uint64(fn.constants[idx]))
			kind[sp] = fn.constants[idx].IsNumber()
			sp++
			ip += 5

		case OpGetLocal:
			i := int(readU16(code, ip+1))
			if i >= fn.maxLocals || sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp], jitRegLocals, int32(i)*8)
			if !cur[i] {
				// Not assigned on every path that reaches here, which is the
				// ordinary shape of a `var` read before its assignment and also
				// the shape of a lexical binding read inside its dead zone. The
				// interpreter tells the two apart with one compare: an empty
				// slot is the dead zone and throws, and anything else is the
				// value, undefined included. So does this.
				//
				// The throw sequence sits in the instruction stream rather than
				// out of line, because a template compiler emits in bytecode
				// order and has nowhere else to put it. It is never executed.
				ok := a.NewLabel()
				a.MovRegImm64(jitRegScratch, uint64(tEmpty))
				a.CmpRegReg(jitStackRegs[sp], jitRegScratch)
				a.Jcc(jitasm.CondNE, ok)
				if !jitCallHelper(a, sp, jitHelperDeadZone, &fixups) {
					return refuse(why, "stack-too-deep")
				}
				a.Bind(ok)
				// Whatever the slot holds, it is not something this tier knows
				// to be a Number — jitNumericLocals seeds such a local
				// non-numeric so that the two agree about everything copied out
				// of it.
				kind[sp] = false
			} else {
				kind[sp] = numeric[i]
				if numeric[i] {
					readsNumeric[i] = true
				}
			}
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
				return refuse(why, "stack-underflow")
			}
			sp--
			ip++

		case OpAdd, OpSub, OpMul, OpDiv:
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			x, y := jitStackRegs[sp-2], jitStackRegs[sp-1]
			// When either operand's type is unknown the same SSE sequence is
			// emitted behind a guard, with a call out to the runtime's operator
			// for everything it turns away — a String to concatenate, an object
			// with a valueOf, a BigInt. See jit_generic_amd64.go.
			generic := !kind[sp-1] || !kind[sp-2]
			var slow, done *jitasm.Label
			if generic {
				slow, done = a.NewLabel(), a.NewLabel()
				jitEmitNumberPair(a, x, y, slow)
			}
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
			if generic {
				a.Jmp(done)
				a.Bind(slow)
				if !jitCallBinary(a, sp, op, jitHelperArith, &fixups) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(x, jitasm.RegCtx, jitmem.CtxOffRet)
				a.Bind(done)
			}
			// Only the guarded path is known to have produced a Number. `+` on
			// two Strings is a String, and `*` on two BigInts is a BigInt.
			kind[sp-2] = !generic
			sp--
			ip++

		case OpLt, OpLe, OpGt, OpGe:
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			x, y := jitStackRegs[sp-2], jitStackRegs[sp-1]
			generic := !kind[sp-1] || !kind[sp-2]
			// A comparison whose result a branch consumes never has to produce
			// the Boolean at all, which is both faster and how this tier managed
			// before it could represent one.
			l, whenTrue, after, fused := jitFuse(code, labels, ip+1)

			var slow, done *jitasm.Label
			if generic {
				slow, done = a.NewLabel(), a.NewLabel()
				jitEmitNumberPair(a, x, y, slow)
			}
			a.MovqXReg(jitasm.X0, x)
			a.MovqXReg(jitasm.X1, y)
			a.UcomisdXX(jitasm.X0, jitasm.X1)
			if fused {
				jitCompareBranch(a, op, whenTrue, l)
			} else {
				jitRelationalValue(a, op, x)
			}
			if generic {
				a.Jmp(done)
				a.Bind(slow)
				if !jitCallBinary(a, sp, op, jitHelperRelational, &fixups) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(x, jitasm.RegCtx, jitmem.CtxOffRet)
				if fused {
					jitBoolBranch(a, whenTrue, x, l)
				}
				a.Bind(done)
			}
			if fused {
				sp -= 2
				if sp != depths[after] {
					return refuse(why, "stack-at-target")
				}
				ip = after
				continue
			}
			kind[sp-2] = false
			sp--
			ip++

		case OpJmp:
			// The depth at the target rather than zero. This was the other half
			// of carrying operands across a branch and it was missed: the label
			// binding learned the depth and the jump to it did not, so
			// `a ? b : c` still refused — and silently, as the catch-all reason,
			// which is why it took splitting that reason to find. Two DeltaBlue
			// functions and 31.9 million interpreted instructions.
			target := int(readU32(code, ip+1))
			l, known := labels[target]
			if !known {
				return refuse(why, "branch-into-instruction")
			}
			if sp != depths[target] {
				return refuse(why, "stack-at-target")
			}
			if target <= ip {
				loopHeaders[target] = true
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
			// Guarded when the operands' types are unknown, exactly as the
			// arithmetic operators are: two compares against the NaN-box
			// threshold, then the same instructions, and on the other side of the
			// guard the call the interpreter makes for this opcode.
			//
			// The arithmetic operators got this and these did not, and it was
			// worth 66.6 million interpreted bytecode instructions in Richards —
			// the largest single item once refusals were weighted by work rather
			// than by frame entries. Richards is bit manipulation over packet
			// queues, and a String or an object operand means ToPrimitive while a
			// BigInt means a different operator entirely.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			x, y := jitStackRegs[sp-2], jitStackRegs[sp-1]
			generic := !kind[sp-1] || !kind[sp-2]
			var slow, done *jitasm.Label
			if generic {
				slow, done = a.NewLabel(), a.NewLabel()
				jitEmitNumberPair(a, x, y, slow)
			}
			if !jitBitwise(a, op, x, y, sp, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			if generic {
				a.Jmp(done)
				a.Bind(slow)
				if !jitCallBinary(a, sp, op, jitHelperBitwise, &fixups) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(x, jitasm.RegCtx, jitmem.CtxOffRet)
				a.Bind(done)
			}
			// A bitwise operator produces an integer either way, so the result is
			// a Number whichever side of the guard it came from.
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
			// comparison, so all four differ only in polarity. Between anything
			// else they are two quite different operators, which is what the
			// runtime is asked for when the guard turns the operands away.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			x, y := jitStackRegs[sp-2], jitStackRegs[sp-1]
			generic := !kind[sp-1] || !kind[sp-2]
			negate := op == OpNe || op == OpSne
			l, whenTrue, after, fused := jitFuse(code, labels, ip+1)

			var slow, done *jitasm.Label
			if generic {
				slow, done = a.NewLabel(), a.NewLabel()
				jitEmitNumberPair(a, x, y, slow)
			}
			a.MovqXReg(jitasm.X0, x)
			a.MovqXReg(jitasm.X1, y)
			a.UcomisdXX(jitasm.X0, jitasm.X1)
			if fused {
				jitEqualsBranch(a, negate, whenTrue, l)
			} else {
				// Not consumed by a branch, so the Boolean has to exist.
				// Emitting it is what lets `a === b` be stored or returned.
				jitEqualsValue(a, negate, x)
			}
			if generic {
				a.Jmp(done)
				a.Bind(slow)
				if !jitCallBinary(a, sp, op, jitHelperEquals, &fixups) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(x, jitasm.RegCtx, jitmem.CtxOffRet)
				if fused {
					jitBoolBranch(a, whenTrue, x, l)
				}
				a.Bind(done)
			}
			if fused {
				sp -= 2
				if sp != depths[after] {
					return refuse(why, "stack-at-target")
				}
				ip = after
				continue
			}
			kind[sp-2] = false
			sp--
			ip++

		case OpGetField:
			// The cache probe is emitted; everything it declines is the
			// runtime's — a prototype walk, an accessor, a Proxy trap, an
			// unparsed JSON span, or simply a shape this site has not seen.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			idx := readU32(code, ip+1)
			if int(idx) >= len(fn.constNames) || idx > 0x7FFFFFFF {
				return refuse(why, "shape")
			}
			if isPrivateKey(fn.constNames[idx]) {
				// GET_FIELD also carries private names, which are not
				// properties: they resolve against the class environment the
				// frame carries, which compiled code does not have. Refusing is
				// what keeps `other.#x` in a method out of getField, where the
				// mangled name would find nothing.
				return refuse(why, "private-name")
			}
			icx := int(readU16(code, ip+5))
			recv := jitStackRegs[sp-1]
			slow := a.NewLabel()
			done := a.NewLabel()
			if icx != icNoSlot && icx < len(ics) && sp+jitICGetSpareRegs <= len(jitStackRegs) {
				jitEmitICGet(a, recv, jitStackRegs[sp], jitStackRegs[sp+1], jitStackRegs[sp+2],
					jitICWayAddr(ics, icx), jitEpochAddr(), jitICHitAddr(), slow, done)
			}
			a.Bind(slow)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+16, recv)
			// The name and the cache site, both constants for this site, packed
			// into the one argument slot the helper protocol leaves untraced.
			a.MovRegImm64(jitRegScratch, uint64(idx)|uint64(uint32(icx))<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperGetField, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(recv, jitasm.RegCtx, jitmem.CtxOffRet)
			a.Bind(done)
			// A field holds anything, so the result is not a Number as far as
			// this tier knows. Arithmetic on it is refused rather than guarded.
			kind[sp-1] = false
			ip += 7

		case OpGetField2:
			// `obj -> obj val`: the same probe as GET_FIELD, but the receiver
			// stays for the CALL_METHOD that follows. This is what `o.m()`
			// compiles to, and between them the pair is 2.83M interpreted frame
			// entries in DeltaBlue and 1.87M in Richards.
			//
			// The probe writes its answer over the register it was given, so the
			// receiver is copied up first and the copy is what gets probed. That
			// also leaves the slow path holding the receiver where the helper
			// wants it, which is why this can reuse GET_FIELD's helper unchanged.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			idx := readU32(code, ip+1)
			if int(idx) >= len(fn.constNames) || idx > 0x7FFFFFFF {
				return refuse(why, "shape")
			}
			if isPrivateKey(fn.constNames[idx]) {
				return refuse(why, "private-name")
			}
			icx := int(readU16(code, ip+5))
			dst := jitStackRegs[sp]
			a.MovRegReg(dst, jitStackRegs[sp-1])
			slow := a.NewLabel()
			done := a.NewLabel()
			if icx != icNoSlot && icx < len(ics) && sp+jitICGlobalSpareRegs <= len(jitStackRegs) {
				jitEmitICGet(a, dst, jitStackRegs[sp+1], jitStackRegs[sp+2], jitStackRegs[sp+3],
					jitICWayAddr(ics, icx), jitEpochAddr(), jitICHitAddr(), slow, done)
			}
			a.Bind(slow)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+16, dst)
			a.MovRegImm64(jitRegScratch, uint64(idx)|uint64(uint32(icx))<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperGetField, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(dst, jitasm.RegCtx, jitmem.CtxOffRet)
			a.Bind(done)
			kind[sp] = false
			sp++
			ip += 7

		case OpCallMethod:
			// CALL with a receiver: [this, callee, arg0 .. argN-1].
			argc := int(readU16(code, ip+1))
			if argc < 0 || sp < argc+2 {
				return refuse(why, "stack-underflow")
			}
			a.MovRegImm64(jitRegScratch, uint64(uint32(argc)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperCallMethod, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			dst := jitStackRegs[sp-argc-2]
			a.MovRegMem(dst, jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-argc-2] = false
			sp -= argc + 1
			ip += 3

		case OpCall:
			// The operands are already where the helper wants them. A call site
			// holds [callee, arg0 .. argN-1] on the operand stack, spilling
			// roots the whole stack anyway, and SpillN says how much of it is
			// live — so nothing has to be passed but the count.
			//
			// This is the exit-and-re-enter detour rather than a compiled
			// function calling a compiled function, which is measured at 4.69 ns
			// against 1.15. It is here first because it is what lets a function
			// that calls anything compile at all: until now the whole body was
			// refused, which is why GET_FIELD2 was worth zero frame entries in
			// the weighted table — CALL was always the instruction after it.
			argc := int(readU16(code, ip+1))
			if argc < 0 || sp < argc+1 {
				return refuse(why, "stack-underflow")
			}
			a.MovRegImm64(jitRegScratch, uint64(uint32(argc)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperCall, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			// The result replaces the callee, which is the deepest of the slots
			// this consumed.
			dst := jitStackRegs[sp-argc-1]
			a.MovRegMem(dst, jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-argc-1] = false
			sp -= argc
			ip += 3

		case OpGetElem:
			// `a[i]`, with no cache site: an array's elements are a slice of
			// their own, so what is emitted is a guard chain rather than a probe.
			// See jit_getelem_amd64.go.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			recv, key := jitStackRegs[sp-2], jitStackRegs[sp-1]
			slow := a.NewLabel()
			done := a.NewLabel()
			if sp+jitICElemSpareRegs <= len(jitStackRegs) {
				jitEmitGetElem(a, recv, key, jitStackRegs[sp], jitStackRegs[sp+1], slow, done)
			}
			a.Bind(slow)
			if !jitCallHelper(a, sp, jitHelperGetElem, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(recv, jitasm.RegCtx, jitmem.CtxOffRet)
			a.Bind(done)
			kind[sp-2] = false
			sp--
			ip++

		case OpGetUpval:
			// A closed-over variable: the closure's upvalue array, the upvalue,
			// the location it points at, and the value there. Four dependent
			// loads and no guard chain — much less work than the property cache
			// this tier already emits, and it is what NavierStokes is refused
			// for. That benchmark compiles 0.4% of its frame entries, so all it
			// gets from the tier today is the tiering check.
			//
			// The index is checked here rather than emitted, because the count
			// is known: a function's upvalDescs say how many it captures, and
			// every closure over it is built with exactly that many.
			idx := int(readU16(code, ip+1))
			if idx >= len(fn.upvalDescs) {
				return refuse(why, "shape")
			}
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			dst := jitStackRegs[sp]
			a.MovRegMem(dst, jitasm.RegCtx, jitmem.CtxOffUpvals)
			a.MovRegMem(dst, dst, int32(idx)*8)
			a.MovRegMem(dst, dst, int32(jitOffUpvalLocation))
			a.MovRegMem(dst, dst, 0)
			// A captured binding has a dead zone of its own, and the sentinel
			// for it is the same one a local carries.
			ok := a.NewLabel()
			a.MovRegImm64(jitRegScratch, uint64(tEmpty))
			a.CmpRegReg(dst, jitRegScratch)
			a.Jcc(jitasm.CondNE, ok)
			if !jitCallHelper(a, sp, jitHelperDeadZone, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.Bind(ok)
			kind[sp] = false
			sp++
			ip += 3

		case OpThis:
			// The receiver, from the context rather than the locals, because it
			// is the one thing a frame carries that is neither a local nor an
			// operand. The interpreter has already resolved it — coerced to an
			// object in sloppy mode, left alone in strict — so there is nothing
			// to do here but read it.
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp], jitasm.RegCtx, jitmem.CtxOffThis)
			kind[sp] = false
			sp++
			ip++

		case OpGetGlobal:
			// The same cache again, over a receiver compiled code fetches
			// instead of one it was handed. A top-level function or constant is
			// read this way in every loop that uses it, and the measurement in
			// jitrefusal.go says this one opcode is the only thing standing
			// between the tier and 41% of richards and 27% of deltablue.
			//
			// The lexical record — a Script-level let/const shadowing a global
			// property — is not part of the object's shape, so it cannot be
			// checked here. It does not have to be: registering one bumps the
			// cache epoch, which is the guard already emitted.
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			idx := readU32(code, ip+1)
			if int(idx) >= len(fn.constNames) || idx > 0x7FFFFFFF {
				return refuse(why, "shape")
			}
			icx := int(readU16(code, ip+5))
			dst := jitStackRegs[sp]
			slow := a.NewLabel()
			done := a.NewLabel()
			if icx != icNoSlot && icx < len(ics) && sp+jitICGlobalSpareRegs <= len(jitStackRegs) {
				a.MovRegMem(jitRegScratch, jitasm.RegCtx, jitmem.CtxOffHost)
				a.MovRegMem(dst, jitRegScratch, int32(jitOffRTGlobal))
				jitEmitICGet(a, dst, jitStackRegs[sp+1], jitStackRegs[sp+2], jitStackRegs[sp+3],
					jitICWayAddr(ics, icx), jitEpochAddr(), jitICGlobalHitAddr(), slow, done)
			}
			a.Bind(slow)
			a.MovRegImm64(jitRegScratch, uint64(idx)|uint64(uint32(icx))<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperGetGlobal, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(dst, jitasm.RegCtx, jitmem.CtxOffRet)
			a.Bind(done)
			kind[sp] = false
			sp++
			ip += 7

		case OpPutField:
			// The store side of the same cache, declining the same things and
			// two of its own: a receiver whose property this store would create,
			// and one whose slot lives past the inline area.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			idx := readU32(code, ip+1)
			if int(idx) >= len(fn.constNames) || idx > 0x7FFFFFFF {
				return refuse(why, "shape")
			}
			if isPrivateKey(fn.constNames[idx]) {
				return refuse(why, "private-name")
			}
			icx := int(readU16(code, ip+5))
			recv, val := jitStackRegs[sp-2], jitStackRegs[sp-1]
			slow := a.NewLabel()
			done := a.NewLabel()
			if icx != icNoSlot && icx < len(ics) && sp+jitICPutSpareRegs <= len(jitStackRegs) {
				jitEmitICPut(a, recv, val, jitStackRegs[sp], jitStackRegs[sp+1],
					jitICWayAddr(ics, icx), jitEpochAddr(), slow, done)
			}
			a.Bind(slow)
			// Only the name and the site go in an argument. The receiver and the
			// value are already where the helper wants them: spilling roots the
			// whole operand stack, and these are the top two of it.
			a.MovRegImm64(jitRegScratch, uint64(idx)|uint64(uint32(icx))<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperPutField, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.Bind(done)
			sp -= 2
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
			// Only the top of the stack matters: a return leaves the frame, so
			// whatever is under the value being returned goes with it. Requiring
			// a depth of exactly one refused `return a ? b : c`, where the
			// ternary's operand is still live below the result.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffRet, jitStackRegs[sp-1])
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

	// The prologue, emitted after the body because which parameters it has to
	// check is not known until the body has been walked.
	//
	// Only the parameters the body does arithmetic on. That is not a saving; it
	// is what lets an object be a parameter at all. The check is "untagged or
	// leave", so a checked parameter can never be an object, a String, or a
	// binding still in its temporal dead zone — and for as long as every
	// parameter was checked, the property read below could only ever be handed a
	// primitive, which made a cache for it unreachable rather than merely
	// unwritten.
	//
	// An unchecked parameter is not thereby trusted: jitNumberDemand and
	// jitNumericLocals agree that it is not a Number, so the templates that need
	// one refuse the function instead.
	a.Bind(prologue)
	a.MovRegMem(jitRegLocals, jitasm.RegCtx, jitmem.CtxOffArgs)
	a.MovRegImm64(jitRegGuard, uint64(nanboxPrefix))
	for i := 0; i < fn.paramCount && i < fn.maxLocals; i++ {
		if !readsNumeric[i] {
			continue
		}
		a.MovRegMem(jitasm.RAX, jitRegLocals, int32(i)*8)
		a.CmpRegReg(jitasm.RAX, jitRegGuard)
		a.Jcc(jitasm.CondA, bail)
	}
	a.Jmp(body)

	// One entry stub per loop header, so that a loop already spinning in the
	// interpreter can move into compiled code without being called again.
	//
	// The ordinary entry checks the parameters because everything else is a
	// value the body itself produced. Entering at a header inherits locals the
	// interpreter wrote instead, so the stub checks every local the body relies
	// on being a Number. A local it only stores to needs no check, and one it
	// reads without doing arithmetic on has none to fail.
	//
	// Jumping straight to the header is sound for the same reason the fuel exit
	// is: a back edge is reached with the operand stack empty and every live
	// value in the locals array, so there is nothing else to restore. The
	// definite-assignment set at the header holds too, because the interpreter
	// got there through the same control-flow graph.
	osrLabels := make(map[int]*jitasm.Label, len(loopHeaders))
	for h := range loopHeaders {
		l, ok := labels[h]
		if !ok {
			continue
		}
		stub := a.NewLabel()
		a.Bind(stub)
		a.MovRegMem(jitRegLocals, jitasm.RegCtx, jitmem.CtxOffArgs)
		a.MovRegImm64(jitRegGuard, uint64(nanboxPrefix))
		for i := 0; i < fn.maxLocals; i++ {
			if !readsNumeric[i] || !blocks[h].in[i] {
				continue
			}
			a.MovRegMem(jitasm.RAX, jitRegLocals, int32(i)*8)
			a.CmpRegReg(jitasm.RAX, jitRegGuard)
			a.Jcc(jitasm.CondA, bail)
		}
		a.Jmp(l)
		osrLabels[h] = stub
	}

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
		return refuse(why, "no-executable-memory")
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
	c := &jitCode{block: block, entry: block.AddrAt(prologue.Offset())}
	if len(osrLabels) > 0 {
		c.osr = make(map[int]uintptr, len(osrLabels))
		for h, l := range osrLabels {
			c.osr[h] = block.AddrAt(l.Offset())
		}
	}
	return c
}

// jitHasTemplate reports whether the emitter can compile this opcode at all.
//
// The emitter can still refuse an opcode it has a template for — a stack too
// deep for the registers, an operand whose type it does not know — so this is a
// necessary condition rather than a sufficient one.
func jitHasTemplate(op Opcode) bool {
	switch op {
	case OpConst, OpConstI8, OpUndef, OpNull, OpTrue, OpFalse,
		OpGetLocal, OpPutLocal, OpSetLocal, OpPop, OpDup, OpInsert2,
		OpAdd, OpSub, OpMul, OpDiv,
		OpBand, OpBor, OpBxor, OpShl, OpShr, OpUshr, OpBnot,
		OpNeg, OpInc, OpDec, OpNot,
		OpLt, OpLe, OpGt, OpGe, OpEq, OpNe, OpSeq, OpSne,
		OpJmp, OpJmpFalse, OpJmpTrue, OpGetField, OpGetField2, OpPutField,
		OpGetGlobal, OpGetUpval, OpGetElem, OpCall, OpCallMethod,
		OpReturn, OpReturnUndef, OpThis:
		return true
	}
	return false
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
		if !jitHasTemplate(op) {
			return opTable[op].Name, false
		}
		ip += size
	}
	return "", true
}

// jitMissingTemplates lists the distinct opcodes in fn's body the emitter has no
// template for, capped at two — the diagnostic that uses it only asks whether
// there is exactly one, so counting past that is work for an answer nobody
// reads.
func jitMissingTemplates(fn *svFunc) []string {
	code := fn.code
	var out []string
	for ip := fn.startIP; ip < len(code) && len(out) < 2; {
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		if size <= 0 || ip+size > len(code) {
			return []string{"undecodable", "undecodable"}
		}
		if !jitHasTemplate(op) {
			name := opTable[op].Name
			if len(out) == 0 || out[0] != name {
				out = append(out, name)
			}
		}
		ip += size
	}
	return out
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
	jitHelperGetField   = 1
	jitHelperToInt32    = 2
	jitHelperArith      = 3
	jitHelperRelational = 4
	jitHelperEquals     = 5
	jitHelperPutField   = 6
	jitHelperGetGlobal  = 7
	jitHelperDeadZone   = 8
	jitHelperCall       = 9
	jitHelperCallMethod = 10
	jitHelperGetElem    = 11
	jitHelperBitwise    = 12
)

// jitICGlobalSpareRegs is how many operand-stack registers a global read needs:
// one for the value it produces, which starts out holding the global object,
// and two for the probe.
const jitICGlobalSpareRegs = 4

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
func (c *jitCode) jitRun(rt *Runtime, fn *svFunc, cl *closure, locals []Value, this Value) (Value, *ThrowError, bool) {
	return c.jitRunAt(rt, fn, cl, locals, this, c.entry)
}

// jitRunOSR enters at the stub for a loop header, if there is one.
//
// Reports false when there is not, or when the stub's guards decline the locals
// the interpreter has produced — in which case nothing has happened and the
// interpreter carries on from where it was.
func (c *jitCode) jitRunOSR(rt *Runtime, fn *svFunc, cl *closure, locals []Value, this Value, header int) (Value, *ThrowError, bool) {
	pc, ok := c.osr[header]
	if !ok {
		return mkundef(), nil, false
	}
	return c.jitRunAt(rt, fn, cl, locals, this, pc)
}

func (c *jitCode) jitRunAt(rt *Runtime, fn *svFunc, cl *closure, locals []Value, this Value, entry uintptr) (Value, *ThrowError, bool) {
	if len(locals) == 0 {
		return mkundef(), nil, false
	}
	// The context is rooted for as long as compiled code can be suspended in a
	// helper holding values nothing else refers to — see markRoots — and that
	// root stack is also where it comes from.
	//
	// Building one per entry cost an allocation of 160 bytes per call, which for
	// a callee small enough to be worth compiling is most of what compiling it
	// saved. The stack is LIFO and a context is dead the moment it is popped, so
	// the slice past its length is a free list that needs no bookkeeping: the
	// only care required is that a reused context starts clear, because the
	// collector reads Ret and the spill slots and the previous call's are stale.
	n := len(rt.jitFrames)
	var ctx *jitmem.ExecContext
	if n < cap(rt.jitFrames) {
		ctx = rt.jitFrames[:n+1][n] // popped by an earlier call, still allocated
	}
	if ctx == nil {
		ctx = new(jitmem.ExecContext)
	}
	// Written field by field rather than as a struct literal, and this is worth
	// the ugliness: a literal clears the whole context, and eighty of its bytes
	// are the spill area. That memset was the largest single item in the profile
	// of entering a compiled function once the interpreted frame around it was
	// gone.
	//
	// Leaving the spill slots stale is sound for the same reason SpillN exists at
	// all. The collector reads Spill[0:SpillN] and nothing more, SpillN is zero
	// here, and the only thing that raises it is compiled code writing the slots
	// immediately before it exits. A stale slot is therefore never read, and the
	// alternative — clearing ten words on every call so that nobody looks at them
	// — is the cost this pays to avoid.
	//
	// Everything the collector *does* read unconditionally is written: Args[2],
	// Ret and This. Cleared before the context is published rather than after, so
	// the root stack never holds an entry with a previous call's values in it.
	ctx.Exit, ctx.Helper = 0, 0
	ctx.Args[0] = uint64(uintptr(unsafe.Pointer(&locals[0])))
	ctx.Args[1] = jitFuel
	ctx.Args[2], ctx.Args[3] = 0, 0
	ctx.Ret, ctx.Resume = 0, 0
	ctx.SpillN = 0
	ctx.Pool = jitObjectPoolAddr(rt)
	ctx.Host = jitRuntimeAddr(rt)
	ctx.This = uint64(this)
	// The closure's upvalue array, or zero when there is none. A function with
	// no GET_UPVAL never reads it, and one with a GET_UPVAL is only ever
	// entered through a closure that has the array the index was compiled
	// against.
	ctx.Upvals = 0
	if cl != nil && len(cl.upvalues) > 0 {
		ctx.Upvals = uintptr(unsafe.Pointer(&cl.upvalues[0]))
	}
	if n < cap(rt.jitFrames) {
		rt.jitFrames = rt.jitFrames[:n+1]
		rt.jitFrames[n] = ctx
	} else {
		rt.jitFrames = append(rt.jitFrames, ctx)
	}
	defer func() { rt.jitFrames = rt.jitFrames[:n] }()

	pc := entry
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
		recv := Value(ctx.Args[2])
		// The cache first, for the same reason as the store: what compiled code
		// emits is narrower than what the cache can answer. A receiver that is
		// not a plain object, a slot past the end of the overflow slice, a site
		// with no spare registers — all of them arrive here, and all of them are
		// reads the cache can serve without a lookup.
		if icx := uint32(ctx.Args[3] >> 32); icx != icNoSlot {
			if ics := frameICs(fn); int(icx) < len(ics) {
				if o := rt.icReceiver(recv); o != nil {
					if v, ok := rt.icCachedRead(&ics[icx], o); ok {
						if jitStats.enabled {
							jitStats.icMiss++
						}
						ctx.Ret = uint64(v)
						return nil
					}
				}
			}
		}
		name := fn.constNames[uint32(ctx.Args[3])]
		v, e := rt.getField(recv, name)
		if e != nil {
			return e
		}
		// Filling the cache from here is what lets compiled code warm its own
		// sites. A function tiered up on its call count has been interpreted
		// often enough that its caches are already full, but one entered at a
		// loop header may never have run this site at all — and a site nothing
		// ever fills is a probe that misses forever.
		if icx := uint32(ctx.Args[3] >> 32); icx != icNoSlot {
			if ics := frameICs(fn); int(icx) < len(ics) && !ics[icx].dead() {
				rt.icFillGet(&ics[icx], rt.icReceiver(recv), name)
			}
		}
		if jitStats.enabled {
			jitStats.icMiss++
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperCall, jitHelperCallMethod:
		// The interpreter's CALL and CALL_METHOD, reading their operands out of
		// the spill area. The two differ only in whether a receiver sits below
		// the callee.
		//
		// The arguments are copied into a slice of their own rather than handed
		// over as a window onto Spill. A sloppy callee's mapped `arguments`
		// object writes through to the array it was given, and that array must
		// not be this frame's operand stack. The interpreter allocates the same
		// slice for the same reason, so this is parity rather than a cost.
		argc := int(uint32(ctx.Args[3]))
		depth := argc + 1
		if ctx.Helper == jitHelperCallMethod {
			depth++
		}
		n := int(ctx.SpillN)
		if argc < 0 || n < depth {
			return rt.typeError("JIT operand stack")
		}
		base := n - depth
		thisArg := mkundef()
		if ctx.Helper == jitHelperCallMethod {
			thisArg = Value(ctx.Spill[base])
			base++
		}
		callee := Value(ctx.Spill[base])
		args := make([]Value, argc)
		for i := 0; i < argc; i++ {
			args[i] = Value(ctx.Spill[base+1+i])
		}
		v, e := rt.callValue(callee, thisArg, args)
		if e != nil {
			return e
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperGetElem:
		// The interpreter's GET_ELEM. Its operands are the top two of the spill
		// area, like every other multi-operand helper here.
		n := int(ctx.SpillN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		v, e := rt.getElement(Value(ctx.Spill[n-2]), Value(ctx.Spill[n-1]))
		if e != nil {
			return e
		}
		if jitStats.enabled {
			jitStats.elemMiss++
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperDeadZone:
		// Reached only when a local held the empty sentinel, which is a lexical
		// binding read before its initialiser ran. The interpreter's message,
		// because the two paths must be indistinguishable from a script.
		return rt.referenceError("Cannot access a lexical binding before initialization")
	case jitHelperGetGlobal:
		// The interpreter's GET_GLOBAL with its cache fast path already tried
		// and declined: the declarative record first, then HasProperty, then
		// [[Get]]. The order is the specified one and not an optimisation — a
		// Script-level let shadows a global property of the same name, and a
		// bare undeclared name is a ReferenceError rather than undefined.
		name := fn.constNames[uint32(ctx.Args[3])]
		if b := rt.lookupGlobalLex(name); b != nil {
			v, e := rt.globalLexRead(b, name)
			if e != nil {
				return e
			}
			ctx.Ret = uint64(v)
			return nil
		}
		if !rt.hasProp(rt.global, name) {
			return rt.referenceError(name + " is not defined")
		}
		// Through [[Get]] rather than the slot, because a global bound as an
		// accessor has to run its getter.
		v, e := rt.getField(rt.global, name)
		if e != nil {
			return e
		}
		if icx := uint32(ctx.Args[3] >> 32); icx != icNoSlot {
			if ics := frameICs(fn); int(icx) < len(ics) && !ics[icx].dead() {
				rt.icFillGet(&ics[icx], rt.objPtr(rt.global), name)
			}
		}
		if jitStats.enabled {
			jitStats.glbMiss++
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperPutField:
		// The interpreter's PUT_FIELD with its cache fast path already tried and
		// declined, which is exactly what the compiled probe reaching here means.
		//
		// The operands come out of the spill area rather than an argument slot.
		// They have to be there anyway — they are the top of the operand stack,
		// and the collector traces it while this runs — so passing them again
		// would be two stores to say what SpillN already says.
		n := int(ctx.SpillN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		obj, val := Value(ctx.Spill[n-2]), Value(ctx.Spill[n-1])
		// The cache first, because the emitted probe serves strictly less than
		// the cache does. It declines the store that *creates* a property, and
		// that is what building an object is made of: EarleyBoyer makes 18.75
		// million stores and the probe served 0.8% of them, so the rest arrived
		// here and paid a full OrdinarySet for what the interpreter was doing
		// with a shape install and a slot write.
		if icx := uint32(ctx.Args[3] >> 32); icx != icNoSlot {
			if ics := frameICs(fn); int(icx) < len(ics) {
				if o := rt.icReceiver(obj); o != nil && rt.icCachedStore(&ics[icx], obj, o, val) {
					if jitStats.enabled {
						jitStats.putMiss++
					}
					return nil
				}
			}
		}
		name := fn.constNames[uint32(ctx.Args[3])]
		// The shape before the store, which is what tells the fill below whether
		// the store created the property. Read before, because after it is gone.
		var preShape *shape
		icx := uint32(ctx.Args[3] >> 32)
		recv := rt.icReceiver(obj)
		if icx != icNoSlot && recv != nil {
			preShape = recv.shape
		}
		ok, e := rt.setFieldR(obj, name, val)
		if e != nil {
			return e
		}
		if !ok && fn.isStrict {
			return rt.typeError("Cannot assign to read only property '" + name + "'")
		}
		if icx != icNoSlot && ok {
			if ics := frameICs(fn); int(icx) < len(ics) && !ics[icx].dead() {
				// icReceiver again: setFieldR may have been handed something
				// that was not an object at all, and a store that reached a
				// prototype's setter can have replaced what obj resolves to.
				o := rt.icReceiver(obj)
				if o != nil && preShape != nil && o.shape != preShape {
					rt.icFillPutTransition(&ics[icx], o, preShape, name)
				} else {
					rt.icFillPut(&ics[icx], o, name)
				}
			}
		}
		if jitStats.enabled {
			jitStats.putMiss++
		}
		return nil
	case jitHelperToInt32:
		// Reached only for a finite magnitude of 2^63 or more, which CVTTSD2SI
		// cannot report. Zero-extended, because compiled code keeps every
		// intermediate integer that way — see jitToInt32.
		ctx.Ret = uint64(uint32(toInt32(Value(ctx.Args[2]).Number())))
		return nil

	// The three below are what an operator does when its operands turned out not
	// to be Numbers. Each is the call the interpreter makes for the same opcode,
	// which is what keeps a compiled `+` and an interpreted one from disagreeing
	// about a Date, a Symbol, or a String that looks like a number.
	case jitHelperArith:
		op, x, y, ok := jitBinaryOperands(ctx)
		if !ok {
			return rt.typeError("JIT operand stack")
		}
		if jitStats.enabled {
			jitStats.genSlow++
		}
		var v Value
		var e *ThrowError
		if op == OpAdd {
			v, e = rt.jsAdd(x, y)
		} else {
			v, e = rt.jsArith(op, x, y)
		}
		if e != nil {
			return e
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperBitwise:
		op, x, y, ok := jitBinaryOperands(ctx)
		if !ok {
			return rt.typeError("JIT operand stack")
		}
		if jitStats.enabled {
			jitStats.genSlow++
		}
		v, e := rt.jsBitwise(op, x, y)
		if e != nil {
			return e
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperRelational:
		op, x, y, ok := jitBinaryOperands(ctx)
		if !ok {
			return rt.typeError("JIT operand stack")
		}
		if jitStats.enabled {
			jitStats.genSlow++
		}
		v, e := rt.jsRelational(op, x, y)
		if e != nil {
			return e
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperEquals:
		op, x, y, ok := jitBinaryOperands(ctx)
		if !ok {
			return rt.typeError("JIT operand stack")
		}
		if jitStats.enabled {
			jitStats.genSlow++
		}
		switch op {
		case OpSeq, OpSne:
			// Strict equality coerces nothing, so it cannot throw and needs no
			// error path of its own.
			ctx.Ret = uint64(mkbool(rt.strictEquals(x, y) == (op == OpSeq)))
			return nil
		}
		r, e := rt.abstractEquals(x, y)
		if e != nil {
			return e
		}
		ctx.Ret = uint64(mkbool(r == (op == OpEq)))
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
