package engine

import "runtime"

// Giving executable memory back.
//
// Compiled code used to be kept for the life of the process. The reasoning was
// sound as far as it went — a block has to outlive every entry into it, and
// nothing in the engine can prove an entry has ended, so `free` was called from
// nothing but tests — and for a process that runs one script and exits it costs
// nothing.
//
// It is not what a host does. A twelve-hour fuzzing campaign is the same shape
// as a worker that keeps loading new flows: each program compiles its own
// functions into their own mappings, and none of it is ever reachable again.
// Replaying a 3,543-entry corpus measured a Go heap that stayed flat at 5-8 MB
// and executable memory that climbed monotonically to 180 MB in 27,461 blocks —
// about 52 KB per program, never returned. The worker that produced that corpus
// was killed by the OOM killer at 1.79 GB resident on a machine with 3.8 GB,
// and every Runtime inside it was under a 64 MB heap limit at the time. That is
// the point worth stating plainly: SetHeapLimit governs the JavaScript heap and
// says nothing about code, so the one control a host has over goant's memory
// did not cover the part that was growing.
//
// The proof that was missing is easier than it looked, because it is not needed
// per entry. Entering a function's code requires reaching the function: the
// interpreter enters through fn.jit.code, a compiled call site's frame holds a
// jitCallee that names fn, and a suspended generator or an outer recursive frame
// holds fn as well. So while any entry is possible, *svFunc is reachable — and
// when it is not, no entry is. That makes the function's own lifetime the exact
// lifetime of its code, and a finalizer the mechanism, since svFunc is an
// ordinary Go allocation with no back-pointer to make a cycle.
//
// This reclaims the retired blocks too. They are the ones a recompilation left
// behind, and they are kept for the same reason and released by the same
// argument: a frame suspended in a retired block holds the function it is
// running.
//
// What it does NOT do is free code while its function is alive. A pooled host
// running the same flow forever still compiles once and keeps it, which is the
// behaviour the tier is built around; see jit_codemem_test.go, which pins both
// halves.

// jitOwnCode ties a function's compiled code to the function's own lifetime.
//
// Called at each of the three points a function acquires code. The flag is why
// it is safe to call more than once: SetFinalizer panics on an object that
// already has one, and a function that is rebuilt without its parameter check
// reaches here a second time.
func jitOwnCode(fn *svFunc) {
	if fn == nil || fn.jit.owned {
		return
	}
	fn.jit.owned = true
	runtime.SetFinalizer(fn, jitReleaseCode)
}

// jitReleaseCode unmaps everything a function ever compiled to.
//
// Runs on the collector's goroutine with fn unreachable from anywhere else, so
// there is nothing to synchronise with: nothing can be executing this code and
// nothing can start. It touches only fn's own fields — resurrecting fn by
// publishing it somewhere would put a function back in play whose code is
// already unmapped.
func jitReleaseCode(fn *svFunc) {
	if c := fn.jit.code; c != nil {
		fn.jit.code = nil
		c.free()
	}
	for _, c := range fn.jit.retired {
		c.free()
	}
	fn.jit.retired = nil
}
