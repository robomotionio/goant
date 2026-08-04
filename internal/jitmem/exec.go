package jitmem

import "unsafe"

// Exit codes generated code leaves in ExecContext.Exit before returning to the
// trampoline. Zero means "finished"; everything else means the runtime has to do
// something and then resume.
const (
	ExitReturn  uint64 = iota // the compiled function is done; Ret holds its value
	ExitHelper                // run Helper with Args, put the result in Ret, resume
	ExitPreempt               // Go wants this goroutine to yield
	ExitDeopt                 // a guard failed; fall back to the interpreter
	// ExitTailCall is a proper tail call: the frame is finished and the spill
	// area holds the callee, its arguments and — for the method form — the
	// receiver. It is an exit rather than a helper because the point of a tail
	// call is that this frame is gone before the next one starts, and a helper
	// runs with the compiled frame still on the Go stack.
	ExitTailCall
	// ExitCallout says nothing about this frame except that a frame it called in
	// machine code wants something. See the note on Next: a compiled call is a
	// real CALL, so when the callee has to reach the runtime every frame between
	// it and Go has to step out of the way first. Each one saves its operands and
	// its resume address and returns, and this is the code it returns with.
	ExitCallout
)

// ExecContext is the state that generated code and the runtime share.
//
// Generated code reaches it through a register the entry trampoline loads
// (R13 on amd64, R10 on arm64) and addresses its fields by fixed offsets, so the
// layout is part of the JIT ABI: reordering these fields silently miscompiles
// every block already emitted. ctxOffset* below are that layout, and
// TestExecContextLayout is what keeps the two in agreement.
//
// It holds no Go pointers on purpose. goant's Value is a NaN-boxed uint64 over
// non-moving pools, so everything generated code touches is an integer as far as
// Go's collector is concerned, and none of this has to be traced or shadowed.
type ExecContext struct {
	Exit   uint64
	Helper uint64
	Args   [4]uint64
	Ret    uint64
	Resume uintptr
	// Spill is the frame's operand stack, for the functions it is big enough
	// for — which is all but nineteen of the Octane corpus.
	//
	// Generated code keeps the top of the stack in registers, because that is
	// what makes arithmetic worth compiling, and returning to Go loses every one
	// of them. A compiler that knows its depth at each point — which a template
	// compiler does, because it assigns the stack positionally — writes exactly
	// the live slots here and reads them back on the way in. That, rather than
	// anything about deoptimisation, is what a compiled function needs before it
	// can call out mid-expression.
	Spill [InlineSlots]uint64
	// StackN is how many slots are live, written by compiled code before it
	// leaves. The runtime's collector has no other way to know: a slot holds a
	// Value, and a stale one from an earlier call holds a handle to a cell that
	// may since have been freed.
	StackN uint64
	// Stack is where the operand stack lives instead, for a function that wants
	// more slots than Spill has. Zero otherwise.
	//
	// The depth a function needs is a property of the function: most want two or
	// three, and a source file that is one enormous array literal wants
	// seventeen thousand. Sizing every frame for the second is absurd and
	// refusing it is arbitrary, so such a function gets an array of its own and
	// generated code addresses it through this pointer.
	//
	// Which of the two a function uses is decided once, before a byte is
	// emitted, and never mixed, so no site tests anything at run time.
	//
	// An unsafe.Pointer rather than a uintptr, unlike every other address here.
	// This one can point into a heap array that nothing else refers to, so it
	// has to be a pointer Go's collector understands; and the runtime reads
	// operands through it on every call out, which a uintptr would make either
	// unsound or a conversion per access.
	Stack unsafe.Pointer
	// Pool is the address of the runtime's object pool, which compiled code
	// needs to turn a Value's handle into an object.
	//
	// Passed in rather than compiled in because two runtimes have two pools, and
	// code compiled while running one must never resolve a handle against the
	// other's — it would land on a live cell holding an unrelated object. One
	// load per property access is what makes that impossible rather than
	// unlikely.
	Pool uintptr
	// Host is the address of the runtime itself, for the few pieces of runtime
	// state compiled code has to read or write directly rather than through a
	// helper. A property store is the one that matters: it has to record that it
	// reached an object older than the running invocation, and a call out to
	// report that would cost more than the store.
	//
	// A uintptr rather than a pointer for the same reason as everything else
	// here — the field is not a root, and it does not need to be: the runtime is
	// live for as long as any of its compiled frames can run, held by the call
	// that entered them.
	Host uintptr
	// This is the frame's receiver, which is the one thing a frame carries that
	// is neither a local nor an operand. Compiled code is handed a locals slice
	// and the prologue that binds `this` writes a slot from a value that is not
	// in it, so without this field a method could not be compiled at all.
	//
	// Unlike everything else here it holds a Value, so the runtime's collector
	// must trace it — see the jitFrames loop in collect.go.
	This uint64
	// Upvals is the address of the closure's upvalue array — the `[]*upvalue`
	// itself, not a copy — for the frames that have one. Zero when the closure
	// has no upvalues, which is also every frame whose function contains no
	// GET_UPVAL, so compiled code never reads it in that case.
	//
	// The array holds Go pointers and this does not root them; the closure does,
	// and the frame publishes the closure for as long as it runs.
	Upvals uintptr
	// FnVal is the running function's own Value, for the self-reference a named
	// function expression binds: `(function f(){ return f; })` resolves `f` to
	// the function itself, and it is not in the locals or anywhere else compiled
	// code can reach.
	//
	// A Value, so the collector traces it beside This — the closure is reachable
	// from the caller too, but not on every path a compiled frame can be entered
	// by, and a root that is usually redundant is cheaper than one that is
	// usually absent.
	FnVal uint64
	// Next is the context one frame deeper, and it is what lets compiled code
	// call compiled code without going through Go at all.
	//
	// A call was an exit and a re-entry: the caller returned to the runtime, the
	// runtime built the callee's frame and entered it, and the answer came back
	// the same way. Two transitions through Go per call, each an indirect branch
	// to an address that changes every time. Half of DeltaBlue was that round
	// trip rather than either function's work.
	//
	// Generated code can do the whole of it — set up the callee's context, copy
	// the arguments and CALL — provided it has somewhere to put the callee's
	// frame. That is this: the contexts are a chain, one per depth, built by the
	// runtime and never freed, so the callee's is a load rather than an
	// allocation and its address is stable for as long as anything can be
	// suspended in it. Zero ends the chain, and a call site that finds it there
	// takes the old path.
	Next uintptr
	// Site is the call site that entered this frame, or nil when the runtime
	// did. It is the frame's identity: a suspended frame has to be given back its
	// function, its closure and its compiled code before a helper can run for it,
	// and this is where all three are.
	//
	// The second field here that is a pointer rather than an address, and the
	// only one generated code writes. Two things make that sound. Go does not
	// move what it has allocated, so the address a call site compiled in is the
	// address it stays at; and the array it points into is reachable from the
	// compiled code that holds it whether this field names it or not, so the
	// write barrier generated code cannot perform is one that had nothing to do
	// — both the value being overwritten and the value replacing it are live for
	// other reasons. What is not optional is that the field always holds either
	// nil or one of those addresses, because Go's collector reads it as a
	// pointer; an eight-byte aligned store is what keeps that true at every
	// instant rather than merely afterwards.
	Site unsafe.Pointer
	// Nest counts the compiled frames between this one and the last entry from
	// Go, and it is a stack-depth bound rather than a statistic.
	//
	// Generated code runs on the goroutine's stack, and it is entered from a
	// NOSPLIT trampoline: nothing can grow that stack while a compiled frame is
	// on it, because morestack would have to walk frames the Go runtime has no
	// map for. Compiled calls nest on the same stack, so how many of them may be
	// live at once is a fixed budget rather than a question — jitMaxNest — and
	// past it a call site takes the old path, which starts by unwinding back to
	// Go and so leaves the stack as Go expects to find it.
	Nest uint64
	// NLocals is how many of Locals this frame is using, for the collector, and
	// zero for a frame the runtime entered — those keep their locals in the
	// frame slab, where markFrames already finds them.
	NLocals uint64
	// Deep says which of the two operand stacks the frame running here is using,
	// and it is checked by a compiled call site rather than only read by the
	// runtime. A context is reused at its own depth, so one that once held a
	// function wanting a large operand stack still points Stack at that array —
	// and a callee compiled for the inline one would write past it. Such a call
	// takes the old path, which sizes the stack on the way in.
	Deep uint64
	// Locals is the frame's variables, for a frame compiled code entered on its
	// own.
	//
	// The runtime's own frames take theirs from a per-depth slab, which is the
	// same idea and cannot be used here: handing one out means deciding whether
	// the last frame at this depth let its locals escape, and that decision is a
	// Go pointer's worth of bookkeeping. A compiled call is only made to a
	// function that cannot let them escape — no closure over a local, no
	// `arguments` — so the context can carry them itself and the whole question
	// disappears.
	Locals [InlineLocals]uint64
	// stack is the array Stack points into. Held here so that Go's collector
	// keeps it alive for as long as the context can be entered, and unexported
	// so that it sits past every offset generated code compiles against.
	stack []uint64
}

// EnsureStack gives the context room for n operand slots and points Stack at
// them.
//
// Called once per frame entry, never while generated code is running, so the
// address it publishes cannot move under a frame that is using it. A context is
// reused at its own depth, so a function that once needed a large stack leaves
// one behind for the next frame at that depth rather than allocating again.
func (ctx *ExecContext) EnsureStack(n int) {
	ctx.StackN = 0
	if n <= InlineSlots {
		// The usual case, and it writes nothing unless the previous frame at
		// this depth was a deep one: a pointer store into the context is a Go
		// write barrier, and this runs on every compiled call.
		if ctx.Deep != 0 || ctx.Stack == nil {
			ctx.Stack = unsafe.Pointer(&ctx.Spill[0])
			ctx.Deep = 0
		}
		return
	}
	if len(ctx.stack) < n {
		ctx.stack = make([]uint64, n)
	}
	ctx.Stack = unsafe.Pointer(&ctx.stack[0])
	ctx.Deep = 1
}

// Slots is the live part of the operand stack.
//
// The runtime reads it through the slice rather than through Stack, because a
// uintptr keeps nothing alive and the slice does. Generated code reads it
// through Stack, because it indexes by a compile-time offset and cannot follow
// a slice header. They are the same array.
func (ctx *ExecContext) Slots() []uint64 {
	n, max := int(ctx.StackN), len(ctx.Spill)
	if ctx.Deep != 0 {
		max = len(ctx.stack)
	}
	if n > max {
		n = max
	}
	if ctx.Deep != 0 {
		return ctx.stack[:n]
	}
	return ctx.Spill[:n]
}

// SlotPtr is the address of operand slot i, for the runtime's hot path.
//
// The alternative is building a slice per call out, and a call out is about
// five nanoseconds all in — the slice header was measurable across Octane.
func (ctx *ExecContext) SlotPtr(i int) unsafe.Pointer {
	return unsafe.Add(ctx.Stack, 8*i)
}

// InlineSlots is how many operand slots Spill holds. Thirty-two covers every
// function in the Octane corpus but nineteen, and all nineteen are a source
// file that is mostly one array literal.
const InlineSlots = 32

// InlineLocals is how many variables Locals holds, and so how large a function
// compiled code will call directly. A function wanting more is called the way
// every function used to be.
const InlineLocals = 32

// Field offsets generated code compiles against.
const (
	CtxOffExit   = 0
	CtxOffHelper = 8
	CtxOffArgs   = 16
	CtxOffRet    = 48
	CtxOffResume = 56
	CtxOffSpill  = 64
	CtxOffStackN = 64 + 8*InlineSlots
	CtxOffStack  = 72 + 8*InlineSlots
	CtxOffPool   = 80 + 8*InlineSlots
	CtxOffHost   = 88 + 8*InlineSlots
	CtxOffThis    = 96 + 8*InlineSlots
	CtxOffUpvals  = 104 + 8*InlineSlots
	CtxOffFnVal   = 112 + 8*InlineSlots
	CtxOffNext    = 120 + 8*InlineSlots
	CtxOffSite    = 128 + 8*InlineSlots
	CtxOffNest    = 136 + 8*InlineSlots
	CtxOffNLocals = 144 + 8*InlineSlots
	CtxOffDeep    = 152 + 8*InlineSlots
	CtxOffLocals  = 160 + 8*InlineSlots
	CtxSize       = 160 + 8*InlineSlots + 8*InlineLocals
)

// Which fields hold a Value, and so must be traced by the runtime's collector
// while compiled code is suspended in a helper:
//
//	Args[2]  the operand a helper was handed
//	Ret      the operand it produced
//	Stack[0:StackN]
//	This     the frame's receiver
//	FnVal    the running function itself
//	Locals[0:NLocals]
//
// Args[0] and Args[1] are a pointer and a counter, and Args[3] is an immediate.
// Tracing either as a Value would be worse than missing one.

// Enter calls generated code at pc with ctx in the ABI's context register and
// returns whatever the code left in the return register.
//
// The trampoline is NOSPLIT: generated code must not cause the goroutine stack
// to grow, because morestack would run with a return address the runtime cannot
// map to a Go function.
//
//go:noescape
func Enter(pc uintptr, ctx *ExecContext) uint64

// Run drives a compiled function to completion, servicing whatever generated
// code asks for along the way.
//
// This is the exit-and-re-enter protocol: generated code cannot CALL a Go
// function directly, so instead of calling it records what it wants and returns.
// The cost of one lap of this loop is what decides how much a compiled function
// can afford to delegate — see BenchmarkRoundTrip.
func Run(pc uintptr, ctx *ExecContext, helper func(*ExecContext)) uint64 {
	for {
		Enter(pc, ctx)
		switch ctx.Exit {
		case ExitReturn:
			return ctx.Ret
		case ExitHelper:
			helper(ctx)
			pc = ctx.Resume
		default:
			return ctx.Ret
		}
	}
}

// Addr is the address of ctx, for generated code that needs to embed it as an
// immediate rather than receive it in the context register.
func (ctx *ExecContext) Addr() uintptr { return uintptr(unsafe.Pointer(ctx)) }
