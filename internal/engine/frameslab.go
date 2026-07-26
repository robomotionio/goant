package engine

// Frame storage reuse.
//
// Entering a call allocated two slices: the frame's locals and its operand
// stack. On a call-heavy program that is most of the garbage the engine
// produces — Octane's Richards spent a sixth of its time in the allocator and
// the collector, for buffers whose lifetime is exactly one call.
//
// Frames are strictly nested, so the storage a frame used is free the moment it
// returns, and the next frame at that depth can have it. One buffer per depth
// therefore serves every frame that will ever run there; a program settles into
// its working set within the first few calls and stops allocating for frames
// altogether.
//
// The exception is a frame that lets its locals outlive it. Four things do
// that: capturing an upvalue, a mapped `arguments` object aliasing the
// parameters, a module environment, and a Script's global lexical bindings. Any
// of them drops the depth's entry, so the escaped slice is never handed out
// again and the next frame at that depth allocates its own.
//
// Only the driving goroutine may use these. A suspended generator holds live
// frames at depths the driver is about to reuse, so it gets its own set,
// swapped in for the duration of each resume (see genDrive).

// frameSlab is the storage retained for one call depth.
type frameSlab struct {
	locals []Value
	stack  []Value
}

// vmFrame is what a live call publishes about itself so the collector can find
// the values it is holding.
//
// The interpreter keeps its hot state — the operand stack, the locals, the
// instruction pointer — in Go locals, which no collector can walk. This is the
// side channel: written once at frame entry, and again only where a published
// slice can actually change.
//
// stack is scanned to its full capacity rather than its length. That is not
// laziness: an opcode handler pops its operands before calling out, so at the
// moment a nested call collects, the receiver and arguments of the call in
// progress live in exactly that region above the stack pointer. Scanning it
// retains a little garbage and keeps those alive.
type vmFrame struct {
	// fn and cl are the code this frame is running, published so the collector
	// can reach its constant pool. An ordinary function is reachable through the
	// function object that was called, but a script or an eval body is reachable
	// from nowhere at all. Most constants are interned strings and would survive
	// regardless; a tagged template's frozen strings array is built at compile
	// time, lives only in the pool, and would not.
	fn *svFunc
	cl *closure

	locals    []Value
	stack     []Value
	args      []Value
	withStack []Value
	thisVal   Value
	fnVal     Value
	varObj    Value
	newTarget Value
	// pending and completed are the values in flight during an unwind: a thrown
	// exception, and the operand of a return or a throw being carried across a
	// finally block. A finally body runs ordinary code, so a collection can
	// happen while one of these is the only reference to it.
	pending   Value
	completed Value
}

// slabMaxDepth bounds how deep the cache goes. Beyond it frames allocate
// normally rather than retaining a buffer per level of a deep recursion, whose
// peak depth can be thousands of frames.
const slabMaxDepth = 256

// slabMaxValues bounds what a single depth may retain. A function with a huge
// frame is rare and usually called once; keeping its buffer alive for the
// program's lifetime costs more than reallocating it.
const slabMaxValues = 1024

// frameLocals returns a locals slice of exactly n values, all undefined,
// reusing the buffer held for this depth when there is one.
func (rt *Runtime) frameLocals(depth, n int) []Value {
	if depth < 0 || depth >= slabMaxDepth || n > slabMaxValues {
		return freshLocals(n)
	}
	if depth >= len(rt.slabs) {
		rt.growSlabs(depth)
	}
	s := &rt.slabs[depth]
	if cap(s.locals) < n {
		s.locals = make([]Value, n)
	}
	v := s.locals[:n]
	fillUndef(v)
	return v
}

// frameStack returns an empty operand stack with room for n values.
//
// Nothing outside a frame ever holds its operand stack, so unlike locals this
// buffer needs no escape accounting. A stack that outgrows n is reallocated by
// append and simply is not the cached one any more.
func (rt *Runtime) frameStack(depth, n int) []Value {
	if depth < 0 || depth >= slabMaxDepth || n > slabMaxValues {
		return make([]Value, 0, n)
	}
	if depth >= len(rt.slabs) {
		rt.growSlabs(depth)
	}
	s := &rt.slabs[depth]
	if cap(s.stack) < n {
		s.stack = make([]Value, 0, n)
	}
	return s.stack[:0]
}

// dropFrameLocals gives up the buffer cached for this depth, because the frame
// occupying it has handed its locals to something that outlives the call.
func (rt *Runtime) dropFrameLocals(depth int) {
	if depth >= 0 && depth < len(rt.slabs) {
		rt.slabs[depth].locals = nil
	}
}

func (rt *Runtime) growSlabs(depth int) {
	grown := make([]frameSlab, depth+8)
	copy(grown, rt.slabs)
	rt.slabs = grown
}

// swapSlabs installs a different set of per-depth buffers and returns the
// previous one, so a coroutine's live frames are not overwritten by the
// driver's (or the other way round).
func (rt *Runtime) swapSlabs(next []frameSlab) []frameSlab {
	prev := rt.slabs
	rt.slabs = next
	return prev
}

func freshLocals(n int) []Value {
	v := make([]Value, n)
	fillUndef(v)
	return v
}

// fillUndef sets every element to undefined.
//
// The zero Value decodes as the number 0.0, so make's zeroing is not enough and
// a reused buffer holds the last frame's values. Written as a doubling copy
// because the loop the compiler emits for a non-zero constant assigns one
// element at a time, where copy moves them a cache line at a time.
func fillUndef(v []Value) {
	if len(v) == 0 {
		return
	}
	v[0] = mkundef()
	for i := 1; i < len(v); i *= 2 {
		copy(v[i:], v[:i])
	}
}
