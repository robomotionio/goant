package engine

// Execution interruption.
//
// A host embedding the engine needs to be able to stop a script that will not
// stop on its own — an unbounded loop, a runaway recursion, a script that has
// simply exceeded its time budget. The engine is otherwise single-goroutine, so
// this is the one piece of Runtime state written from outside.
//
// Termination is not a JS exception. It is raised as a control throw, which the
// unwinder propagates past every catch and finally, so a script cannot swallow
// its own cancellation with try {} catch {}. Once terminated a Runtime stays
// terminated until the host calls ClearInterrupt: the interrupt outlives the
// script that was running, which is what lets a host abandon a runtime safely
// even if the script spawned further work.

import (
	"errors"
	"sync/atomic"
)

// ErrTerminated reports that execution was stopped by Interrupt rather than
// finishing or throwing. Hosts distinguish it with errors.Is.
var ErrTerminated = errors.New("goant: execution terminated")

// interruptCheckInterval is how many loop back-edges pass between checks of the
// interrupt flag. The check itself is an atomic load, cheap but not free, and a
// back-edge is the hottest branch in the interpreter; amortising it over 1024
// iterations makes it unmeasurable while still bounding how long a runaway loop
// can ignore a cancellation. Function entry is checked unconditionally, so
// recursion is caught without waiting for a back-edge at all.
const interruptCheckInterval = 1024

// Interrupt reasons. The flag doubles as the reason so the check on the hot
// path stays a single atomic load compared against zero.
const (
	interruptNone   = 0
	interruptHost   = 1 // Interrupt() — a timeout, a cancellation, a shutdown
	interruptMemory = 2 // the heap budget was exceeded; see SetHeapLimit
	interruptBlob   = 3 // a referenced blob could not be fetched; see SetBlobResolver
	// interruptYield is the odd one out: it does not stop the script. Another
	// agent wants its turn, and this is how it is asked for — by borrowing the
	// check the hot path already makes rather than adding one beside it. See
	// agent.go. It is raised only from interruptNone, so a real interrupt is
	// never displaced by a turn.
	interruptYield = 4
)

// interruptState is the cross-goroutine half of the Runtime. It is a separate
// struct so it can be copied into a realm without dragging the rest along, and
// so the atomic stays pointer-stable.
type interruptState struct {
	flag atomic.Uint32
}

// SetHeapLimit stops a script once its live heap exceeds limit bytes, instead
// of letting it run until the Go allocator gives up.
//
// This is the difference between a bad script and a dead process. Go's
// out-of-memory is runtime.throw: no panic, no recover, no deferred anything —
// the process aborts and takes every other flow on it along. A host cannot
// defend against that after the fact, so the engine has to decline before it
// happens, and it is the only party that can, because it owns the allocation.
//
// The limit is checked after a collection, never before one, so what it
// measures is memory that survived being collected rather than garbage on its
// way out. A script that churns hard but retains little is never stopped by it.
//
// Exceeding it terminates the script the same way Interrupt does — a control
// throw that no catch or finally can swallow — and the host distinguishes the
// two with HeapLimitExceeded. Pass 0 to disable.
func (rt *Runtime) SetHeapLimit(limit uint64) { rt.heapLimit = limit }

// HeapLimit reports the current budget, 0 if none.
func (rt *Runtime) HeapLimit() uint64 { return rt.heapLimit }

// HeapLimitExceeded reports that this Runtime was terminated for exceeding its
// heap budget rather than by a host Interrupt. The distinction is what lets a
// caller report "this script needed more memory than it is allowed" instead of
// "this script was cancelled", which are different problems with different
// fixes.
func (rt *Runtime) HeapLimitExceeded() bool {
	return rt.interrupt != nil && rt.interrupt.flag.Load() == interruptMemory
}

// BlobResolveFailed reports that this Runtime was terminated because a lazily
// parsed envelope named a blob the resolver could not produce. The error itself
// is BlobResolveError.
//
// It is reported rather than swallowed because the alternative is worse than a
// stopped script: the value would arrive as the raw envelope, and the failure
// would surface as a type error in the middle of someone's JavaScript with
// nothing pointing at the missing blob.
func (rt *Runtime) BlobResolveFailed() bool {
	return rt.interrupt != nil && rt.interrupt.flag.Load() == interruptBlob
}

// Interrupt requests that any script currently running on this Runtime stop as
// soon as it reaches the next check point. Safe to call from any goroutine, and
// safe to call when nothing is running.
func (rt *Runtime) Interrupt() {
	if rt.interrupt == nil {
		return
	}
	rt.interrupt.flag.Store(interruptHost)
}

// ClearInterrupt cancels a pending or delivered interrupt, making the Runtime
// usable again. Call it only once the interrupted script has actually returned.
func (rt *Runtime) ClearInterrupt() {
	if rt.interrupt == nil {
		return
	}
	rt.interrupt.flag.Store(interruptNone)
}

// Interrupted reports whether an interrupt is pending or has been delivered.
// A pending turn hand-over is not one: the script is not stopping.
func (rt *Runtime) Interrupted() bool {
	if rt.interrupt == nil {
		return false
	}
	f := rt.interrupt.flag.Load()
	return f != interruptNone && f != interruptYield
}

// interruptPending is the interpreter-side check, and it is byte for byte what
// it was: nil test, one atomic load, compare against zero. It runs on every
// function entry and every 1024th back edge, in the interpreter and in compiled
// code alike, and it has to stay inlinable — adding the agent case to it cost
// 87 against the inliner's budget of 80, and every safepoint in the engine
// became a call.
//
// So it answers "is the flag raised", and interruptStops answers what a raised
// flag MEANS. The second question is only asked when the answer to the first is
// yes, which for a Runtime with no second agent is never.
func (rt *Runtime) interruptPending() bool {
	return rt.interrupt != nil && rt.interrupt.flag.Load() != 0
}

// interruptStops reports whether a raised flag means the script must stop. All
// of them do except a turn hand-over, which is dealt with here and then is no
// longer pending.
//
// Deliberately out of line: it is reached once per raised flag, and inlining it
// would put its cost back on the path that tests for one.
//
//go:noinline
func (rt *Runtime) interruptStops() bool {
	if rt.interrupt.flag.Load() == interruptYield {
		rt.yieldTurn()
		return false
	}
	return true
}

// checkBackEdge is called on every backward jump. It counts locally and only
// touches the atomic once per interruptCheckInterval iterations.
func (rt *Runtime) checkBackEdge() bool {
	rt.backEdges++
	if rt.backEdges < interruptCheckInterval {
		return false
	}
	rt.backEdges = 0
	return rt.interruptPending()
}

// terminated builds the control throw that unwinds an interrupted script. The
// value is undefined and unreachable from JS: control throws bypass catch, so
// nothing can observe it.
func (rt *Runtime) terminated() *ThrowError {
	return &ThrowError{Value: mkundef(), rt: rt, control: true, terminate: true}
}

// backEdgeWantsGC reports that a backward jump has landed on the interpreter's
// second collection safepoint.
//
// A loop that allocates without ever calling a function — building a large
// array of literals, say — passes no frame entry, so without this the heap
// would grow until the loop ended. The caller publishes its frame and collects.
//
// Deliberately not written as backEdge(sync func()): handing the interpreter's
// syncFrame closure to a function makes it escape, and with it every frame
// variable it captures — including the operand stack, which then lives behind a
// heap pointer for the rest of the loop. That measured as a hang.
// Byte for byte what it was before the memory limit existed, and that is the
// point: this runs on every loop back edge in the engine, so a script that sets
// no limit must not read one extra field here. Reading a second one is not free
// — it is another cache line in the interpreter's working set, and it measured
// 7-10% on an idle machine.
//
// A loop that calls nothing still reaches a collection when all its memory is
// in string bytes, which is the case this whole exercise was about. chargeBytes
// lowers gc.next when the byte budget runs out, so growth in bytes arrives here
// already converted into the cell count this has always tested.
func (rt *Runtime) backEdgeWantsGC() bool {
	return rt.gc.enabled && rt.nativeDepth == 0 && rt.gc.next != 0 &&
		rt.objects.liveN >= rt.gc.next
}
