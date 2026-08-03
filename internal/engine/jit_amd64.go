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
		return refuse(why, "stack-across-blocks/depths")
	}
	demand, ok := jitNumberDemand(fn, blocks, depths)
	if !ok {
		return refuse(why, "stack-across-blocks/demand")
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
		return refuse(why, "stack-across-blocks/numeric")
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
	// The stores that seed a dead zone rather than assign a value. See
	// jitTDZStores: the analyses use the same set, so what this elides a
	// dead-zone check for and what they call a proven read are the same reads.
	tdz := jitTDZStores(fn)
	// kind[i] records whether operand-stack slot i is known to hold a Number.
	// Arithmetic needs that guarantee; storing and returning do not, which is
	// what lets undefined, null and the Booleans travel through compiled code
	// without any of it having to guard.
	kind := make([]bool, len(jitStackRegs)+1)
	// The previous instruction, maintained across the loop below.
	prevOp, prevIP := Opcode(0), -1
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
			// Unreachable: control cannot fall in here and no label was bound
			// above. Skip the instruction and keep walking rather than stop —
			// stopping left every branch target beyond this point unbound, which
			// then refused the whole function as `unreachable-target`, and that
			// was 12.6 million interpreted instructions in Richards. A `return`
			// in one arm of a conditional is enough to produce it.
			size := int(opTable[Opcode(code[ip])].Size)
			if size <= 0 {
				return refuse(why, "undecodable")
			}
			ip += size
			continue
		}

		// The instruction before this one, for the templates that can answer
		// exactly when their operand is a literal — see jitSingletonRHS. Recorded
		// rather than derived, because the emitter advances ip by each opcode's
		// own size and there is no walking backwards through that.
		thisIP, thisOp := ip, Opcode(code[ip])

		switch op := Opcode(code[ip]); op {
		case OpConstI8:
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegImm64(jitStackRegs[sp], uint64(tov(float64(int8(code[ip+1])))))
			kind[sp] = true
			sp++
			ip += 2

		case OpUndef, OpNull, OpTrue, OpFalse, OpEmpty:
			// Immediates with no heap reference, so nothing has to stay alive
			// for the code to remain valid. A String or object constant would,
			// which is why OpConst still refuses one.
			//
			// EMPTY is the temporal-dead-zone sentinel rather than a language
			// value: it is pushed only to be stored into a lexical slot that a
			// later initialiser overwrites, and reading such a slot already
			// throws through the dead-zone check on GET_LOCAL.
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
			case OpEmpty:
				v = tEmpty
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
			// A store makes the slot readable from here on — unless what it
			// stores is the dead-zone sentinel, which puts the slot back into
			// the state every read of it has to throw on.
			cur[i] = !tdz[ip]
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
			// An unconditional jump ends the block: what follows is reachable
			// only through a label, so the operand depth here says nothing about
			// it. Leaving this unset carried a stale depth into the next label
			// and refused `a ? b : c` as a target mismatch.
			returned = true
			ip += 5

		case OpJmpFalse, OpJmpTrue:
			// Reached only when the branch was not fused with a comparison: the
			// fused form is emitted from the comparison's own case, which
			// consumes both together. This is the other one — `if (x)`,
			// `a && b`, `a ? b : c` — where the condition is a value and the
			// branch has to ask whether it is true. See jit_truthy_amd64.go.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			target := int(readU32(code, ip+1))
			l, known := labels[target]
			if !known {
				return refuse(why, "branch-into-instruction")
			}
			if sp-1 != depths[target] {
				return refuse(why, "stack-at-target")
			}
			back := target <= ip
			if !jitEmitTruthyBranch(a, jitStackRegs[sp-1], op == OpJmpTrue, back, l, sp, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			if back {
				loopHeaders[target] = true
			}
			sp--
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

		case OpThrowError:
			// Never returns, like THROW. The name index and the kind travel
			// together in the one immediate the exit protocol carries.
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1))|uint64(code[ip+5])<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperThrowError, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			sp = 0
			returned = true
			ip += 6

		case OpUplus:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperUplus, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-1], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-1] = false
			ip++

		case OpToPropkey:
			// ToPropertyKey, which template substitution emits so `${obj}` takes
			// the string path. It can run a toString, so it is a call.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperToPropkey, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-1], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-1] = false
			ip++

		case OpGlobal:
			// The global object itself. Read through a helper rather than baked in
			// as an immediate: compiled code hangs off the svFunc, and the same
			// function can be reached from more than one realm.
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			if !jitCallHelper(a, sp, jitHelperGlobal, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp] = false
			sp++
			ip++

		case OpDeleteVar:
			// Strict-mode ResolveBinding as much as `delete x`: it answers whether
			// the name is bound, and the OpPutConst after it throws if it was not.
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperDeleteVar, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp] = false
			sp++
			ip += 5

		case OpPutConst:
			// DELETE_VAR's partner: [resolvable, value] -> [value]. It throws
			// when the name did not resolve, and again when the right-hand side
			// deleted the global out from under the reference.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperPutConst, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-2], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-2] = false
			sp--
			ip += 5

		case OpGetLength:
			// `.length` without a name index, which the for-in and for-of loops
			// emit to size their key array. A plain getField: an array's length
			// is a real property here rather than a slot the emitter could read
			// out of the object header.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperGetLength, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-1], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-1] = false
			ip++

		case OpForIn:
			// The head of a for-in loop: the source is replaced by the keys to
			// walk. A call, because a proxy's ownKeys trap runs here.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperForIn, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-1], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-1] = false
			ip++

		case OpHasPrivate:
			// The for-in re-validation check rather than `#x in o`, which is
			// what IN compiles: [key, obj] -> whether the property is still
			// there and still enumerable. The loop re-asks before every
			// iteration, because the body is free to delete what it is walking.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperStillEnum, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-2], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-2] = false
			sp--
			ip++

		case OpSetHomeObj:
			// [obj, method] unchanged: the method's [[HomeObject]] becomes obj,
			// so a `super` reference inside it resolves against obj's prototype.
			// Nothing moves on the stack and nothing can throw.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperSetHomeObj, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			ip++

		case OpDefineMethod:
			// [target, method] -> [target]. The name index and the flags byte
			// travel in one immediate, as THROW_ERROR's pair does.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1))|uint64(code[ip+5])<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperDefineMethod, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			sp--
			ip += 6

		case OpClosure:
			// Making a function, which is what 583 of the corpus's remaining
			// refusals were waiting for — the largest single blocker once the
			// self-reference was in.
			//
			// A child that captures a `with` scope is refused: the chain comes
			// off the enclosing frame's withStack, and a compiled frame has none.
			// Refusing by name rather than as "op:CLOSURE" so the diagnostic does
			// not point at an opcode that is already implemented.
			idx := readU32(code, ip+1)
			if int(idx) >= len(fn.childFuncs) {
				return refuse(why, "closure-index")
			}
			if fn.childFuncs[idx].capturesWith {
				return refuse(why, "closure/captures-with")
			}
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegImm64(jitRegScratch, uint64(idx))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperClosure, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp] = false
			sp++
			ip += 5

		case OpPutUpval, OpSetUpval:
			// Assigning through an upvalue. SET_UPVAL is the expression form and
			// leaves the value behind; PUT_UPVAL is the statement form.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			a.MovRegImm64(jitRegScratch, uint64(readU16(code, ip+1)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperPutUpval, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			if op == OpSetUpval {
				a.MovRegMem(jitStackRegs[sp-1], jitasm.RegCtx, jitmem.CtxOffRet)
				kind[sp-1] = false
			} else {
				sp--
			}
			ip += 3

		case OpSpecialObj:
			// Five different things share this opcode, and only one of them is a
			// value compiled code already has. Kind 1 is the self-reference a
			// named function expression binds — `(function f(){ return f; })` —
			// and it is the frame's own function, carried in the context beside
			// the receiver for exactly this.
			//
			// It is also, by a distance, the one that matters: of the 832
			// functions in the Octane corpus that contain SPECIAL_OBJ, **618 use
			// only this kind** and 207 only `arguments`. Nothing in that corpus
			// uses new.target, import.meta or a private-name binding through it.
			//
			// The rest are refused by name rather than as "op:SPECIAL_OBJ", so
			// the diagnostic says which one is missing instead of pointing at an
			// opcode that is already half implemented.
			if k := code[ip+1]; k != 1 && k != 0 {
				return refuse(why, "special-obj/"+jitSpecialObjKind(k))
			}
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			if code[ip+1] == 1 {
				a.MovRegMem(jitStackRegs[sp], jitasm.RegCtx, jitmem.CtxOffFnVal)
			} else {
				if !jitCallHelper(a, sp, jitHelperArguments, &fixups) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(jitStackRegs[sp], jitasm.RegCtx, jitmem.CtxOffRet)
			}
			kind[sp] = false
			sp++
			ip += 2

		case OpDefineField:
			// obj val -> obj. The receiver is left in place because the literal is
			// still being built on top of it.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperDefineField, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-2], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-2] = false
			sp--
			ip += 5

		case OpJmpNotNullish:
			// What `a ?? b` compiles to: consume the value and branch when it is
			// neither null nor undefined. The same range test as everything else
			// in jit_singleton_amd64.go, straight into a Jcc rather than through
			// a Boolean.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			target := int(readU32(code, ip+1))
			l, known := labels[target]
			if !known {
				return refuse(why, "branch-target")
			}
			jitEmitNullishFlags(a, jitStackRegs[sp-1])
			a.Jcc(jitasm.CondA, l)
			sp--
			if d, ok := depths[ip+5]; !ok || sp != d {
				return refuse(why, "stack-at-target")
			}
			ip += 5

		case OpThrow:
			// The helper never returns normally, so nothing after this runs. The
			// emitter still has to stop believing it can fall through, which is
			// what `returned` marks — and jitAnalyze ends the block here for the
			// same reason, so the depth analysis does not carry a stack past it.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperThrow, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			sp = 0
			returned = true
			ip++

		case OpArray:
			// An array literal of n elements, which the helper takes off the top
			// of the spill area. The count is passed as an immediate rather than
			// inferred from the depth, because a literal can be built with
			// operands of the surrounding expression still beneath it.
			n := int(readU16(code, ip+1))
			if n < 0 || sp < n {
				return refuse(why, "stack-underflow")
			}
			a.MovRegImm64(jitRegScratch, uint64(uint32(n)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperArray, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-n], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-n] = false
			sp = sp - n + 1
			ip += 3

		case OpMod:
			// `%` has no SSE instruction and no fast path worth emitting, so it is
			// the generic arithmetic call with a different opcode. jsArith is what
			// the interpreter runs, so a BigInt modulus and an object coercing
			// through valueOf behave identically on both tiers.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallBinary(a, sp, op, jitHelperArith, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-2], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-2] = false
			sp--
			ip++

		case OpIsUndefOrNull:
			// `v ?? x` and `v?.p` test this, and it is the same range check the
			// `x == null` template uses: TUndef is 7, TNull is 8, and an untagged
			// Number wraps past both. Emitted rather than called, because there is
			// nothing here a helper could do better.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			jitEmitNullishTest(a, jitStackRegs[sp-1], jitStackRegs[sp-1])
			kind[sp-1] = false
			ip++

		case OpTypeof:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperTypeof, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-1], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-1] = false
			ip++

		case OpIn, OpDelete:
			// Two operands consumed, a Boolean produced. `in` needs the closure's
			// private environment, which is why its helper takes one.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			h := uint32(jitHelperIn)
			if op == OpDelete {
				h = jitHelperDelete
			}
			if !jitCallHelper(a, sp, h, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-2], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-2] = false
			sp--
			ip++

		case OpObject:
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			if !jitCallHelper(a, sp, jitHelperObject, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp] = false
			sp++
			ip++

		case OpRegexp:
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperRegexp, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-2], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-2] = false
			sp--
			ip++

		case OpGetGlobalUndef:
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperGlobalUndef, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp] = false
			sp++
			ip += 7

		case OpInstanceof:
			// `x instanceof C`, which is a call-out and nothing else: there is no
			// fast path worth emitting, because even the simplest case walks a
			// prototype chain, and C may carry a @@hasInstance that is arbitrary
			// user code. What the template buys is the rest of the function.
			//
			// It buys a great deal. EarleyBoyer refuses 25 functions for this one
			// opcode and runs **547 million interpreted instructions** in them —
			// more than every other refusal in that benchmark put together, and
			// 12.0 million frame entries that this alone unblocks.
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperInstanceof, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitStackRegs[sp-2], jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-2] = false
			sp--
			ip += 3

		case OpBnot:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			r := jitStackRegs[sp-1]
			var slow, done *jitasm.Label
			if !kind[sp-1] {
				slow, done = a.NewLabel(), a.NewLabel()
				jitEmitNumber(a, r, slow)
			}
			if !jitToInt32(a, r, r, sp, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			a.Not32Reg(r)
			jitFromInt32(a, r, true)
			if !kind[sp-1] {
				a.Jmp(done)
				a.Bind(slow)
				if !jitCallUnary(a, sp, op, &fixups) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(r, jitasm.RegCtx, jitmem.CtxOffRet)
				a.Bind(done)
			}
			kind[sp-1] = true
			ip++

		case OpNeg:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			r := jitStackRegs[sp-1]
			generic := !kind[sp-1]
			var slow, done *jitasm.Label
			if generic {
				slow, done = a.NewLabel(), a.NewLabel()
				jitEmitNumber(a, r, slow)
			}
			// Negation is the sign bit and nothing else, which is also why it
			// needs canonicalising: flipping the sign of the canonical NaN puts
			// it above the tag threshold.
			a.MovRegImm64(jitRegScratch, 1<<63)
			a.XorRegReg(r, jitRegScratch)
			jitCanonicalizeNaN(a, r)
			if generic {
				a.Jmp(done)
				a.Bind(slow)
				if !jitCallUnary(a, sp, op, &fixups) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(r, jitasm.RegCtx, jitmem.CtxOffRet)
				a.Bind(done)
			}
			// A BigInt negates to a BigInt, so the runtime arm may return one.
			kind[sp-1] = !generic
			ip++

		case OpInc, OpDec:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			r := jitStackRegs[sp-1]
			generic := !kind[sp-1]
			var slow, done *jitasm.Label
			if generic {
				// ToNumeric on anything else, and a BigInt increments as one
				// rather than coercing.
				slow, done = a.NewLabel(), a.NewLabel()
				jitEmitNumber(a, r, slow)
			}
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
			if generic {
				a.Jmp(done)
				a.Bind(slow)
				if !jitCallUnary(a, sp, op, &fixups) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(r, jitasm.RegCtx, jitmem.CtxOffRet)
				a.Bind(done)
			}
			kind[sp-1] = !generic
			ip++

		case OpNot:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			r := jitStackRegs[sp-1]
			if kind[sp-1] {
				// A Number is falsy when it is zero or a NaN, and UCOMISD sets
				// the zero flag for both — equal, or unordered. So the flag is
				// `!x` already, for either signed zero.
				a.XorRegReg(jitRegScratch, jitRegScratch)
				a.MovqXReg(jitasm.X1, jitRegScratch)
				a.MovqXReg(jitasm.X0, r)
				a.UcomisdXX(jitasm.X0, jitasm.X1)
				jitBoolean(a, jitasm.CondE, r)
			} else {
				// ToBoolean of anything else is a different question — the empty
				// string is false and every object is true — and the same one the
				// truthy branch answers, so it answers this too and materialises
				// the Boolean rather than branching on it.
				if !jitCallUnary(a, sp, op, &fixups) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(r, jitasm.RegCtx, jitmem.CtxOffRet)
			}
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

			// `x === null`, `x !== false`, `x == null` and the rest: the right
			// operand is a literal the instruction before pushed, and against one
			// of those the answer is exact and needs no guard. See
			// jit_singleton_amd64.go — this is 14.7M helper exits in richards and
			// 11.6M in earley-boyer, more than any other operator by an order of
			// magnitude.
			if k, ok := jitSingletonRHS(labels, prevOp, prevIP, ip); ok && jitSingletonComparable(op, k) {
				jitEmitSingletonEquals(a, op, k, x, x)
				if fused {
					jitBoolBranch(a, whenTrue, x, l)
					sp -= 2
					if sp != depths[after] {
						return refuse(why, "stack-at-target")
					}
					ip = after
				} else {
					kind[sp-2] = false
					sp--
					ip++
				}
				prevOp, prevIP = thisOp, thisIP
				continue
			}

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

		case OpInsert3:
			// obj prop a -> a obj prop a. A stack shuffle and nothing else, which
			// in a positional register assignment is three moves — and it is all
			// Richards has left, 13.6 million interpreted instructions, because
			// it is what `a[i] = v` used as an expression begins with.
			if sp < 3 {
				return refuse(why, "stack-underflow")
			}
			if sp >= len(jitStackRegs) {
				return refuse(why, "stack-too-deep")
			}
			obj, prop, val := jitStackRegs[sp-3], jitStackRegs[sp-2], jitStackRegs[sp-1]
			a.MovRegReg(jitStackRegs[sp], val)
			a.MovRegReg(val, prop)
			a.MovRegReg(prop, obj)
			a.MovRegReg(obj, jitStackRegs[sp])
			kind[sp] = kind[sp-1]
			kind[sp-1] = kind[sp-2]
			kind[sp-2] = kind[sp-3]
			kind[sp-3] = kind[sp]
			sp++
			ip++

		case OpPutElem:
			// `a[i] = v`. Everything the read's guard chain establishes has to
			// hold for a write too, and one thing more — the slot has to be
			// writable and the array extensible — so this goes to the runtime
			// rather than being emitted. What it buys is the rest of the
			// function, which is the whole of Richards' remainder.
			if sp < 3 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperPutElem, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			sp -= 3
			ip++

		case OpPutGlobal:
			idx := readU32(code, ip+1)
			if int(idx) >= len(fn.constNames) || idx > 0x7FFFFFFF {
				return refuse(why, "shape")
			}
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			a.MovRegImm64(jitRegScratch, uint64(idx))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperPutGlobal, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			sp--
			ip += 5

		case OpNew:
			// `new F(...)`, which is CALL with a different runtime entry point:
			// the object, its prototype and what a constructor returning an
			// object means are all rt.construct's. 15.2 million interpreted
			// instructions in DeltaBlue across ten functions.
			argc := int(readU16(code, ip+1))
			if argc < 0 || sp < argc+1 {
				return refuse(why, "stack-underflow")
			}
			a.MovRegImm64(jitRegScratch, uint64(uint32(argc)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperNew, &fixups) {
				return refuse(why, "stack-too-deep")
			}
			dst := jitStackRegs[sp-argc-1]
			a.MovRegMem(dst, jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-argc-1] = false
			sp -= argc
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
		prevOp, prevIP = thisOp, thisIP
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
	case OpConst, OpConstI8, OpUndef, OpNull, OpTrue, OpFalse, OpEmpty,
		OpGetLocal, OpPutLocal, OpSetLocal, OpPop, OpDup, OpInsert2,
		OpAdd, OpSub, OpMul, OpDiv,
		OpBand, OpBor, OpBxor, OpShl, OpShr, OpUshr, OpBnot,
		OpNeg, OpInc, OpDec, OpNot,
		OpLt, OpLe, OpGt, OpGe, OpEq, OpNe, OpSeq, OpSne,
		OpJmp, OpJmpFalse, OpJmpTrue, OpGetField, OpGetField2, OpPutField,
		OpGetGlobal, OpPutGlobal, OpGetUpval, OpGetElem, OpPutElem, OpInsert3,
		OpCall, OpCallMethod, OpNew, OpReturn, OpReturnUndef, OpThis,
		OpInstanceof, OpMod, OpTypeof, OpIsUndefOrNull, OpIn, OpDelete,
		OpObject, OpRegexp, OpGetGlobalUndef, OpThrow, OpArray,
		OpDefineField, OpJmpNotNullish, OpSpecialObj,
		OpClosure, OpPutUpval, OpSetUpval, OpThrowError, OpUplus,
		OpToPropkey, OpGlobal, OpDeleteVar, OpDefineMethod, OpPutConst,
		OpSetHomeObj, OpForIn, OpHasPrivate, OpGetLength:
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
		case OpJmp, OpJmpFalse, OpJmpTrue, OpJmpNotNullish:
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
	jitHelperGetField     = 1
	jitHelperToInt32      = 2
	jitHelperArith        = 3
	jitHelperRelational   = 4
	jitHelperEquals       = 5
	jitHelperPutField     = 6
	jitHelperGetGlobal    = 7
	jitHelperDeadZone     = 8
	jitHelperCall         = 9
	jitHelperCallMethod   = 10
	jitHelperGetElem      = 11
	jitHelperBitwise      = 12
	jitHelperToBoolean    = 13
	jitHelperUnary        = 14
	jitHelperNew          = 15
	jitHelperPutElem      = 16
	jitHelperPutGlobal    = 17
	jitHelperInstanceof   = 18
	jitHelperTypeof       = 19
	jitHelperIn           = 20
	jitHelperDelete       = 21
	jitHelperThrow        = 22
	jitHelperObject       = 23
	jitHelperArray        = 24
	jitHelperRegexp       = 25
	jitHelperGlobalUndef  = 26
	jitHelperDefineField  = 27
	jitHelperClosure      = 28
	jitHelperPutUpval     = 29
	jitHelperThrowError   = 30
	jitHelperUplus        = 31
	jitHelperArguments    = 32
	jitHelperToPropkey    = 33
	jitHelperGlobal       = 34
	jitHelperDeleteVar    = 35
	jitHelperDefineMethod = 36
	jitHelperPutConst     = 37
	jitHelperSetHomeObj   = 38
	jitHelperForIn        = 39
	jitHelperStillEnum    = 41
	jitHelperGetLength    = 42
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
func (c *jitCode) jitRun(rt *Runtime, fn *svFunc, cl *closure, fnVal Value, args, locals []Value, this Value) (Value, *ThrowError, bool) {
	return c.jitRunAt(rt, fn, cl, fnVal, args, locals, this, c.entry)
}

// jitRunOSR enters at the stub for a loop header, if there is one.
//
// Reports false when there is not, or when the stub's guards decline the locals
// the interpreter has produced — in which case nothing has happened and the
// interpreter carries on from where it was.
func (c *jitCode) jitRunOSR(rt *Runtime, fn *svFunc, cl *closure, fnVal Value, args, locals []Value, this Value, header int) (Value, *ThrowError, bool) {
	pc, ok := c.osr[header]
	if !ok {
		return mkundef(), nil, false
	}
	return c.jitRunAt(rt, fn, cl, fnVal, args, locals, this, pc)
}

func (c *jitCode) jitRunAt(rt *Runtime, fn *svFunc, cl *closure, fnVal Value, args, locals []Value, this Value, entry uintptr) (Value, *ThrowError, bool) {
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
	reused := false
	if n < cap(rt.jitFrames) {
		if ctx = rt.jitFrames[:n+1][n]; ctx != nil {
			// Popped by an earlier call and still in the slot, which is what
			// lets the push below re-extend the slice without storing a pointer
			// into it again. That store is a Go write barrier and it showed as
			// 1.5% of DeltaBlue.
			reused = true
		}
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
	ctx.FnVal = uint64(fnVal)
	if cl != nil && len(cl.upvalues) > 0 {
		ctx.Upvals = uintptr(unsafe.Pointer(&cl.upvalues[0]))
	}
	switch {
	case reused:
		rt.jitFrames = rt.jitFrames[:n+1] // the slot already holds ctx
	case n < cap(rt.jitFrames):
		rt.jitFrames = rt.jitFrames[:n+1]
		rt.jitFrames[n] = ctx
	default:
		rt.jitFrames = append(rt.jitFrames, ctx)
	}

	// Popped explicitly on each way out rather than by a deferred closure. A
	// defer here is a call per compiled frame entry, and this function has three
	// exits and no panic path of its own — a Go panic below is a bug in the
	// engine, and the process is going down with it either way.
	// A frame that captured a local owns cells that only it can have created,
	// and the next frame at this depth must not find them. Cleared on every way
	// out, and the map itself is only ever allocated by a capture.
	if rt.jitOpenUpvals != nil {
		delete(rt.jitOpenUpvals, rt.frameDepth)
	}
	pc := entry
	for {
		jitmem.Enter(pc, ctx)
		// The locals slice reaches compiled code as an integer, so nothing in
		// the call graph keeps it reachable for the collector.
		runtime.KeepAlive(locals)
		switch ctx.Exit {
		case jitmem.ExitReturn:
			rt.jitFrames = rt.jitFrames[:n]
			if rt.jitOpenUpvals != nil {
				delete(rt.jitOpenUpvals, rt.frameDepth)
			}
			return Value(ctx.Ret), nil, true
		case jitmem.ExitPreempt:
			// Being here is the safepoint: this is ordinary Go, so the runtime
			// can collect and preempt before the loop is re-entered.
			ctx.Args[1] = jitFuel
			pc = ctx.Resume
		case jitmem.ExitHelper:
			if e := jitHelper(rt, fn, cl, args, locals, ctx); e != nil {
				rt.jitFrames = rt.jitFrames[:n]
				if rt.jitOpenUpvals != nil {
					delete(rt.jitOpenUpvals, rt.frameDepth)
				}
				return mkundef(), e, true
			}
			pc = ctx.Resume
		default:
			rt.jitFrames = rt.jitFrames[:n]
			if rt.jitOpenUpvals != nil {
				delete(rt.jitOpenUpvals, rt.frameDepth)
			}
			return mkundef(), nil, false
		}
	}
}

// jitSpillArgs is the call's arguments where compiled code already left them,
// as a slice rather than a copy.
//
// A call site holds [callee, arg0 .. argN-1] on the operand stack and spilling
// writes the whole stack to the context, so by the time this runs the arguments
// are contiguous, in order, and rooted — SpillN covers them for as long as the
// nested call runs. Copying them into a slice of their own was 9% of DeltaBlue
// in `makeslice` alone, and every byte of it was already sitting here.
//
// It is not sound for an arbitrary callee, which is why jitCallCompiled takes
// this and callValue does not. A sloppy function's mapped `arguments` object
// writes *through* to the array it was given, and that array must not be
// another frame's operand stack. The guarantee that it cannot happen here is
// structural rather than a check: a mapped `arguments` needs SPECIAL_OBJ, that
// opcode has no template, so a function containing one never compiles — and
// jitCallCompiled runs only what compiled. TestCompiledCalleeCannotSeeArguments
// is what keeps that true if SPECIAL_OBJ ever gets one.
//
// The other half of the argument is the collector's. These Values are marked
// twice while the call runs, once through the caller's spill area and once
// through the callee's published frame, which is harmless. What would not be
// harmless is the callee's frame outliving the window, so jitCallCompiled drops
// its reference on the way out rather than leaving a slice pointing into a
// context that is about to be reused.
func jitSpillArgs(ctx *jitmem.ExecContext, base, argc int) []Value {
	if argc == 0 {
		return nil
	}
	return unsafe.Slice((*Value)(unsafe.Pointer(&ctx.Spill[base])), argc)
}

// jitCopyArgs is the copy the general path still needs, for a callee that may
// retain or write through the array it is given.
func jitCopyArgs(window []Value) []Value {
	args := make([]Value, len(window))
	copy(args, window)
	return args
}

// jitHelper runs what compiled code asked for.
func jitHelper(rt *Runtime, fn *svFunc, cl *closure, args, locals []Value, ctx *jitmem.ExecContext) *ThrowError {
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
							// The emitted probe declined a read the cache could
							// answer, which means a guard it emits is narrower
							// than icWay.hit. Counted apart from a genuine miss
							// because the two want opposite work.
							jitStats.icMiss++
							jitStats.icNarrow++
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
	case jitHelperCall, jitHelperCallMethod, jitHelperNew:
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
		// The arguments as they already sit in the spill area, without copying
		// them anywhere. See jitSpillArgs for why that is sound for a compiled
		// callee and not in general.
		window := jitSpillArgs(ctx, base+1, argc)
		var v Value
		var e *ThrowError
		switch {
		case ctx.Helper == jitHelperNew:
			v, e = rt.construct(callee, jitCopyArgs(window))
		default:
			// The call compiled code makes most often is to another compiled
			// function, and reaching one goes through callValue's dispatch,
			// runFrame's bookkeeping, runCompiledFrame and jitTry — four
			// functions and about 28% of DeltaBlue. jitCallCompiled is those
			// four collapsed for that one case; anything it does not recognise
			// falls through to the general path unchanged, with the copy this
			// path skipped made there instead.
			var ok bool
			if v, e, ok = rt.jitCallCompiled(callee, thisArg, window); !ok {
				v, e = rt.callValue(callee, thisArg, jitCopyArgs(window))
			}
		}
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
	case jitHelperPutElem:
		// The interpreter's PUT_ELEM, including the strict-mode report of a
		// store that did not happen.
		n := int(ctx.SpillN)
		if n < 3 {
			return rt.typeError("JIT operand stack")
		}
		ok, e := rt.setElementR(Value(ctx.Spill[n-3]), Value(ctx.Spill[n-2]), Value(ctx.Spill[n-1]))
		if e != nil {
			return e
		}
		if !ok && fn.isStrict {
			return rt.typeError("Cannot assign to read only property")
		}
		return nil
	case jitHelperPutGlobal:
		// The interpreter's PUT_GLOBAL: the declarative record first, because a
		// Script-level let shadows a property of the same name, then the strict
		// requirement that the binding already exist.
		n := int(ctx.SpillN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		val := Value(ctx.Spill[n-1])
		name := fn.constNames[uint32(ctx.Args[3])]
		if b := rt.lookupGlobalLex(name); b != nil {
			return rt.globalLexWrite(b, name, val)
		}
		if fn.isStrict && !rt.hasProp(rt.global, name) {
			return rt.referenceError(name + " is not defined")
		}
		if !rt.setProp(rt.global, name, val) && fn.isStrict {
			return rt.typeError("Cannot assign to read only property '" + name + "'")
		}
		return nil
	case jitHelperUnary:
		// The operand this tier could not prove was a Number. jsUnary is what the
		// interpreter runs for the same opcode, so a BigInt increments as one and
		// an object coerces through valueOf exactly as it would there.
		n := int(ctx.SpillN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		v, e := rt.jsUnary(Opcode(uint32(ctx.Args[3])), Value(ctx.Spill[n-1]))
		if e != nil {
			return e
		}
		if jitStats.enabled {
			jitStats.genSlow++
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperToBoolean:
		// A String or a BigInt, whose truth is in what the Value points at. The
		// emitted branch answers every other type itself.
		n := int(ctx.SpillN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		ctx.Ret = 0
		if rt.toBoolean(Value(ctx.Spill[n-1])) {
			ctx.Ret = 1
		}
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
	case jitHelperInstanceof:
		// The interpreter's OpInstanceof, over the two spilled operands. Both
		// arms are rt.jsInstanceof's, so a compiled `instanceof` and an
		// interpreted one cannot disagree about a bound function, a @@hasInstance
		// or a proxy.
		n := int(ctx.SpillN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		r, e := rt.jsInstanceof(Value(ctx.Spill[n-2]), Value(ctx.Spill[n-1]))
		if e != nil {
			return e
		}
		ctx.Ret = uint64(mkbool(r))
		return nil
	case jitHelperDefineField:
		// `{ v: expr }` and a class field initialiser. The receiver stays on the
		// stack — the literal is still being built — so only the value is
		// consumed here and the emitter leaves the object where it was.
		//
		// The whole of the interpreter's arm, private names included, which is
		// the other reason this helper takes the closure: a private field
		// resolves against the class environment and its double-install and
		// non-extensible cases are TypeErrors that must not go missing.
		n := int(ctx.SpillN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		recv, val := Value(ctx.Spill[n-2]), Value(ctx.Spill[n-1])
		name := fn.constNames[uint32(ctx.Args[3])]
		o := rt.objPtr(recv)
		if o == nil {
			ctx.Ret = uint64(recv)
			return nil
		}
		var privEnv *privScope
		if cl != nil {
			privEnv = cl.privEnv
		}
		switch {
		case isPrivateKey(name):
			if !o.flags.extensible {
				return rt.typeError("Cannot add private member " + privDisplay(name) + " to a non-extensible object")
			}
			if !o.definePrivateField(name, privEnv, val) {
				return rt.typeError("Cannot initialize private field " + privDisplay(name) + " twice on the same object")
			}
		case o.proxy != nil || !o.flags.extensible:
			if e := rt.createDataProperty(recv, rt.internString(name), val); e != nil {
				return e
			}
		default:
			o.defineOwn(name, val, attrDefault)
		}
		ctx.Ret = uint64(recv)
		return nil
	case jitHelperClosure:
		// Creating a closure, which is the one thing in this tier that lets a
		// frame's locals outlive the frame.
		//
		// An upvalue over a local is a pointer into the locals slice, so the
		// slice must never be handed to another frame at the same depth again —
		// dropFrameLocals is how the interpreter says that, and it says it here
		// for the same reason. The frame is then free to keep writing through
		// that slice and the closure sees it, which is the whole point.
		//
		// The cells are shared per frame: two closures over the same slot must
		// capture the same upvalue, or assigning through one would be invisible
		// to the other. rt.jitOpenUpvals is that sharing, keyed by depth and
		// dropped when the frame leaves.
		child := fn.childFuncs[uint32(ctx.Args[3])]
		upvals := make([]*upvalue, len(child.upvalDescs))
		for i, d := range child.upvalDescs {
			if !d.isLocal {
				if cl == nil || d.index >= len(cl.upvalues) {
					return rt.typeError("JIT upvalue")
				}
				upvals[i] = cl.upvalues[d.index]
				continue
			}
			if d.index >= len(locals) {
				return rt.typeError("JIT upvalue")
			}
			open := rt.jitOpenUpvals[rt.frameDepth]
			if open == nil {
				open = map[int]*upvalue{}
				if rt.jitOpenUpvals == nil {
					rt.jitOpenUpvals = map[int]map[int]*upvalue{}
				}
				rt.jitOpenUpvals[rt.frameDepth] = open
				// The first capture in this frame is what commits it: from here
				// the locals belong to the closures, not to the depth.
				rt.dropFrameLocals(rt.frameDepth)
			}
			u, ok := open[d.index]
			if !ok {
				u = &upvalue{location: &locals[d.index]}
				open[d.index] = u
			}
			upvals[i] = u
		}
		fv := rt.newFunction(child, upvals)
		// An arrow that reads an inherited `super` takes the enclosing method's
		// [[HomeObject]], and a function created in a class body belongs to that
		// evaluation's private environment. Both come off the running closure,
		// exactly as they do in the interpreter. A `with` capture cannot arrive
		// here at all: the emitter refuses a child that wants one, because a
		// compiled frame has no with-chain to give it.
		if cl != nil {
			if child.capturesHome && cl.home.IsObjectType() {
				if ncl := rt.closureOf(fv); ncl != nil {
					ncl.home = cl.home
				}
			}
			if cl.privEnv != nil {
				if ncl := rt.closureOf(fv); ncl != nil {
					ncl.privEnv = cl.privEnv
				}
			}
		}
		ctx.Ret = uint64(fv)
		return nil
	case jitHelperPutUpval:
		// Writing through an upvalue the running closure holds. SET_UPVAL leaves
		// the value on the stack and PUT_UPVAL does not; the emitter decides
		// that, so both reach here the same way.
		n := int(ctx.SpillN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		i := int(uint32(ctx.Args[3]))
		if cl == nil || i >= len(cl.upvalues) || cl.upvalues[i] == nil {
			return rt.typeError("JIT upvalue")
		}
		v := Value(ctx.Spill[n-1])
		cl.upvalues[i].set(v)
		ctx.Ret = uint64(v)
		return nil
	case jitHelperArguments:
		// The `arguments` object, which is the last of SPECIAL_OBJ's five kinds
		// this tier refuses and the largest block of functions left in the
		// corpus at 213.
		//
		// The reason it was held back turned out to be the wrong reason. A
		// compiled callee is handed the caller's spill area as its arguments
		// rather than a copy — see jitSpillArgs — and a mapped `arguments`
		// writes through to an array, so the two looked incompatible. They are
		// not: newArgumentsMap aliases the frame's **locals**, which every frame
		// owns, and the indexed properties are copied out of the argument values
		// rather than aliasing them.
		//
		// The copy below is what keeps that a fact rather than an observation.
		// It costs one allocation on a path that already allocates an object and
		// a property per argument, and it means the caller's spill area is read
		// exactly once, here, by code that cannot retain it.
		own := jitCopyArgs(args)
		a := rt.newObject(rt.objectProto)
		ao := rt.objPtr(a)
		for i, v := range own {
			ao.defineOwn(numberToString(float64(i)), v, attrDefault)
		}
		if fn.mappedArgs {
			// The map points into the locals and the object can outlive the
			// call, so this depth gives up its buffer — the same commitment a
			// capture makes, and for the same reason.
			ao.argMap = newArgumentsMap(locals, fn.paramCount, len(own))
			rt.dropFrameLocals(rt.frameDepth)
		}
		ao.defineOwn("length", mknum(float64(len(own))), attrWritable|attrConfigurable)
		if fn.mappedArgs {
			ao.defineOwn("callee", Value(ctx.FnVal), attrWritable|attrConfigurable)
		} else {
			// Unmapped covers strict code and a sloppy function with a
			// non-simple parameter list; `callee` is the poison pill.
			ao.defineAccessor("callee", rt.poison, rt.poison, true, true, 0)
		}
		if rt.symIterator != 0 {
			if vals, e := rt.getField(rt.arrayProto, "values"); e == nil {
				ao.defineOwnSymbol(rt.symIterator.handle(), vals, attrWritable|attrConfigurable)
			}
		}
		ctx.Ret = uint64(a)
		return nil
	case jitHelperThrowError:
		// A native error with a constant message, which the compiler emits where
		// the language requires a specific throw — assigning to a constant, a
		// class constructor called without `new`. The kind is packed above the
		// name index because both are immediates of the same instruction.
		msg := fn.constNames[uint32(ctx.Args[3])]
		switch byte(ctx.Args[3] >> 32) {
		case 1:
			return rt.referenceError(msg)
		case 2:
			return rt.syntaxError(msg)
		case 3:
			return rt.rangeError(msg)
		default:
			return rt.typeError(msg)
		}
	case jitHelperUplus:
		// Unary `+`, which is ToNumber and can run a valueOf.
		n := int(ctx.SpillN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		v, e := rt.toNumber(Value(ctx.Spill[n-1]))
		if e != nil {
			return e
		}
		ctx.Ret = uint64(mknum(v))
		return nil
	case jitHelperToPropkey:
		// ToPropertyKey, which is ToPrimitive with hint "string" and can run
		// user code through toString or Symbol.toPrimitive.
		n := int(ctx.SpillN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		pk, e := rt.toPropertyKey(Value(ctx.Spill[n-1]))
		if e != nil {
			return e
		}
		ctx.Ret = uint64(pk)
		return nil
	case jitHelperGlobal:
		ctx.Ret = uint64(rt.global)
		return nil
	case jitHelperDeleteVar:
		// Whether the name resolves to anything, which is both `delete x` in
		// sloppy code and the strict-mode reference check before an assignment.
		nm := fn.constNames[uint32(ctx.Args[3])]
		ctx.Ret = uint64(mkbool(rt.lookupGlobalLex(nm) != nil || rt.hasProp(rt.global, nm)))
		return nil
	case jitHelperGetLength:
		n := int(ctx.SpillN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		v, e := rt.getField(Value(ctx.Spill[n-1]), "length")
		if e != nil {
			return e
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperForIn:
		// The keys a for-in walks, snapshotted the way the interpreter takes
		// them: a proxy's ownKeys trap runs here.
		n := int(ctx.SpillN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		keys, e := rt.forInKeys(Value(ctx.Spill[n-1]))
		if e != nil {
			return e
		}
		ctx.Ret = uint64(keys)
		return nil
	case jitHelperStillEnum:
		n := int(ctx.SpillN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		ctx.Ret = uint64(mkbool(rt.forInStillEnumerable(Value(ctx.Spill[n-1]), Value(ctx.Spill[n-2]))))
		return nil
	case jitHelperSetHomeObj:
		n := int(ctx.SpillN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		rt.setMethodHome(Value(ctx.Spill[n-1]), Value(ctx.Spill[n-2]))
		return nil
	case jitHelperPutConst:
		// The write half of a strict assignment to an unqualified name. The
		// resolvable flag DELETE_VAR left is checked here, and HasProperty is
		// checked again because the right-hand side has run in between and may
		// have deleted the binding the reference resolved to.
		n := int(ctx.SpillN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		val := Value(ctx.Spill[n-1])
		name := fn.constNames[uint32(ctx.Args[3])]
		if !rt.toBoolean(Value(ctx.Spill[n-2])) {
			return rt.referenceError(name + " is not defined")
		}
		if b := rt.lookupGlobalLex(name); b != nil {
			if e := rt.globalLexWrite(b, name, val); e != nil {
				return e
			}
		} else if !rt.hasProp(rt.global, name) {
			return rt.referenceError(name + " is not defined")
		} else if !rt.setProp(rt.global, name, val) {
			return rt.typeError("Cannot assign to read only property '" + name + "'")
		}
		ctx.Ret = uint64(val)
		return nil
	case jitHelperDefineMethod:
		// [target, method] -> [target]. The private arms need the class
		// environment the closure carries, the same as `#x in obj`.
		n := int(ctx.SpillN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		var privEnv *privScope
		if cl != nil {
			privEnv = cl.privEnv
		}
		name := fn.constNames[uint32(ctx.Args[3])]
		return rt.defineMethod(name, byte(ctx.Args[3]>>32), Value(ctx.Spill[n-1]), Value(ctx.Spill[n-2]), privEnv)
	case jitHelperTypeof:
		// `typeof v`. The string is interned, so what lands on the operand stack
		// is a handle into the runtime's own table rather than a fresh string.
		n := int(ctx.SpillN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		ctx.Ret = uint64(rt.internString(rt.typeofString(Value(ctx.Spill[n-1]))))
		return nil
	case jitHelperIn:
		// `key in obj`, which is why this helper takes the closure: `#x in obj`
		// resolves the private name against the class environment the closure
		// carries, and passing nil there would answer the wrong question rather
		// than fail.
		n := int(ctx.SpillN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		var privEnv *privScope
		if cl != nil {
			privEnv = cl.privEnv
		}
		r, e := rt.jsIn(Value(ctx.Spill[n-2]), Value(ctx.Spill[n-1]), privEnv)
		if e != nil {
			return e
		}
		ctx.Ret = uint64(mkbool(r))
		return nil
	case jitHelperDelete:
		// `delete o[k]`. The strict-mode arm is the interpreter's: a failed
		// delete is a TypeError there and silently false in sloppy code.
		n := int(ctx.SpillN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		ok, e := rt.deleteElement(Value(ctx.Spill[n-2]), Value(ctx.Spill[n-1]))
		if e != nil {
			return e
		}
		if !ok && fn.isStrict {
			return rt.typeError("Cannot delete property of a non-configurable object")
		}
		ctx.Ret = uint64(mkbool(ok))
		return nil
	case jitHelperThrow:
		// `throw v`. This tier has no handlers of its own — TRY_PUSH is still
		// refused — so the throw leaves the frame, which is what returning an
		// error from here does.
		n := int(ctx.SpillN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		return &ThrowError{Value: Value(ctx.Spill[n-1]), rt: rt}
	case jitHelperObject:
		ctx.Ret = uint64(rt.newPlainObject())
		return nil
	case jitHelperArray:
		// An array literal, built from the top `n` spilled slots. The count is an
		// immediate rather than the spill depth, because a literal can appear
		// with operands of the surrounding expression beneath it.
		want := int(uint32(ctx.Args[3]))
		n := int(ctx.SpillN)
		if want < 0 || n < want {
			return rt.typeError("JIT operand stack")
		}
		arrv := rt.newArray()
		ao := rt.objPtr(arrv)
		ao.arr = make([]Value, want)
		ao.arrLen = uint32(want)
		for i := 0; i < want; i++ {
			ao.arr[i] = Value(ctx.Spill[n-want+i])
		}
		ctx.Ret = uint64(arrv)
		return nil
	case jitHelperRegexp:
		n := int(ctx.SpillN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		v, e := rt.newRegExp(rt.strGo(Value(ctx.Spill[n-2])), rt.strGo(Value(ctx.Spill[n-1])))
		if e != nil {
			return e
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperGlobalUndef:
		// The lenient global read `typeof maybeUndeclared` compiles to. An absent
		// name needs no test of its own: a plain [[Get]] on the global answers
		// undefined for one, and the ReferenceError the strict read raises is
		// raised by that read rather than by getField. A global lexical binding
		// is not absent, so its dead zone still throws.
		name := fn.constNames[uint32(ctx.Args[3])]
		if b := rt.lookupGlobalLex(name); b != nil {
			v, e := rt.globalLexRead(b, name)
			if e != nil {
				return e
			}
			ctx.Ret = uint64(v)
			return nil
		}
		v, e := rt.getField(rt.global, name)
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

// jitSpecialObjKind names what a SPECIAL_OBJ was asking for, so a refusal says
// which of the five is missing rather than naming the opcode they share.
func jitSpecialObjKind(k byte) string {
	switch k {
	case 0:
		return "arguments"
	case 1:
		return "self"
	case 2:
		return "new.target"
	case 3:
		return "import.meta"
	case 4, 5:
		return "private-name"
	}
	return "unknown"
}
