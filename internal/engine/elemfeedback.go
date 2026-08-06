package engine

// Per-site type feedback for element access.
//
// This is the first thing in the engine the compiler emits from what the
// program DID rather than from what its bytecode says. A property read has a
// shape cache the compiled probe walks; `a[i]` has no cache at all, because an
// array's elements are a slice of their own — so until now the compiled guard
// chain could only be written for the one receiver shape the emitter knew about
// in advance, a fast array, and every other receiver left compiled code.
//
// The counting said that was almost all of it. Element reads are 73.7% of
// zlib's exits from compiled code, 61.8% of mandreel's, 37.6% of gbemu's; the
// emitted chain served 6,059 of zlib's 2,128,486,098 of them, because it starts
// by checking for tag TArr and every one of those receivers is a TypedArray.
// The profile put the two element paths at 52.6% of zlib's wall clock and 47.6%
// of mandreel's — not the round trip, which is 1.7%, but the work the helper
// does once it arrives: resolving handles, recomputing the view's bounds and
// switching on its element kind, for every single access.
//
// What makes that emittable rather than a dispatch is the second thing the
// counting said: no element KIND dominates a workload — zlib is 53% Int32,
// mandreel 50% Uint16, gbemu 67% Uint8 — but every SITE is monomorphic, 100.0%
// single-kind across the 2,580 sites the three of them execute. So a site needs
// one recorded byte and emits one load, and the six-way inline dispatch the
// per-workload numbers would have suggested is not needed anywhere.
//
// The interpreter fills the record, exactly as it fills the property caches, and
// for the same reason: a function only reaches the compiler after the
// interpreter has already run it. Recording stops once compilation has been
// attempted, which bounds what a program that never tiers up pays for this to
// its warm-up.
//
// GOANT_JIT_THRESHOLD=1 THEREFORE DOES NOT EXERCISE ANY OF THIS. That setting is
// how the corpus is made to test the tier, and it compiles a function before its
// body has run even once — so no site has a record, every one is given the
// fast-array chain, and a suite run that way is green without the code below
// ever having been reached. Threshold 2 is the setting that does both: it still
// compiles everything, and it leaves the interpreter exactly one pass to record
// from. Measured, not assumed — at 1 the chain serves 0.0% of a typed-array
// loop and at 2 it serves 100.0%.

// One byte says both which receiver a site saw and, for a TypedArray, which
// element kind. Both, rather than the kind alone, because the two chains are
// alternatives: a site that gets the TypedArray chain does not also get the
// fast-array one, and giving the wrong chain to a site is a permanent exit for
// every access it makes. Recording only TypedArrays would have handed the
// TypedArray chain to a site that indexes plain arrays a thousand times for
// every view it sees, and taken away the chain that was serving it.
const (
	// elemKindNone means nothing has been recorded at this offset — the site
	// never ran interpreted, or its receiver was neither of the two. The
	// emitter treats it as no feedback and emits what it always did.
	elemKindNone uint8 = 0

	// elemKindArr means every receiver seen here was a fast array, which is the
	// chain that was already being emitted.
	elemKindArr uint8 = 254

	// elemKindPoly means the site saw more than one, so there is no single
	// chain to emit. Distinct from None so that a genuinely polymorphic site
	// cannot be mistaken for one that never ran.
	elemKindPoly uint8 = 255
)

// noteElemKind records the receiver at a GET_ELEM or PUT_ELEM.
//
// ip is the offset of the instruction, which is what indexes the record: both
// opcodes are one byte with no operands, so there is nowhere in the instruction
// itself to put a site number, and adding one would be a bytecode format change
// for a byte the interpreter reads once per compile.
func (rt *Runtime) noteElemKind(fn *svFunc, ip int, recv Value) {
	// Only while the function might still be compiled. After that nothing reads
	// the record, and this is on the interpreter's element path.
	if fn.jit.tried {
		return
	}
	var k uint8
	switch recv.Type() {
	case TTypedArray:
		o := rt.objPtr(recv)
		if o == nil || o.ta == nil {
			return
		}
		// jitKind is zero for a view compiled code cannot read — length-tracking,
		// BigInt, Float16. Recorded as polymorphic rather than left blank, so
		// the site is not later given a chain that would send every access to
		// the runtime anyway, one guard poorer.
		if k = o.ta.jitKind; k == elemKindNone {
			k = elemKindPoly
		}
	case TArr:
		k = elemKindArr
	default:
		return
	}
	if fn.elemKinds == nil {
		fn.elemKinds = make([]uint8, len(fn.code))
	}
	switch prev := fn.elemKinds[ip]; prev {
	case elemKindNone:
		fn.elemKinds[ip] = k
	case k, elemKindPoly:
	default:
		fn.elemKinds[ip] = elemKindPoly
	}
}

// elemKindAt is the raw record: elemKindNone, elemKindArr, elemKindPoly, or a
// TypedArray kind plus one.
func (fn *svFunc) elemKindAt(ip int) uint8 {
	if ip >= len(fn.elemKinds) {
		return elemKindNone
	}
	return fn.elemKinds[ip]
}

// elemKindTyped reports the single TypedArray element kind this site has seen,
// and whether it saw exactly that and nothing else.
func (fn *svFunc) elemKindTyped(ip int) (taKind, bool) {
	if ip >= len(fn.elemKinds) {
		return 0, false
	}
	k := fn.elemKinds[ip]
	if k == elemKindNone || k == elemKindArr || k == elemKindPoly {
		return 0, false
	}
	return taKind(k - 1), true
}
