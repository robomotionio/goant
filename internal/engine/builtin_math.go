package engine

// The Math object (ant modules/math.c / builtin_math). Most methods delegate to
// Go's math package; the JS-specific edge cases (round-half-up, sign, trunc,
// clz32, imul, fround, hypot) are implemented to spec.

import "math"

func (rt *Runtime) initMath() {
	m := rt.newObject(mknull())
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
		sum := 0.0
		for _, a := range args {
			x, e := rt.toNumber(a)
			if e != nil {
				return mkundef(), e
			}
			sum += x * x
		}
		return mknum(math.Sqrt(sum)), nil
	}), attrWritable|attrConfigurable)

	// Math.random — deterministic-free PRNG (xorshift; not for crypto).
	mo.defineOwn("random", rt.newNativeFunc("random", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mknum(rt.nextRandom()), nil
	}), attrWritable|attrConfigurable)

	rt.objPtr(rt.global).defineOwn("Math", m, attrWritable|attrConfigurable)
}

func (rt *Runtime) mathReduce(args []Value, init float64, wantMax bool) (Value, *ThrowError) {
	acc := init
	for _, a := range args {
		x, e := rt.toNumber(a)
		if e != nil {
			return mkundef(), e
		}
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
	return math.Floor(x + 0.5)
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

// nextRandom returns a pseudo-random float in [0,1) via a per-Runtime xorshift
// generator seeded lazily.
func (rt *Runtime) nextRandom() float64 {
	if rt.randState == 0 {
		rt.randState = 0x9E3779B97F4A7C15 // fixed nonzero seed (deterministic)
	}
	x := rt.randState
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	rt.randState = x
	// 53-bit mantissa → [0,1)
	return float64(x>>11) / float64(1<<53)
}
