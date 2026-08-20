package engine

// The Math object (ant modules/math.c / builtin_math). Most methods delegate to
// Go's math package; the JS-specific edge cases (round-half-up, sign, trunc,
// clz32, imul, fround, hypot) are implemented to spec.

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"math"
	"math/big"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

func (rt *Runtime) initMath() {
	m := rt.newObject(rt.objectProto) // [[Prototype]] is %Object.prototype%
	mo := rt.objPtr(m)

	constant := func(name string, v float64) {
		mo.defineOwn(name, mknum(v), 0) // non-writable, non-enumerable, non-configurable
	}
	constant("PI", math.Pi)
	constant("E", math.E)
	constant("LN2", math.Ln2)
	constant("LN10", math.Log(10))
	constant("LOG2E", 1/math.Ln2)
	constant("LOG10E", 1/math.Log(10))
	constant("SQRT2", math.Sqrt2)
	constant("SQRT1_2", math.Sqrt(0.5))

	unary := func(name string, f func(float64) float64) {
		mo.defineOwn(name, rt.newNativeFunc(name, 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			x, e := rt.arg1Number(args)
			if e != nil {
				return mkundef(), e
			}
			return mknum(f(x)), nil
		}), attrWritable|attrConfigurable)
	}
	unary("abs", math.Abs)
	unary("floor", math.Floor)
	unary("ceil", math.Ceil)
	unary("round", jsRound)
	unary("trunc", math.Trunc)
	unary("sign", jsSign)
	unary("sqrt", math.Sqrt)
	unary("cbrt", math.Cbrt)
	unary("exp", math.Exp)
	unary("expm1", math.Expm1)
	unary("log", math.Log)
	unary("log2", math.Log2)
	unary("log10", math.Log10)
	unary("log1p", math.Log1p)
	unary("sin", math.Sin)
	unary("cos", math.Cos)
	unary("tan", math.Tan)
	unary("asin", math.Asin)
	unary("acos", math.Acos)
	unary("atan", math.Atan)
	unary("sinh", math.Sinh)
	unary("cosh", math.Cosh)
	unary("tanh", math.Tanh)
	unary("asinh", math.Asinh)
	unary("acosh", math.Acosh)
	unary("atanh", math.Atanh)
	unary("fround", jsFround)
	unary("f16round", jsF16round)
	unary("clz32", jsClz32)

	binary := func(name string, f func(a, b float64) float64) {
		mo.defineOwn(name, rt.newNativeFunc(name, 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			a, e := rt.argNumber(args, 0)
			if e != nil {
				return mkundef(), e
			}
			b, e := rt.argNumber(args, 1)
			if e != nil {
				return mkundef(), e
			}
			return mknum(f(a, b)), nil
		}), attrWritable|attrConfigurable)
	}
	binary("atan2", math.Atan2)
	binary("pow", jsExp)
	binary("imul", func(a, b float64) float64 { return float64(int32(toUint32(a) * toUint32(b))) })

	// min/max are variadic.
	mo.defineOwn("max", rt.newNativeFunc("max", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.mathReduce(args, math.Inf(-1), true)
	}), attrWritable|attrConfigurable)
	mo.defineOwn("min", rt.newNativeFunc("min", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.mathReduce(args, math.Inf(1), false)
	}), attrWritable|attrConfigurable)
	mo.defineOwn("hypot", rt.newNativeFunc("hypot", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Coerce every argument (in order, for side effects) before deciding: any
		// ±Infinity yields +Infinity even if another argument is NaN.
		anyInf, anyNaN, sum := false, false, 0.0
		for _, a := range args {
			x, e := rt.toNumber(a)
			if e != nil {
				return mkundef(), e
			}
			switch {
			case math.IsInf(x, 0):
				anyInf = true
			case math.IsNaN(x):
				anyNaN = true
			default:
				sum += x * x
			}
		}
		if anyInf {
			return mknum(math.Inf(1)), nil
		}
		if anyNaN {
			return mknum(math.NaN()), nil
		}
		return mknum(math.Sqrt(sum)), nil
	}), attrWritable|attrConfigurable)

	// Math.sumPrecise(items) — correctly-rounded sum of an iterable of Numbers
	// (the Math.sumPrecise proposal). Every element must already be a Number (no
	// coercion); the finite terms are summed exactly with a wide big.Float so the
	// single final rounding to float64 is correctly rounded (ties to even). The
	// ±0/±∞/NaN result follows the proposal's state machine.
	mo.defineOwn("sumPrecise", rt.newNativeFunc("sumPrecise", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		source := mkundef()
		if len(args) > 0 {
			source = args[0]
		}
		sawNaN, sawPosInf, sawNegInf, finite := false, false, false, false
		// 4096 bits dwarfs a double's full dynamic range (~2098 bits from the
		// largest to the smallest magnitude), so every partial sum is exact and
		// only Float64 rounds.
		acc := new(big.Float).SetPrec(4096).SetMode(big.ToNearestEven)
		e := rt.iterateWithClose(source, func(v Value) (bool, *ThrowError) {
			if !v.IsNumber() {
				return false, rt.typeError("Math.sumPrecise: all values must be numbers")
			}
			x := v.Number()
			switch {
			case math.IsNaN(x):
				sawNaN = true
			case math.IsInf(x, 1):
				sawPosInf = true
			case math.IsInf(x, -1):
				sawNegInf = true
			case x == 0 && math.Signbit(x):
				// -0 does not transition the state out of minus-zero.
			default:
				finite = true
				acc.Add(acc, new(big.Float).SetFloat64(x))
			}
			return false, nil
		})
		if e != nil {
			return mkundef(), e
		}
		switch {
		case sawNaN || (sawPosInf && sawNegInf):
			return mknum(math.NaN()), nil
		case sawPosInf:
			return mknum(math.Inf(1)), nil
		case sawNegInf:
			return mknum(math.Inf(-1)), nil
		case !finite:
			return mknum(math.Copysign(0, -1)), nil
		}
		out, _ := acc.Float64()
		return mknum(out), nil
	}), attrWritable|attrConfigurable)

	// Math.random — xorshift PRNG, seeded per Runtime; not for crypto.
	mo.defineOwn("random", rt.newNativeFunc("random", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mknum(rt.nextRandom()), nil
	}), attrWritable|attrConfigurable)

	rt.setStringTag(m, "Math")
	rt.objPtr(rt.global).defineOwn("Math", m, attrWritable|attrConfigurable)
}

func (rt *Runtime) mathReduce(args []Value, init float64, wantMax bool) (Value, *ThrowError) {
	// ToNumber every argument first (in order) so all coercion side effects run,
	// even when an earlier argument already coerced to NaN.
	nums := make([]float64, len(args))
	for i, a := range args {
		x, e := rt.toNumber(a)
		if e != nil {
			return mkundef(), e
		}
		nums[i] = x
	}
	acc := init
	for _, x := range nums {
		if math.IsNaN(x) {
			return mknum(math.NaN()), nil
		}
		if wantMax {
			// +0 is greater than -0 per spec.
			if x > acc || (x == 0 && acc == 0 && !math.Signbit(x)) {
				acc = x
			}
		} else {
			if x < acc || (x == 0 && acc == 0 && math.Signbit(x)) {
				acc = x
			}
		}
	}
	return mknum(acc), nil
}

// ---- argument coercion helpers ----

func (rt *Runtime) argNumber(args []Value, i int) (float64, *ThrowError) {
	if i >= len(args) {
		return math.NaN(), nil
	}
	return rt.toNumber(args[i])
}

func (rt *Runtime) arg1Number(args []Value) (float64, *ThrowError) {
	return rt.argNumber(args, 0)
}

// ---- JS-specific math edge cases ----

// jsRound implements Math.round: round half toward +Infinity (not away from 0).
func jsRound(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) || x == 0 {
		return x
	}
	if x >= -0.5 && x < 0 {
		return math.Copysign(0, -1) // rounds to -0
	}
	// Round half toward +Infinity without the precision loss of Floor(x + 0.5),
	// which rounds 0.49999999999999994 up to 1 instead of 0.
	r := math.Floor(x)
	if x-r >= 0.5 {
		r += 1
	}
	return r
}

func jsSign(x float64) float64 {
	switch {
	case math.IsNaN(x):
		return math.NaN()
	case x > 0:
		return 1
	case x < 0:
		return -1
	default:
		return x // preserves ±0
	}
}

func jsFround(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	return float64(float32(x))
}

// jsF16round implements Math.f16round: round to the nearest IEEE-754 binary16
// (half-precision) value, ties to even, then widen back to float64. Reuses the
// exact binary64↔binary16 conversion from the Float16Array/DataView path (which
// already handles NaN, ±Inf, ±0, subnormals, and overflow-to-infinity).
func jsF16round(x float64) float64 {
	return float16ToFloat64(float16FromFloat64(x))
}

func jsClz32(x float64) float64 {
	v := toUint32(x)
	if v == 0 {
		return 32
	}
	n := 0
	for v&0x80000000 == 0 {
		n++
		v <<= 1
	}
	return float64(n)
}

// randBase is the process's entropy for Math.random, read once. Every Runtime
// derives its own stream from it, so two isolates in one process — and two
// robots on one machine — do not hand out the same numbers.
//
// GOANT_RANDOM_SEED replaces the entropy with a fixed base, which makes the
// whole process reproducible: same seed, same sequence, in every Runtime it
// creates, in that order. That is what a benchmark comparing two builds or a
// fuzzer replaying a failure wants, and it is the only thing the old fixed
// constant was ever good for.
var randBase = func() uint64 {
	if s := os.Getenv("GOANT_RANDOM_SEED"); s != "" {
		if n, err := strconv.ParseUint(s, 0, 64); err == nil {
			return n
		}
	}
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err == nil {
		return binary.LittleEndian.Uint64(b[:])
	}
	// crypto/rand does not fail on any platform goant builds for, but a
	// generator that silently repeats is exactly the bug this function exists
	// to fix, so there is a fallback rather than a panic.
	return uint64(time.Now().UnixNano())
}()

// randSeq numbers the streams handed out from randBase, so realms and isolates
// created in one process each get their own.
var randSeq atomic.Uint64

// randSeed returns this Runtime's starting state.
//
// Consecutive stream numbers must not produce correlated first outputs — a
// script that reads one Math.random() and stops is the common case, and under a
// plain counter those reads would march in lockstep across isolates. The
// splitmix64 finalizer decorrelates them.
func randSeed() uint64 {
	x := randBase + randSeq.Add(1)*0x9E3779B97F4A7C15
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	if x == 0 {
		return 0x9E3779B97F4A7C15 // xorshift is dead at zero
	}
	return x
}

// nextRandom returns a pseudo-random float in [0,1) via a per-Runtime xorshift
// generator, seeded on first use.
//
// Seeded here rather than in New so a Runtime that never calls Math.random
// never pays for the seed, and — more importantly — so the stream numbering
// follows the order scripts actually ask for randomness rather than the order
// realms happen to be built in, which is what makes GOANT_RANDOM_SEED
// reproducible across runs.
func (rt *Runtime) nextRandom() float64 {
	if rt.randState == 0 {
		rt.randState = randSeed()
	}
	x := rt.randState
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	rt.randState = x
	// 53-bit mantissa → [0,1)
	return float64(x>>11) / float64(1<<53)
}
