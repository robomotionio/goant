//go:build amd64

package engine

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
	in         []bool // locals assigned on entry, on every path
	reachable  bool
}

// jitAnalyze splits the body into basic blocks and solves definite assignment
// over them. It returns the entry set for each block, keyed by its start ip.
//
// Parameters are seeded as assigned because the compiled prologue checks them,
// and a function is refused outright if the bytecode does not decode cleanly —
// which cannot happen with goant's compiler, but an analysis that guessed would
// be worse than one that declined.
func jitAnalyze(fn *svFunc, start int, targets map[int]bool) (map[int]*jitBlock, bool) {
	code := fn.code
	nloc := fn.maxLocals

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
			return nil, false
		}
		switch op {
		case OpJmp, OpJmpFalse, OpJmpTrue, OpReturn, OpReturnUndef:
			if ip+size < len(code) {
				leaders[ip+size] = true
			}
		}
		ip += size
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
			cur = &jitBlock{start: ip, gen: make([]bool, nloc), in: make([]bool, nloc)}
			blocks[ip] = cur
		}
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		switch op {
		case OpPutLocal, OpSetLocal:
			i := int(readU16(code, ip+1))
			if i >= nloc {
				return nil, false
			}
			cur.gen[i] = true
		case OpJmp:
			cur.succ = []int{int(readU32(code, ip+1))}
		case OpJmpFalse, OpJmpTrue:
			cur.succ = []int{int(readU32(code, ip+1)), ip + size}
		case OpReturn, OpReturnUndef:
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
				return nil, false // a branch into the middle of an instruction
			}
		}
	}

	// Reachability, so that a block no path arrives at cannot drag the
	// intersection down to nothing for the blocks that follow it.
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

	out := func(b *jitBlock) []bool {
		o := make([]bool, nloc)
		for i := range o {
			o[i] = b.in[i] || b.gen[i]
		}
		return o
	}

	for changed := true; changed; {
		changed = false
		for ip, b := range blocks {
			if ip == start || !b.reachable {
				continue
			}
			for i := range b.in {
				if !b.in[i] {
					continue
				}
				for _, p := range preds[ip] {
					if o := out(blocks[p]); !o[i] {
						b.in[i] = false
						changed = true
						break
					}
				}
			}
		}
	}
	return blocks, true
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
func jitNumberDemand(fn *svFunc, blocks map[int]*jitBlock) ([]bool, bool) {
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
			// Every block is entered with an empty operand stack, so the origins
			// of one block's slots never reach another's.
			var org []int
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
				case OpGetField:
					// The receiver is demanded of nothing: the template reads a
					// field of whatever it is handed, or asks the runtime to.
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
				case OpCall, OpCallMethod:
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
				case OpConst, OpConstI8, OpUndef, OpNull, OpTrue, OpFalse, OpThis,
					OpGetGlobal:
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
					cur[i] = true
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
func jitNumericLocals(fn *svFunc, blocks map[int]*jitBlock, demand []bool) ([]bool, bool) {
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
			// Every block is entered with an empty operand stack — the emitter
			// requires it — so each can be walked on its own.
			var kinds []bool
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
				case OpUndef, OpNull, OpTrue, OpFalse:
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
					// Still refused unless both operands are Numbers, so the
					// 32-bit result always is one.
					if _, ok := pop(); !ok {
						return nil, false
					}
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(true)
				case OpNeg, OpBnot, OpInc, OpDec:
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(true)
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
				case OpGetField:
					// The object goes in and whatever it held comes out, which
					// is not a Number as far as this tier can tell.
					if _, ok := pop(); !ok {
						return nil, false
					}
					push(false)
				case OpGetGlobal:
					// A global holds anything at all.
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
				case OpCall, OpCallMethod:
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
