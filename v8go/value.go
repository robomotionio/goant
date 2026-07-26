package v8go

import (
	"errors"
	"fmt"
	"math"

	"github.com/robomotionio/goant/internal/engine"
)

// Value is a JavaScript value. It carries the runtime that produced it: engine
// values are handles into that runtime's pools and mean nothing without it.
type Value struct {
	rt engine.Value
	r  *engine.Runtime
}

func wrap(r *engine.Runtime, v engine.Value) *Value { return &Value{rt: v, r: r} }

// NewValue converts a Go value to a JavaScript one. The accepted types are the
// ones the binding accepts: string, bool, the numeric types, and nil for null.
func NewValue(i *Isolate, v interface{}) (*Value, error) {
	r, err := i.runtime()
	if err != nil {
		return nil, err
	}
	return newValueOn(r, v)
}

func newValueOn(r *engine.Runtime, v interface{}) (*Value, error) {
	switch x := v.(type) {
	case nil:
		return wrap(r, r.Null()), nil
	case string:
		return wrap(r, r.NewString(x)), nil
	case bool:
		return wrap(r, r.NewBool(x)), nil
	case float64:
		return wrap(r, r.NewNumber(x)), nil
	case float32:
		return wrap(r, r.NewNumber(float64(x))), nil
	case int:
		return wrap(r, r.NewNumber(float64(x))), nil
	case int8:
		return wrap(r, r.NewNumber(float64(x))), nil
	case int16:
		return wrap(r, r.NewNumber(float64(x))), nil
	case int32:
		return wrap(r, r.NewNumber(float64(x))), nil
	case int64:
		return wrap(r, r.NewNumber(float64(x))), nil
	case uint:
		return wrap(r, r.NewNumber(float64(x))), nil
	case uint8:
		return wrap(r, r.NewNumber(float64(x))), nil
	case uint16:
		return wrap(r, r.NewNumber(float64(x))), nil
	case uint32:
		return wrap(r, r.NewNumber(float64(x))), nil
	case uint64:
		return wrap(r, r.NewNumber(float64(x))), nil
	case *Value:
		if x == nil {
			return wrap(r, r.Undefined()), nil
		}
		return x, nil
	}
	return nil, fmt.Errorf("v8go: cannot convert %T to a JavaScript value", v)
}

// Undefined returns the undefined value.
func Undefined(i *Isolate) *Value {
	r, err := i.runtime()
	if err != nil {
		return nil
	}
	return wrap(r, r.Undefined())
}

// Null returns the null value.
func Null(i *Isolate) *Value {
	r, err := i.runtime()
	if err != nil {
		return nil
	}
	return wrap(r, r.Null())
}

// NewExternalOneByteValue creates a string value from Latin-1 bytes. Under V8
// this avoided a copy by pinning the caller's buffer; goant copies, so this is
// an ordinary string constructor kept for call-site compatibility. The buffer
// is NOT retained, which makes it safer than the binding it replaces.
func NewExternalOneByteValue(i *Isolate, s string) (*Value, error) {
	return NewValue(i, s)
}

// String applies ToString. A conversion that throws yields the empty string,
// matching the binding's behaviour of never failing here.
func (v *Value) String() string {
	if v == nil || v.r == nil {
		return ""
	}
	s, err := v.r.ToString(v.rt)
	if err != nil {
		return ""
	}
	return s
}

// DetailString is String plus, for an Error, its stack. Used when reporting a
// rejected promise, where the bare message loses the origin.
func (v *Value) DetailString() string {
	if v == nil || v.r == nil {
		return ""
	}
	if v.r.IsObject(v.rt) {
		if st, err := v.r.GetProp(v.rt, "stack"); err == nil && !v.r.IsUndefined(st) {
			if s, err := v.r.ToString(st); err == nil && s != "" {
				return s
			}
		}
	}
	return v.String()
}

// MarshalJSON returns JSON.stringify(v). A value that stringifies to nothing —
// undefined, or a function — marshals as the JSON literal null, because
// encoding/json requires valid JSON and there is no other faithful answer.
func (v *Value) MarshalJSON() ([]byte, error) {
	if v == nil || v.r == nil {
		return []byte("null"), nil
	}
	s, ok, err := v.r.JSONStringify(v.rt)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []byte("null"), nil
	}
	return []byte(s), nil
}

// Type predicates, all without coercion.
func (v *Value) IsUndefined() bool { return v != nil && v.r != nil && v.r.IsUndefined(v.rt) }
func (v *Value) IsNull() bool      { return v != nil && v.r != nil && v.r.IsNull(v.rt) }
func (v *Value) IsBoolean() bool   { return v != nil && v.r != nil && v.r.IsBool(v.rt) }
func (v *Value) IsNumber() bool    { return v != nil && v.r != nil && v.r.IsNumber(v.rt) }
func (v *Value) IsString() bool    { return v != nil && v.r != nil && v.r.IsString(v.rt) }
func (v *Value) IsObject() bool    { return v != nil && v.r != nil && v.r.IsObject(v.rt) }
func (v *Value) IsFunction() bool  { return v != nil && v.r != nil && v.r.IsFunction(v.rt) }
func (v *Value) IsArray() bool     { return v != nil && v.r != nil && v.r.IsArray(v.rt) }
func (v *Value) IsPromise() bool   { return v != nil && v.r != nil && v.r.IsPromise(v.rt) }

// Number applies ToNumber, returning NaN if the conversion throws.
func (v *Value) Number() float64 {
	if v == nil || v.r == nil {
		return math.NaN()
	}
	f, err := v.r.ToNumber(v.rt)
	if err != nil {
		return math.NaN()
	}
	return f
}

// Boolean applies ToBoolean.
func (v *Value) Boolean() bool {
	return v != nil && v.r != nil && v.r.ToBool(v.rt)
}

// Int32, Uint32 and Integer truncate toward zero, as the ToInt32 family does.
// A caller needing the untruncated value should use Number — see the bridge-ID
// parsing in the robomotion tree for why that distinction matters.
func (v *Value) Int32() int32   { return int32(truncate(v.Number())) }
func (v *Value) Uint32() uint32 { return uint32(truncate(v.Number())) }
func (v *Value) Integer() int64 { return int64(truncate(v.Number())) }

func truncate(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Trunc(f)
}

// Promise settlement states.
type PromiseState int

const (
	Pending PromiseState = iota
	Fulfilled
	Rejected
)

// Promise is a JavaScript promise.
type Promise struct{ v *Value }

// AsPromise views v as a promise, failing if it is not one.
func (v *Value) AsPromise() (*Promise, error) {
	if !v.IsPromise() {
		return nil, errors.New("v8go: value is not a Promise")
	}
	return &Promise{v: v}, nil
}

// State returns the promise's settlement state.
func (p *Promise) State() PromiseState {
	if p == nil || p.v == nil {
		return Pending
	}
	st, _, ok := p.v.r.PromiseState(p.v.rt)
	if !ok {
		return Pending
	}
	switch st {
	case engine.PromiseFulfilled:
		return Fulfilled
	case engine.PromiseRejected:
		return Rejected
	}
	return Pending
}

// Result returns the fulfilment value or rejection reason. It is undefined
// while the promise is still pending.
func (p *Promise) Result() *Value {
	if p == nil || p.v == nil {
		return nil
	}
	_, res, ok := p.v.r.PromiseState(p.v.rt)
	if !ok {
		return nil
	}
	return wrap(p.v.r, res)
}

// Value returns the promise as an ordinary value.
func (p *Promise) Value() *Value { return p.v }
