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

// interruptState is the cross-goroutine half of the Runtime. It is a separate
// struct so it can be copied into a realm without dragging the rest along, and
// so the atomic stays pointer-stable.
type interruptState struct {
	flag atomic.Uint32
}

// Interrupt requests that any script currently running on this Runtime stop as
// soon as it reaches the next check point. Safe to call from any goroutine, and
// safe to call when nothing is running.
func (rt *Runtime) Interrupt() {
	if rt.interrupt == nil {
		return
	}
	rt.interrupt.flag.Store(1)
}

// ClearInterrupt cancels a pending or delivered interrupt, making the Runtime
// usable again. Call it only once the interrupted script has actually returned.
func (rt *Runtime) ClearInterrupt() {
	if rt.interrupt == nil {
		return
	}
	rt.interrupt.flag.Store(0)
}

// Interrupted reports whether an interrupt is pending or has been delivered.
func (rt *Runtime) Interrupted() bool {
	return rt.interrupt != nil && rt.interrupt.flag.Load() != 0
}

// interruptPending is the interpreter-side check. Inlined into the hot paths.
func (rt *Runtime) interruptPending() bool {
	return rt.interrupt != nil && rt.interrupt.flag.Load() != 0
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
func (rt *Runtime) backEdgeWantsGC() bool {
	return rt.gc.enabled && rt.nativeDepth == 0 && rt.gc.next != 0 &&
		rt.objects.liveN >= rt.gc.next
}
