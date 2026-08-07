package engine

import (
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/robomotionio/goant/internal/jitmem"
)

// Tiering: when a compiled function is worth having, and when to try.
//
// The trigger is a call count rather than anything cleverer. A function entered
// often enough is worth spending microseconds compiling; one that has not is
// worth nothing, and the count is a single increment on a path that already
// touches the same cache line.
//
// jitEnabled is off by default. An execution path that is not the default is
// one that has to be justified rather than assumed, and GOANT_JIT=1 turns it on
// — which is how the differential over Octane is run.
//
// It reads the value rather than merely the presence of the variable, and that
// is not tidiness. `GOANT_JIT=0` used to mean *on*, because any non-empty string
// was, so every A/B run of the tier against the interpreter compared the tier
// against itself. Weeks of "Octane is unchanged with the tier on" were measured
// that way. A flag whose name carries a value has to honour it.
var jitEnabled = envOn("GOANT_JIT")

// envOn reads a boolean environment variable, treating the spellings of "no" as
// no. Absent is off, and anything not recognised as off is on, so a typo turns a
// diagnostic on rather than silently off.
func envOn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// jitThreshold is how many entries a function needs before it is compiled.
//
// Low, because compiling is cheap here: two dataflow passes and straight-line
// templates, with no register allocation to speak of. The cost of trying and
// failing is lower still, and it is paid once — a refusal is remembered.
// Tunable through GOANT_JIT_THRESHOLD, and that is a test hook rather than a
// knob for tuning: at 8, most of test262 never compiles anything, because most
// of its tests call a function once. Setting it to 1 turns the suite into a
// test of the compiled tier — which is the only tier whose bugs the
// interpreter cannot find, since the interpreter is the oracle.
var jitThreshold = int32(envInt("GOANT_JIT_THRESHOLD", 8))

// envInt reads an integer environment variable, falling back to def.
func envInt(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			return v
		}
	}
	return def
}

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
	icNarrow uint64
	putHit   uint64
	putMiss  uint64
	glbHit   uint64
	glbMiss  uint64
	elemHit  uint64
	elemMiss uint64
	// elemPutHit counts element STORES the emitted chain made, against
	// elemPutMiss for the ones that left compiled code. Separate from elemHit
	// because the two are separate decisions: a site can serve every read and no
	// write, which is exactly what this tier did before the store was emitted.
	elemPutHit  uint64
	elemPutMiss uint64
	genFast  uint64
	genSlow  uint64
	// callFast counts calls a compiled call site made in machine code, against
	// callSlow for the ones that went round through the runtime. It is the
	// number that says whether the compiled call is reaching the calls that
	// matter: a site that never fills looks exactly like one that was never
	// emitted, and both look like a function that simply calls a lot.
	callFast uint64
	callSlow uint64
	// helper counts calls out of compiled code by which helper was asked for.
	// Indexed by the helper id, masked to the array, so a renumbering cannot
	// index out of range — it can only mislabel, which JITHelperStats makes
	// visible by printing the number beside the name.
	helper [64]uint64
	// bails counts frames compiled code handed back partway. Unlike everything
	// above it this is counted whether or not the diagnostics are on, because it
	// is not a statistic about a hot path — it is how a test tells a bail that
	// fired from one that was compiled and never reached, and those look the
	// same in the answer.
	bails uint64
}

func init() { jitStats.enabled = envOn("GOANT_JIT_STATS") }

// JITStats reports frame entries served by compiled code, by compiled code that
// declined its arguments, and by the interpreter for want of any compiled form.
//
// Entries the runtime made. A compiled call site entering a compiled function
// does not pass through here at all, which is the point of it — JITCallStats is
// where those are counted, and on a call-heavy program they are most of them.
// JITCodeMemory reports the executable memory the tier holds: how many blocks
// are mapped, how many bytes they total, and the high-water mark.
//
// This is the number a long-running host has to watch, and the reason it is
// public rather than diagnostic. Compiled code is NEVER RELEASED — a block has
// to outlive every entry into it, and nothing here can prove an entry has ended,
// so a suspended generator or an outer frame of a recursive function would be
// left holding freed executable memory. A process running one script exits
// before that matters; one running thousands of different flows over days
// accumulates a block per hot function, plus one more each time a function is
// recompiled.
//
// "Never freed" is justified. "Unbounded and unmeasured" is not, which is what
// this closes: a host can sample it, alarm on it, and recycle a Runtime before
// it becomes a problem.
func JITCodeMemory() (blocks, bytes, peak int64) { return jitmem.Accounting() }

func JITStats() (compiled, declined, interpreted uint64) {
	return jitStats.compiled, jitStats.declined, jitStats.interp
}

// JITPropertyStats reports compiled property reads served by the emitted
// inline-cache probe, and those that fell through to the runtime.
func JITPropertyStats() (hit, miss uint64) { return jitStats.icHit, jitStats.icMiss }

// JITNarrowStats reports reads the emitted probe declined that the cache could
// answer anyway — the gap between what icWay.hit accepts and what the guard
// chain in machine code accepts. Every one is a helper round trip that a wider
// guard would remove.
func JITNarrowStats() uint64 { return jitStats.icNarrow }

// JITStoreStats is JITPropertyStats for the other direction. Counted apart
// because the two probes decline for different reasons and a combined figure
// would hide whichever of them is doing worse.
func JITStoreStats() (hit, miss uint64) { return jitStats.putHit, jitStats.putMiss }

// JITGlobalStats is the same for a global read, which is the same cache over a
// receiver compiled code fetches rather than one it was handed.
func JITGlobalStats() (hit, miss uint64) { return jitStats.glbHit, jitStats.glbMiss }

// JITElementStats is the same for `a[i]`, which has no cache site and a guard
// chain of its own.
func JITElementStats() (hit, miss uint64) { return jitStats.elemHit, jitStats.elemMiss }

// JITElementStoreStats is JITElementStats for `a[i] = v`.
func JITElementStoreStats() (hit, miss uint64) { return jitStats.elemPutHit, jitStats.elemPutMiss }

// JITOperatorStats reports operators compiled without a known operand type, by
// whether the guard let them take the machine instruction or sent them to the
// runtime.
func JITOperatorStats() (fast, slow uint64) { return jitStats.genFast, jitStats.genSlow }

// JITCallStats reports calls made from compiled code, by whether the call site
// made them itself or went through the runtime.
func JITCallStats() (fast, slow uint64) { return jitStats.callFast, jitStats.callSlow }

// JITBailStats reports frames compiled code handed back to the interpreter
// partway through. See jitbail.go.
func JITBailStats() uint64 { return jitStats.bails }

// JITHelperCount is one reason compiled code left, and how often it left for it.
type JITHelperCount struct {
	Name  string
	Count uint64
}

// JITHelperStats reports calls out of compiled code by which helper was wanted,
// heaviest first.
//
// This is the measurement that decides what is worth speculating on, and it is
// the one the tier has been improved by twice: what a compiled function cannot
// do itself, it leaves to say so, and the leaving is most of the cost. Emitting
// `x == null` rather than helping it was 18% of Octane, and nothing but this
// count says which of sixty helpers is the next one of those.
//
// Deliberately not the static histogram of what functions contain. That has
// pointed at the wrong work every time it has been consulted, because a
// program's time is not spread evenly over its opcodes.
func JITHelperStats() []JITHelperCount {
	out := make([]JITHelperCount, 0, len(jitStats.helper))
	for id, n := range jitStats.helper {
		if n == 0 {
			continue
		}
		out = append(out, JITHelperCount{Name: jitHelperNameOf(uint64(id)), Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

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
var jitOSRThreshold = int32(envInt("GOANT_JIT_OSR_THRESHOLD", 2000))

// jitAttempt is svFunc's tiering state, in one place so the interpreter's hot
// path touches one field.
type jitAttempt struct {
	count int32
	loops int32
	code  *jitCode
	// retired is every block this function has had before the current one. They
	// are kept for as long as the function is rather than freed at the moment of
	// recompilation, because a recursive function can have an outer frame
	// suspended inside one, and because a call site in another function can be
	// holding its call caches — see jitCallSite, whose address a suspended frame
	// publishes as its own identity. Both of those hold the function, which is
	// what lets jit_reclaim.go release them with it.
	retired []*jitCode
	tried   bool
	// owned records that this function's code is tied to its lifetime, so the
	// finalizer is installed once however many times it is rebuilt.
	owned bool
	// declines counts entries the prologue's parameter check turned away, and
	// unchecked records that it has stopped making that check. Functional, not
	// diagnostic — see jitNoteDecline.
	declines  int32
	unchecked bool
	// why, sole and entries are the diagnostic in jitrefusal.go and are only
	// ever written when GOANT_JIT_STATS is set. They live here rather than in a
	// map because the increment is on the interpreter's frame entry, which is
	// the hottest path in the engine and must not grow a hash lookup even in a
	// diagnostic build.
	why     uint32
	sole    uint32
	entries uint32
	insns   uint64
}

// jitDeclineLimit is how many times a compiled function may turn its arguments
// away before it is rebuilt without the parameter check.
//
// The check is a bet that the caller passes Numbers, and the bet is worth making
// — a checked parameter needs neither a type guard nor a call out, which is most
// of what a numeric kernel gains. What changed is that losing it used to be
// invisible: the functions that lose it are methods, and methods did not compile
// at all until the receiver reached compiled code. Richards then declined 195,342
// entries against 639 that ran.
const jitDeclineLimit = 32

// jitNoteDecline records that compiled code turned its arguments away, and
// rebuilds the function without the parameter check once that has happened often
// enough to stop being an accident.
//
// Rebuilding is sound at any moment because a decline happens before compiled
// code has written anything. The old code is deliberately *not* freed, and is
// kept rather than merely leaked: a recursive function can have an outer frame
// suspended inside the very block being replaced, and its resume address has to
// stay mapped — and its call sites have to stay allocated, because a frame that
// call opened names one of them as its identity. One retired block per function
// that ever recompiles is a bounded cost; an unmapped resume address is not a
// cost, it is a crash.
func jitNoteDecline(fn *svFunc) {
	if fn.jit.unchecked {
		return
	}
	fn.jit.declines++
	if fn.jit.declines < jitDeclineLimit {
		return
	}
	fn.jit.unchecked = true
	if c := jitCompile(fn, nil); c != nil {
		fn.jit.retired = append(fn.jit.retired, fn.jit.code)
		fn.jit.code = c
		jitOwnCode(fn)
	}
}

// jitTryLoop moves a loop that is already running into compiled code.
//
// Called from the interpreter's backward jumps, which is the one place a
// function can be spending its time without entering a frame. A false return
// means nothing happened and interpretation continues from the same place;
// there is no half-transferred state, because the stub either takes the whole
// frame or declines before touching it.
func jitTryLoop(rt *Runtime, fn *svFunc, cl *closure, fnVal Value, args, locals []Value, this Value, header int) (Value, *ThrowError, bool) {
	if fn.jit.code != nil {
		return fn.jit.code.jitRunOSR(rt, fn, cl, fnVal, args, locals, this, header)
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
	jitOwnCode(fn)
	return fn.jit.code.jitRunOSR(rt, fn, cl, fnVal, args, locals, this, header)
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
	// dynamicVars is no longer here. A function containing a direct eval carries
	// a variable object that the eval's `var` declarations land on and that this
	// function's own free names resolve through — and both of those are now
	// things a compiled frame can do, because it builds the object on entry and
	// puts it on the same `with` chain the interpreter does. evalVarObj is
	// still refused: eval CODE adopts its caller's object, which arrives through
	// a piece of runtime state a compiled frame is not part of.
	for _, skip := range jitSkipNames {
		if skip != "" && strings.Contains(fn.name, skip) {
			return false
		}
	}
	if jitMask != ^uint64(0) && jitMask&(1<<jitNameBucket(fn.name)) == 0 {
		return false
	}
	return !fn.isAsync && !fn.isGenerator && !fn.isClassCtor &&
		!fn.evalVarObj &&
		fn.globalLex == nil && fn.moduleExports == nil
}

// jitTry runs fn's compiled form if there is one, and decides whether to make
// one if there is not.
//
// A false return means the interpreter runs the function as usual: either
// nothing was compiled, or the compiled code declined the arguments it was
// given. Declining is safe at any point, because the only exit that reports it
// is the entry check — which happens before compiled code has written anything.
func jitTry(rt *Runtime, fn *svFunc, cl *closure, fnVal Value, args, locals []Value, this Value) (Value, *ThrowError, bool) {
	if fn.jit.code != nil {
		v, e, ok := fn.jit.code.jitRun(rt, fn, cl, fnVal, args, locals, this)
		if jitStats.enabled {
			if ok {
				jitStats.compiled++
			} else {
				jitStats.declined++
			}
		}
		if !ok {
			jitNoteDecline(fn)
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
	jitOwnCode(fn)
	return fn.jit.code.jitRun(rt, fn, cl, fnVal, args, locals, this)
}

// The knobs for hunting a miscompilation, which is the one bug class this tier
// can have that nothing else in the engine can: the interpreter is the oracle,
// so the question is always "which compiled function disagrees with it".
//
// GOANT_JIT_SKIP=<substr>[,<substr>…] refuses any function whose name contains
// one of them, and
// GOANT_JIT_MASK=<u64> refuses every function whose name does not hash into one
// of the 64 buckets the mask selects. The mask is what makes a bisect possible:
// compiling a function changes how often the others are entered, so an index
// over compile order is not stable between runs and a hash of the name is.
var jitSkipNames = strings.Split(os.Getenv("GOANT_JIT_SKIP"), ",")

var jitMask = func() uint64 {
	if s := os.Getenv("GOANT_JIT_MASK"); s != "" {
		v, err := strconv.ParseUint(s, 0, 64)
		if err == nil {
			return v
		}
	}
	return ^uint64(0)
}()

// GOANT_JIT_BAILAT=<offset> makes the emitter plant an unconditional bail
// before the instruction at that bytecode offset in every function it compiles,
// and −1 (the default) plants none.
//
// It exists because a bail is the one thing in the tier that cannot be tested
// where it is used. Every real bail sits behind a guard that is meant never to
// fail, so the paths that matter are the ones a corpus does not reach — and a
// handover that is wrong about the operand stack, or lands one instruction off,
// produces a plausible answer rather than a crash. Forcing one at a *chosen*
// point turns that into something a test can sweep: run the same function once
// per instruction in its body, and every answer has to be the interpreter's.
//
// A package variable rather than only an environment variable because the sweep
// recompiles between runs, and because the same knob then works from a shell for
// bisecting a real one.
//
// Parsed here rather than through envInt, which treats a zero as absent. Zero is
// the first instruction of every function body, so it is the one offset this
// must not quietly decline to plant.
var jitBailAt = func() int {
	if s := os.Getenv("GOANT_JIT_BAILAT"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return -1
}()

// jitNameBucket is FNV-1a of the name, folded to one of 64 buckets.
func jitNameBucket(name string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= 1099511628211
	}
	return h % 64
}
