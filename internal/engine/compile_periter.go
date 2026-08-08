package engine

// Per-iteration lexical bindings, and when they cost nothing.
//
// `for (let i = 0; i < n; i++)` gives every iteration its own binding, so a
// closure made on one iteration keeps that iteration's value. The compiler
// implements it by detaching any open upvalue over the slot at each turn of the
// loop — OpCloseUpval, twice per loop: once before the first iteration and once
// on the back edge.
//
// Only a closure can open an upvalue. A loop that creates none has nothing for
// the opcode to detach, and it runs anyway: an interpreted loop pays a map
// lookup per iteration for an answer that is always "nothing", and a compiled
// one does not run at all, because the tier has no template for OpCloseUpval
// and refuses any function containing it.
//
// That second cost is the one that mattered. `for (let ...)` is how loops are
// written now, so the refusal covered most modern JavaScript — including every
// Robomotion Function node — while Octane, which is old enough to use `var`
// throughout, compiled fine and reported 99.9% coverage. A corpus only tells
// you about the code that is in it.
//
// So: emit the opcode when the loop contains something that could capture, and
// not otherwise. The test is deliberately crude — any function-creating
// construct anywhere in the loop, or any mention of `eval`, counts — because a
// wrong "no" here is a closure silently sharing one binding across every
// iteration, which is the exact bug per-iteration bindings exist to prevent. A
// wrong "yes" only costs what the loop cost before this change.

// mayCapture reports whether anything in n could hold a binding past the
// iteration that made it.
//
// Memoised, and that is not an optimisation. Loops nest, and each one asks
// about its whole subtree; without the memo a chain of nested loops walks the
// innermost body once per enclosing loop, which is a compile-time quadratic of
// exactly the kind this compiler has grown before.
func (c *compiler) mayCapture(n *Node) bool {
	if n == nil {
		return false
	}
	if c.capCache == nil {
		c.capCache = make(map[*Node]bool, 32)
	}
	if v, ok := c.capCache[n]; ok {
		return v
	}
	// Seeded false before descending so a cyclic or re-entered node cannot
	// recurse forever; the real answer overwrites it below.
	c.capCache[n] = false
	v := c.mayCaptureUncached(n)
	c.capCache[n] = v
	return v
}

func (c *compiler) mayCaptureUncached(n *Node) bool {
	switch n.Kind {
	case NFunc, NArrow, NClass, NMethod:
		// A function, however it is written, is the only thing that can hold a
		// binding past the iteration that made it.
		return true
	case NIdent:
		// A direct eval can create a closure this compiler never sees. Any
		// mention of the name counts: whether a call is a direct eval is settled
		// later, and being wrong here costs one opcode.
		if n.Str == "eval" {
			return true
		}
	}
	for _, k := range [...]*Node{
		n.Left, n.Right, n.Cond, n.Body,
		n.Init, n.Update,
		n.CatchParam, n.CatchBody, n.FinallyBody,
	} {
		if c.mayCapture(k) {
			return true
		}
	}
	for _, a := range n.Args {
		if c.mayCapture(a) {
			return true
		}
	}
	return false
}
