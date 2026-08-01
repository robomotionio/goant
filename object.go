package goant

import (
	"errors"
	"fmt"
	"sort"

	"github.com/robomotionio/goant/internal/engine"
)

// Object is a JavaScript object: anything with properties, which includes
// arrays, functions, dates and typed arrays.
//
// It embeds Value, so every Value method is available on an Object too.
type Object struct{ Value }

// errNilObject is what every method reports for a nil receiver, so a chain that
// hit a non-object earlier fails with a message rather than a panic.
var errNilObject = errors.New("goant: nil object")

// Get reads obj[key], running any getter and following the prototype chain.
// A key that is not there reads as undefined, which is not an error — that is
// what JavaScript does. An error means the read itself failed: a getter or a
// Proxy trap threw.
func (o *Object) Get(key string) (Value, error) {
	if o == nil {
		return Value{}, errNilObject
	}
	e, ok := o.live()
	if !ok {
		return Value{}, ErrClosed
	}
	v, err := e.GetProp(o.v, key)
	if err != nil {
		return Value{}, o.rt.wrap(err)
	}
	return o.rt.val(v), nil
}

// Set writes obj[key], running any setter. val may be any Go value ToValue
// accepts.
func (o *Object) Set(key string, val any) error {
	if o == nil {
		return errNilObject
	}
	e, ok := o.live()
	if !ok {
		return ErrClosed
	}
	cv, err := o.rt.ToValue(val)
	if err != nil {
		return fmt.Errorf("goant: set %q: %w", key, err)
	}
	if serr := e.SetProp(o.v, key, cv.v); serr != nil {
		return o.rt.wrap(serr)
	}
	return nil
}

// SetAll writes several properties, stopping at the first that fails.
func (o *Object) SetAll(vals map[string]any) error {
	for _, k := range sortedNames(vals) {
		if err := o.Set(k, vals[k]); err != nil {
			return err
		}
	}
	return nil
}

// Has reports whether key resolves on obj or anywhere up its prototype chain —
// the `in` operator.
func (o *Object) Has(key string) (bool, error) {
	if o == nil {
		return false, errNilObject
	}
	e, ok := o.live()
	if !ok {
		return false, ErrClosed
	}
	has, err := e.HasProp(o.v, key)
	if err != nil {
		return false, o.rt.wrap(err)
	}
	return has, nil
}

// Delete removes obj[key], reporting whether it is gone afterwards. A
// non-configurable property survives and reports false rather than failing,
// which is what `delete` does outside strict mode.
func (o *Object) Delete(key string) (bool, error) {
	if o == nil {
		return false, errNilObject
	}
	e, ok := o.live()
	if !ok {
		return false, ErrClosed
	}
	gone, err := e.DeleteProp(o.v, key)
	if err != nil {
		return false, o.rt.wrap(err)
	}
	return gone, nil
}

// Keys returns the object's own enumerable string keys, in property order:
// integer indices ascending, then the rest in insertion order. This is
// Object.keys — no symbols, no inherited properties, nothing non-enumerable.
func (o *Object) Keys() ([]string, error) {
	if o == nil {
		return nil, errNilObject
	}
	e, ok := o.live()
	if !ok {
		return nil, ErrClosed
	}
	keys, err := e.OwnKeys(o.v)
	if err != nil {
		return nil, o.rt.wrap(err)
	}
	return keys, nil
}

// Len reads obj.length and coerces it as the array built-ins do. For an array
// that is its element count; for anything without a numeric length it is 0.
func (o *Object) Len() (int, error) {
	if o == nil {
		return 0, errNilObject
	}
	e, ok := o.live()
	if !ok {
		return 0, ErrClosed
	}
	n, err := e.LengthOf(o.v)
	if err != nil {
		return 0, o.rt.wrap(err)
	}
	return n, nil
}

// At reads obj[i]. Out of range reads as undefined.
func (o *Object) At(i int) (Value, error) {
	if o == nil {
		return Value{}, errNilObject
	}
	e, ok := o.live()
	if !ok {
		return Value{}, ErrClosed
	}
	v, err := e.GetIndex(o.v, i)
	if err != nil {
		return Value{}, o.rt.wrap(err)
	}
	return o.rt.val(v), nil
}

// SetAt writes obj[i], growing an array's length if needed.
func (o *Object) SetAt(i int, val any) error {
	if o == nil {
		return errNilObject
	}
	e, ok := o.live()
	if !ok {
		return ErrClosed
	}
	cv, err := o.rt.ToValue(val)
	if err != nil {
		return fmt.Errorf("goant: set index %d: %w", i, err)
	}
	if serr := e.SetIndex(o.v, i, cv.v); serr != nil {
		return o.rt.wrap(serr)
	}
	return nil
}

// Values returns an array's elements, or an object's own enumerable values.
// It is the one call to reach for when you want the contents rather than a
// particular member.
func (o *Object) Values() ([]Value, error) {
	if o == nil {
		return nil, errNilObject
	}
	if o.IsArray() || o.IsTypedArray() {
		n, err := o.Len()
		if err != nil {
			return nil, err
		}
		out := make([]Value, 0, n)
		for i := 0; i < n; i++ {
			v, err := o.At(i)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	keys, err := o.Keys()
	if err != nil {
		return nil, err
	}
	out := make([]Value, 0, len(keys))
	for _, k := range keys {
		v, err := o.Get(k)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Method returns obj[name] as a callable, or an error if it is missing or not
// a function. The method keeps obj as its receiver, so calling it is what
// `obj.name(...)` does.
func (o *Object) Method(name string) (*Function, error) {
	v, err := o.Get(name)
	if err != nil {
		return nil, err
	}
	fn := v.Function()
	if fn == nil {
		return nil, fmt.Errorf("goant: %s is not a function", name)
	}
	fn.this = o.Value
	fn.hasThis = true
	return fn, nil
}

// Call invokes obj.name(args...) with obj as the receiver.
func (o *Object) Call(name string, args ...any) (Value, error) {
	fn, err := o.Method(name)
	if err != nil {
		return Value{}, err
	}
	return fn.Call(args...)
}

// --- functions --------------------------------------------------------------

// Function is a callable JavaScript value.
type Function struct {
	Value

	// this is the receiver a Method-derived Function carries. hasThis
	// distinguishes "bound to undefined" from "not bound", so a bare function
	// keeps calling with undefined rather than silently acquiring a receiver.
	this    Value
	hasThis bool
}

// Call invokes the function. A Function obtained from Object.Method is called
// on its object; any other is called with `this` undefined.
func (f *Function) Call(args ...any) (Value, error) {
	if f == nil {
		return Value{}, errors.New("goant: nil function")
	}
	this := f.this
	if !f.hasThis {
		this = f.rt.Undefined()
	}
	return f.CallOn(this, args...)
}

// CallOn invokes the function with an explicit receiver.
func (f *Function) CallOn(this Value, args ...any) (Value, error) {
	if f == nil {
		return Value{}, errors.New("goant: nil function")
	}
	e, ok := f.live()
	if !ok {
		return Value{}, ErrClosed
	}
	ev, err := f.rt.toValues(args)
	if err != nil {
		return Value{}, err
	}
	v, cerr := e.Call(f.v, this.v, ev)
	if cerr != nil {
		return Value{}, f.rt.wrap(cerr)
	}
	return f.rt.val(v), nil
}

// Construct invokes the function as a constructor — `new f(args...)`.
func (f *Function) Construct(args ...any) (*Object, error) {
	if f == nil {
		return nil, errors.New("goant: nil function")
	}
	e, ok := f.live()
	if !ok {
		return nil, ErrClosed
	}
	ev, err := f.rt.toValues(args)
	if err != nil {
		return nil, err
	}
	v, cerr := e.Construct(f.v, ev)
	if cerr != nil {
		return nil, f.rt.wrap(cerr)
	}
	obj := f.rt.val(v).object()
	if obj == nil {
		return nil, errors.New("goant: constructor did not return an object")
	}
	return obj, nil
}

// --- promises ---------------------------------------------------------------

// Promise is a JavaScript promise.
//
// A promise reports what it has settled to; it does not schedule anything.
// Nothing settles until the job queue runs, so read one after Runtime.RunJobs —
// or use Runtime.Await, which drains and unwraps in one step.
type Promise struct{ Value }

// PromiseState is a promise's settlement state.
type PromiseState uint8

// The three promise states.
const (
	Pending PromiseState = iota
	Fulfilled
	Rejected
)

// String names the state.
func (s PromiseState) String() string {
	switch s {
	case Fulfilled:
		return "fulfilled"
	case Rejected:
		return "rejected"
	}
	return "pending"
}

// State returns the promise's settlement state.
func (p *Promise) State() PromiseState {
	if p == nil {
		return Pending
	}
	e, ok := p.live()
	if !ok {
		return Pending
	}
	st, _, valid := e.PromiseState(p.v)
	if !valid {
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

// Result returns the fulfilment value or the rejection reason. It is undefined
// while the promise is pending.
func (p *Promise) Result() Value {
	if p == nil {
		return Value{}
	}
	e, ok := p.live()
	if !ok {
		return Value{}
	}
	_, res, valid := e.PromiseState(p.v)
	if !valid {
		return Value{}
	}
	return p.rt.val(res)
}

// sortedNames orders a map's keys, so that a conversion failure is reported on
// the same key every run rather than on whichever the map iterator reached
// first, and so an object built from a map has a stable property order.
func sortedNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
