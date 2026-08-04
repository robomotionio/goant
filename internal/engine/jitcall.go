package engine

import (
	"unsafe"

	"github.com/robomotionio/goant/internal/jitmem"
)

// A compiled function calling a compiled function.
//
// Every call a compiled body made used to leave. The frame spilled its
// operands, returned to Go, and the runtime resolved the callee, built its
// frame and entered it — then the answer came back the same way. Two
// transitions through the entry trampoline per call, each an indirect branch to
// an address that changes every time, around a dozen Go frames of bookkeeping.
// It measured 4.69 nanoseconds against 1.15 for the machine instruction it
// stands in for, and on DeltaBlue it was half the run: 29% of the profile was
// generated code and 50% was the round trip around it.
//
// The seven attempts to make that cheaper from the Go side are what say it
// cannot be: what costs is the trip, not what is done during it. So the call is
// emitted. A call site guarded on the callee it saw last sets up the callee's
// context, copies the arguments into it and CALLs — and when nothing in the
// callee has to reach the runtime, nothing does.
//
// Three things make that possible without giving the collector a frame it
// cannot see:
//
//   - The contexts are a chain rather than an allocation. jitCtxAt builds one
//     per depth and links it to the next, so a call site finds the callee's
//     frame with a load, and the chain is walked by markRoots exactly as the old
//     slice was — see jitDepth.
//   - The callee's locals live in its context, so the frame it publishes for
//     the collector is the context itself. That is only sound for a function
//     that cannot let its locals outlive it, which is what jitMachineCallable
//     is about.
//   - Anything the callee cannot do for itself unwinds first. Generated code
//     runs on the goroutine stack under a NOSPLIT trampoline, so Go must never
//     run with a compiled frame still on that stack — morestack would have to
//     walk frames it has no map for. A compiled frame that needs the runtime
//     therefore returns to its caller, which saves its own operands and returns
//     in turn, all the way out. See ExitCallout.
//
// What it costs, beyond the restrictions: a frame entered this way is not in
// rt.frames, so it does not appear in Error.stack. The trace walks the runtime's
// own frame array — see captureTrace — and putting a compiled frame there means
// writing a *svFunc and a slice header from machine code, which is the one thing
// none of this may do. A trace taken under a compiled call chain is therefore
// short by however many of these frames are between the throw and the last
// frame the runtime entered. Nothing in the language depends on it and nothing
// in test262 tests it, but it is a real difference and not a subtle one.

// jitMaxNest is how many compiled frames may be on the goroutine stack at once.
//
// A bound rather than a tuning knob. Each one costs a return address and a
// saved context register, and the trampoline that entered the outermost has a
// zero-byte frame and is NOSPLIT — so what is below it is the Go runtime's
// fixed guard allowance, about eight hundred bytes, and there is no way to ask
// for more: growing the stack means copying frames that have no stack map.
// Sixteen frames at sixteen bytes is under a third of it.
//
// Passing it is not a failure. The call site takes the path every call took
// before, which begins by unwinding back to Go and so hands the stack back in
// the state Go expects — and the frame entered from there starts a fresh
// budget. Measured across the four call-heavy Octane workloads, one is clearly
// too few (DeltaBlue 648 against 882) and four, eight and sixteen are the same
// number.
const jitMaxNest = 16

// jitMaxChain bounds how far the context chain is built ahead of the deepest
// frame that has run, and so how much memory a deep recursion leaves behind.
const jitMaxChain = 4096

// jitCallSite is one compiled call site's memory of what it called.
//
// Read by generated code at fixed offsets and filled only by the runtime, which
// is what lets it hold Go pointers: the fields machine code touches are a Value
// and three integers, and fn, cl and code are here so that a frame suspended in
// this callee can be given back its identity — machine code cannot write a Go
// pointer, so what it writes into the frame's context is this site's address.
//
// Soundness is the epoch's, exactly as for jitResolveCallee. callee is a
// handle, and a handle names a cell only until the next collection; entry and
// upvals are raw addresses into structures reached from it. collect() bumps the
// counter, so one compare retires all of them together, and a retired site
// fills again on its next call.
type jitCallSite struct {
	callee Value
	epoch  uint32
	// argc is the site's argument count, which the fill requires the callee's
	// parameter list to match exactly: the caller copies a fixed number of
	// arguments and the callee's entry stub clears from a fixed index, and
	// neither knows the other's number at the time it is emitted.
	argc uint16
	// method records that a receiver sits below the callee on the operand stack,
	// so the runtime can find the arguments again if the callee declines them.
	method bool
	// declines counts entries the callee's stub turned away, and dead records
	// that this site has stopped offering them. A decline is not free — the
	// frame was built and thrown away — so a site whose receiver the stub cannot
	// settle stops paying for the attempt.
	declines uint8
	dead     bool

	// entry is the callee's machine-call entry: the stub that clears the locals
	// the arguments did not fill, settles `this`, and checks the parameters.
	entry uintptr
	// upvals is the closure's upvalue array. Part of the cache rather than
	// fetched per call because the site is guarded on the closure's identity,
	// which is what makes the array's address a constant here.
	upvals uintptr

	fn   *svFunc
	cl   *closure
	code *jitCode
}

// Where generated code reads a site. Derived rather than written down, for the
// reason jitlayout.go gives.
const (
	jitOffSiteCallee = unsafe.Offsetof(jitCallSite{}.callee)
	jitOffSiteEpoch  = unsafe.Offsetof(jitCallSite{}.epoch)
	jitOffSiteEntry  = unsafe.Offsetof(jitCallSite{}.entry)
	jitOffSiteUpvals = unsafe.Offsetof(jitCallSite{}.upvals)
)

// jitSiteAddr is the address of one site, baked into the code that reads it.
//
// Constant for the life of the compiled function: the slice is allocated once,
// before a byte is emitted, and nothing grows it. The array is reachable from
// the compiled code that uses it, so it cannot be collected while any of that
// code can run — the same argument jitICWayAddr makes.
func jitSiteAddr(sites []jitCallSite, i int) uintptr {
	return uintptr(unsafe.Pointer(&sites[i]))
}

// jitCtxAt is the context for a compiled frame at this depth, building it and
// the chain below it if this is the deepest anything has gone.
//
// The chain is built ahead of where it is needed, and it has to be: a compiled
// call finds the callee's context through the caller's Next, so if the chain
// only ever grew where the runtime had already entered a frame, the first
// compiled call at a depth would be the only one — the second would find no
// context one deeper, take the slow path, and never build one, because taking
// the slow path is what building one used to require.
func (rt *Runtime) jitCtxAt(depth int) *jitmem.ExecContext {
	want := depth + 1
	if want < jitMaxChain {
		want = depth + 1 + jitMaxNest
		if want > jitMaxChain {
			want = jitMaxChain
		}
	}
	for len(rt.jitFrames) < want {
		ctx := new(jitmem.ExecContext)
		// The pieces that are the runtime's rather than the frame's, written
		// once. A compiled call site sets what changes per call and no more, and
		// these are the ones it would otherwise have to copy from its own
		// context every time.
		ctx.Pool = jitObjectPoolAddr(rt)
		ctx.Host = jitRuntimeAddr(rt)
		ctx.EnsureStack(0)
		rt.jitFrames = append(rt.jitFrames, ctx)
		// Past the cap the contexts are still built — the runtime needs one per
		// frame it enters — but they are not linked, so compiled calls stop
		// being made and the depth is counted by the runtime's own frame
		// counter again. That is what keeps runaway recursion a RangeError
		// rather than a chain of contexts sixteen times longer than the
		// recursion that made it.
		if n := len(rt.jitFrames); n > 1 && n <= jitMaxChain {
			rt.jitFrames[n-2].Next = uintptr(unsafe.Pointer(ctx))
		}
	}
	return rt.jitFrames[depth]
}

// jitCtxLocals is the frame's locals, as the slice the runtime's own paths take.
//
// Only for a frame a compiled call site entered; a frame the runtime entered
// has its locals in the frame slab and NLocals is zero.
func jitCtxLocals(ctx *jitmem.ExecContext) []Value {
	n := int(ctx.NLocals)
	if n <= 0 {
		return nil
	}
	if n > len(ctx.Locals) {
		n = len(ctx.Locals)
	}
	return unsafe.Slice((*Value)(unsafe.Pointer(&ctx.Locals[0])), n)
}

// jitCtxSite is the call site that opened this frame, or nil for a frame the
// runtime entered.
//
// One load. It was a walk down the chain from the frame the runtime entered,
// each level naming its site by index into the level above it, so that nothing
// but integers had to travel through machine code — and on EarleyBoyer, whose
// compiled calls nest as deeply as the budget allows, that walk was 38% of the
// run. Sixteen levels of pointer-chasing through contexts that are nowhere near
// the cache, on a path taken every time a compiled frame reaches the runtime for
// anything at all.
func jitCtxSite(ctx *jitmem.ExecContext) *jitCallSite {
	return (*jitCallSite)(ctx.Site)
}
