package engine

import (
	"runtime"

	"github.com/robomotionio/goant/internal/jitmem"
)

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
// per entry. Entering compiled code requires reaching the jitCode that describes
// it: the interpreter enters through fn.jit.code, a compiled call site's frame
// holds a jitCallee that names one, a suspended generator or an outer recursive
// frame reaches one through the function it is running, and jitCompile hands one
// back to whoever asked. So while any entry is possible the jitCode is
// reachable, and when it is not, no entry is. The jitCode's own lifetime is
// therefore the exact lifetime of its mapping.
//
// WHERE THE FINALIZER GOES is the whole of the difficulty, and two placements
// that both look right are both wrong.
//
// Not on svFunc, which is the obvious one and was the first thing tried. A
// compiled call site holds a jitCallee, and a jitCallee names the function it
// resolved to, so
//
//	fn -> jit.code -> sites -> bind -> fn
//
// closes for any function that calls itself, and two of them close for any pair
// that call each other. Go runs no finalizer on an object in a reference cycle
// and frees no such cycle either, so every recursive compiled function would be
// pinned forever together with everything it reaches — which includes a
// jitCallee's closure and so a pointer into the JavaScript heap's chunk.
// Measured, four fuzz workers went from 1.7 GB to 10 GB each in thirty seconds
// and took a 31 GB machine to zero free.
//
// And not on svFunc even with the cycle avoided, because a jitCode does not have
// to be reached through its function: jitCompile RETURNS one, and a caller
// holding it holds nothing that keeps an svFunc alive. Tying the mapping to the
// function meant that block could be unmapped while a live *jitCode still
// pointed into it — which is a use-after-free of executable memory, and which
// turned up immediately as a test reading Len() off a freed block and as the
// race detector finding a finalizer writing it.
//
// So the finalizer goes on a jitCodeOwner hanging off the jitCode: a record that
// holds the mapping and NOTHING that can point back into the engine's graph. The
// cycle above keeps no finalizer and is collected normally; the owner is
// reachable from the jitCode and from nothing else, and dies with it.
//
// The retired blocks a recompilation left behind need no special handling under
// this rule. Each is its own jitCode, held in fn.jit.retired for exactly as long
// as a frame suspended in it could still be running, and released when that
// stops being true.
//
// What this does NOT do is free code while anything can enter it. A pooled host
// running the same flow forever still compiles once and keeps it, which is the
// behaviour the tier is built around; see jit_codemem_test.go, which pins every
// half of this including the cycle.

// jitCodeOwner is one compiled function's mapping and nothing else.
//
// The absence of other fields is the design. Anything here that reached back
// into the engine's graph would put this record inside the very cycle it exists
// to stay out of, and the finalizer would then never run — silently, and only
// for the functions that happen to recurse.
type jitCodeOwner struct {
	block *jitmem.Block
}

// release unmaps the block.
//
// Runs on the collector's goroutine with the owner unreachable, which means its
// jitCode is unreachable, which means nothing is executing this code and nothing
// can start. Free is idempotent, so a block a caller has already released
// explicitly is not freed twice.
func (o *jitCodeOwner) release() {
	if o.block != nil {
		o.block.Free()
		o.block = nil
	}
}

// jitOwnBlock ties a mapping to the lifetime of the jitCode that describes it.
//
// Called once per successful compilation, at the point the mapping is handed
// over, and the result is stored in that jitCode and nowhere else.
func jitOwnBlock(b *jitmem.Block) *jitCodeOwner {
	if b == nil {
		return nil
	}
	o := &jitCodeOwner{block: b}
	runtime.SetFinalizer(o, (*jitCodeOwner).release)
	return o
}
