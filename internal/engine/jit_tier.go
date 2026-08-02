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

// jitAttempt is svFunc's tiering state, in one place so the interpreter's hot
// path touches one field.
type jitAttempt struct {
	count int32
	code  *jitCode
	tried bool
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
func jitTry(fn *svFunc, locals []Value) (Value, bool) {
	if fn.jit.code != nil {
		return fn.jit.code.jitRun(locals)
	}
	if fn.jit.tried {
		return mkundef(), false
	}
	fn.jit.count++
	if fn.jit.count < jitThreshold {
		return mkundef(), false
	}
	fn.jit.tried = true // compiling is attempted once; a refusal is remembered
	if !jitEligible(fn) {
		return mkundef(), false
	}
	fn.jit.code = jitCompile(fn, nil)
	if fn.jit.code == nil {
		return mkundef(), false
	}
	return fn.jit.code.jitRun(locals)
}
