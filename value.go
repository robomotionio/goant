package goant

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/robomotionio/goant/internal/engine"
)

// Value is a JavaScript value.
//
// It is a small struct passed by value — a handle plus the Runtime that owns
// it — so holding one costs nothing and reading one allocates nothing. The
// pairing matters: a Value means nothing without its Runtime, and a Value from
// one Runtime must never be given to another.
//
// A Value does not outlive its Runtime, or the Scope it was created in. Read
// what you need before closing either.
//
// The zero Value is undefined, and every method on it is safe.
type Value struct {
	v  engine.Value
	rt *Runtime
}

// Kind is a JavaScript language type, as the specification defines them —
// which is not quite the typeof operator: typeof null is "object", and typeof
// a function is "function", but neither is a distinct language type.
//
// Use IsFunction, IsArray, IsDate and friends for the object subtypes.
type Kind uint8

// The JavaScript language types.
const (
	KindUndefined Kind = iota
	KindNull
	KindBoolean
	KindNumber
	KindString
	KindSymbol
	KindBigInt
	KindObject
)

// String names the kind, using the specification's spelling.
func (k Kind) String() string {
	switch k {
	case KindUndefined:
		return "undefined"
	case KindNull:
		return "null"
	case KindBoolean:
		return "boolean"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindSymbol:
		return "symbol"
	case KindBigInt:
		return "bigint"
	case KindObject:
		return "object"
	}
	return "unknown"
}

// live reports whether this Value still has a usable Runtime behind it.
func (v Value) live() (*engine.Runtime, bool) {
	if v.rt == nil {
		return nil, false
	}
	e, err := v.rt.engineOf()
	if err != nil {
		return nil, false
	}
	return e, true
}

// Runtime returns the Runtime this Value belongs to, or nil for the zero Value.
func (v Value) Runtime() *Runtime { return v.rt }

// Kind returns the value's language type.
func (v Value) Kind() Kind {
	e, ok := v.live()
	if !ok {
		return KindUndefined
	}
	switch {
	case e.IsUndefined(v.v):
		return KindUndefined
	case e.IsNull(v.v):
		return KindNull
	case e.IsBool(v.v):
		return KindBoolean
	case e.IsNumber(v.v):
		return KindNumber
	case e.IsString(v.v):
		return KindString
	case e.IsSymbol(v.v):
		return KindSymbol
	case e.IsBigInt(v.v):
		return KindBigInt
	}
	return KindObject
}

// TypeOf returns the JavaScript typeof string.
func (v Value) TypeOf() string {
	e, ok := v.live()
	if !ok {
		return "undefined"
	}
	return e.TypeOf(v.v)
}

// Type predicates, all without coercion: IsNumber is false for a string that
// happens to look numeric.
func (v Value) IsUndefined() bool { e, ok := v.live(); return !ok || e.IsUndefined(v.v) }
func (v Value) IsNull() bool      { e, ok := v.live(); return ok && e.IsNull(v.v) }
func (v Value) IsBool() bool      { e, ok := v.live(); return ok && e.IsBool(v.v) }
func (v Value) IsNumber() bool    { e, ok := v.live(); return ok && e.IsNumber(v.v) }
func (v Value) IsString() bool    { e, ok := v.live(); return ok && e.IsString(v.v) }
func (v Value) IsSymbol() bool    { e, ok := v.live(); return ok && e.IsSymbol(v.v) }
func (v Value) IsBigInt() bool    { e, ok := v.live(); return ok && e.IsBigInt(v.v) }
func (v Value) IsObject() bool    { e, ok := v.live(); return ok && e.IsObject(v.v) }
func (v Value) IsFunction() bool  { e, ok := v.live(); return ok && e.IsFunction(v.v) }
func (v Value) IsArray() bool     { e, ok := v.live(); return ok && e.IsArray(v.v) }
func (v Value) IsPromise() bool   { e, ok := v.live(); return ok && e.IsPromise(v.v) }
func (v Value) IsDate() bool      { e, ok := v.live(); return ok && e.IsDate(v.v) }
func (v Value) IsError() bool     { e, ok := v.live(); return ok && e.IsError(v.v) }

// IsTypedArray reports whether v is a typed array — Uint8Array and the rest.
// Such a value is not IsObject: it has its own type, so a host walking a graph
// must test for it separately.
func (v Value) IsTypedArray() bool { e, ok := v.live(); return ok && e.IsTypedArray(v.v) }

// IsNullish reports null or undefined, the two values `== null` matches.
func (v Value) IsNullish() bool { return v.IsUndefined() || v.IsNull() }

// --- primitive conversions --------------------------------------------------

// String applies ToString, so it is the JavaScript String(v) of any value.
// A conversion that throws — a toString that fails — yields the empty string;
// use ToString when that difference matters.
//
// It also makes Value a fmt.Stringer, so a Value formats sensibly in a log line.
func (v Value) String() string {
	s, err := v.ToString()
	if err != nil {
		return ""
	}
	return s
}

// ToString applies ToString and reports a conversion that threw.
func (v Value) ToString() (string, error) {
	e, ok := v.live()
	if !ok {
		return "", ErrClosed
	}
	s, err := e.ToString(v.v)
	if err != nil {
		return "", v.rt.wrap(err)
	}
	return s, nil
}

// Float applies ToNumber, yielding NaN for a conversion that throws.
func (v Value) Float() float64 {
	e, ok := v.live()
	if !ok {
		return math.NaN()
	}
	f, err := e.ToNumber(v.v)
	if err != nil {
		return math.NaN()
	}
	return f
}

// Int applies ToNumber and truncates toward zero, the way ToInteger does. NaN
// and the infinities become 0.
func (v Value) Int() int64 {
	f := v.Float()
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return int64(math.Trunc(f))
}

// Bool applies ToBoolean — JavaScript truthiness, so "" and 0 and NaN are
// false and every object is true.
func (v Value) Bool() bool {
	e, ok := v.live()
	return ok && e.ToBool(v.v)
}

// BigInt returns a BigInt's value. ok is false for anything else; the returned
// big.Int is a copy the caller owns.
func (v Value) BigInt() (x *big.Int, ok bool) {
	e, live := v.live()
	if !live {
		return nil, false
	}
	return e.BigInt(v.v)
}

// Time returns a Date's instant. ok is false if v is not a Date or holds an
// invalid time.
func (v Value) Time() (t time.Time, ok bool) {
	e, live := v.live()
	if !live {
		return time.Time{}, false
	}
	ms, isDate := e.DateMillis(v.v)
	if !isDate || math.IsNaN(ms) {
		return time.Time{}, false
	}
	sec := math.Floor(ms / 1000)
	return time.Unix(int64(sec), int64((ms-sec*1000)*1e6)).UTC(), true
}

// Bytes returns the bytes behind an ArrayBuffer or any typed-array view of one,
// without copying. ok is false for anything else, and for a detached buffer.
//
// The bytes are live: a write through the returned slice is visible to the
// script, and a script's write is visible here. Copy if you need a snapshot.
func (v Value) Bytes() (b []byte, ok bool) {
	e, live := v.live()
	if !live {
		return nil, false
	}
	return e.Bytes(v.v)
}

// --- object views -----------------------------------------------------------

// Object views v as an object, returning nil if it is not one. Functions and
// arrays are objects; primitives are not, and are not boxed into one.
func (v Value) Object() *Object { return v.object() }

func (v Value) object() *Object {
	if !v.IsObject() && !v.IsTypedArray() {
		return nil
	}
	return &Object{Value: v}
}

// Function views v as a callable, returning nil if it is not one.
func (v Value) Function() *Function {
	if !v.IsFunction() {
		return nil
	}
	return &Function{Value: v}
}

// Promise views v as a promise, returning nil if it is not one.
func (v Value) Promise() *Promise {
	if !v.IsPromise() {
		return nil
	}
	return &Promise{Value: v}
}

// --- comparison and serialisation -------------------------------------------

// Equals applies ===. Two Values from different Runtimes are never equal.
func (v Value) Equals(o Value) bool {
	e, ok := v.live()
	if !ok || v.rt != o.rt {
		return false
	}
	return e.StrictEquals(v.v, o.v)
}

// MarshalJSON returns JSON.stringify(v), making Value usable anywhere
// encoding/json is.
//
// A value that stringifies to nothing — undefined, a function, a symbol —
// marshals as the JSON literal null, because encoding/json requires valid JSON
// and there is no other faithful answer. AppendJSON reports the difference.
func (v Value) MarshalJSON() ([]byte, error) {
	b, ok, err := v.AppendJSON(nil)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []byte("null"), nil
	}
	return b, nil
}

// AppendJSON serializes v as JSON and appends it to dst, returning the extended
// buffer. ok is false when the value has no JSON form at all — undefined, a
// function, a symbol — which is not the same as the string "null", and nothing
// is appended in that case.
//
// Appending into the caller's buffer is what makes a message pump cheap: the
// bytes are written once, into memory the host already has, instead of being
// built as a JavaScript string, copied to a Go string and copied again.
func (v Value) AppendJSON(dst []byte) (out []byte, ok bool, err error) {
	e, live := v.live()
	if !live {
		return dst, false, ErrClosed
	}
	out, ok, serr := e.JSONStringifyToBytes(v.v, dst)
	if serr != nil {
		return dst, false, v.rt.wrap(serr)
	}
	return out, ok, nil
}

// UnmarshalJSON is not supported: a Value has to exist inside a Runtime, and
// encoding/json cannot supply one. Use Runtime.ParseJSON.
func (v *Value) UnmarshalJSON([]byte) error {
	return errors.New("goant: cannot unmarshal into a Value; use Runtime.ParseJSON")
}

// The standard interfaces a Value satisfies, checked at compile time.
var (
	_ json.Marshaler = Value{}
	_ fmt.Stringer   = Value{}
)
