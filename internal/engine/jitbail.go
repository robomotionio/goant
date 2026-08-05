package engine

// Handing a frame back partway through, which is the thing a speculating tier
// is built on and the thing this one has never been able to do.
//
// Until now a compiled frame had two ways to end: it produced the answer, or it
// declined its arguments before running a single instruction. Both are total.
// The second is what every guard in the tier has had to be expressible as, and
// that is a hard constraint on what may be assumed: an assumption has to be
// checkable from the arguments alone, at entry, or it cannot be made. It is why
// jitNumericLocals is a proof over the whole body rather than a guess about the
// common case — a local that is a Number nine hundred times and a String once
// has to be compiled for the String, because there is nowhere for the discovery
// to go.
//
// A bail is the third ending. The frame stops where it is, says which bytecode
// instruction it stopped before, and the interpreter carries it the rest of the
// way. What that buys is the right to be wrong: code can be emitted for what the
// program has actually been doing, with a check that costs a compare, and the
// case it was not written for stops being a reason to refuse the function.
//
// What makes it affordable here is that there is no state to translate. A
// compiled frame and an interpreted one hold the same things in the same
// shapes — locals are one flat array of NaN-boxed Values that both address
// directly, and the operand stack is another. So the entire description of a
// bail point is one number, the bytecode offset, and the work at run time is
// spilling the few operands that were in registers. There is no register map,
// no per-site descriptor, and nothing to box.

// jitResume is a compiled frame handed to the interpreter to be finished.
//
// It goes one way only, from jitRunAt to runFrameBody, and nobody between them
// sees it. That is deliberate: a bail is not a third answer the callers of
// compiled code have to learn about — jitRun still either produces the value or
// declines, and a frame that stopped partway produces the value, by the longer
// route of interpreting the rest of itself.
//
// Which means the interpreter is entered one Go frame below the compiled runner
// rather than above it. The alternative was a fourth return value threaded
// through five signatures and every test that calls them, to tell the callers
// something none of them would have done anything with.
type jitResume struct {
	// ip is the instruction the interpreter must run next. The frame stopped
	// *before* it, so this is the offset the emitter was about to compile rather
	// than the one after.
	ip int
	// stack is the operand stack as compiled code left it, copied rather than
	// aliased: it comes out of a context that goes back on the chain on the way
	// out of the run loop, and the next frame at that depth would write over it.
	//
	// One allocation per bail, which is a cost speculation is allowed to pay. A
	// guard that fails often enough for this to be measurable is a guard that
	// should not have been emitted.
	stack []Value
	// locals is the frame's variables — the array compiled code has been writing
	// this whole time, which the interpreter must go on writing rather than
	// fetch again for itself.
	locals []Value
	// openUpvals is the cells a direct eval or a CLOSURE in the compiled body
	// created over those locals, which the interpreter adopts as its own.
	//
	// Nil for nearly every frame. It matters because both halves keep the same
	// map for the same reason — one cell per captured slot, shared by every
	// closure over it — and a frame that built some in compiled code and then
	// built more in the interpreter would otherwise have two cells for one
	// variable, which is a write that stops being visible rather than a crash.
	openUpvals map[int]*upvalue
}
