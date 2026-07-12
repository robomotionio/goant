package engine

// Number and Boolean constructors + prototypes, plus the numeric global
// functions parseInt/parseFloat/isNaN/isFinite (ant builtin_number / globals).

import "math"

func (rt *Runtime) initNumberBuiltin() {
	proto := rt.objPtr(rt.numberProto)

	numOf := func(this Value) (float64, bool) {
		if this.Type() == TNum {
			return this.Number(), true
		}
		// Unwrap a boxed Number object (new Number(x) / Object(x)).
		if o := rt.objPtr(this); o != nil && o.boxed.Type() == TNum {
			return o.boxed.Number(), true
		}
		return 0, false
	}

	rt.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if n, ok := numOf(this); ok {
			return mknum(n), nil
		}
		return mkundef(), rt.typeError("Number.prototype.valueOf requires a number")
	})
	rt.defMethod(proto, "toString", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, ok := numOf(this)
		if !ok {
			return mkundef(), rt.typeError("Number.prototype.toString requires a number")
		}
		radix := 10
		if !arg(args, 0).IsUndefined() {
			r, e := rt.toNumber(args[0])
			if e != nil {
				return mkundef(), e
			}
			radix = int(r)
			if radix < 2 || radix > 36 {
				return mkundef(), rt.rangeError("toString() radix must be between 2 and 36")
			}
		}
		return rt.newString(numberToStringRadix(n, radix)), nil
	})
	rt.defMethod(proto, "toFixed", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, ok := numOf(this)
		if !ok {
			return mkundef(), rt.typeError("Number.prototype.toFixed requires a number")
		}
		digits := rt.intArg(args, 0)
		if digits < 0 || digits > 100 {
			return mkundef(), rt.rangeError("toFixed() digits argument must be between 0 and 100")
		}
		if math.IsNaN(n) {
			return rt.internString("NaN"), nil
		}
		return rt.newString(strconvFixed(n, digits)), nil
	})

	rt.defMethod(proto, "toExponential", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, ok := numOf(this)
		if !ok {
			return mkundef(), rt.typeError("Number.prototype.toExponential requires a number")
		}
		// A non-finite value returns before the fractionDigits range check (so
		// NaN.toExponential(Infinity) yields "NaN", not a RangeError).
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return rt.newString(toExponentialStr(n, 0, false)), nil
		}
		if arg(args, 0).IsUndefined() {
			return rt.newString(toExponentialStr(n, 0, false)), nil
		}
		d := rt.intArg(args, 0)
		if d < 0 || d > 100 {
			return mkundef(), rt.rangeError("toExponential() argument must be between 0 and 100")
		}
		return rt.newString(toExponentialStr(n, d, true)), nil
	})
	rt.defMethod(proto, "toPrecision", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, ok := numOf(this)
		if !ok {
			return mkundef(), rt.typeError("Number.prototype.toPrecision requires a number")
		}
		if arg(args, 0).IsUndefined() {
			return rt.newString(numberToString(n)), nil
		}
		p := rt.intArg(args, 0)
		if p < 1 || p > 100 {
			return mkundef(), rt.rangeError("toPrecision() argument must be between 1 and 100")
		}
		return rt.newString(toPrecisionStr(n, p)), nil
	})

	rt.defMethod(proto, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, ok := numOf(this)
		if !ok {
			return mkundef(), rt.typeError("Number.prototype.toLocaleString requires a number")
		}
		return rt.newString(numberToString(n)), nil
	})

	ctor := rt.newNativeFunc("Number", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n := 0.0
		if len(args) > 0 {
			v, e := rt.toNumber(args[0])
			if e != nil {
				return mkundef(), e
			}
			n = v
		}
		// new Number(x) (incl. `super(x)` from a subclass): a Number exotic object
		// wrapping the primitive in its [[NumberData]] slot.
		if o := rt.objPtr(this); o != nil {
			o.boxed = mknum(n)
			return this, nil
		}
		return mknum(n), nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", rt.numberProto, 0)
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)

	numConst := func(name string, v float64) { cobj.defineOwn(name, mknum(v), 0) }
	numConst("MAX_VALUE", math.MaxFloat64)
	numConst("MIN_VALUE", math.SmallestNonzeroFloat64)
	numConst("MAX_SAFE_INTEGER", 9007199254740991)
	numConst("MIN_SAFE_INTEGER", -9007199254740991)
	numConst("POSITIVE_INFINITY", math.Inf(1))
	numConst("NEGATIVE_INFINITY", math.Inf(-1))
	numConst("NaN", math.NaN())
	numConst("EPSILON", 2.220446049250313e-16)

	rt.defMethod(cobj, "isNaN", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v := arg(args, 0)
		return mkbool(v.Type() == TNum && math.IsNaN(v.Number())), nil
	})
	rt.defMethod(cobj, "isFinite", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v := arg(args, 0)
		return mkbool(v.Type() == TNum && !math.IsNaN(v.Number()) && !math.IsInf(v.Number(), 0)), nil
	})
	rt.defMethod(cobj, "isInteger", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v := arg(args, 0)
		if v.Type() != TNum {
			return mkfalse(), nil
		}
		d := v.Number()
		return mkbool(!math.IsNaN(d) && !math.IsInf(d, 0) && d == math.Trunc(d)), nil
	})
	rt.defMethod(cobj, "isSafeInteger", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v := arg(args, 0)
		if v.Type() != TNum {
			return mkfalse(), nil
		}
		d := v.Number()
		return mkbool(!math.IsNaN(d) && !math.IsInf(d, 0) && d == math.Trunc(d) && math.Abs(d) <= 9007199254740991), nil
	})

	rt.initGlobalNumberFns()
	// Number.parseInt / Number.parseFloat mirror the globals.
	if pf, ok := rt.objPtr(rt.global).getOwn("parseFloat"); ok {
		cobj.defineOwn("parseFloat", pf, attrWritable|attrConfigurable)
	}
	if pi, ok := rt.objPtr(rt.global).getOwn("parseInt"); ok {
		cobj.defineOwn("parseInt", pi, attrWritable|attrConfigurable)
	}

	rt.defGlobal("Number", ctor)
}

func (rt *Runtime) initBooleanBuiltin() {
	proto := rt.objPtr(rt.booleanProto)
	// thisBool resolves the receiver's boolean primitive: either a boolean value
	// or the [[BooleanData]] of a Boolean wrapper object.
	thisBool := func(this Value) (bool, bool) {
		if this.Type() == TBool {
			return this.Bool(), true
		}
		if o := rt.objPtr(this); o != nil && o.boxed.Type() == TBool {
			return o.boxed.Bool(), true
		}
		return false, false
	}
	rt.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if b, ok := thisBool(this); ok {
			return mkbool(b), nil
		}
		return mkundef(), rt.typeError("Boolean.prototype.valueOf requires a boolean")
	})
	rt.defMethod(proto, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if b, ok := thisBool(this); ok {
			if b {
				return rt.internString("true"), nil
			}
			return rt.internString("false"), nil
		}
		return mkundef(), rt.typeError("Boolean.prototype.toString requires a boolean")
	})
	ctor := rt.newNativeFunc("Boolean", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b := rt.toBoolean(arg(args, 0))
		// new Boolean(x) (incl. subclass `super(x)`): a Boolean wrapper object.
		if o := rt.objPtr(this); o != nil {
			o.boxed = mkbool(b)
			return this, nil
		}
		return mkbool(b), nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", rt.booleanProto, 0)
	proto.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.defGlobal("Boolean", ctor)
}

// initGlobalNumberFns installs parseInt/parseFloat/isNaN/isFinite globals.
func (rt *Runtime) initGlobalNumberFns() {
	g := rt.objPtr(rt.global)
	rt.defMethod(g, "isNaN", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.toNumber(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return mkbool(math.IsNaN(n)), nil
	})
	rt.defMethod(g, "isFinite", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		n, e := rt.toNumber(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return mkbool(!math.IsNaN(n) && !math.IsInf(n, 0)), nil
	})
	rt.defMethod(g, "parseFloat", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		return mknum(jsParseFloat(string(b))), nil
	})
	rt.defMethod(g, "parseInt", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := rt.stringArg(args, 0)
		if e != nil {
			return mkundef(), e
		}
		radix := 0
		if !arg(args, 1).IsUndefined() {
			r, e := rt.toNumber(args[1])
			if e != nil {
				return mkundef(), e
			}
			radix = int(int32(r))
		}
		return mknum(jsParseInt(string(b), radix)), nil
	})
}
