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
}

// Field offsets generated code compiles against.
const (
	CtxOffExit   = 0
	CtxOffHelper = 8
	CtxOffArgs   = 16
	CtxOffRet    = 48
	CtxOffResume = 56
	CtxSize      = 64
)

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
