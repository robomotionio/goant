//go:build amd64 || arm64

package engine

import "sort"

// Definite assignment.
//
// The tier's invariant is that every local it reads holds a Number: either a
// parameter, checked once on entry, or a slot it assigned itself, which can only
// have received the result of double arithmetic. What the invariant needs is a
// way to know that a read is genuinely preceded by an assignment on *every* path
// that reaches it, not merely on the one that reads best.
//
// The first version of this asked whether the assignment appeared in the
// straight-line run before any branch. That is sound but it refused 1555 of
// Octane's functions — more than any unimplemented opcode — because a variable
// declared inside a loop or an if is the ordinary way to write JavaScript. This
// replaces it with the usual forward analysis over the control-flow graph, whose
// meet is intersection: a local is assigned at a block's entry when it is
// assigned at the exit of every block that can precede it.

// jitBlock is a basic block: a run of instructions with one entry and one exit.
type jitBlock struct {
	start, end int    // [start, end)
	succ       []int  // start ip of each successor
	gen        []bool // locals this block assigns
	kill       []bool // locals this block puts back into their dead zone
	in         []bool // locals assigned on entry, on every path
	reachable  bool
}

// jitTDZStores reports the stores that put a local into its temporal dead zone
// rather than giving it a value.
//
// `let x`, `const x`, a class name, a parameter with a default and a for-head
// binding all compile to EMPTY followed by PUT_LOCAL, and the sentinel that
// lands in the slot is not a value: every read of it before the initialiser
// runs has to throw. Definite assignment therefore has to count such a store as
// un-assigning the slot, or GET_LOCAL drops its dead-zone check and hands
// compiled code the sentinel as though it were data.
//
// Reading the instruction before the store, rather than tracking what the
// operand stack holds, is the same choice jitSingletonRHS makes and it is exact
// here: the compiler emits the two adjacently everywhere it emits them at all,
// and EMPTY's other uses — an array hole, a derived constructor's `this`, a
// generator's barrier yield — never reach a local. A store wrongly counted as
// a seed costs a dead-zone check; the converse is a miscompilation, so where
// the rule is imprecise it is imprecise in the safe direction.
//
// The three walks that model assignment — this file's two and the emitter's —
// consult the same set, keyed by ip, so none of them has to reproduce the
// reasoning or keep its own idea of what came before.
func jitTDZStores(fn *svFunc) map[int]bool {
	code := fn.code
	var seeds map[int]bool
	prev := Opcode(0)
	for ip := 0; ip < len(code); {
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		if size <= 0 || ip+size > len(code) {
			break
		}
		if prev == OpEmpty && (op == OpPutLocal || op == OpSetLocal) {
			if seeds == nil {
				seeds = map[int]bool{}
			}
			seeds[ip] = true
		}
		prev = op
		ip += size
	}
	return seeds
}

// jitAnalysisBudget bounds the work the analysis below will do, counted in
// block-local pairs, which is what its sets cost to allocate and to intersect.
//
// It is a budget rather than a limit on anything a programmer would recognise,
// because the two dimensions multiply: a function with many blocks and few
// locals is cheap and so is the converse. The largest real function in Octane
// comes to 8,978 pairs; this is two hundred times that, and about seventeen
// milliseconds of analysis at the rate measured on the bench VM.
//
// What sits above it is generated code — mjsunit has a function that switches on
// eighty thousand cases, and one that opens eight thousand catch blocks and so
// declares eight thousand locals. Declining those is the right answer twice
// over: they cost more to compile than to interpret, and they are cold.
const jitAnalysisBudget = 1 << 21

// jitAnalyze splits the body into basic blocks and solves definite assignment
// over them. It returns the entry set for each block, keyed by its start ip,
// and the empty string, or nothing and the reason it declined.
//
// Parameters are seeded as assigned because the compiled prologue checks them,
// and a function is refused outright if the bytecode does not decode cleanly —
// which cannot happen with goant's compiler, but an analysis that guessed would
// be worse than one that declined.
func jitAnalyze(fn *svFunc, start int, targets map[int]bool) (map[int]*jitBlock, string) {
	code := fn.code
	nloc := fn.maxLocals
	tdz := jitTDZStores(fn)

	// Leaders begin a block: the entry, every branch target, and whatever
	// follows a branch or a return.
	leaders := map[int]bool{start: true}
	for t := range targets {
		if t >= start {
			leaders[t] = true
		}
	}
	for ip := start; ip < len(code); {
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		if size <= 0 || ip+size > len(code) {
			return nil, "undecodable"
		}
		switch op {
		case OpJmp, OpJmpFalse, OpJmpTrue, OpJmpNotNullish, OpTryPush,
			OpReturn, OpReturnUndef, OpThrow, OpThrowError, OpTailCall, OpTailCallMethod:
			if ip+size < len(code) {
				leaders[ip+size] = true
			}
		}
		ip += size
	}
	// Checked here, which is after the block count is known and before a single
	// set has been allocated.
	if len(leaders)*nloc > jitAnalysisBudget {
		return nil, "body-too-large"
	}

	// Walk once more, cutting at leaders and recording what each block assigns
	// and where it can go.
	blocks := map[int]*jitBlock{}
	var cur *jitBlock
	for ip := start; ip < len(code); {
		if leaders[ip] {
			if cur != nil {
				cur.end = ip
				if len(cur.succ) == 0 {
					cur.succ = []int{ip} // falls through
				}
			}
			cur = &jitBlock{start: ip, gen: make([]bool, nloc), kill: make([]bool, nloc), in: make([]bool, nloc)}
			blocks[ip] = cur
		}
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		switch op {
		case OpPutLocal, OpSetLocal:
			i := int(readU16(code, ip+1))
			if i >= nloc {
				return nil, "undecodable"
			}
			// Last store in the block wins, which is why this clears the other
			// set rather than only setting one: a slot seeded and then really
			// assigned is assigned, and one assigned and then re-seeded — a
			// `let` in a loop body — is back in its dead zone.
			cur.gen[i], cur.kill[i] = !tdz[ip], tdz[ip]
		case OpJmp:
			cur.succ = []int{int(readU32(code, ip+1))}
		case OpJmpFalse, OpJmpTrue, OpJmpNotNullish:
			cur.succ = []int{int(readU32(code, ip+1)), ip + size}
		case OpTryPush:
			// Not a branch the machine takes, but an edge control really has:
			// every instruction in the body can reach the catch. Modelling it from
			// the TRY_PUSH rather than from each of them is also what gives the
			// catch block the depth it resumes at, which is this one.
			cur.succ = []int{int(readU32(code, ip+1)), ip + size}
		case OpReturn, OpReturnUndef, OpThrow, OpThrowError, OpTailCall, OpTailCallMethod:
			cur.succ = []int{} // nothing follows
		}
		ip += size
		if cur.end == 0 && ip >= len(code) {
			cur.end = ip
		}
	}
	if cur != nil && cur.end == 0 {
		cur.end = len(code)
	}
	for _, b := range blocks {
		for _, s := range b.succ {
			if _, ok := blocks[s]; !ok {
				return nil, "branch-into-instruction" // into the middle of one
			}
		}
	}

	// Reachability, so that a block no path arrives at cannot drag the
	// intersection down to nothing for the blocks that follow it — and, from the
	// same walk, a reverse post-order for the fixpoint below to visit them in.
	var post []int
	var mark func(int)
	mark = func(ip int) {
		b, ok := blocks[ip]
		if !ok || b.reachable {
			return
		}
		b.reachable = true
		for _, s := range b.succ {
			mark(s)
		}
		post = append(post, ip)
	}
	mark(start)

	// The entry block starts with the parameters assigned; every other reachable
	// block starts optimistic and is cut down to the intersection of what its
	// predecessors guarantee. Optimistic initialisation is what lets a loop body
	// keep an assignment made before the loop rather than losing it to the back
	// edge on the first pass.
	entry := blocks[start]
	for i := 0; i < fn.paramCount && i < nloc; i++ {
		entry.in[i] = true
	}
	for ip, b := range blocks {
		if ip == start || !b.reachable {
			continue
		}
		for i := range b.in {
			b.in[i] = true
		}
	}

	preds := map[int][]int{}
	for ip, b := range blocks {
		if !b.reachable {
			continue
		}
		for _, s := range b.succ {
			preds[s] = append(preds[s], ip)
		}
	}

	// One buffer, refilled per predecessor, rather than one allocation per
	// (block, local, predecessor) triple. The set this computes used to be built
	// inside the innermost loop, so a function with many blocks and many locals
	// spent nearly all of its compile time allocating the same answer over and
	// over: 69% of one mjsunit test, which is two thousand evals in a file.
	scratch := make([]bool, nloc)
	out := func(b *jitBlock) []bool {
		for i := range scratch {
			scratch[i] = b.gen[i] || (b.in[i] && !b.kill[i])
		}
		return scratch
	}

	// Reverse post-order, and the order is the whole point. The information here
	// travels forwards — a block loses a bit because a predecessor lost it — so
	// visiting predecessors first carries a fact the length of a straight run in
	// a single pass, and the loop below settles in two. Iterating a Go map
	// instead, which is what this used to do, visits blocks in an order unrelated
	// to the flow and needs a pass per link of the chain: quadratic, and the
	// dominant cost of compiling anything with a few thousand blocks in it.
	for i, j := 0, len(post)-1; i < j; i, j = i+1, j-1 {
		post[i], post[j] = post[j], post[i]
	}
	for changed := true; changed; {
		changed = false
		for _, ip := range post {
			if ip == start {
				continue
			}
			b := blocks[ip]
			for _, p := range preds[ip] {
				o := out(blocks[p])
				for i := range b.in {
					if b.in[i] && !o[i] {
						b.in[i] = false
						changed = true
					}
				}
			}
		}
	}
	return blocks, ""
}

// jitNumberDemand reports which locals the body needs to be Numbers.
//
// This decides which parameters the prologue checks, and checking fewer of them
// is not a saving but the point. A parameter that is checked cannot be an
// object, because the check is "untagged or leave" — so for as long as every
// parameter was checked, no object could reach compiled code at all, and the
// property read below it could only ever see a primitive. Guarding a parameter
// the body only reads fields from is what made an inline cache in compiled code
// unreachable rather than merely unwritten.
//
// A local is in demand when a read of it reaches an operation defined on Numbers
// — arithmetic, a bitwise operator, a comparison. Being a property's receiver,
// being stored, being returned: none of those demand anything, because the
// templates for them work on any value.
//
// Arithmetic still demands even though it no longer has to. A generic operator
// would take any operand, but it would take it behind a guard and with a call
// out on the other side of it, where a checked parameter needs neither — so a
// parameter this function does arithmetic on is worth guessing is a Number, and
// worth leaving on the interpreter when the guess is wrong.
//
// The relation is transitive through the locals, which is what the outer loop is
// for: `let t = o; t * 2` demands a Number of t, and therefore of o. The origin
// of a stack slot is the local whose read produced it, or none for a computed
// value, and a store propagates the destination's demand back to that origin.
func jitNumberDemand(fn *svFunc, blocks map[int]*jitBlock, depths map[int]int) ([]bool, bool) {
	code := fn.code
	demand := make([]bool, fn.maxLocals)

	const noOrigin = -1
	for changed := true; changed; {
		changed = false
		want := func(i int) {
			if i != noOrigin && !demand[i] {
				demand[i] = true
				changed = true
			}
		}
		for _, b := range blocks {
			if !b.reachable {
				continue
			}
			// A block may be entered with operands already live — `a && b` jumps
			// with one — and their origins are not knowable from here, so they
			// are seeded as having none. Conservative in the right direction: an
			// origin nobody claims demands nothing of any local.
			org := make([]int, depths[b.start])
			for i := range org {
				org[i] = noOrigin
			}
			push := func(o int) { org = append(org, o) }
			pop := func() (int, bool) {
				if len(org) == 0 {
					return noOrigin, false
				}
				o := org[len(org)-1]
				org = org[:len(org)-1]
				return o, true
			}
			popWant := func() bool {
				o, ok := pop()
				if ok {
					want(o)
				}
				return ok
			}
			for ip := b.start; ip < b.end; {
				op := Opcode(code[ip])
				size := int(opTable[op].Size)
				if size <= 0 || ip+size > len(code) {
					return nil, false
				}
				switch op {
				case OpGetLocal:
					i := int(readU16(code, ip+1))
					if i >= fn.maxLocals {
						return nil, false
					}
					push(i)
				case OpPutLocal, OpSetLocal:
					i := int(readU16(code, ip+1))
					if i >= fn.maxLocals {
						return nil, false
					}
					o, ok := pop()
					if !ok {
						return nil, false
					}
					// What the destination needs, the source must supply.
					if demand[i] {
						want(o)
					}
					if op == OpSetLocal {
						push(o)
					}
				case OpAdd, OpSub, OpMul, OpDiv,
					OpBand, OpBor, OpBxor, OpShl, OpShr, OpUshr,
					OpLt, OpLe, OpGt, OpGe, OpEq, OpNe, OpSeq, OpSne:
					if !popWant() || !popWant() {
						return nil, false
					}
					push(noOrigin)
				case OpNeg, OpBnot, OpInc, OpDec, OpNot:
					if !popWant() {
						return nil, false
					}
					push(noOrigin)
				case OpDup:
					o, ok := pop()
					if !ok {
						return nil, false
					}
					push(o)
					push(o)
				case OpInsert2:
					av, ok := pop()
					if !ok {
						return nil, false
					}
					obj, ok := pop()
					if !ok {
						return nil, false
					}
					push(av)
					push(obj)
					push(av)
				case OpInsert3:
					// obj prop a -> a obj prop a
					av, ok := pop()
					if !ok {
						return nil, false
					}
					pr, ok := pop()
					if !ok {
						return nil, false
					}
					ob, ok := pop()
					if !ok {
						return nil, false
					}
					push(av)
					push(ob)
					push(pr)
					push(av)
				case OpPutElem:
					for i := 0; i < 3; i++ {
						if _, ok := pop(); !ok {
							return nil, false
						}
					}
				case OpPutGlobal:
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpGetField:
					// The receiver is demanded of nothing: the template reads a
					// field of whatever it is handed, or asks the runtime to.
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(noOrigin)
				case OpGetElem:
					// Nor is the index: the template guards it rather than
					// requiring the prologue to have checked it.
					if _, ok := pop(); !ok {
						return nil, false
					}
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(noOrigin)
				case OpPutField:
					// Neither the receiver nor the value is demanded, for the
					// same reason, and the store leaves nothing behind — an
					// assignment used as an expression duplicates the value
					// first (INSERT2).
					if _, ok := pop(); !ok {
						return nil, false
					}
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpGetField2:
					// obj -> obj val: the receiver stays, keeping whatever
					// origin it had.
					o, ok := pop()
					if !ok {
						return nil, false
					}
					push(o)
					push(noOrigin)
				case OpCall, OpCallMethod, OpNew:
					// The callee, its arguments and — for a method call — the
					// receiver, none of them demanded: what a call does with a
					// value is the callee's business.
					n := int(readU16(code, ip+1)) + 1
					if op == OpCallMethod {
						n++
					}
					for i := 0; i < n; i++ {
						if _, ok := pop(); !ok {
							return nil, false
						}
					}
					push(noOrigin)
				case OpConst, OpConstI8, OpUndef, OpNull, OpTrue, OpFalse, OpEmpty, OpThis,
					OpGetGlobal, OpGetUpval, OpObject, OpGetGlobalUndef, OpSpecialObj,
					OpClosure, OpGlobal, OpDeleteVar:
					push(noOrigin)
				case OpArray:
					for i := int(readU16(code, ip+1)); i > 0; i-- {
						if _, ok := pop(); !ok {
							return nil, false
						}
					}
					push(noOrigin)
				case OpSetHomeObj:
					// Leaves the stack exactly as it found it.
				case OpPutConst:
					// [resolvable, value] -> [value]: two consumed, one produced,
					// and the one produced is whatever was assigned.
					if _, ok := pop(); !ok {
						return nil, false
					}
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(noOrigin)
				case OpDefineField, OpDefineMethod, OpPutUpval:
					// [target, value] -> [target]: the value goes, the target
					// stays exactly as it was.
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpSetUpval:
					// The value stays on the stack, and what it is demanded of is
					// nothing: the write goes through a cell the runtime owns.
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(noOrigin)
				case OpJmpNotNullish:
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpThrow:
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpTailCall, OpTailCallMethod:
					// Everything it consumes and nothing produced: the frame is
					// over, and the value the call produces is returned from
					// somewhere this analysis cannot see.
					n := int(readU16(code, ip+1)) + 1
					if op == OpTailCallMethod {
						n++
					}
					for ; n > 0; n-- {
						if _, ok := pop(); !ok {
							return nil, false
						}
					}
				case OpThrowError:
					// Consumes nothing and never returns.
				case OpTryPush, OpTryPop:
					// The handler stack, which is not the operand stack.
				case OpExitWith:
					// The with-chain, which is not the operand stack either.
				case OpEnterWith:
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpCatch, OpWithDelVar:
					// A value from outside this frame's arithmetic.
					push(noOrigin)
				case OpEval:
					n := int(readU16(code, ip+3)) + 1
					if int(readU16(code, ip+1))&evalWithThisFlag != 0 {
						n++
					}
					for ; n > 0; n-- {
						if _, ok := pop(); !ok {
							return nil, false
						}
					}
					push(noOrigin)
				case OpWithGetVar:
					push(noOrigin)
					if code[ip+7]&withFlagRef != 0 && code[ip+7]&withFlagBaseOnly == 0 {
						push(noOrigin)
					}
				case OpWithPutVar:
					if _, ok := pop(); !ok {
						return nil, false
					}
					if code[ip+7]&withFlagRef != 0 {
						if _, ok := pop(); !ok {
							return nil, false
						}
						push(noOrigin)
					}
				case OpUplus, OpTypeof, OpIsUndefOrNull, OpToPropkey, OpForIn, OpGetLength:
					// One operand consumed and demanded of nothing. `typeof`
					// takes any value, and the nullish test is a tag comparison
					// that a Number simply fails.
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(noOrigin)
				case OpMod, OpIn, OpDelete, OpInstanceof, OpRegexp, OpHasPrivate:
					// Two consumed, one produced, and none of the four demands a
					// Number of its operands: every one is emitted as a call to
					// the same runtime the interpreter uses. `%` in particular is
					// deliberately not with the arithmetic above — there is no SSE
					// instruction for it, so nothing is gained by requiring the
					// prologue to have checked the operands.
					if _, ok := pop(); !ok {
						return nil, false
					}
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(noOrigin)
				case OpPop, OpJmpFalse, OpJmpTrue, OpReturn:
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpJmp, OpReturnUndef:
					// no stack effect
				default:
					return nil, false
				}
				ip += size
			}
		}
	}
	return demand, true
}

// jitStackEffect reports how many operands the instruction at ip consumes and
// produces.
//
// Only the opcodes this tier has a template for; anything else reports false and
// the function is refused before this matters. The counts come from opTable,
// which the compiler and the interpreter already agree on, plus the argument
// count for the call forms — a call's operand is how many arguments follow the
// callee, and opTable records only the callee and the receiver.
func jitStackEffect(fn *svFunc, ip int) (pop, push int, ok bool) {
	code := fn.code
	op := Opcode(code[ip])
	if !jitHasTemplate(op) {
		return 0, 0, false
	}
	// The call forms are written out rather than taken from opTable, because
	// their operand is how many arguments follow and the table's fixed counts do
	// not all agree with what the interpreter pops — NEW's says two and the
	// interpreter takes the callee and the arguments, which is one plus the
	// count. A wrong effect here would refuse a function rather than
	// miscompile it, the emitter checking the prediction at every label, but a
	// refusal nobody can explain is its own cost.
	switch op {
	case OpCall, OpNew:
		return int(readU16(code, ip+1)) + 1, 1, true
	case OpCallMethod:
		return int(readU16(code, ip+1)) + 2, 1, true
	case OpArray:
		// Same shape: the operand is how many elements the literal takes off the
		// stack, and opTable records a fixed zero because the count is in the
		// instruction. Left unwritten, this made the depth analysis disagree with
		// the emitter at every label after an array literal — 355 more functions
		// refused as `stack-across-blocks` the moment the template existed, which
		// is what a wrong prediction looks like when it is not a miscompilation.
		return int(readU16(code, ip+1)), 1, true
	case OpThrow:
		// Nothing after it runs, so what it leaves is not a depth the analysis
		// should propagate. One popped and none pushed is the honest local
		// answer; jitBlockDepths stops at the branch this ends the block with.
		return 1, 0, true
	case OpEval:
		// The operand is how many arguments follow, plus the callee, plus a
		// receiver when the callee came through a `with` chain. opTable cannot
		// say any of that.
		n := int(readU16(code, ip+3)) + 1
		if int(readU16(code, ip+1))&evalWithThisFlag != 0 {
			n++
		}
		return n, 1, true
	case OpWithGetVar:
		// opTable records one pushed, and reference mode pushes two — the base a
		// paired write goes back through, beneath the value — while the
		// base-only form pushes just the base. The flags byte is the operand
		// that decides, exactly as the count is for CALL.
		flags := code[ip+7]
		if flags&withFlagRef == 0 || flags&withFlagBaseOnly != 0 {
			return 0, 1, true
		}
		return 0, 2, true
	case OpWithPutVar:
		// One consumed and none produced in the plain form; in reference mode
		// two consumed and the assigned value produced, which is net one off.
		if code[ip+7]&withFlagRef != 0 {
			return 2, 1, true
		}
		return 1, 0, true
	case OpTailCall, OpTailCallMethod:
		// Also a terminator, and opTable's fixed counts do not describe it: the
		// operand is how many arguments follow, and the table records the one
		// or two slots below them. The callee and the receiver are those slots.
		n := int(readU16(code, ip+1)) + 1
		if op == OpTailCallMethod {
			n++
		}
		return n, 0, true
	case OpJmpNotNullish:
		// opTable records one popped and one pushed; the interpreter pops and
		// pushes nothing. Both are right about their own question — `a ?? b`
		// emits DUP before the branch, so the value the taken path keeps is the
		// duplicate and the table describes the pair. This function answers for
		// the single instruction, which is net one off the stack.
		return 1, 0, true
	}
	info := opTable[op]
	return int(info.NPop), int(info.NPush), true
}

// jitBlockDepths reports the operand-stack depth each block is entered with.
//
// Every branch target used to have to be reached with an empty stack, which is
// most of what goant's compiler emits and not all of it: `a && b`, `a || b` and
// `a ? b : c` all jump with a value live. That rule refused 313,372 frame
// entries across eleven Crypto functions and 1.08M in a single Richards one, all
// of them fully unblocking — the largest item left on the list once the property
// accesses were in.
//
// The register assignment is positional, so a target reached at the same depth
// from every predecessor needs no fixing up at all: the operands are already in
// the registers the code after the label expects. What this computes is exactly
// that agreement, and reports false when the predecessors disagree.
//
// It is a prediction of what the emitter will do rather than a description of
// it, and the emitter checks it: at every label the depth it actually has must
// equal the one predicted here, or the function is refused. So a wrong
// prediction costs a refusal and can never cost a miscompilation.
func jitBlockDepths(fn *svFunc, start int, blocks map[int]*jitBlock) (map[int]int, bool) {
	depth := map[int]int{start: 0}
	// A worklist rather than a fixpoint over the whole map, because a block's
	// depth is decided the first time it is reached: a second predecessor either
	// agrees, in which case there is nothing to propagate, or disagrees, in which
	// case the function is refused. So each block is walked exactly once.
	//
	// The fixpoint this replaces re-walked every settled block on every pass, and
	// needed one pass per link of the longest chain because a Go map hands its
	// keys back in an order unrelated to the flow. That is quadratic, and the
	// comment here used to say the graph was small. V8's mjsunit has a test that
	// switches on eighty thousand cases; it took over two hundred seconds to
	// compile and 267ms to interpret.
	work := []int{start}
	for len(work) > 0 {
		ip := work[len(work)-1]
		work = work[:len(work)-1]
		b := blocks[ip]
		if b == nil || !b.reachable {
			continue
		}
		d := depth[ip]
		for at := b.start; at < b.end; {
			pop, push, ok := jitStackEffect(fn, at)
			if !ok {
				return nil, false
			}
			if d -= pop; d < 0 {
				return nil, false
			}
			d += push
			at += int(opTable[Opcode(fn.code[at])].Size)
		}
		for _, s := range b.succ {
			if was, ok := depth[s]; ok {
				if was != d {
					return nil, false // predecessors disagree
				}
				continue
			}
			depth[s] = d
			work = append(work, s)
		}
	}
	for ip, b := range blocks {
		if b.reachable {
			if _, ok := depth[ip]; !ok {
				return nil, false
			}
		}
	}
	return depth, true
}

// jitUnprovenLocals reports which locals the emitter will read without being
// able to prove they were written first.
//
// It walks the blocks exactly the way the emitter does — seed from the block's
// entry set, and a store makes the slot assigned for the rest of the block — so
// the two cannot disagree about which reads are unproven. What the emitter does
// about such a read is emit a dead-zone check; what this is for is the
// consequence, which is that the value is not a Number.
func jitUnprovenLocals(fn *svFunc, blocks map[int]*jitBlock) []bool {
	code := fn.code
	tdz := jitTDZStores(fn)
	unproven := make([]bool, fn.maxLocals)
	cur := make([]bool, fn.maxLocals)
	for _, b := range blocks {
		if !b.reachable {
			continue
		}
		copy(cur, b.in)
		for ip := b.start; ip < b.end; {
			op := Opcode(code[ip])
			size := int(opTable[op].Size)
			if size <= 0 || ip+size > len(code) {
				return unproven
			}
			switch op {
			case OpGetLocal:
				if i := int(readU16(code, ip+1)); i < fn.maxLocals && !cur[i] {
					unproven[i] = true
				}
			case OpPutLocal, OpSetLocal:
				if i := int(readU16(code, ip+1)); i < fn.maxLocals {
					cur[i] = !tdz[ip]
				}
			}
			ip += size
		}
	}
	return unproven
}

// jitNumericLocals reports which locals only ever receive Numbers.
//
// The tier used to be able to assume this of every local, because the only
// values it could produce were results of double arithmetic. Once undefined,
// null and the Booleans can be pushed, that stops being true, and a local that
// has held one of them must not be handed to an ADDSD.
//
// The analysis is flow-insensitive on purpose: a local is numeric only if every
// store to it anywhere in the body stores a Number. That refuses a slot reused
// for a Number in one branch and a Boolean in another, which is rare and not
// worth a lattice per program point. It starts optimistic and shrinks, so it
// terminates.
//
// Parameters are seeded from demand rather than optimistically, because what
// makes a parameter a Number is the prologue checking it, and the prologue only
// checks the ones in demand. An unchecked parameter holds whatever the caller
// passed, so it is not numeric, and neither is anything copied from it.
//
// It walks the same stack discipline the emitter does. The two agreeing matters,
// so anything unexpected refuses the function rather than guessing — and since
// the emitter refuses the same opcodes, they cannot drift apart on a function
// either of them accepts.
func jitNumericLocals(fn *svFunc, blocks map[int]*jitBlock, depths map[int]int, demand []bool) ([]bool, bool) {
	code := fn.code
	numeric := make([]bool, fn.maxLocals)
	for i := range numeric {
		numeric[i] = true
	}
	for i := 0; i < fn.paramCount && i < fn.maxLocals; i++ {
		numeric[i] = demand[i]
	}
	// A local read where the emitter cannot prove it was written holds whatever
	// the frame left there — undefined for a `var`, the dead-zone sentinel for a
	// lexical binding — and neither is a Number. Seeded here rather than handled
	// at the read, because the fixpoint below has to carry it: without this a
	// local copied out of one would stay marked numeric and reach an ADDSD
	// holding undefined.
	for i, unproven := range jitUnprovenLocals(fn, blocks) {
		if unproven {
			numeric[i] = false
		}
	}

	for changed := true; changed; {
		changed = false
		for _, b := range blocks {
			if !b.reachable {
				continue
			}
			// Operands already live at the block's entry are seeded non-numeric,
			// which is what the emitter does with them too: a slot arriving from
			// another block has no kind this walk can know, and calling it a
			// Number is the one answer that would be unsound.
			kinds := make([]bool, depths[b.start])
			push := func(k bool) { kinds = append(kinds, k) }
			pop := func() (bool, bool) {
				if len(kinds) == 0 {
					return false, false
				}
				k := kinds[len(kinds)-1]
				kinds = kinds[:len(kinds)-1]
				return k, true
			}
			for ip := b.start; ip < b.end; {
				op := Opcode(code[ip])
				size := int(opTable[op].Size)
				if size <= 0 || ip+size > len(code) {
					return nil, false
				}
				switch op {
				case OpConstI8:
					push(true)
				case OpConst:
					idx := readU32(code, ip+1)
					if int(idx) >= len(fn.constants) {
						return nil, false
					}
					push(fn.constants[idx].IsNumber())
				case OpUndef, OpNull, OpTrue, OpFalse, OpEmpty:
					push(false)
				case OpThis:
					push(false)
				case OpGetLocal:
					i := int(readU16(code, ip+1))
					if i >= fn.maxLocals {
						return nil, false
					}
					push(numeric[i])
				case OpPutLocal, OpSetLocal:
					i := int(readU16(code, ip+1))
					if i >= fn.maxLocals {
						return nil, false
					}
					k, ok := pop()
					if !ok {
						return nil, false
					}
					if !k && numeric[i] {
						numeric[i] = false
						changed = true
					}
					if op == OpSetLocal {
						push(k) // SET_LOCAL leaves the value behind
					}
				case OpAdd, OpSub, OpMul, OpDiv:
					// Only when both operands were Numbers. The generic form of
					// these calls the runtime's operator, and `+` on two Strings
					// is a String, `*` on two BigInts a BigInt.
					y, ok := pop()
					if !ok {
						return nil, false
					}
					x, ok := pop()
					if !ok {
						return nil, false
					}
					push(x && y)
				case OpBand, OpBor, OpBxor, OpShl, OpShr, OpUshr:
					// A Number either side of the guard, but only if both
					// operands were Numbers to begin with: these are compiled
					// with a runtime arm now, and `1n & 3n` is a BigInt.
					y, ok := pop()
					if !ok {
						return nil, false
					}
					x, ok := pop()
					if !ok {
						return nil, false
					}
					push(x && y)
				case OpNeg, OpBnot, OpInc, OpDec:
					// Likewise: ToNumeric leaves a BigInt a BigInt, so each of
					// these answers whatever its operand was. The emitter
					// already says so — this is the same rule, stated for the
					// locals rather than for the operand stack.
					k, ok := pop()
					if !ok {
						return nil, false
					}
					push(k)
				case OpNot:
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(false) // a Boolean
				case OpDup:
					k, ok := pop()
					if !ok {
						return nil, false
					}
					push(k)
					push(k)
				case OpInsert2:
					// obj a -> a obj a
					av, ok := pop()
					if !ok {
						return nil, false
					}
					obj, ok := pop()
					if !ok {
						return nil, false
					}
					push(av)
					push(obj)
					push(av)
				case OpInsert3:
					// obj prop a -> a obj prop a
					av, ok := pop()
					if !ok {
						return nil, false
					}
					pr, ok := pop()
					if !ok {
						return nil, false
					}
					ob, ok := pop()
					if !ok {
						return nil, false
					}
					push(av)
					push(ob)
					push(pr)
					push(av)
				case OpPutElem:
					for i := 0; i < 3; i++ {
						if _, ok := pop(); !ok {
							return nil, false
						}
					}
				case OpPutGlobal:
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpGetField:
					// The object goes in and whatever it held comes out, which
					// is not a Number as far as this tier can tell.
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(false)
				case OpArray:
					for i := int(readU16(code, ip+1)); i > 0; i-- {
						if _, ok := pop(); !ok {
							return nil, false
						}
					}
					push(false)
				case OpSetHomeObj:
					// Leaves the stack exactly as it found it.
				case OpPutConst:
					if _, ok := pop(); !ok {
						return nil, false
					}
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(false)
				case OpDefineField, OpDefineMethod, OpJmpNotNullish, OpPutUpval:
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpSetUpval:
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(false)
				case OpThrow:
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpTailCall, OpTailCallMethod:
					n := int(readU16(code, ip+1)) + 1
					if op == OpTailCallMethod {
						n++
					}
					for ; n > 0; n-- {
						if _, ok := pop(); !ok {
							return nil, false
						}
					}
				case OpThrowError:
					// Consumes nothing and never returns.
				case OpTryPush, OpTryPop:
					// The handler stack, which is not the operand stack.
				case OpExitWith:
				case OpEnterWith:
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpCatch, OpWithDelVar:
					push(false)
				case OpEval:
					n := int(readU16(code, ip+3)) + 1
					if int(readU16(code, ip+1))&evalWithThisFlag != 0 {
						n++
					}
					for ; n > 0; n-- {
						if _, ok := pop(); !ok {
							return nil, false
						}
					}
					push(false)
				case OpWithGetVar:
					push(false)
					if code[ip+7]&withFlagRef != 0 && code[ip+7]&withFlagBaseOnly == 0 {
						push(false)
					}
				case OpWithPutVar:
					if _, ok := pop(); !ok {
						return nil, false
					}
					if code[ip+7]&withFlagRef != 0 {
						if _, ok := pop(); !ok {
							return nil, false
						}
						push(false)
					}
				case OpUplus, OpTypeof, OpIsUndefOrNull, OpToPropkey, OpForIn, OpGetLength:
					// Consumes one and produces a value this tier does not model
					// as a Number — even UPLUS, whose result is one, because it
					// arrives through a helper that can hand back anything
					// ToNumber produced.
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(false)
				case OpMod, OpIn, OpDelete, OpInstanceof, OpRegexp, OpHasPrivate:
					// Two consumed, one produced. MOD does produce a Number, but
					// saying so would let the arithmetic after it skip a guard on
					// a value this tier fetches through a helper — and jsArith is
					// free to answer with a BigInt. Not a Number is the sound
					// answer for all five.
					if _, ok := pop(); !ok {
						return nil, false
					}
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(false)
				case OpObject, OpGetGlobalUndef, OpSpecialObj, OpClosure,
					OpGlobal, OpDeleteVar:
					push(false)
				case OpGetGlobal, OpGetUpval:
					// A global or a captured binding holds anything at all.
					push(false)
				case OpGetElem:
					// Array and index in, an element out, which is not a Number
					// as far as this tier can tell.
					if _, ok := pop(); !ok {
						return nil, false
					}
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(false)
				case OpGetField2:
					// obj -> obj val: the receiver stays as it was, and a field
					// is not a Number as far as this tier can tell.
					k, ok := pop()
					if !ok {
						return nil, false
					}
					push(k)
					push(false)
				case OpCall, OpCallMethod, OpNew:
					// Callee, arguments and receiver in, one result out, and a
					// call can return anything.
					n := int(readU16(code, ip+1)) + 1
					if op == OpCallMethod {
						n++
					}
					for i := 0; i < n; i++ {
						if _, ok := pop(); !ok {
							return nil, false
						}
					}
					push(false)
				case OpPutField:
					// Receiver and value in, nothing out.
					if _, ok := pop(); !ok {
						return nil, false
					}
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpLt, OpLe, OpGt, OpGe, OpEq, OpNe, OpSeq, OpSne:
					if _, ok := pop(); !ok {
						return nil, false
					}
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(false) // a Boolean
				case OpPop, OpJmpFalse, OpJmpTrue, OpReturn:
					if _, ok := pop(); !ok {
						return nil, false
					}
				case OpJmp, OpReturnUndef:
					// no stack effect
				default:
					return nil, false
				}
				ip += size
			}
		}
	}
	return numeric, true
}

// A live exception handler, as the emitter sees it.
//
// catchIP is where the interpreter would land, which is always a CATCH; depth
// is the operand stack the handler was installed at, and the depth the catch
// resumes with.
type jitHandler struct {
	catchIP int
	depth   int
}

// jitHandlerStacks reports the handler stack in force at the start of each
// block, or false when the function's handlers are not something this tier can
// model.
//
// The interpreter keeps this stack at runtime, pushing at TRY_PUSH and popping
// at TRY_POP, and a throw walks it. A template compiler has to know the answer
// at compile time instead, and it is knowable: the handler stack is a property
// of where control is, so it propagates along the same edges as everything else
// the block graph carries.
//
// Matching each TRY_PUSH to a TRY_POP by counting would be wrong, and quietly:
// `break` out of a try body emits its own TRY_POP before the jump, so a scan
// that stops at the first one leaves the rest of the body looking unprotected —
// a throw there would escape a catch that should have taken it. The dataflow
// has no such gap, and where predecessors disagree it refuses rather than
// picking one.
//
// The catch block is not reachable through any ordinary edge; jitAnalyze makes
// TRY_PUSH a branch to it, which is also what gives it the right entry depth,
// since a catch resumes at the depth its TRY_PUSH was installed at.
func jitHandlerStacks(fn *svFunc, blocks map[int]*jitBlock, depths map[int]int, start int) (map[int][]jitHandler, bool) {
	code := fn.code
	in := map[int][]jitHandler{start: nil}

	same := func(a, b []jitHandler) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// Blocks in ip order, so one pass settles a body with no back edge over a
	// try and the loop below rarely runs twice.
	order := make([]int, 0, len(blocks))
	for ip := range blocks {
		order = append(order, ip)
	}
	sort.Ints(order)

	for changed := true; changed; {
		changed = false
		for _, bs := range order {
			b := blocks[bs]
			if !b.reachable {
				continue
			}
			cur, seen := in[bs]
			if !seen {
				continue // not reached yet on this pass
			}
			stack := append([]jitHandler(nil), cur...)
			// The catches this block installs. jitAnalyze gives TRY_PUSH an edge
			// to its catch so that the catch block inherits the depth it resumes
			// at, but that edge carries the wrong handler stack — a throw inside
			// a catch belongs to the next handler out, not to the one the catch
			// belongs to. So the edge is seeded here and skipped below.
			seeded := map[int]bool{}
			d := depths[bs]
			for ip := b.start; ip < b.end; {
				op := Opcode(code[ip])
				switch op {
				case OpTryPush:
					// The catch lands at the depth the try was entered with, and
					// anything else would need the operands the throw destroyed.
					// A try is a statement, so this is zero in practice.
					if d != 0 {
						return nil, false
					}
					// The catch runs with this handler already popped: a throw
					// inside it belongs to the next one out. That is the stack
					// as it stands here, before the push below.
					outer := append([]jitHandler(nil), stack...)
					stack = append(stack, jitHandler{catchIP: int(readU32(code, ip+1)), depth: d})
					catchIP := int(readU32(code, ip+1))
					seeded[catchIP] = true
					if prev, ok := in[catchIP]; ok {
						if !same(prev, outer) {
							return nil, false
						}
					} else {
						in[catchIP] = outer
						changed = true
					}
				case OpTryPop:
					if len(stack) == 0 {
						return nil, false
					}
					stack = stack[:len(stack)-1]
				}
				pop, push, ok := jitStackEffect(fn, ip)
				if !ok {
					return nil, false
				}
				d = d - pop + push
				ip += int(opTable[op].Size)
			}
			for _, s := range b.succ {
				sb, ok := blocks[s]
				if !ok || !sb.reachable || seeded[s] {
					continue
				}
				if prev, ok := in[s]; ok {
					if !same(prev, stack) {
						return nil, false
					}
					continue
				}
				in[s] = append([]jitHandler(nil), stack...)
				changed = true
			}
		}
	}
	return in, true
}

// jitMaxOperandDepth reports how deep the operand stack gets, which is what the
// frame's operand array is sized to and what decides whether that array fits in
// the context.
//
// Computed before a byte is emitted, from the same per-instruction effect the
// emitter uses — and the emitter checks it, refusing when its own depth after
// an instruction is not what this predicted. So a wrong answer here costs a
// refusal and never a store past the end of the array.
func jitMaxOperandDepth(fn *svFunc, blocks map[int]*jitBlock, depths map[int]int) (int, bool) {
	code := fn.code
	max := 0
	for bs, b := range blocks {
		if !b.reachable {
			continue
		}
		d := depths[bs]
		for ip := b.start; ip < b.end; {
			pop, push, ok := jitStackEffect(fn, ip)
			if !ok {
				return 0, false
			}
			d = d - pop + push
			if d > max {
				max = d
			}
			size := int(opTable[Opcode(code[ip])].Size)
			if size <= 0 {
				return 0, false
			}
			ip += size
		}
	}
	return max, true
}
