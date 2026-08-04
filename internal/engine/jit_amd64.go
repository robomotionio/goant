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

// The operand stack is a window of registers over an array in the context.
//
// Nine registers is all the machine has left once the locals pointer, the tag
// guard, the context and a scratch are pinned, and nine was the whole depth a
// function was allowed — which refused 95 of the Octane corpus, three quarters
// of them for wanting between ten and fifteen slots. An object literal with
// eight properties is over the line.
//
// So the registers hold the TOP nine slots and everything below them lives in
// ctx.Spill, which is where a helper call was already going to put them. Slot i
// is in register i mod 9 whenever it is in the window at all, which makes the
// two edges cheap: a push past nine evicts the slot exactly nine below, and
// that slot's register is the one the new slot wants. One store on the way out,
// one load on the way back.
//
// jitStackWindow is that nine. jitMaxDepth is how deep the array goes, and
// past it a function is still refused — 16 of the 95 want more than 32 slots,
// and one of them wants 746.
const jitStackWindow = 9

// jitMaxDepth is how deep the operand stack may get before a function is
// refused.
//
// The array it slides over is allocated per frame at the depth the function
// actually needs, so this is not a budget — it is a bound on how far a wrong
// answer from the stack analysis could go before it is caught. The deepest real
// function in the Octane corpus is a source file that is one array literal,
// which wants seventeen thousand slots.
const jitMaxDepth = 1 << 20

func jitSlot(i int) jitasm.Reg { return jitStackRegs[i%jitStackWindow] }

// jitWindowBase is the shallowest slot that is in a register at this depth.
func jitWindowBase(sp int) int {
	if sp <= jitStackWindow {
		return 0
	}
	return sp - jitStackWindow
}

// jitEvictSlots writes out the slots that leave the register window when the
// depth goes from sp to after. It has to run before the template that pushes
// them: the register a new slot wants is the one the slot nine below it is in.
func jitEvictSlots(a *jitasm.Asm, sp, after int, deep bool) {
	if jitWindowBase(after) <= jitWindowBase(sp) {
		return
	}
	base, off := jitStackBase(a, deep)
	for k := jitWindowBase(sp); k < jitWindowBase(after); k++ {
		a.MovMemReg(base, off+int32(8*k), jitSlot(k))
	}
}

// jitWindowShifts reports whether the window moves between two depths, which is
// what makes a branch across it unemittable.
//
// The refill that brings a slot back into a register is emitted after the
// template, so on a conditional branch it sits on the fall-through path alone —
// the taken edge would leave the window claiming a slot is in a register while
// the register holds the condition that was just tested. It cannot simply be
// hoisted above the branch either: the slot re-entering the window is the same
// register the condition is in, so refilling first would destroy the thing being
// branched on. So the shape is refused instead. It needs ten live operands
// around a branch to arise at all — an array literal with a conditional element
// past the ninth, which is where it was found — and the interpreter runs those
// perfectly well.
func jitWindowShifts(before, after int) bool {
	return jitWindowBase(before) != jitWindowBase(after)
}

// jitStackBase is where the operand array is, as a register and an offset.
//
// A function whose stack fits in the context addresses it straight off the
// context register, which is what every function did when that was the only
// kind of stack there was. A deeper one loads the pointer into the scratch
// register first.
//
// Which of the two a function uses is decided before a byte is emitted and
// never changes, so no site tests anything at run time. Making every function
// pay the load instead cost between three and eleven percent across Octane —
// not a trade worth making for nineteen functions that each run once.
func jitStackBase(a *jitasm.Asm, deep bool) (jitasm.Reg, int32) {
	if !deep {
		return jitasm.RegCtx, jitmem.CtxOffSpill
	}
	a.MovRegMem(jitRegScratch, jitasm.RegCtx, jitmem.CtxOffStack)
	return jitRegScratch, 0
}

// jitRefillSlots reads back the slots that re-enter the window when the depth
// drops from sp to after, given that the template has written the top push of
// them itself.
//
// It has to run after the template, because until then those registers still
// hold operands the template is reading. And it has to stop short of what the
// template produced: a call that takes the stack from ten to one leaves its
// result in slot zero, and slot zero is also the slot re-entering the window —
// reading it back from the context would replace the answer with the callee.
func jitRefillSlots(a *jitasm.Asm, sp, after, push int, deep bool) {
	end := jitWindowBase(sp)
	if after-push < end {
		end = after - push
	}
	if end <= jitWindowBase(after) {
		return
	}
	base, off := jitStackBase(a, deep)
	for k := jitWindowBase(after); k < end; k++ {
		a.MovRegMem(jitSlot(k), base, off+int32(8*k))
	}
}

const (
	jitRegLocals  = jitasm.R12
	jitRegGuard   = jitasm.R15
	jitRegScratch = jitasm.RCX
	// jitRegReturn carries a returning frame's value to the compiled call site
	// that entered it, beside the exit code in RAX. It is an operand-stack
	// register like any other — the frame it belongs to is over by the time this
	// is read, and the caller's copy of it is in the spill area.
	jitRegReturn = jitasm.RDX
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
	// slots is how many operand slots the body needs, which is what the frame's
	// stack array is sized to. Not a property of the tier — a property of the
	// function, and the reason the array is not in the context.
	slots int
	// catchAt maps a call site's resume address to where a throw from it is
	// caught. Keyed that way because a resume address is the only thing jitRunAt
	// holds when a helper hands back an error, and nil for a function with no
	// try in it — which is nearly all of them.
	catchAt map[uintptr]uintptr
	// mentry is the entry a compiled call site enters this function at, and zero
	// for a function only the runtime may enter. It differs from entry in doing
	// what jitRunAt would otherwise have done first — see jitEmitMachineEntry.
	mentry uintptr
	// sites is this function's call sites, one per CALL in the body. Allocated
	// before a byte is emitted, because the address of each is compiled into the
	// code that reads it.
	sites []jitCallSite
}

// jitHasHandlers reports whether the body installs an exception handler.
func jitHasHandlers(fn *svFunc, start int) bool {
	code := fn.code
	for ip := start; ip < len(code); {
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		if size <= 0 || ip+size > len(code) {
			return false
		}
		if op == OpTryPush {
			return true
		}
		ip += size
	}
	return false
}

// jitResumeFixup records a resume address that could not be written until the
// code had somewhere to live.
type jitResumeFixup struct {
	immOff int
	label  *jitasm.Label
	// catch is where a throw from this call site is caught, when the site is
	// inside a try. Recorded here because the resume address is what jitRunAt
	// has when a helper returns an error, and this is where it is minted.
	catch *jitasm.Label
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
	// How deep the operand stack gets, and so which of the two kinds of stack
	// this function uses. Decided here, before anything is emitted, because
	// every site that touches a slot compiles the choice in.
	maxDepth, ok := jitMaxOperandDepth(fn, blocks, depths)
	if !ok {
		return refuse(why, "stack-across-blocks/depth")
	}
	if maxDepth > jitMaxDepth {
		return refuse(why, "stack-too-deep")
	}
	deepStack := maxDepth > jitmem.InlineSlots

	// Which exception handler is in force where. Only computed for a function
	// that has one, so nothing else pays for the walk.
	var handlers map[int][]jitHandler
	if jitHasHandlers(fn, start) {
		handlers, ok = jitHandlerStacks(fn, blocks, depths, start)
		if !ok {
			return refuse(why, "handlers-across-blocks")
		}
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
	// And the same for the call sites, one per CALL in the body. Counted first
	// rather than appended to as they are emitted, for exactly the same reason:
	// appending moves the array, and the address of every site already emitted
	// is baked into the code that reads it.
	sites := make([]jitCallSite, jitCountCalls(fn, start))
	nextSite := 0

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
	// Sized from the body rather than from jitMaxDepth: the depth cannot exceed
	// the number of instructions, since no single one leaves more than one more
	// operand than it found, and jitMaxDepth is a bound on nonsense rather than
	// a budget — allocating a megabyte per compile made the test suite eight
	// times slower.
	kind := make([]bool, len(code)+jitStackWindow+1)
	// The previous instruction, maintained across the loop below.
	prevOp, prevIP := Opcode(0), -1
	// The handlers in force here, seeded from the block's entry set the same way
	// cur is. What it is for is the fixups: a helper that throws has to resume
	// at the innermost catch rather than leave the frame.
	var live []jitHandler
	if handlers != nil {
		live = handlers[start]
	}
	// One entry stub per catch that something can actually throw into. Landing
	// there comes from Go rather than from a branch, and the entry trampoline
	// sets the context register and nothing else — so the two registers the body
	// keeps pinned have to be re-established first, exactly as a helper's resume
	// point does. Jumping straight at the catch instead left the locals pointer
	// holding whatever the machine had, which reads as a frame of nonsense
	// rather than as a crash.
	catchStubs := map[int]*jitasm.Label{}
	// The locals whose Number-ness the body relies on, and the loop headers it
	// could be entered at. Both are only known once the body has been walked,
	// which is why the entry stubs are emitted after it.
	readsNumeric := make([]bool, fn.maxLocals)
	loopHeaders := map[int]bool{}

	for ip < len(code) {
		if b, isLeader := blocks[ip]; isLeader {
			copy(cur, b.in)
			if handlers != nil {
				live = handlers[ip]
			}
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

		// The register window follows the depth, and the depth after this
		// instruction is what says which slots move. jitStackEffect is the same
		// answer the analyses were built on, so this is also where the emitter
		// and those analyses are checked against each other — once per
		// instruction rather than once per label.
		fixupsBefore := len(fixups)
		effPop, effPush, effOK := jitStackEffect(fn, ip)
		if !effOK {
			return refuse(why, "undecodable")
		}
		spBefore := sp
		spAfter := sp - effPop + effPush
		if spAfter > maxDepth {
			return refuse(why, "stack-deeper-than-predicted")
		}
		if spAfter > spBefore {
			jitEvictSlots(a, spBefore, spAfter, deepStack)
		}

		switch op := Opcode(code[ip]); op {
		case OpConstI8:
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegImm64(jitSlot(sp), uint64(tov(float64(int8(code[ip+1])))))
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
			if sp >= jitMaxDepth {
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
			a.MovRegImm64(jitSlot(sp), uint64(v))
			kind[sp] = false
			sp++
			ip++

		case OpConst:
			idx := readU32(code, ip+1)
			if int(idx) >= len(fn.constants) || sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			// A String or a regexp constant is a handle, and baking one into
			// code is safe for exactly the reason reading it from the pool is:
			// the pool does not move, and the collector marks fn.constants for
			// as long as fn is reachable — which is longer than the code, since
			// fn owns it. A constant pool belongs to the runtime that built it,
			// and so does the function, so the two can never be crossed.
			a.MovRegImm64(jitSlot(sp), uint64(fn.constants[idx]))
			kind[sp] = fn.constants[idx].IsNumber()
			sp++
			ip += 5

		case OpGetLocal:
			i := int(readU16(code, ip+1))
			if i >= fn.maxLocals || sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp), jitRegLocals, int32(i)*8)
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
				a.CmpRegReg(jitSlot(sp), jitRegScratch)
				a.Jcc(jitasm.CondNE, ok)
				if !jitCallHelper(a, sp, jitHelperDeadZone, &fixups, deepStack) {
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
			a.MovMemReg(jitRegLocals, int32(i)*8, jitSlot(sp-1))
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
			x, y := jitSlot(sp-2), jitSlot(sp-1)
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
				if !jitCallBinary(a, sp, op, jitHelperArith, &fixups, deepStack) {
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
			x, y := jitSlot(sp-2), jitSlot(sp-1)
			generic := !kind[sp-1] || !kind[sp-2]
			// A comparison whose result a branch consumes never has to produce
			// the Boolean at all, which is both faster and how this tier managed
			// before it could represent one.
			l, whenTrue, after, fused := jitFuse(code, labels, ip+1)
			if fused && jitWindowShifts(sp, sp-2) {
				return refuse(why, "branch-across-window")
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
				jitCompareBranch(a, op, whenTrue, l)
			} else {
				jitRelationalValue(a, op, x)
			}
			if generic {
				a.Jmp(done)
				a.Bind(slow)
				if !jitCallBinary(a, sp, op, jitHelperRelational, &fixups, deepStack) {
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
				if sp != 0 {
					return refuse(why, "backedge-with-operands")
				}
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
			if jitWindowShifts(sp, sp-1) {
				return refuse(why, "branch-across-window")
			}
			back := target <= ip
			if !jitEmitTruthyBranch(a, jitSlot(sp-1), op == OpJmpTrue, back, l, sp, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			if back {
				loopHeaders[target] = true
			}
			sp--
			ip += 5

		case OpDup:
			if sp < 1 || sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegReg(jitSlot(sp), jitSlot(sp-1))
			kind[sp] = kind[sp-1]
			sp++
			ip++

		case OpInsert2:
			// obj a -> a obj a. Three moves rather than a rotate, because the
			// value being duplicated has to survive its own slot being written.
			if sp < 2 || sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			obj, av := jitSlot(sp-2), jitSlot(sp-1)
			a.MovRegReg(jitSlot(sp), av)
			a.MovRegReg(av, obj)
			a.MovRegReg(obj, jitSlot(sp))
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
			x, y := jitSlot(sp-2), jitSlot(sp-1)
			generic := !kind[sp-1] || !kind[sp-2]
			var slow, done *jitasm.Label
			if generic {
				slow, done = a.NewLabel(), a.NewLabel()
				jitEmitNumberPair(a, x, y, slow)
			}
			if !jitBitwise(a, op, x, y, sp, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			if generic {
				a.Jmp(done)
				a.Bind(slow)
				if !jitCallBinary(a, sp, op, jitHelperBitwise, &fixups, deepStack) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(x, jitasm.RegCtx, jitmem.CtxOffRet)
				a.Bind(done)
			}
			// An integer on the fast side, and whatever the runtime returned on
			// the other — which for two BigInt operands is a BigInt, since these
			// operators apply to those as well. So the result is a Number
			// exactly when the operands were known to be.
			kind[sp-2] = !generic
			sp--
			ip++

		case OpThrowError:
			// Never returns, like THROW. The name index and the kind travel
			// together in the one immediate the exit protocol carries.
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1))|uint64(code[ip+5])<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperThrowError, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			sp = 0
			returned = true
			ip += 6

		case OpUplus:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperUplus, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-1), jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-1] = false
			ip++

		case OpTryPush:
			// Installing a handler, which for compiled code has already
			// happened: which catch a throw belongs to is decided at compile
			// time, and what carries it is the fixup for each call out inside
			// the body. Nothing is emitted.
			if _, known := labels[int(readU32(code, ip+1))]; !known {
				return refuse(why, "catch-target")
			}
			live = append(live, jitHandler{catchIP: int(readU32(code, ip+1)), depth: sp})
			ip += 5

		case OpTryPop:
			if len(live) == 0 {
				return refuse(why, "handler-underflow")
			}
			live = live[:len(live)-1]
			ip++

		case OpCatch:
			// Entering a catch. jitRunAt has put the thrown value in Ret, which
			// is where every other call out leaves its result, so the collector
			// already traces it and nothing new has to be rooted.
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp] = false
			sp++
			ip += 5

		case OpEval:
			// A direct eval site: [this?, callee, arg0..argN-1] -> the result.
			// Whether the callee is still the intrinsic %eval% is a run-time
			// question — the binding can be reassigned — so the helper decides,
			// exactly as the interpreter's case does.
			//
			// A site the compiler marked as being in tail position is refused.
			// When the callee turns out not to be %eval% the language requires a
			// PROPER tail call there, and this template cannot make one — it is
			// a helper, and a helper runs with the compiled frame still on the
			// Go stack. Compiling it as an ordinary call passes every test that
			// checks the value and fails the three that recurse 100,000 deep.
			scopeIdx := int(readU16(code, ip+1))
			if scopeIdx&evalTailFlag != 0 {
				return refuse(why, "eval-in-tail-position")
			}
			argc := int(readU16(code, ip+3))
			need := argc + 1
			if scopeIdx&evalWithThisFlag != 0 {
				need++
			}
			if argc < 0 || sp < need {
				return refuse(why, "stack-underflow")
			}
			a.MovRegImm64(jitRegScratch, uint64(uint32(scopeIdx))|uint64(uint32(argc))<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperEval, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			dst := jitSlot(sp - need)
			a.MovRegMem(dst, jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-need] = false
			sp -= need - 1
			ip += 5

		case OpEnterWith, OpExitWith:
			// The `with` chain lives on the published frame, which the collector
			// already walks — see markFrames. A compiled frame therefore keeps
			// it exactly where an interpreted one does, and the helpers below
			// resolve against the same slice.
			if op == OpEnterWith && sp < 1 {
				return refuse(why, "stack-underflow")
			}
			h := uint32(jitHelperEnterWith)
			if op == OpExitWith {
				h = jitHelperExitWith
			}
			if !jitCallHelper(a, sp, h, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			if op == OpEnterWith {
				sp--
			}
			ip++

		case OpWithGetVar:
			// Reading a free name through the chain. How many operands it
			// produces is in the flags byte rather than in opTable: reference
			// mode also yields the base a paired write goes back through, and
			// the base-only form yields that and nothing else.
			flags := code[ip+7]
			push := 1
			if flags&withFlagRef != 0 && flags&withFlagBaseOnly == 0 {
				push = 2
			}
			if sp+push > jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1))|uint64(readU16(code, ip+5))<<32|uint64(flags)<<48)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperWithGetVar, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			// The base travels in Args[2], which the collector traces, and the
			// value in Ret — the two slots every other helper already uses.
			if flags&withFlagRef != 0 {
				a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffArgs+16)
				kind[sp] = false
				sp++
			}
			if flags&withFlagRef == 0 || flags&withFlagBaseOnly == 0 {
				a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffRet)
				kind[sp] = false
				sp++
			}
			ip += 8

		case OpWithPutVar:
			flags := code[ip+7]
			need := 1
			if flags&withFlagRef != 0 {
				need = 2
			}
			if sp < need {
				return refuse(why, "stack-underflow")
			}
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1))|uint64(readU16(code, ip+5))<<32|uint64(flags)<<48)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperWithPutVar, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			if flags&withFlagRef != 0 {
				// Reference mode is an expression: the value stays, in the slot
				// the base occupied.
				a.MovRegMem(jitSlot(sp-2), jitasm.RegCtx, jitmem.CtxOffRet)
				kind[sp-2] = false
				sp--
			} else {
				sp--
			}
			ip += 8

		case OpWithDelVar:
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperWithDelVar, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp] = false
			sp++
			ip += 5

		case OpToPropkey:
			// ToPropertyKey, which template substitution emits so `${obj}` takes
			// the string path. It can run a toString, so it is a call.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperToPropkey, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-1), jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-1] = false
			ip++

		case OpGlobal:
			// The global object itself. Read through a helper rather than baked in
			// as an immediate: compiled code hangs off the svFunc, and the same
			// function can be reached from more than one realm.
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			if !jitCallHelper(a, sp, jitHelperGlobal, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp] = false
			sp++
			ip++

		case OpDeleteVar:
			// Strict-mode ResolveBinding as much as `delete x`: it answers whether
			// the name is bound, and the OpPutConst after it throws if it was not.
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperDeleteVar, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffRet)
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
			if !jitCallHelper(a, sp, jitHelperPutConst, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-2), jitasm.RegCtx, jitmem.CtxOffRet)
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
			if !jitCallHelper(a, sp, jitHelperGetLength, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-1), jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-1] = false
			ip++

		case OpForIn:
			// The head of a for-in loop: the source is replaced by the keys to
			// walk. A call, because a proxy's ownKeys trap runs here.
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperForIn, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-1), jitasm.RegCtx, jitmem.CtxOffRet)
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
			if !jitCallHelper(a, sp, jitHelperStillEnum, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-2), jitasm.RegCtx, jitmem.CtxOffRet)
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
			if !jitCallHelper(a, sp, jitHelperSetHomeObj, &fixups, deepStack) {
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
			if !jitCallHelper(a, sp, jitHelperDefineMethod, &fixups, deepStack) {
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
			if fn.childFuncs[idx].capturesWith && !jitHasWith(fn) {
				// The child resolves its free names against a chain it takes from
				// here. A compiled frame has one only if this function builds it,
				// so without a `with` of its own there is nothing to hand over.
				return refuse(why, "closure/captures-with")
			}
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegImm64(jitRegScratch, uint64(idx))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperClosure, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffRet)
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
			if !jitCallHelper(a, sp, jitHelperPutUpval, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			if op == OpSetUpval {
				a.MovRegMem(jitSlot(sp-1), jitasm.RegCtx, jitmem.CtxOffRet)
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
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			if code[ip+1] == 1 {
				a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffFnVal)
			} else {
				if !jitCallHelper(a, sp, jitHelperArguments, &fixups, deepStack) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffRet)
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
			if !jitCallHelper(a, sp, jitHelperDefineField, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-2), jitasm.RegCtx, jitmem.CtxOffRet)
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
			if jitWindowShifts(sp, sp-1) {
				return refuse(why, "branch-across-window")
			}
			jitEmitNullishFlags(a, jitSlot(sp-1))
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
			if !jitCallHelper(a, sp, jitHelperThrow, &fixups, deepStack) {
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
			if !jitCallHelper(a, sp, jitHelperArray, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-n), jitasm.RegCtx, jitmem.CtxOffRet)
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
			if !jitCallBinary(a, sp, op, jitHelperArith, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-2), jitasm.RegCtx, jitmem.CtxOffRet)
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
			jitEmitNullishTest(a, jitSlot(sp-1), jitSlot(sp-1))
			kind[sp-1] = false
			ip++

		case OpTypeof:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperTypeof, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-1), jitasm.RegCtx, jitmem.CtxOffRet)
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
			if !jitCallHelper(a, sp, h, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-2), jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-2] = false
			sp--
			ip++

		case OpObject:
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			if !jitCallHelper(a, sp, jitHelperObject, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp] = false
			sp++
			ip++

		case OpRegexp:
			if sp < 2 {
				return refuse(why, "stack-underflow")
			}
			if !jitCallHelper(a, sp, jitHelperRegexp, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-2), jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-2] = false
			sp--
			ip++

		case OpGetGlobalUndef:
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegImm64(jitRegScratch, uint64(readU32(code, ip+1)))
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperGlobalUndef, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffRet)
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
			if !jitCallHelper(a, sp, jitHelperInstanceof, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp-2), jitasm.RegCtx, jitmem.CtxOffRet)
			kind[sp-2] = false
			sp--
			ip += 3

		case OpBnot:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			r := jitSlot(sp - 1)
			generic := !kind[sp-1]
			var slow, done *jitasm.Label
			if generic {
				slow, done = a.NewLabel(), a.NewLabel()
				jitEmitNumber(a, r, slow)
			}
			if !jitToInt32(a, r, r, sp, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			a.Not32Reg(r)
			jitFromInt32(a, r, true)
			if generic {
				a.Jmp(done)
				a.Bind(slow)
				if !jitCallUnary(a, sp, op, &fixups, deepStack) {
					return refuse(why, "stack-too-deep")
				}
				a.MovRegMem(r, jitasm.RegCtx, jitmem.CtxOffRet)
				a.Bind(done)
			}
			// An int32 on the fast side; on the other, whatever the runtime
			// returned — and `~` of a BigInt is a BigInt. Saying "a Number
			// either way" here is what made `~~1n` answer -1: the second `~`
			// took the unguarded path over a value that was not one.
			kind[sp-1] = !generic
			ip++

		case OpNeg:
			if sp < 1 {
				return refuse(why, "stack-underflow")
			}
			r := jitSlot(sp - 1)
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
				if !jitCallUnary(a, sp, op, &fixups, deepStack) {
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
			r := jitSlot(sp - 1)
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
				if !jitCallUnary(a, sp, op, &fixups, deepStack) {
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
			r := jitSlot(sp - 1)
			if kind[sp-1] {
				// A Number is falsy when it is zero or a NaN, and UCOMISD sets
				// the zero flag for both — equal, or unordered. So the flag is
				// `!x` already, for either signed zero.
				a.XorRegReg(jitRegScratch, jitRegScratch)
				a.MovqXReg(jitasm.X1, jitRegScratch)
				a.MovqXReg(jitasm.X0, r)
				a.UcomisdXX(jitasm.X0, jitasm.X1)
				jitFBoolean(a, jitasm.FCondE, r)
			} else {
				// ToBoolean of anything else is a different question — the empty
				// string is false and every object is true — and the same one the
				// truthy branch answers, so it answers this too and materialises
				// the Boolean rather than branching on it.
				if !jitCallUnary(a, sp, op, &fixups, deepStack) {
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
			x, y := jitSlot(sp-2), jitSlot(sp-1)
			generic := !kind[sp-1] || !kind[sp-2]
			negate := op == OpNe || op == OpSne
			l, whenTrue, after, fused := jitFuse(code, labels, ip+1)
			if fused && jitWindowShifts(sp, sp-2) {
				return refuse(why, "branch-across-window")
			}

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
				if !jitCallBinary(a, sp, op, jitHelperEquals, &fixups, deepStack) {
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
			recv := jitSlot(sp - 1)
			slow := a.NewLabel()
			done := a.NewLabel()
			if icx != icNoSlot && icx < len(ics) && sp+jitICGetSpareRegs <= jitStackWindow {
				jitEmitICGet(a, recv, jitSlot(sp), jitSlot(sp+1), jitSlot(sp+2),
					jitICWayAddr(ics, icx), jitEpochAddr(), jitICHitAddr(), slow, done)
			}
			a.Bind(slow)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+16, recv)
			// The name and the cache site, both constants for this site, packed
			// into the one argument slot the helper protocol leaves untraced.
			a.MovRegImm64(jitRegScratch, uint64(idx)|uint64(uint32(icx))<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperGetField, &fixups, deepStack) {
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
			if sp >= jitMaxDepth {
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
			dst := jitSlot(sp)
			a.MovRegReg(dst, jitSlot(sp-1))
			slow := a.NewLabel()
			done := a.NewLabel()
			if icx != icNoSlot && icx < len(ics) && sp+jitICGlobalSpareRegs <= jitStackWindow {
				jitEmitICGet(a, dst, jitSlot(sp+1), jitSlot(sp+2), jitSlot(sp+3),
					jitICWayAddr(ics, icx), jitEpochAddr(), jitICHitAddr(), slow, done)
			}
			a.Bind(slow)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+16, dst)
			a.MovRegImm64(jitRegScratch, uint64(idx)|uint64(uint32(icx))<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperGetField, &fixups, deepStack) {
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
			site := nextSite
			nextSite++
			sites[site].argc, sites[site].method = uint16(argc), true
			done := jitBeginCall(a, sites, site, sp, argc, true, &fixups, deepStack)
			a.MovRegImm64(jitRegScratch, uint64(uint32(argc))|uint64(site+1)<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperCallMethod, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			dst := jitSlot(sp - argc - 2)
			a.MovRegMem(dst, jitasm.RegCtx, jitmem.CtxOffRet)
			jitEndCall(a, done)
			kind[sp-argc-2] = false
			sp -= argc + 1
			ip += 3

		case OpTailCall, OpTailCallMethod:
			// `return f(args)` in strict code, which the language requires not
			// grow the stack. The interpreter honours that by resetting its own
			// frame and jumping back to the top; compiled code cannot, because
			// the frame it would have to reset is a block of machine code with
			// its operands in registers.
			//
			// So this is an exit rather than a helper. The operands go to the
			// spill area exactly as CALL's do — [this?, callee, arg0..argN-1] —
			// and jitRunAt reads them out after this frame is gone, which is the
			// one point where a tail call can be made without adding to the Go
			// stack. What it does with them is the trampoline.
			argc := int(readU16(code, ip+1))
			need := argc + 1
			if op == OpTailCallMethod {
				need++
			}
			if argc < 0 || sp < need {
				return refuse(why, "stack-underflow")
			}
			if sp > jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			tb, to := jitStackBase(a, deepStack)
			for i := jitWindowBase(sp); i < sp; i++ {
				a.MovMemReg(tb, to+int32(8*i), jitSlot(i))
			}
			a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffStackN, uint32(sp))
			// The count, and whether a receiver sits below the callee, in the
			// one immediate the exit protocol carries.
			recv := uint64(0)
			if op == OpTailCallMethod {
				recv = 1
			}
			a.MovRegImm64(jitRegScratch, uint64(uint32(argc))|recv<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitTailCall))
			a.MovRegImm64(jitasm.RAX, jitmem.ExitTailCall)
			a.Ret()
			sp = 0
			returned = true
			ip += 3

		case OpInsert3:
			// obj prop a -> a obj prop a. A stack shuffle and nothing else, which
			// in a positional register assignment is three moves — and it is all
			// Richards has left, 13.6 million interpreted instructions, because
			// it is what `a[i] = v` used as an expression begins with.
			if sp < 3 {
				return refuse(why, "stack-underflow")
			}
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			obj, prop, val := jitSlot(sp-3), jitSlot(sp-2), jitSlot(sp-1)
			a.MovRegReg(jitSlot(sp), val)
			a.MovRegReg(val, prop)
			a.MovRegReg(prop, obj)
			a.MovRegReg(obj, jitSlot(sp))
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
			if !jitCallHelper(a, sp, jitHelperPutElem, &fixups, deepStack) {
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
			if !jitCallHelper(a, sp, jitHelperPutGlobal, &fixups, deepStack) {
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
			if !jitCallHelper(a, sp, jitHelperNew, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			dst := jitSlot(sp - argc - 1)
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
			site := nextSite
			nextSite++
			sites[site].argc = uint16(argc)
			done := jitBeginCall(a, sites, site, sp, argc, false, &fixups, deepStack)
			a.MovRegImm64(jitRegScratch, uint64(uint32(argc))|uint64(site+1)<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperCall, &fixups, deepStack) {
				return refuse(why, "stack-too-deep")
			}
			// The result replaces the callee, which is the deepest of the slots
			// this consumed.
			dst := jitSlot(sp - argc - 1)
			a.MovRegMem(dst, jitasm.RegCtx, jitmem.CtxOffRet)
			jitEndCall(a, done)
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
			recv, key := jitSlot(sp-2), jitSlot(sp-1)
			slow := a.NewLabel()
			done := a.NewLabel()
			if sp+jitICElemSpareRegs <= jitStackWindow {
				jitEmitGetElem(a, recv, key, jitSlot(sp), jitSlot(sp+1), slow, done)
			}
			a.Bind(slow)
			if !jitCallHelper(a, sp, jitHelperGetElem, &fixups, deepStack) {
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
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			dst := jitSlot(sp)
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
			if !jitCallHelper(a, sp, jitHelperDeadZone, &fixups, deepStack) {
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
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			a.MovRegMem(jitSlot(sp), jitasm.RegCtx, jitmem.CtxOffThis)
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
			if sp >= jitMaxDepth {
				return refuse(why, "stack-too-deep")
			}
			idx := readU32(code, ip+1)
			if int(idx) >= len(fn.constNames) || idx > 0x7FFFFFFF {
				return refuse(why, "shape")
			}
			icx := int(readU16(code, ip+5))
			dst := jitSlot(sp)
			slow := a.NewLabel()
			done := a.NewLabel()
			if icx != icNoSlot && icx < len(ics) && sp+jitICGlobalSpareRegs <= jitStackWindow {
				a.MovRegMem(jitRegScratch, jitasm.RegCtx, jitmem.CtxOffHost)
				a.MovRegMem(dst, jitRegScratch, int32(jitOffRTGlobal))
				jitEmitICGet(a, dst, jitSlot(sp+1), jitSlot(sp+2), jitSlot(sp+3),
					jitICWayAddr(ics, icx), jitEpochAddr(), jitICGlobalHitAddr(), slow, done)
			}
			a.Bind(slow)
			a.MovRegImm64(jitRegScratch, uint64(idx)|uint64(uint32(icx))<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperGetGlobal, &fixups, deepStack) {
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
			recv, val := jitSlot(sp-2), jitSlot(sp-1)
			slow := a.NewLabel()
			done := a.NewLabel()
			if icx != icNoSlot && icx < len(ics) && sp+jitICPutSpareRegs <= jitStackWindow {
				jitEmitICPut(a, recv, val, jitSlot(sp), jitSlot(sp+1),
					jitICWayAddr(ics, icx), jitEpochAddr(), slow, done)
			}
			a.Bind(slow)
			// Only the name and the site go in an argument. The receiver and the
			// value are already where the helper wants them: spilling roots the
			// whole operand stack, and these are the top two of it.
			a.MovRegImm64(jitRegScratch, uint64(idx)|uint64(uint32(icx))<<32)
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffArgs+24, jitRegScratch)
			if !jitCallHelper(a, sp, jitHelperPutField, &fixups, deepStack) {
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
			// Also in a register, for the one caller that is not Go. A compiled
			// call site reads the answer from here rather than from the context,
			// which is a load it does not make on the path this exists to make
			// short. Harmless to the runtime, which reads Ret as it always did.
			a.MovRegReg(jitRegReturn, jitasm.RAX)
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
			a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffRet, jitSlot(sp-1))
			if jitSlot(sp-1) != jitRegReturn {
				a.MovRegReg(jitRegReturn, jitSlot(sp-1))
			}
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
		// The other edge of the window, and the check. A template that ends the
		// frame — RETURN, THROW, a tail call — sets sp to zero and says so, and
		// what it leaves is not a depth anything downstream propagates.
		if !returned {
			if sp != spAfter {
				return refuse(why, "stack-effect-disagrees")
			}
			if spAfter < spBefore {
				jitRefillSlots(a, spBefore, spAfter, effPush, deepStack)
			}
		}
		// Every call out this instruction emitted belongs to the handler that
		// was in force when it ran. Stamped here rather than passed down through
		// sixty-five templates, none of which has any other reason to know.
		if len(live) > 0 && len(fixups) > fixupsBefore {
			h := live[len(live)-1]
			if _, known := labels[h.catchIP]; !known {
				return refuse(why, "catch-target")
			}
			stub, ok := catchStubs[h.catchIP]
			if !ok {
				stub = a.NewLabel()
				catchStubs[h.catchIP] = stub
			}
			for i := fixupsBefore; i < len(fixups); i++ {
				if fixups[i].catch == nil {
					fixups[i].catch = stub
				}
			}
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

	for h, stub := range catchStubs {
		l, known := labels[h]
		if !known {
			return refuse(why, "catch-target")
		}
		a.Bind(stub)
		a.MovRegMem(jitRegLocals, jitasm.RegCtx, jitmem.CtxOffArgs)
		a.MovRegImm64(jitRegGuard, uint64(nanboxPrefix))
		a.Jmp(l)
	}

	// The entry a compiled call site uses, which does for itself what jitRunAt
	// does for a frame the runtime enters. Emitted only for a function that can
	// live in a context alone — see jitMachineCallable — and for one whose
	// operand stack is the context's, since a call site has nowhere to put
	// another.
	var mentry *jitasm.Label
	if !deepStack && jitMachineCallable(fn) {
		nargs := fn.paramCount
		if nargs > fn.maxLocals {
			nargs = fn.maxLocals
		}
		mentry = a.NewLabel()
		a.Bind(mentry)
		jitEmitMachineEntry(a, fn, nargs, prologue, bail)
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
	c := &jitCode{block: block, entry: block.AddrAt(prologue.Offset()), slots: maxDepth, sites: sites}
	if mentry != nil {
		c.mentry = block.AddrAt(mentry.Offset())
	}
	for _, f := range fixups {
		if f.catch == nil {
			continue
		}
		if c.catchAt == nil {
			c.catchAt = map[uintptr]uintptr{}
		}
		c.catchAt[block.AddrAt(f.label.Offset())] = block.AddrAt(f.catch.Offset())
	}
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
		OpSetHomeObj, OpForIn, OpHasPrivate, OpGetLength,
		OpTailCall, OpTailCallMethod, OpTryPush, OpTryPop, OpCatch,
		OpEnterWith, OpExitWith, OpWithGetVar, OpWithPutVar, OpWithDelVar, OpEval:
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
		case OpJmp, OpJmpFalse, OpJmpTrue, OpJmpNotNullish, OpTryPush:
			// TRY_PUSH's operand is where a throw lands, which is a target the
			// machine never jumps to and control reaches all the same.
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
		a.Jfcc(jitasm.FCondUnordered, skip)
		switch op {
		case OpLt:
			a.Jfcc(jitasm.FCondB, target)
		case OpLe:
			a.Jfcc(jitasm.FCondBE, target)
		case OpGt:
			a.Jfcc(jitasm.FCondA, target)
		case OpGe:
			a.Jfcc(jitasm.FCondAE, target)
		}
		a.Bind(skip)
		return
	}
	a.Jfcc(jitasm.FCondUnordered, target) // unordered: the comparison is false
	switch op {
	case OpLt:
		a.Jfcc(jitasm.FCondAE, target)
	case OpLe:
		a.Jfcc(jitasm.FCondA, target)
	case OpGt:
		a.Jfcc(jitasm.FCondBE, target)
	case OpGe:
		a.Jfcc(jitasm.FCondB, target)
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
	jitHelperEnterWith    = 43
	jitHelperExitWith     = 44
	jitHelperWithGetVar   = 45
	jitHelperWithPutVar   = 46
	jitHelperWithDelVar   = 47
	jitHelperEval         = 48
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
func jitCallHelper(a *jitasm.Asm, sp int, helper uint32, fixups *[]jitResumeFixup, deep bool) bool {
	if sp > jitMaxDepth {
		return false
	}
	resume := a.NewLabel()

	// Only the window: everything below it is already in these slots, which is
	// where it lives between instructions rather than only across a call.
	spillBase, spillOff := jitStackBase(a, deep)
	for i := jitWindowBase(sp); i < sp; i++ {
		a.MovMemReg(spillBase, spillOff+int32(8*i), jitSlot(i))
	}
	// SpillN is what tells the collector how much of Spill to trace. Writing it
	// before the exit rather than after is the whole point: between the RET
	// below and the helper returning, this context is the only reference to
	// those values.
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffStackN, uint32(sp))
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
	fillBase, fillOff := jitStackBase(a, deep)
	for i := jitWindowBase(sp); i < sp; i++ {
		a.MovRegMem(jitSlot(i), fillBase, fillOff+int32(8*i))
	}
	// Back to zero, and the slots below the window keep holding operands with
	// nothing pointing at them. Sound because the only places compiled code can
	// be collected at are this one, where SpillN covers the whole stack, and a
	// back edge, which the emitter refuses to emit with operands live.
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffStackN, 0)
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
	return c.jitRunAt(rt, fn, cl, fnVal, args, locals, this, c.entry, true)
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
	// mayTail is false: the frame this is taking over belongs to an interpreted
	// invocation that is still on the Go stack, and the tail-call trampoline
	// works by reusing that frame. Its openUpvals point into the locals slab and
	// handing the slab to a tail callee would leave them pointing at the
	// callee's variables. A tail call reached this way makes an ordinary call
	// instead, which costs one Go frame and happens at most once per frame,
	// because the next invocation enters through jitRun rather than here.
	return c.jitRunAt(rt, fn, cl, fnVal, args, locals, this, pc, false)
}

// jitRunAt runs compiled code and, when it exits to make a tail call, runs the
// callee too — which is the only way compiled code can make one.
//
// mayTail says whether the JS frame at rt.frameDepth is this call's to reuse.
// It is what makes the tail call proper: the callee takes over the frame the
// caller had, so a chain of them occupies one frame however long it runs. See
// the ExitTailCall arm for what it covers and what it does not.
func (c *jitCode) jitRunAt(rt *Runtime, fn *svFunc, cl *closure, fnVal Value, args, locals []Value, this Value, entry uintptr, mayTail bool) (Value, *ThrowError, bool) {
	if len(locals) == 0 {
		return mkundef(), nil, false
	}
	pc := entry
	// A function containing a direct eval carries a variable object: the eval's
	// `var` declarations that name nothing this function declared are created on
	// it, and this function's free names — compiled as with-routed accesses —
	// find them there. Innermost of the chain, outside this frame's locals, and
	// null-prototyped so nothing from Object.prototype can shadow an outer name.
	//
	// A function compiled lexically inside a `with` resolves its free names
	// against the chain it captured, so a frame running one starts with that
	// chain rather than an empty one — the same line runFrameBody opens with.
	//
	// Only on a normal entry. An OSR entry takes over a frame that is already
	// running, whose chain is whatever it has entered since, and syncFrame has
	// already published it.
	if entry == c.entry && (fn.dynamicVars || (cl != nil && len(cl.capturedWith) > 0)) {
		if f := rt.jitFrame(); f != nil {
			f.withStack = f.withStack[:0]
			if cl != nil {
				f.withStack = append(f.withStack, cl.capturedWith...)
			}
			if fn.dynamicVars {
				f.varObj = rt.newObject(mknull())
				f.withStack = append(f.withStack, f.varObj)
			}
		}
	}
	// Whether a tail call has already been taken in this frame, which changes
	// what a decline downstream is allowed to mean. See the default arm below.
	tailed := false
	// This frame's place in the context chain. Deeper entries belong to calls
	// compiled code made for itself, and this loop drives those too — there is
	// nowhere else for them to be driven from, because the machine frames they
	// ran on are gone by the time control is back here.
	base := rt.jitDepth
	for {
		// The context is rooted for as long as compiled code can be suspended in a
		// helper holding values nothing else refers to — see markRoots — and it
		// comes from the chain rather than from an allocation, so that a compiled
		// call site can reach the next one without asking.
		ctx := rt.jitCtxAt(base)
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
		// The frame's operand stack, sized to what this function needs. Reused
		// across frames at this depth, so a function with a large one leaves it
		// behind rather than allocating again.
		ctx.EnsureStack(c.slots)
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
		// The runtime entered this one, so its locals are in the frame slab where
		// markFrames finds them, and it belongs to no call site.
		ctx.Site = nil
		ctx.NLocals = 0
		rt.jitDepth = base + 1

		// Popped explicitly on each way out rather than by a deferred closure. A
		// defer here is a call per compiled frame entry, and this function has three
		// exits and no panic path of its own — a Go panic below is a bug in the
		// engine, and the process is going down with it either way.
		// A frame that captured a local owns cells that only it can have created,
		// and the next frame at this depth must not find them. Cleared on every way
		// out, and the map itself is only ever allocated by a capture.
		rt.dropOpenUpvals(rt.frameDepth)
		tail := false
		// The frame to enter, which is this one until a compiled call site opens
		// one below it.
		run, runPC := ctx, pc
		for !tail {
			// A fresh budget of machine frames: whatever nesting was on the
			// goroutine stack before is gone, because getting back here is what
			// unwound it.
			run.Nest = 0
			jitmem.Enter(runPC, run)
			// The locals slice reaches compiled code as an integer, so nothing in
			// the call graph keeps it reachable for the collector.
			runtime.KeepAlive(locals)

			// Which frame stopped. A compiled call does not come back through here,
			// so the innermost live frame can be deeper than the one entered above
			// — and everything between the two has saved its operands and its
			// resume address on the way out.
			d := rt.jitDepth - 1
			cur := ctx
			if d != base {
				cur = rt.jitFrames[d]
			}

			switch cur.Exit {
			case jitmem.ExitReturn:
				if d > base {
					// Hand the answer to the caller and put it back on the machine.
					// The depth comes down here rather than in the resume path,
					// because the caller is entered next and would otherwise be
					// running one frame above where it thinks it is.
					rt.jitDepth = d
					up := rt.jitFrames[d-1]
					up.Ret = cur.Ret
					run, runPC = up, up.Resume
					continue
				}
				rt.jitDepth = base
				rt.dropOpenUpvals(rt.frameDepth)
				return Value(ctx.Ret), nil, true
			case jitmem.ExitPreempt:
				// Being here is the safepoint: this is ordinary Go, so the runtime
				// can collect and preempt before the loop is re-entered.
				cur.Args[1] = jitFuel
				run, runPC = cur, cur.Resume
			case jitmem.ExitHelper:
				// Whose frame this is. A frame compiled code built is identified
				// by the site that built it, which is what it published; the one
				// the runtime entered is identified by this call's own arguments.
				// Read here rather than above because this is the only arm that
				// needs it, and the arms that do not are the ones taken most.
				var e *ThrowError
				if d == base {
					e = jitHelper(rt, fn, cl, args, locals, cur, c)
				} else if s := jitCtxSite(cur); s != nil {
					e = jitHelper(rt, s.fn, s.cl, nil, jitCtxLocals(cur), cur, s.code)
				} else {
					rt.jitDepth = base
					rt.dropOpenUpvals(rt.frameDepth)
					return mkundef(), rt.typeError("JIT frame chain"), true
				}
				if e == nil {
					run, runPC = cur, cur.Resume
					break
				}
				// A throw from inside a try lands in its catch rather than
				// leaving the frame. Which catch is a compile-time answer:
				// catchAt is keyed by the address the call site would have
				// resumed at, which is the only thing identifying it that
				// survives the trip through machine code — and the frames a
				// compiled call opened are searched the same way, outwards,
				// because a throw in one of them passes through the sites that
				// called it.
				//
				// The catch enters with an empty operand stack — the emitter
				// refuses a handler installed at any other depth, because the
				// operands the throw destroyed were in registers — so nothing
				// has to be restored, and the thrown value travels in Ret,
				// where CATCH reads it and the collector already traces it.
				if at, to, ok := rt.jitCatch(base, d, c, e); ok {
					run, runPC = at, to
					break
				}
				rt.jitDepth = base
				rt.dropOpenUpvals(rt.frameDepth)
				return mkundef(), e, true
			case jitmem.ExitTailCall:
				if d > base {
					// Not reachable: jitMachineCallable refuses a body with a
					// tail call in it, precisely because a tail call takes over a
					// frame and a frame built by a call site is not one to take.
					// Handled rather than asserted, as an ordinary call one level
					// deeper, so that widening that predicate cannot turn into a
					// wrong answer.
					v, e := rt.jitRunTail(cur)
					if e != nil {
						if at, to, ok := rt.jitCatch(base, d, c, e); ok {
							run, runPC = at, to
							break
						}
						rt.jitDepth = base
						rt.dropOpenUpvals(rt.frameDepth)
						return mkundef(), e, true
					}
					rt.jitDepth = d
					up := rt.jitFrames[d-1]
					up.Ret = uint64(v)
					run, runPC = up, up.Resume
					continue
				}
				tail = true
			default:
				if d > base {
					// The callee declined the arguments the call site handed it,
					// at its entry guard and so before it had written anything.
					// The frame is discarded and the call made again the long way,
					// which is where every shape this path cannot take is handled
					// — a receiver that has to be boxed, a parameter that is not a
					// Number after all.
					v, e := rt.jitRedoCall(d)
					if e != nil {
						if at, to, ok := rt.jitCatch(base, d-1, c, e); ok {
							run, runPC = at, to
							break
						}
						rt.jitDepth = base
						rt.dropOpenUpvals(rt.frameDepth)
						return mkundef(), e, true
					}
					rt.jitDepth = d
					up := rt.jitFrames[d-1]
					up.Ret = uint64(v)
					run, runPC = up, up.Resume
					continue
				}
				rt.jitDepth = base
				rt.dropOpenUpvals(rt.frameDepth)
				// Declined its arguments at the entry guard, which is safe to
				// report as "nothing has happened, run me in the interpreter
				// instead" only while nothing has. After a tail call something
				// has: this frame belongs to the callee, and the caller it took
				// over from has already run to its return. Reporting a decline
				// then makes the caller's caller run the ORIGINAL function a
				// second time — Octane's pdfjs read the same stream twice and
				// called the document malformed.
				//
				// So the callee is run here instead, through the general path a
				// decline is asking for. One Go frame deeper than a proper tail
				// call, which is the guarantee this gives up in exchange for
				// being right, and only when a compiled callee refuses the
				// arguments a tail call brought it.
				if tailed {
					v, e := rt.callValue(fnVal, this, args)
					return v, e, true
				}
				return mkundef(), nil, false
			}
		}

		// A proper tail call. The operands are laid out as CALL's are —
		// [this?, callee, arg0..argN-1] at the top of the spill area — and the
		// immediate carries the count and whether a receiver is there.
		argc := int(uint32(ctx.Args[3]))
		need := argc + 1
		if ctx.Args[3]>>32 != 0 {
			need++
		}
		sn := int(ctx.StackN)
		if argc < 0 || sn < need {
			rt.jitDepth = base
			rt.dropOpenUpvals(rt.frameDepth)
			return mkundef(), rt.typeError("JIT operand stack"), true
		}
		cbase := sn - need
		tailThis := mkundef()
		if ctx.Args[3]>>32 != 0 {
			tailThis = jitSlotAt(ctx, cbase)
			cbase++
		}
		callee := jitSlotAt(ctx, cbase)
		// Copied rather than borrowed. Everywhere else a callee reads its arguments
		// out of the caller's spill area while the caller waits; here the caller is
		// finished and its context goes back on the free list on the next line.
		callArgs := jitCopyArgs(jitSpillArgs(ctx, cbase+1, argc))

		rt.jitDepth = base
		rt.dropOpenUpvals(rt.frameDepth)

		// The callee this can take over the frame for: an ordinary JS function with
		// compiled code of its own. That covers a function tail-calling itself,
		// which is what the language's guarantee is nearly always about and what
		// every tail-call test in test262 is about.
		//
		// Anything else is an ordinary call one frame deeper, and the guarantee is
		// weaker there than the interpreter's: a compiled function alternating tail
		// calls with one the tier permanently refuses grows the Go stack by a frame
		// per bounce. Closing that needs the request handed back to the interpreter
		// so its own trampoline can take it, which is a change to four signatures
		// for a shape nothing measured has produced.
		var next *svFunc
		var nextCl *closure
		if mayTail {
			next, nextCl = rt.jitResolveCallee(callee)
			if next != nil && (next.jit.code == nil || next.isGenerator || next.isAsync ||
				next.isClassCtor || next.maxLocals == 0 || !jitEligible(next)) {
				next = nil
			}
		}
		if next == nil {
			v, e := rt.callValue(callee, tailThis, callArgs)
			return v, e, true
		}
		// Entering a function is a check point for the host interrupt, and a tail
		// call is the one entry that never reaches the depth check — so unbounded
		// tail recursion has nothing else to stop it.
		if rt.interruptPending() {
			return mkundef(), rt.terminated(), true
		}

		// Everything jitCallCompiled does on entry, at the same depth rather than
		// one deeper. Reusing the depth is the whole of the optimisation.
		rt.frameStrict = next.isStrict
		f := rt.publishFrame(rt.frameDepth)
		f.args, f.thisVal, f.fnVal = callArgs, tailThis, callee
		f.fn, f.cl = next, nextCl
		rt.maybeCollect()
		if !next.isStrict {
			if tailThis.IsNullish() {
				tailThis = rt.global
			} else if !tailThis.IsObjectType() && tailThis.Type() != TTypedArray {
				tailThis, _ = rt.toObjectValue(tailThis)
			}
		}
		rt.pendingNewTarget = mkundef()
		locals = rt.frameLocals(rt.frameDepth, next.maxLocals)
		for i := 0; i < next.paramCount && i < next.maxLocals && i < len(callArgs); i++ {
			locals[i] = callArgs[i]
		}
		f.locals = locals

		fn, cl, fnVal, this, args = next, nextCl, callee, tailThis, callArgs
		c = next.jit.code
		pc = c.entry
		tailed = true
	}
}

// jitCatch is where a throw raised in the frame at depth d is caught, searching
// outwards through the frames a compiled call site opened and stopping at the
// one the runtime entered.
//
// The frames in between are popped as it goes: each is a call that will not
// return, and leaving them on the chain would hand the collector operand stacks
// belonging to frames that no longer exist.
//
// A control-flow signal is not a throw: `break`, `continue` and a return through
// a finally travel as one, and a catch must not take them. The interpreter's
// unwind draws the same line.
func (rt *Runtime) jitCatch(base, d int, outer *jitCode, e *ThrowError) (*jitmem.ExecContext, uintptr, bool) {
	for ; d >= base; d-- {
		cur := rt.jitFrames[d]
		code := outer
		if d > base {
			code = nil
			if s := jitCtxSite(cur); s != nil {
				code = s.code
			}
		}
		if !e.control && code != nil {
			if catch, ok := code.catchAt[cur.Resume]; ok {
				rt.jitDepth = d + 1
				cur.Ret = uint64(e.Value)
				cur.StackN = 0
				return cur, catch, true
			}
		}
	}
	return nil, 0, false
}

// jitRedoCall makes the call the frame at depth d was entered for again, the
// long way, because that frame declined the arguments it was given.
//
// Sound at any point, because a decline happens at the entry guard: the frame
// has written nothing, and the arguments are still in the caller's spill area
// where the call site left them.
func (rt *Runtime) jitRedoCall(d int) (Value, *ThrowError) {
	up := rt.jitFrames[d-1]
	bind := jitCtxSite(rt.jitFrames[d])
	rt.jitDepth = d
	if bind == nil || bind.site == nil {
		return mkundef(), rt.typeError("JIT frame chain")
	}
	site := bind.site
	// The site is retired rather than merely missed: whatever the entry stub
	// could not settle about this call it will not settle next time either,
	// unless the callee is rebuilt — which is what noting the decline arranges.
	jitRetireSite(site)
	if bind.fn != nil {
		jitNoteDecline(bind.fn)
	}
	argc := int(site.argc)
	depth := argc + 1
	if site.method {
		depth++
	}
	n := int(up.StackN)
	if argc < 0 || n < depth {
		return mkundef(), rt.typeError("JIT operand stack")
	}
	b := n - depth
	thisArg := mkundef()
	if site.method {
		thisArg = jitSlotAt(up, b)
		b++
	}
	callee := jitSlotAt(up, b)
	return rt.callValue(callee, thisArg, jitCopyArgs(jitSpillArgs(up, b+1, argc)))
}

// jitRunTail makes a tail call from a frame a compiled call site opened, as an
// ordinary call. See the ExitTailCall arm: nothing reaches this today, and what
// it costs if something ever does is the tail-call guarantee rather than an
// answer.
func (rt *Runtime) jitRunTail(cur *jitmem.ExecContext) (Value, *ThrowError) {
	argc := int(uint32(cur.Args[3]))
	need := argc + 1
	if cur.Args[3]>>32 != 0 {
		need++
	}
	n := int(cur.StackN)
	if argc < 0 || n < need {
		return mkundef(), rt.typeError("JIT operand stack")
	}
	b := n - need
	thisArg := mkundef()
	if cur.Args[3]>>32 != 0 {
		thisArg = jitSlotAt(cur, b)
		b++
	}
	callee := jitSlotAt(cur, b)
	return rt.callValue(callee, thisArg, jitCopyArgs(jitSpillArgs(cur, b+1, argc)))
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
	return jitFrameStack(ctx)[base : base+argc : base+argc]
}

// jitCopyArgs is the copy the general path still needs, for a callee that may
// retain or write through the array it is given.
func jitCopyArgs(window []Value) []Value {
	args := make([]Value, len(window))
	copy(args, window)
	return args
}

// jitHelper runs what compiled code asked for.
//
// code is the block the asking frame is running, which is not always fn's
// current one — a function rebuilt after too many declines leaves the block a
// suspended frame is inside still mapped. Only the call arm reads it, to fill
// the site the call came through.
func jitHelper(rt *Runtime, fn *svFunc, cl *closure, args, locals []Value, ctx *jitmem.ExecContext, code *jitCode) *ThrowError {
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
		if jitStats.enabled && ctx.Helper != jitHelperNew {
			jitStats.callSlow++
		}
		argc := int(uint32(ctx.Args[3]))
		depth := argc + 1
		if ctx.Helper == jitHelperCallMethod {
			depth++
		}
		n := int(ctx.StackN)
		if argc < 0 || n < depth {
			return rt.typeError("JIT operand stack")
		}
		base := n - depth
		thisArg := mkundef()
		if ctx.Helper == jitHelperCallMethod {
			thisArg = Value(jitSlotAt(ctx, base))
			base++
		}
		callee := jitSlotAt(ctx, base)
		// The arguments as they already sit in the spill area, without copying
		// them anywhere. See jitSpillArgs for why that is sound for a compiled
		// callee and not in general.
		window := jitSpillArgs(ctx, base+1, argc)
		// `f.call(x, …)` is a call to f, and treating it as one is the whole
		// point of seeing through it here. Compiled as CALL_METHOD on f with the
		// built-in `call` as the callee, it would otherwise run two general
		// frames — one for the built-in and one for f — where f alone needs a
		// compiled one. DeltaBlue reaches its superclass constructors this way
		// and spent 15% of its run in the pair.
		//
		// Identity against the intrinsic, not a name: `f.call` is only this
		// function if nothing has replaced Function.prototype.call, and a
		// script that has replaced it must see its own. One level, because a
		// chain of them (`call.call.call`) is a curiosity rather than a shape
		// worth unrolling, and the general path still handles it.
		seenThrough := false
		if ctx.Helper == jitHelperCallMethod && callee == rt.funcProtoCall && rt.funcProtoCall != 0 && rt.isCallable(thisArg) {
			callee, thisArg = thisArg, mkundef()
			if len(window) > 0 {
				thisArg, window = window[0], window[1:]
			}
			seenThrough = true
		}
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
			} else if !seenThrough {
				// It went to a compiled function through the runtime, which is
				// what this site exists to stop doing. Everything the machine
				// path needs is resolved right here — see jitFillSite for the
				// conditions it still has to hold to.
				if idx := int(ctx.Args[3] >> 32); idx > 0 && code != nil && idx <= len(code.sites) {
					cfn, ccl := rt.jitResolveCallee(callee)
					rt.jitFillSite(&code.sites[idx-1], fn.isStrict, callee, cfn, ccl)
				}
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
		n := int(ctx.StackN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		v, e := rt.getElement(Value(jitSlotAt(ctx, n-2)), Value(jitSlotAt(ctx, n-1)))
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
		n := int(ctx.StackN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		obj, val := Value(jitSlotAt(ctx, n-2)), Value(jitSlotAt(ctx, n-1))
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
		n := int(ctx.StackN)
		if n < 3 {
			return rt.typeError("JIT operand stack")
		}
		ok, e := rt.setElementR(Value(jitSlotAt(ctx, n-3)), Value(jitSlotAt(ctx, n-2)), Value(jitSlotAt(ctx, n-1)))
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
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		val := Value(jitSlotAt(ctx, n-1))
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
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		v, e := rt.jsUnary(Opcode(uint32(ctx.Args[3])), Value(jitSlotAt(ctx, n-1)))
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
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		ctx.Ret = 0
		if rt.toBoolean(Value(jitSlotAt(ctx, n-1))) {
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
		n := int(ctx.StackN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		r, e := rt.jsInstanceof(Value(jitSlotAt(ctx, n-2)), Value(jitSlotAt(ctx, n-1)))
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
		n := int(ctx.StackN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		recv, val := Value(jitSlotAt(ctx, n-2)), Value(jitSlotAt(ctx, n-1))
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
			open := rt.openUpvalsAt(rt.frameDepth)
			if open == nil {
				open = map[int]*upvalue{}
				rt.setOpenUpvals(rt.frameDepth, open)
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
		if child.capturesWith {
			// The child's free names resolve against the objects on this frame's
			// chain when it is later invoked, so it takes a copy — exactly as the
			// interpreter's CLOSURE does, and for the same reason: the chain
			// belongs to this frame and the child outlives it.
			if f := rt.jitFrame(); f != nil && len(f.withStack) > 0 {
				if ncl := rt.closureOf(fv); ncl != nil {
					ncl.capturedWith = append([]Value(nil), f.withStack...)
				}
			}
		}
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
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		i := int(uint32(ctx.Args[3]))
		if cl == nil || i >= len(cl.upvalues) || cl.upvalues[i] == nil {
			return rt.typeError("JIT upvalue")
		}
		v := Value(jitSlotAt(ctx, n-1))
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
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		v, e := rt.toNumber(Value(jitSlotAt(ctx, n-1)))
		if e != nil {
			return e
		}
		ctx.Ret = uint64(mknum(v))
		return nil
	case jitHelperToPropkey:
		// ToPropertyKey, which is ToPrimitive with hint "string" and can run
		// user code through toString or Symbol.toPrimitive.
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		pk, e := rt.toPropertyKey(Value(jitSlotAt(ctx, n-1)))
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
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		v, e := rt.getField(Value(jitSlotAt(ctx, n-1)), "length")
		if e != nil {
			return e
		}
		ctx.Ret = uint64(v)
		return nil
	case jitHelperForIn:
		// The keys a for-in walks, snapshotted the way the interpreter takes
		// them: a proxy's ownKeys trap runs here.
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		keys, e := rt.forInKeys(Value(jitSlotAt(ctx, n-1)))
		if e != nil {
			return e
		}
		ctx.Ret = uint64(keys)
		return nil
	case jitHelperStillEnum:
		n := int(ctx.StackN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		ctx.Ret = uint64(mkbool(rt.forInStillEnumerable(Value(jitSlotAt(ctx, n-1)), Value(jitSlotAt(ctx, n-2)))))
		return nil
	case jitHelperSetHomeObj:
		n := int(ctx.StackN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		rt.setMethodHome(Value(jitSlotAt(ctx, n-1)), Value(jitSlotAt(ctx, n-2)))
		return nil
	case jitHelperEval:
		// A direct eval. The frame hands the eval its dynamic scope — the
		// with-objects its free names must resolve against and the variable
		// object its own `var` declarations create bindings on — and takes it
		// back afterwards, so a nested eval or a call made from the body cannot
		// see a stale chain.
		scopeIdx := int(uint32(ctx.Args[3]))
		argc := int(uint32(ctx.Args[3] >> 32))
		withThis := scopeIdx&evalWithThisFlag != 0
		scopeIdx &^= evalTailFlag | evalWithThisFlag
		depth := argc + 1
		if withThis {
			depth++
		}
		n := int(ctx.StackN)
		if argc < 0 || n < depth {
			return rt.typeError("JIT operand stack")
		}
		base := n - depth
		evalThis := mkundef()
		if withThis {
			evalThis = jitSlotAt(ctx, base)
			base++
		}
		callee := jitSlotAt(ctx, base)
		evalArgs := jitCopyArgs(jitSpillArgs(ctx, base+1, argc))
		f := rt.jitFrame()
		if f == nil {
			return rt.typeError("JIT frame")
		}
		switch {
		case rt.evalFn == 0 || callee != rt.evalFn:
			// The binding was reassigned: an ordinary call after all.
			v, e := rt.callValue(callee, evalThis, evalArgs)
			if e != nil {
				return e
			}
			ctx.Ret = uint64(v)
		case argc == 0:
			ctx.Ret = uint64(mkundef())
		case !evalArgs[0].IsString():
			ctx.Ret = uint64(evalArgs[0])
		default:
			var sc *evalScope
			if scopeIdx < len(fn.evalScopes) {
				sc = fn.evalScopes[scopeIdx]
			}
			var privEnv *privScope
			if cl != nil {
				privEnv = cl.privEnv
			}
			savedVarObj, savedWithStack := rt.callerVarObj, rt.callerWithStack
			savedPrivEnv := rt.callerPrivEnv
			rt.callerVarObj, rt.callerWithStack, rt.callerPrivEnv = f.varObj, f.withStack, privEnv
			v, e := rt.performDirectEval(rt.strGo(evalArgs[0]), sc, cl,
				Value(ctx.This), f.newTarget, jitCaptureUpvalue(rt, locals))
			rt.callerVarObj, rt.callerWithStack, rt.callerPrivEnv = savedVarObj, savedWithStack, savedPrivEnv
			if e != nil {
				return e
			}
			ctx.Ret = uint64(v)
		}
		return nil
	case jitHelperEnterWith:
		// `with (o)`: ToObject, then onto this frame's chain. A primitive is
		// wrapped so that property lookups resolve on the wrapper.
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		obj, e := rt.toObjectValue(jitSlotAt(ctx, n-1))
		if e != nil {
			return e
		}
		f := rt.jitFrame()
		if f == nil {
			return rt.typeError("JIT frame")
		}
		f.withStack = append(f.withStack, obj)
		return nil
	case jitHelperExitWith:
		f := rt.jitFrame()
		if f == nil || len(f.withStack) == 0 {
			return rt.typeError("JIT with-chain")
		}
		f.withStack = f.withStack[:len(f.withStack)-1]
		return nil
	case jitHelperWithGetVar, jitHelperWithPutVar:
		// The name index, the lexical fallback's index and the flags byte, in
		// the one immediate the exit protocol carries.
		name := fn.constNames[uint32(ctx.Args[3])]
		fbIndex := int(uint16(ctx.Args[3] >> 32))
		flags := byte(ctx.Args[3] >> 48)
		f := rt.jitFrame()
		if f == nil {
			return rt.typeError("JIT frame")
		}
		if ctx.Helper == jitHelperWithGetVar {
			base, v, e := rt.withGetVar(fn, cl, locals, f.withStack, name, flags, fbIndex)
			if e != nil {
				return e
			}
			ctx.Args[2], ctx.Ret = uint64(base), uint64(v)
			return nil
		}
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		val, base := jitSlotAt(ctx, n-1), mkundef()
		if flags&withFlagRef != 0 {
			if n < 2 {
				return rt.typeError("JIT operand stack")
			}
			base = jitSlotAt(ctx, n-2)
		}
		if e := rt.withPutVar(fn, cl, locals, f.withStack, name, flags, fbIndex, base, val); e != nil {
			return e
		}
		ctx.Ret = uint64(val)
		return nil
	case jitHelperWithDelVar:
		f := rt.jitFrame()
		if f == nil {
			return rt.typeError("JIT frame")
		}
		ok, e := rt.withDelVar(f.withStack, fn.constNames[uint32(ctx.Args[3])])
		if e != nil {
			return e
		}
		ctx.Ret = uint64(mkbool(ok))
		return nil
	case jitHelperPutConst:
		// The write half of a strict assignment to an unqualified name. The
		// resolvable flag DELETE_VAR left is checked here, and HasProperty is
		// checked again because the right-hand side has run in between and may
		// have deleted the binding the reference resolved to.
		n := int(ctx.StackN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		val := Value(jitSlotAt(ctx, n-1))
		name := fn.constNames[uint32(ctx.Args[3])]
		if !rt.toBoolean(Value(jitSlotAt(ctx, n-2))) {
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
		n := int(ctx.StackN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		var privEnv *privScope
		if cl != nil {
			privEnv = cl.privEnv
		}
		name := fn.constNames[uint32(ctx.Args[3])]
		return rt.defineMethod(name, byte(ctx.Args[3]>>32), Value(jitSlotAt(ctx, n-1)), Value(jitSlotAt(ctx, n-2)), privEnv)
	case jitHelperTypeof:
		// `typeof v`. The string is interned, so what lands on the operand stack
		// is a handle into the runtime's own table rather than a fresh string.
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		ctx.Ret = uint64(rt.internString(rt.typeofString(Value(jitSlotAt(ctx, n-1)))))
		return nil
	case jitHelperIn:
		// `key in obj`, which is why this helper takes the closure: `#x in obj`
		// resolves the private name against the class environment the closure
		// carries, and passing nil there would answer the wrong question rather
		// than fail.
		n := int(ctx.StackN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		var privEnv *privScope
		if cl != nil {
			privEnv = cl.privEnv
		}
		r, e := rt.jsIn(Value(jitSlotAt(ctx, n-2)), Value(jitSlotAt(ctx, n-1)), privEnv)
		if e != nil {
			return e
		}
		ctx.Ret = uint64(mkbool(r))
		return nil
	case jitHelperDelete:
		// `delete o[k]`. The strict-mode arm is the interpreter's: a failed
		// delete is a TypeError there and silently false in sloppy code.
		n := int(ctx.StackN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		ok, e := rt.deleteElement(Value(jitSlotAt(ctx, n-2)), Value(jitSlotAt(ctx, n-1)))
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
		n := int(ctx.StackN)
		if n < 1 {
			return rt.typeError("JIT operand stack")
		}
		return &ThrowError{Value: Value(jitSlotAt(ctx, n-1)), rt: rt}
	case jitHelperObject:
		ctx.Ret = uint64(rt.newPlainObject())
		return nil
	case jitHelperArray:
		// An array literal, built from the top `n` spilled slots. The count is an
		// immediate rather than the spill depth, because a literal can appear
		// with operands of the surrounding expression beneath it.
		want := int(uint32(ctx.Args[3]))
		n := int(ctx.StackN)
		if want < 0 || n < want {
			return rt.typeError("JIT operand stack")
		}
		arrv := rt.newArray()
		ao := rt.objPtr(arrv)
		ao.arr = make([]Value, want)
		ao.arrLen = uint32(want)
		for i := 0; i < want; i++ {
			ao.arr[i] = Value(jitSlotAt(ctx, n-want+i))
		}
		ctx.Ret = uint64(arrv)
		return nil
	case jitHelperRegexp:
		n := int(ctx.StackN)
		if n < 2 {
			return rt.typeError("JIT operand stack")
		}
		v, e := rt.newRegExp(rt.strGo(Value(jitSlotAt(ctx, n-2))), rt.strGo(Value(jitSlotAt(ctx, n-1))))
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
