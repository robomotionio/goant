package engine

import (
	"math"
	"math/big"
	"strings"
)

// BigInt: an arbitrary-precision integer primitive backed by math/big.Int in a
// dedicated pool, boxed as a TBigInt handle value.

type bigIntCell struct {
	v *big.Int
}

// newBigInt boxes a big.Int as a TBigInt value.
func (rt *Runtime) newBigInt(v *big.Int) Value {
	h, cell := rt.bigints.alloc()
	cell.v = v
	return mkval(TBigInt, uint64(h))
}

// bigIntVal returns the big.Int backing a TBigInt value (nil if v is not one).
func (rt *Runtime) bigIntVal(v Value) *big.Int {
	if v.Type() != TBigInt {
		return nil
	}
	if c := rt.bigints.get(Handle(v.handle())); c != nil {
		return c.v
	}
	return nil
}

func (rt *Runtime) bigIntProtoValue() Value { return rt.bigintProto }

// stringToBigInt implements StringToBigInt: leading/trailing whitespace is
// trimmed; "" is 0n; 0x/0o/0b prefixes are honoured. Returns (value, ok).
func stringToBigInt(s string) (*big.Int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return big.NewInt(0), true
	}
	neg := false
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	} else if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	base := 10
	switch {
	case strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X"):
		base, s = 16, s[2:]
	case strings.HasPrefix(s, "0o") || strings.HasPrefix(s, "0O"):
		base, s = 8, s[2:]
	case strings.HasPrefix(s, "0b") || strings.HasPrefix(s, "0B"):
		base, s = 2, s[2:]
	}
	if s == "" {
		return nil, false
	}
	v, ok := new(big.Int).SetString(s, base)
	if !ok {
		return nil, false
	}
	if neg {
		v.Neg(v)
	}
	return v, true
}

// toBigInt implements ToBigInt(argument).
func (rt *Runtime) toBigInt(v Value) (*big.Int, *ThrowError) {
	p, e := rt.toPrimitive(v, "number")
	if e != nil {
		return nil, e
	}
	switch p.Type() {
	case TBigInt:
		return rt.bigIntVal(p), nil
	case TBool:
		if p.Bool() {
			return big.NewInt(1), nil
		}
		return big.NewInt(0), nil
	case TStr:
		bi, ok := stringToBigInt(string(rt.strBytes(p)))
		if !ok {
			return nil, rt.syntaxError("Cannot convert " + string(rt.strBytes(p)) + " to a BigInt")
		}
		return bi, nil
	case TNum:
		// ToBigInt of a Number always throws (7.1.13); the integer-preserving
		// Number→BigInt path (NumberToBigInt) is reachable only via the BigInt()
		// constructor, which handles Number arguments before calling here.
		return nil, rt.typeError("Cannot convert " + numberToString(p.Number()) + " to a BigInt")
	default:
		return nil, rt.typeError("Cannot convert " + rt.typeofString(v) + " to a BigInt")
	}
}

// bigIntToString renders a BigInt in the given radix (default 10).
func bigIntToString(v *big.Int, radix int) string {
	if radix < 2 || radix > 36 {
		radix = 10
	}
	return v.Text(radix)
}

func (rt *Runtime) syntaxError(msg string) *ThrowError {
	return &ThrowError{Value: rt.makeError(rt.errors.syntaxProto, "SyntaxError", msg), rt: rt}
}

func (rt *Runtime) initBigIntBuiltin() {
	proto := rt.newObject(rt.objectProto)
	rt.bigintProto = proto
	po := rt.objPtr(proto)

	ctor := rt.newNativeFunc("BigInt", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if rt.constructing() {
			return mkundef(), rt.typeError("BigInt is not a constructor")
		}
		prim, e := rt.toPrimitive(arg(args, 0), "number")
		if e != nil {
			return mkundef(), e
		}
		if prim.Type() == TNum {
			f := prim.Number()
			if f != math.Trunc(f) || math.IsInf(f, 0) || f != f {
				return mkundef(), rt.rangeError("The number " + numberToString(f) + " cannot be converted to a BigInt because it is not an integer")
			}
			bi, _ := big.NewFloat(f).Int(nil)
			return rt.newBigInt(bi), nil
		}
		bi, e := rt.toBigInt(prim)
		if e != nil {
			return mkundef(), e
		}
		return rt.newBigInt(bi), nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)
	rt.setStringTag(proto, "BigInt")

	rt.defMethod(cobj, "asIntN", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		bits, e := rt.toIndex(arg(args, 0)) // ToIndex first (undefined→0, out-of-range→RangeError)
		if e != nil {
			return mkundef(), e
		}
		bi, e := rt.toBigInt(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return rt.newBigInt(bigIntAsIntN(bits, bi)), nil
	})
	rt.defMethod(cobj, "asUintN", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		bits, e := rt.toIndex(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		bi, e := rt.toBigInt(arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		return rt.newBigInt(bigIntAsUintN(bits, bi)), nil
	})

	thisBig := func(this Value) (*big.Int, *ThrowError) {
		if this.Type() == TBigInt {
			return rt.bigIntVal(this), nil
		}
		if o := rt.objPtr(this); o != nil {
			if b := o.getSlot(slotPrimitive); b.Type() == TBigInt {
				return rt.bigIntVal(b), nil
			}
		}
		return nil, rt.typeError("BigInt.prototype method called on incompatible receiver")
	}
	rt.defMethod(po, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := thisBig(this)
		if e != nil {
			return mkundef(), e
		}
		radix := 10
		if r := arg(args, 0); !r.IsUndefined() {
			n, e := rt.toNumber(r)
			if e != nil {
				return mkundef(), e
			}
			radix = int(n)
		}
		return rt.newString(bigIntToString(b, radix)), nil
	})
	rt.defMethod(po, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := thisBig(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newBigInt(b), nil
	})
	rt.defMethod(po, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		b, e := thisBig(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(bigIntToString(b, 10)), nil
	})

	rt.defGlobal("BigInt", ctor)
}

// compileBigIntLiteral emits a constant for a `123n` literal (handling 0x/0o/0b
// prefixes and numeric separators; the lexer already validated the digits).
func (c *compiler) compileBigIntLiteral(lit string) {
	s := strings.TrimSuffix(lit, "n")
	s = strings.ReplaceAll(s, "_", "")
	base := 10
	if len(s) > 2 {
		switch s[:2] {
		case "0x", "0X":
			base, s = 16, s[2:]
		case "0o", "0O":
			base, s = 8, s[2:]
		case "0b", "0B":
			base, s = 2, s[2:]
		}
	}
	v, ok := new(big.Int).SetString(s, base)
	if !ok {
		c.errorf("invalid BigInt literal '%s'", lit)
		return
	}
	c.emitConst(c.rt.newBigInt(v))
}

// bigIntBinaryOp evaluates a BigInt arithmetic/bitwise op on two BigInts.
func (rt *Runtime) bigIntBinaryOp(op Opcode, x, y *big.Int) (Value, *ThrowError) {
	r := new(big.Int)
	switch op {
	case OpAdd:
		r.Add(x, y)
	case OpSub:
		r.Sub(x, y)
	case OpMul:
		r.Mul(x, y)
	case OpDiv:
		if y.Sign() == 0 {
			return mkundef(), rt.rangeError("Division by zero")
		}
		r.Quo(x, y)
	case OpMod:
		if y.Sign() == 0 {
			return mkundef(), rt.rangeError("Division by zero")
		}
		r.Rem(x, y)
	case OpExp:
		if y.Sign() < 0 {
			return mkundef(), rt.rangeError("Exponent must be non-negative")
		}
		r.Exp(x, y, nil)
	case OpBand:
		r.And(x, y)
	case OpBor:
		r.Or(x, y)
	case OpBxor:
		r.Xor(x, y)
	case OpShl:
		r.Lsh(x, uint(y.Int64()))
	case OpShr:
		r.Rsh(x, uint(y.Int64()))
	default:
		return mkundef(), rt.typeError("unsupported BigInt operation")
	}
	return rt.newBigInt(r), nil
}

// bigIntAsIntN wraps v to a signed two's-complement integer of `bits` bits.
func bigIntAsIntN(bits int, v *big.Int) *big.Int {
	if bits == 0 {
		return big.NewInt(0)
	}
	mod := new(big.Int).Lsh(big.NewInt(1), uint(bits)) // 2^bits
	r := new(big.Int).Mod(v, mod)                       // 0..2^bits-1
	half := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	if r.Cmp(half) >= 0 {
		r.Sub(r, mod)
	}
	return r
}

// bigIntAsUintN wraps v to an unsigned integer of `bits` bits.
func bigIntAsUintN(bits int, v *big.Int) *big.Int {
	if bits == 0 {
		return big.NewInt(0)
	}
	mod := new(big.Int).Lsh(big.NewInt(1), uint(bits))
	return new(big.Int).Mod(v, mod)
}
