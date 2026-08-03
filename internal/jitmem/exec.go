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
	// Spill is where compiled code leaves its operand stack when it has to
	// return to the runtime part way through a function.
	//
	// Generated code keeps the operand stack in registers, and returning to Go
	// loses every one of them. A compiler that knows its stack depth at each
	// point — which a template compiler does, because it assigns the stack
	// positionally — can write exactly the live slots here and read them back on
	// the way in. That, rather than anything about deoptimisation, is what a
	// compiled function needs before it can call out mid-expression.
	Spill [SpillSlots]uint64
	// SpillN is how many of Spill are live, written by compiled code before it
	// leaves. The runtime's collector has no other way to know: a spilled slot
	// holds a Value, and a stale one from an earlier call holds a handle to a
	// cell that may since have been freed.
	SpillN uint64
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
}

// SpillSlots bounds the operand stack a compiled function may hold across a
// call out. It matches the number of registers the emitter has to give it.
const SpillSlots = 10

// Field offsets generated code compiles against.
const (
	CtxOffExit   = 0
	CtxOffHelper = 8
	CtxOffArgs   = 16
	CtxOffRet    = 48
	CtxOffResume = 56
	CtxOffSpill  = 64
	CtxOffSpillN = 64 + 8*SpillSlots
	CtxOffPool   = 72 + 8*SpillSlots
	CtxOffHost   = 80 + 8*SpillSlots
	CtxOffThis   = 88 + 8*SpillSlots
	CtxOffUpvals = 96 + 8*SpillSlots
	CtxOffFnVal  = 104 + 8*SpillSlots
	CtxSize      = 112 + 8*SpillSlots
)

// Which fields hold a Value, and so must be traced by the runtime's collector
// while compiled code is suspended in a helper:
//
//	Args[2]  the operand a helper was handed
//	Ret      the operand it produced
//	Spill[0:SpillN]
//	This     the frame's receiver
//	FnVal    the running function itself
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
