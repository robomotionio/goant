package engine

import "os"

// Tiering: when a compiled function is worth having, and when to try.
//
// The trigger is a call count rather than anything cleverer. A function entered
// often enough is worth spending microseconds compiling; one that has not is
// worth nothing, and the count is a single increment on a path that already
// touches the same cache line.
//
// jitEnabled is off by default. This tier compiles a small fraction of real code
// — see TestJITCoverage — so it cannot yet pay for itself on a mixed workload,
// and an execution path that is not the default is one that has to be justified
// rather than assumed. GOANT_JIT=1 turns it on, which is how the differential
// over Octane is run.
var jitEnabled = os.Getenv("GOANT_JIT") != ""

// jitThreshold is how many entries a function needs before it is compiled.
//
// Low, because compiling is cheap here: two dataflow passes and straight-line
// templates, with no register allocation to speak of. The cost of trying and
// failing is lower still, and it is paid once — a refusal is remembered.
const jitThreshold = 8

// jitStats counts frame entries by where they ran, for GOANT_JIT_STATS=1.
//
// The static count of compilable functions is a poor guide to what a tier is
// worth: a program's time is not spread evenly over its functions, and the ones
// a numeric tier can take are exactly the ones that run in loops. What decides
// the speedup is the share of *entries* that land in compiled code, which is
// what this counts.
// icHit and icMiss count property reads by whether the compiled probe served
// them. They are the number that says whether emitting the cache was worth it,
// and they cannot be inferred from anything else: a site that misses looks
// exactly like a site that was never compiled.
//
// icHit is incremented by compiled code, which is why the increment is only
// emitted when this is on — a counter nobody reads is still a store to a shared
// line on the hottest path there is.
// genFast and genSlow are the same question asked of the generic operators: an
// operator emitted behind a type guard is worth having only if the guard is
// sometimes satisfied, and a guard that always fails is indistinguishable from
// one that always passes unless both sides are counted.
var jitStats struct {
	enabled  bool
	compiled uint64
	declined uint64
	interp   uint64
	icHit    uint64
	icMiss   uint64
	putHit   uint64
	putMiss  uint64
	glbHit   uint64
	glbMiss  uint64
	genFast  uint64
	genSlow  uint64
}

func init() { jitStats.enabled = os.Getenv("GOANT_JIT_STATS") != "" }

// JITStats reports frame entries served by compiled code, by compiled code that
// declined its arguments, and by the interpreter for want of any compiled form.
func JITStats() (compiled, declined, interpreted uint64) {
	return jitStats.compiled, jitStats.declined, jitStats.interp
}

// JITPropertyStats reports compiled property reads served by the emitted
// inline-cache probe, and those that fell through to the runtime.
func JITPropertyStats() (hit, miss uint64) { return jitStats.icHit, jitStats.icMiss }

// JITStoreStats is JITPropertyStats for the other direction. Counted apart
// because the two probes decline for different reasons and a combined figure
// would hide whichever of them is doing worse.
func JITStoreStats() (hit, miss uint64) { return jitStats.putHit, jitStats.putMiss }

// JITGlobalStats is the same for a global read, which is the same cache over a
// receiver compiled code fetches rather than one it was handed.
func JITGlobalStats() (hit, miss uint64) { return jitStats.glbHit, jitStats.glbMiss }

// JITOperatorStats reports operators compiled without a known operand type, by
// whether the guard let them take the machine instruction or sent them to the
// runtime.
func JITOperatorStats() (fast, slow uint64) { return jitStats.genFast, jitStats.genSlow }

// jitOSRThreshold is how many times a loop may go round in the interpreter
// before the function it is in gets compiled.
//
// A separate trigger is needed because entering a frame is not the only way to
// spend time in one. A function called once that then loops for a second would
// never reach jitThreshold, however hot the loop is — measured at 1070ms
// interpreted against 262ms for exactly the same work, split into enough calls
// to tier. The count is higher than the call threshold because a back edge is a
// far cheaper event than a frame entry, so the same absolute work takes more of
// them to be worth compiling for.
const jitOSRThreshold = 2000

// jitAttempt is svFunc's tiering state, in one place so the interpreter's hot
// path touches one field.
type jitAttempt struct {
	count int32
	loops int32
	code  *jitCode
	tried bool
	// why and entries are the diagnostic in jitrefusal.go and are only ever
	// written when GOANT_JIT_STATS is set. They live here rather than in a map
	// because the increment is on the interpreter's frame entry, which is the
	// hottest path in the engine and must not grow a hash lookup even in a
	// diagnostic build.
	why     uint32
	sole    uint32
	entries uint32
}

// jitTryLoop moves a loop that is already running into compiled code.
//
// Called from the interpreter's backward jumps, which is the one place a
// function can be spending its time without entering a frame. A false return
// means nothing happened and interpretation continues from the same place;
// there is no half-transferred state, because the stub either takes the whole
// frame or declines before touching it.
func jitTryLoop(rt *Runtime, fn *svFunc, locals []Value, this Value, header int) (Value, *ThrowError, bool) {
	if fn.jit.code != nil {
		return fn.jit.code.jitRunOSR(rt, fn, locals, this, header)
	}
	if fn.jit.tried {
		return mkundef(), nil, false
	}
	fn.jit.loops++
	if fn.jit.loops < jitOSRThreshold {
		return mkundef(), nil, false
	}
	fn.jit.tried = true
	if !jitEligible(fn) {
		jitNoteRefusal(fn, "ineligible-frame")
		return mkundef(), nil, false
	}
	var why string
	fn.jit.code = jitCompile(fn, jitWhy(&why))
	if fn.jit.code == nil {
		jitNoteRefusal(fn, why)
		return mkundef(), nil, false
	}
	return fn.jit.code.jitRunOSR(rt, fn, locals, this, header)
}

// jitEligible rejects the frame shapes the compiler does not model, before it
// looks at a single opcode.
//
// The opcode whitelist would refuse all of these anyway — a generator suspends
// through opcodes this tier has never heard of — but a function's *frame* can
// carry obligations its body does not mention. A module body hands its locals to
// the record being evaluated; a script's top-level lexical bindings become
// global ones; a function containing a direct eval carries a dynamic variable
// object. Compiled code writes locals straight into the frame's slice and knows
// about none of it.
func jitEligible(fn *svFunc) bool {
	return !fn.isAsync && !fn.isGenerator && !fn.isClassCtor &&
		!fn.evalVarObj && !fn.dynamicVars &&
		fn.globalLex == nil && fn.moduleExports == nil
}

// jitTry runs fn's compiled form if there is one, and decides whether to make
// one if there is not.
//
// A false return means the interpreter runs the function as usual: either
// nothing was compiled, or the compiled code declined the arguments it was
// given. Declining is safe at any point, because the only exit that reports it
// is the entry check — which happens before compiled code has written anything.
func jitTry(rt *Runtime, fn *svFunc, locals []Value, this Value) (Value, *ThrowError, bool) {
	if fn.jit.code != nil {
		v, e, ok := fn.jit.code.jitRun(rt, fn, locals, this)
		if jitStats.enabled {
			if ok {
				jitStats.compiled++
			} else {
				jitStats.declined++
			}
		}
		return v, e, ok
	}
	if jitStats.enabled {
		jitStats.interp++
		jitNoteEntry(fn)
	}
	if fn.jit.tried {
		return mkundef(), nil, false
	}
	fn.jit.count++
	if fn.jit.count < jitThreshold {
		return mkundef(), nil, false
	}
	fn.jit.tried = true // compiling is attempted once; a refusal is remembered
	if !jitEligible(fn) {
		jitNoteRefusal(fn, "ineligible-frame")
		return mkundef(), nil, false
	}
	var why string
	fn.jit.code = jitCompile(fn, jitWhy(&why))
	if fn.jit.code == nil {
		jitNoteRefusal(fn, why)
		return mkundef(), nil, false
	}
	return fn.jit.code.jitRun(rt, fn, locals, this)
}
