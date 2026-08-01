package goant

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/robomotionio/goant/internal/engine"
)

// FieldNamer decides what a Go struct field is called in JavaScript. Returning
// "" leaves the field out.
type FieldNamer func(reflect.StructField) string

// JSONFieldNamer names fields the way encoding/json does: the `json` tag if
// there is one, the field name otherwise, and nothing at all for `json:"-"`.
//
// It is the default, so a struct crosses into JavaScript under the names its
// JSON form already uses — which is almost always the name the script expects,
// and means one set of tags describes both directions.
func JSONFieldNamer(f reflect.StructField) string {
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		return f.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" && !strings.Contains(tag, ",") {
		return ""
	}
	if name == "" {
		return f.Name
	}
	return name
}

// GoFieldNamer uses the Go field name unchanged.
func GoFieldNamer(f reflect.StructField) string { return f.Name }

// LowerCamelFieldNamer lowercases the first letter of the Go field name, so
// UserID becomes userID.
func LowerCamelFieldNamer(f reflect.StructField) string {
	if f.Name == "" {
		return ""
	}
	r := []rune(f.Name)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// --- host functions ---------------------------------------------------------

// Call is one call from JavaScript into Go.
type Call struct {
	// Runtime is the runtime the call came from.
	Runtime *Runtime

	// This is the receiver — the object the function was called on, or
	// undefined for a plain call.
	This Value

	// Args are the arguments, exactly as many as the script passed.
	Args []Value
}

// Arg returns the i'th argument, or undefined if the script did not pass one.
// Reading past the end is not an error: in JavaScript a missing argument is
// undefined, and a host function should behave the same way.
func (c *Call) Arg(i int) Value {
	if c == nil || i < 0 || i >= len(c.Args) {
		if c != nil && c.Runtime != nil {
			return c.Runtime.Undefined()
		}
		return Value{}
	}
	return c.Args[i]
}

// Len returns how many arguments the script passed.
func (c *Call) Len() int {
	if c == nil {
		return 0
	}
	return len(c.Args)
}

// String, Int, Float and Bool read an argument with the coercion JavaScript
// would apply, so a host function can take what it needs without a type switch.
func (c *Call) String(i int) string { return c.Arg(i).String() }
func (c *Call) Int(i int) int64     { return c.Arg(i).Int() }
func (c *Call) Float(i int) float64 { return c.Arg(i).Float() }
func (c *Call) Bool(i int) bool     { return c.Arg(i).Bool() }

// ExportTo converts the i'th argument into dst. See Value.ExportTo.
func (c *Call) ExportTo(i int, dst any) error { return c.Arg(i).ExportTo(dst) }

// Func is a host function in its raw form: it receives the call as the script
// made it and returns any value ToValue accepts. A non-nil error is thrown into
// the script, so a host function reports failure the way Go does.
//
// A panic is not converted: it unwinds through the interpreter and out of the
// Run call that started it, which is what you want for a bug — a host function
// that panics has a defect, and turning it into a JavaScript exception the
// script might catch would hide it. Return an error for the failures you mean.
// A Runtime a panic escaped from should be discarded rather than reused.
//
// Use it when the argument list is variable, or when you want the values
// unconverted. For a fixed signature, pass an ordinary Go function instead —
// ToValue binds one directly:
//
//	rt.Set("hypot", math.Hypot)
//	rt.Set("parse", func(s string) (Config, error) { ... })
type Func func(c *Call) (any, error)

// callType is used to recognise the raw host-function shape.
var callType = reflect.TypeOf((*Call)(nil))
var errorType = reflect.TypeOf((*error)(nil)).Elem()
var anyType = reflect.TypeOf((*any)(nil)).Elem()
var timeType = reflect.TypeOf(time.Time{})
var bigIntPtrType = reflect.TypeOf((*big.Int)(nil))
var valueType = reflect.TypeOf(Value{})
var objectPtrType = reflect.TypeOf((*Object)(nil))
var functionPtrType = reflect.TypeOf((*Function)(nil))
var promisePtrType = reflect.TypeOf((*Promise)(nil))
var byteSliceType = reflect.TypeOf([]byte(nil))
var jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

// --- Go to JavaScript -------------------------------------------------------

// ToValue converts a Go value to a JavaScript one.
//
//	nil                     null
//	bool                    boolean
//	all int/uint/float      number
//	string                  string
//	[]byte                  Uint8Array (no copy — see Runtime.NewBytes)
//	*big.Int                bigint
//	time.Time               Date
//	error                   Error
//	slice, array            Array
//	map                     object (keys via their string form)
//	struct, *struct         object; exported fields by the Runtime's FieldNamer,
//	                        exported methods as functions
//	func                    function
//	Value, *Object, ...     itself
//
// A pointer is followed, and a nil one becomes null. A cycle is preserved
// rather than followed forever: the same Go pointer converts to the same
// JavaScript object.
//
// Types with no JavaScript counterpart — channels, complex numbers,
// unsafe.Pointer — are an error rather than a guess.
func (rt *Runtime) ToValue(v any) (Value, error) {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}, err
	}
	c := &toJS{rt: rt, e: e}
	ev, cerr := c.convert(v)
	if cerr != nil {
		return Value{}, cerr
	}
	return rt.val(ev), nil
}

// MustValue is ToValue for a value known to be convertible. It panics if it is
// not, so it belongs in tests and in initialisation, not on a request path.
func (rt *Runtime) MustValue(v any) Value {
	out, err := rt.ToValue(v)
	if err != nil {
		panic(err)
	}
	return out
}

// toValues converts a call's argument list.
func (rt *Runtime) toValues(args []any) ([]engine.Value, error) {
	e, err := rt.engineOf()
	if err != nil {
		return nil, err
	}
	c := &toJS{rt: rt, e: e}
	out := make([]engine.Value, len(args))
	for i, a := range args {
		ev, cerr := c.convert(a)
		if cerr != nil {
			return nil, fmt.Errorf("goant: argument %d: %w", i, cerr)
		}
		out[i] = ev
	}
	return out, nil
}

// toJS carries the conversion state, which is only the cycle table.
type toJS struct {
	rt   *Runtime
	e    *engine.Runtime
	seen map[seenKey]engine.Value
}

type seenKey struct {
	ptr uintptr
	typ reflect.Type
}

func (c *toJS) convert(v any) (engine.Value, error) {
	// The concrete types a host actually passes, without reflection.
	switch x := v.(type) {
	case nil:
		return c.e.Null(), nil
	case Value:
		return c.adopt(x)
	case *Object:
		if x == nil {
			return c.e.Null(), nil
		}
		return c.adopt(x.Value)
	case *Function:
		if x == nil {
			return c.e.Null(), nil
		}
		return c.adopt(x.Value)
	case *Promise:
		if x == nil {
			return c.e.Null(), nil
		}
		return c.adopt(x.Value)
	case bool:
		return c.e.NewBool(x), nil
	case string:
		// Deliberately not interned. A host passes arbitrary data through here,
		// and the intern table is permanent: interning would retain every
		// distinct string the host ever handed over.
		return c.e.NewStringData(x), nil
	case []byte:
		return c.e.NewUint8Array(x), nil
	case float64:
		return c.e.NewNumber(x), nil
	case float32:
		return c.e.NewNumber(float64(x)), nil
	case int:
		return c.e.NewNumber(float64(x)), nil
	case int8:
		return c.e.NewNumber(float64(x)), nil
	case int16:
		return c.e.NewNumber(float64(x)), nil
	case int32:
		return c.e.NewNumber(float64(x)), nil
	case int64:
		return c.e.NewNumber(float64(x)), nil
	case uint:
		return c.e.NewNumber(float64(x)), nil
	case uint8:
		return c.e.NewNumber(float64(x)), nil
	case uint16:
		return c.e.NewNumber(float64(x)), nil
	case uint32:
		return c.e.NewNumber(float64(x)), nil
	case uint64:
		return c.e.NewNumber(float64(x)), nil
	case *big.Int:
		if x == nil {
			return c.e.Null(), nil
		}
		return c.e.NewBigIntValue(x), nil
	case time.Time:
		return c.e.NewDate(float64(x.UnixMilli()))
	case json.RawMessage:
		return c.e.JSONParseBytes(x)
	case error:
		if x == nil {
			return c.e.Null(), nil
		}
		return c.e.NewError(x.Error()), nil
	case Func:
		return c.rawFunc("", x), nil
	case map[string]any:
		return c.convertMapAny(x)
	case []any:
		return c.convertSliceAny(x)
	}
	return c.reflected(reflect.ValueOf(v))
}

// adopt accepts a Value that already belongs to this Runtime and refuses one
// that does not — a handle from another Runtime names a different object in
// this one, which is the one mistake this API cannot detect later.
func (c *toJS) adopt(v Value) (engine.Value, error) {
	if v.rt == nil {
		return c.e.Undefined(), nil
	}
	if v.rt != c.rt {
		return c.e.Undefined(), fmt.Errorf("goant: value belongs to a different Runtime")
	}
	return v.v, nil
}

func (c *toJS) convertMapAny(m map[string]any) (engine.Value, error) {
	obj := c.e.NewObject()
	for _, k := range sortedNames(m) {
		ev, err := c.convert(m[k])
		if err != nil {
			return c.e.Undefined(), fmt.Errorf("key %q: %w", k, err)
		}
		if serr := c.e.SetProp(obj, k, ev); serr != nil {
			return c.e.Undefined(), serr
		}
	}
	return obj, nil
}

func (c *toJS) convertSliceAny(s []any) (engine.Value, error) {
	vals := make([]engine.Value, len(s))
	for i, x := range s {
		ev, err := c.convert(x)
		if err != nil {
			return c.e.Undefined(), fmt.Errorf("index %d: %w", i, err)
		}
		vals[i] = ev
	}
	return c.e.NewArray(vals...), nil
}

func (c *toJS) remember(rv reflect.Value, ev engine.Value) {
	if c.seen == nil {
		c.seen = make(map[seenKey]engine.Value)
	}
	c.seen[seenKey{rv.Pointer(), rv.Type()}] = ev
}

func (c *toJS) recall(rv reflect.Value) (engine.Value, bool) {
	if c.seen == nil {
		return engine.Value(0), false
	}
	ev, ok := c.seen[seenKey{rv.Pointer(), rv.Type()}]
	return ev, ok
}

func (c *toJS) reflected(rv reflect.Value) (engine.Value, error) {
	if !rv.IsValid() {
		return c.e.Null(), nil
	}

	switch rv.Kind() {
	case reflect.Bool:
		return c.e.NewBool(rv.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return c.e.NewNumber(float64(rv.Int())), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return c.e.NewNumber(float64(rv.Uint())), nil
	case reflect.Float32, reflect.Float64:
		return c.e.NewNumber(rv.Float()), nil
	case reflect.String:
		return c.e.NewStringData(rv.String()), nil

	case reflect.Interface:
		if rv.IsNil() {
			return c.e.Null(), nil
		}
		return c.convert(rv.Elem().Interface())

	case reflect.Pointer:
		if rv.IsNil() {
			return c.e.Null(), nil
		}
		if ev, ok := c.recall(rv); ok {
			return ev, nil
		}
		// A pointer to a struct is converted as the struct, but remembered
		// under the pointer so a cycle through it terminates. Methods with
		// pointer receivers are reachable only from here, so this is also
		// where a Go object with behaviour becomes a JavaScript one.
		if rv.Elem().Kind() == reflect.Struct {
			return c.structValue(rv)
		}
		return c.reflected(rv.Elem())

	case reflect.Slice:
		if rv.IsNil() {
			return c.e.Null(), nil
		}
		if rv.Type() == byteSliceType {
			return c.e.NewUint8Array(rv.Bytes()), nil
		}
		if ev, ok := c.recall(rv); ok {
			return ev, nil
		}
		arr := c.e.NewArray()
		c.remember(rv, arr)
		return c.fillArray(arr, rv)

	case reflect.Array:
		arr := c.e.NewArray()
		return c.fillArray(arr, rv)

	case reflect.Map:
		if rv.IsNil() {
			return c.e.Null(), nil
		}
		if ev, ok := c.recall(rv); ok {
			return ev, nil
		}
		obj := c.e.NewObject()
		c.remember(rv, obj)
		return c.fillMap(obj, rv)

	case reflect.Struct:
		if rv.Type() == timeType {
			return c.e.NewDate(float64(rv.Interface().(time.Time).UnixMilli()))
		}
		return c.structValue(rv)

	case reflect.Func:
		if rv.IsNil() {
			return c.e.Null(), nil
		}
		return c.function("", rv)
	}

	return c.e.Undefined(), fmt.Errorf("goant: cannot convert %s to a JavaScript value", rv.Type())
}

func (c *toJS) fillArray(arr engine.Value, rv reflect.Value) (engine.Value, error) {
	for i := 0; i < rv.Len(); i++ {
		ev, err := c.reflected(rv.Index(i))
		if err != nil {
			return c.e.Undefined(), fmt.Errorf("index %d: %w", i, err)
		}
		if serr := c.e.SetIndex(arr, i, ev); serr != nil {
			return c.e.Undefined(), serr
		}
	}
	return arr, nil
}

func (c *toJS) fillMap(obj engine.Value, rv reflect.Value) (engine.Value, error) {
	iter := rv.MapRange()
	for iter.Next() {
		key, err := mapKeyString(iter.Key())
		if err != nil {
			return c.e.Undefined(), err
		}
		ev, err := c.reflected(iter.Value())
		if err != nil {
			return c.e.Undefined(), fmt.Errorf("key %q: %w", key, err)
		}
		if serr := c.e.SetProp(obj, key, ev); serr != nil {
			return c.e.Undefined(), serr
		}
	}
	return obj, nil
}

// mapKeyString renders a map key the way JavaScript would: object keys are
// strings, so anything else has to become one, and a key that cannot is an
// error rather than a collision waiting to happen.
func mapKeyString(k reflect.Value) (string, error) {
	switch k.Kind() {
	case reflect.String:
		return k.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return fmt.Sprintf("%d", k.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return fmt.Sprintf("%d", k.Uint()), nil
	case reflect.Interface:
		if k.IsNil() {
			return "", fmt.Errorf("goant: nil map key")
		}
		return mapKeyString(k.Elem())
	}
	if k.CanInterface() {
		if s, ok := k.Interface().(fmt.Stringer); ok {
			return s.String(), nil
		}
	}
	return "", fmt.Errorf("goant: cannot use %s as an object key", k.Type())
}

// structValue converts a struct, or a pointer to one, into an object: exported
// fields by the Runtime's namer, then exported methods as callable properties.
func (c *toJS) structValue(rv reflect.Value) (engine.Value, error) {
	obj := c.e.NewObject()
	if rv.Kind() == reflect.Pointer {
		c.remember(rv, obj)
	}

	sv := rv
	if sv.Kind() == reflect.Pointer {
		sv = sv.Elem()
	}
	if err := c.fillStruct(obj, sv); err != nil {
		return c.e.Undefined(), err
	}

	// Methods come after fields so a field never loses its name to a method,
	// and they are taken from the original value: a pointer sees pointer-
	// receiver methods, a struct only value-receiver ones.
	rt := rv.Type()
	for i := 0; i < rt.NumMethod(); i++ {
		m := rt.Method(i)
		if !m.IsExported() {
			continue
		}
		name := lowerFirst(m.Name)
		fn, err := c.function(m.Name, rv.Method(i))
		if err != nil {
			// A method whose signature has no JavaScript form is skipped
			// rather than failing the whole struct: the fields are still
			// useful, and the method was never callable anyway.
			continue
		}
		if has, _ := c.e.HasProp(obj, name); has {
			continue
		}
		if serr := c.e.SetProp(obj, name, fn); serr != nil {
			return c.e.Undefined(), serr
		}
	}
	return obj, nil
}

func (c *toJS) fillStruct(obj engine.Value, sv reflect.Value) error {
	st := sv.Type()
	namer := c.rt.fieldNamer()
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		// An embedded struct is flattened into its parent, as encoding/json
		// does, so a script sees one object rather than a nesting the Go type
		// only has for reuse. This is checked before the exported test because
		// an embedded type may be unexported while its fields are not — the
		// promoted fields are still readable, and are still what a script
		// expects to find.
		if ev, ok := embeddedStruct(f, sv.Field(i)); ok {
			if !ev.IsValid() {
				continue
			}
			if err := c.fillStruct(obj, ev); err != nil {
				return err
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		name := namer(f)
		if name == "" {
			continue
		}
		ev, err := c.reflected(sv.Field(i))
		if err != nil {
			return fmt.Errorf("field %s: %w", f.Name, err)
		}
		if serr := c.e.SetProp(obj, name, ev); serr != nil {
			return serr
		}
	}
	return nil
}

// taggedName reports whether an embedded field carries an explicit JSON name,
// which — as in encoding/json — makes it a named property rather than a
// promotion of its fields.
func taggedName(f reflect.StructField) bool {
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		return false
	}
	name, _, _ := strings.Cut(tag, ",")
	return name != ""
}

// embeddedStruct reports whether f is an embedded struct whose fields are
// promoted into the parent, and returns the struct value to read them from. A
// nil embedded pointer yields an invalid value: there is nothing to promote.
//
// An embedded field carrying an explicit JSON name is a named property, not a
// promotion — the same rule encoding/json applies.
func embeddedStruct(f reflect.StructField, fv reflect.Value) (reflect.Value, bool) {
	if !f.Anonymous || taggedName(f) {
		return reflect.Value{}, false
	}
	switch {
	case f.Type.Kind() == reflect.Struct:
		return fv, true
	case f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.Struct:
		if fv.IsNil() {
			return reflect.Value{}, true
		}
		return fv.Elem(), true
	}
	return reflect.Value{}, false
}

// embeddedTarget is embeddedStruct for the writing direction: a nil embedded
// pointer is allocated so its promoted fields have somewhere to land.
func embeddedTarget(f reflect.StructField, fv reflect.Value) (reflect.Value, error) {
	if f.Type.Kind() != reflect.Pointer {
		return fv, nil
	}
	if !fv.IsNil() {
		return fv.Elem(), nil
	}
	if !fv.CanSet() {
		// An unexported embedded pointer cannot be allocated, so its fields
		// have nowhere to go. encoding/json refuses this too.
		return reflect.Value{}, fmt.Errorf("goant: cannot set embedded %s", f.Type)
	}
	fv.Set(reflect.New(f.Type.Elem()))
	return fv.Elem(), nil
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// --- binding Go functions ---------------------------------------------------

// function turns a Go func value into a JavaScript function.
func (c *toJS) function(name string, fv reflect.Value) (engine.Value, error) {
	ft := fv.Type()
	if ft.NumIn() >= 1 && ft.In(0) == callType {
		if ft.NumIn() != 1 {
			return c.e.Undefined(), fmt.Errorf("goant: a *Call host function takes no other parameters")
		}
		raw, err := rawAdapter(fv, ft)
		if err != nil {
			return c.e.Undefined(), err
		}
		return c.rawFunc(name, raw), nil
	}
	if err := checkResults(ft); err != nil {
		return c.e.Undefined(), err
	}
	return c.reflectFunc(name, fv, ft), nil
}

// rawAdapter normalises the accepted *Call signatures onto Func.
func rawAdapter(fv reflect.Value, ft reflect.Type) (Func, error) {
	switch {
	case ft.NumOut() == 0:
		return func(c *Call) (any, error) {
			fv.Call([]reflect.Value{reflect.ValueOf(c)})
			return nil, nil
		}, nil
	case ft.NumOut() == 1 && ft.Out(0) == errorType:
		return func(c *Call) (any, error) {
			out := fv.Call([]reflect.Value{reflect.ValueOf(c)})
			return nil, errOf(out[0])
		}, nil
	case ft.NumOut() == 1:
		return func(c *Call) (any, error) {
			out := fv.Call([]reflect.Value{reflect.ValueOf(c)})
			return out[0].Interface(), nil
		}, nil
	case ft.NumOut() == 2 && ft.Out(1) == errorType:
		return func(c *Call) (any, error) {
			out := fv.Call([]reflect.Value{reflect.ValueOf(c)})
			if err := errOf(out[1]); err != nil {
				return nil, err
			}
			return out[0].Interface(), nil
		}, nil
	}
	return nil, fmt.Errorf("goant: unsupported host function results %s", ft)
}

// checkResults rejects a signature whose results cannot be reported.
func checkResults(ft reflect.Type) error {
	switch ft.NumOut() {
	case 0, 1:
		return nil
	case 2:
		if ft.Out(1) == errorType {
			return nil
		}
	}
	return fmt.Errorf("goant: a bound function returns at most one value and an error, not %s", ft)
}

func errOf(rv reflect.Value) error {
	if rv.IsNil() {
		return nil
	}
	return rv.Interface().(error)
}

// rawFunc installs a Func as a JavaScript function.
func (c *toJS) rawFunc(name string, fn Func) engine.Value {
	rt := c.rt
	return c.e.NewFunction(name, 0, func(e *engine.Runtime, this engine.Value, args []engine.Value) (engine.Value, *engine.ThrowError) {
		call := &Call{Runtime: rt, This: rt.val(this), Args: make([]Value, len(args))}
		for i, a := range args {
			call.Args[i] = rt.val(a)
		}
		out, err := fn(call)
		if err != nil {
			return e.Undefined(), throwOf(e, rt, err)
		}
		ev, cerr := (&toJS{rt: rt, e: e}).convert(out)
		if cerr != nil {
			return e.Undefined(), e.ThrowError(cerr.Error())
		}
		return ev, nil
	})
}

// reflectFunc installs an ordinary Go function, converting arguments into its
// parameter types and its result back out.
func (c *toJS) reflectFunc(name string, fv reflect.Value, ft reflect.Type) engine.Value {
	rt := c.rt
	nIn := ft.NumIn()
	variadic := ft.IsVariadic()
	arity := nIn
	if variadic {
		arity = nIn - 1
	}

	return c.e.NewFunction(name, arity, func(e *engine.Runtime, this engine.Value, args []engine.Value) (engine.Value, *engine.ThrowError) {
		in := make([]reflect.Value, 0, len(args)+1)
		for i := 0; i < arity; i++ {
			pt := ft.In(i)
			pv := reflect.New(pt).Elem()
			if i < len(args) {
				// A missing argument keeps the parameter's zero value, which is
				// what a JavaScript function does with an omitted one.
				if err := exportInto(rt.val(args[i]), pv); err != nil {
					return e.Undefined(), e.ThrowError(fmt.Sprintf("argument %d: %s", i+1, err))
				}
			}
			in = append(in, pv)
		}
		if variadic {
			et := ft.In(nIn - 1).Elem()
			for i := arity; i < len(args); i++ {
				pv := reflect.New(et).Elem()
				if err := exportInto(rt.val(args[i]), pv); err != nil {
					return e.Undefined(), e.ThrowError(fmt.Sprintf("argument %d: %s", i+1, err))
				}
				in = append(in, pv)
			}
		}

		out := fv.Call(in)
		switch {
		case len(out) == 0:
			return e.Undefined(), nil
		case len(out) == 1 && ft.Out(0) == errorType:
			if err := errOf(out[0]); err != nil {
				return e.Undefined(), throwOf(e, rt, err)
			}
			return e.Undefined(), nil
		case len(out) == 2:
			if err := errOf(out[1]); err != nil {
				return e.Undefined(), throwOf(e, rt, err)
			}
		}
		ev, cerr := (&toJS{rt: rt, e: e}).reflected(out[0])
		if cerr != nil {
			return e.Undefined(), e.ThrowError(cerr.Error())
		}
		return ev, nil
	})
}

// throwOf turns a Go error into a JavaScript throw. An *Error is rethrown as
// the value it carried, so a value that crossed into Go and back is the same
// object the script threw — identity and all — rather than a copy of its text.
func throwOf(e *engine.Runtime, rt *Runtime, err error) *engine.ThrowError {
	var jsErr *Error
	if errors.As(err, &jsErr) && jsErr.val.rt == rt {
		return e.Throw(jsErr.val.v)
	}
	return e.ThrowError(err.Error())
}

// --- JavaScript to Go -------------------------------------------------------

// Export converts a JavaScript value into the closest ordinary Go value.
//
//	undefined, null         nil
//	boolean                 bool
//	number                  float64
//	string                  string
//	bigint                  *big.Int
//	Date                    time.Time
//	Array                   []any
//	Uint8Array, ArrayBuffer []byte
//	function                *Function
//	other objects           map[string]any
//	symbol                  Value (there is no Go equivalent)
//
// Numbers are always float64, never int64 — the same choice encoding/json
// makes, and for the same reason: JavaScript has one number type, and guessing
// which Go type a caller wanted from whether the value happens to be integral
// produces code that works until a value arrives with a fraction.
//
// A cycle is preserved: an object reached twice is the same Go map both times.
//
// A read that fails part-way — a getter or a Proxy trap that throws — yields
// nil, because there is no second return value to report it in. Use ExportTo
// when you need to know, and to convert into a type you already have.
func (v Value) Export() any {
	x, err := v.export(make(map[engine.Value]any))
	if err != nil {
		return nil
	}
	return x
}

func (v Value) export(seen map[engine.Value]any) (any, error) {
	e, ok := v.live()
	if !ok {
		return nil, ErrClosed
	}
	switch {
	case e.IsUndefined(v.v), e.IsNull(v.v):
		return nil, nil
	case e.IsBool(v.v):
		return e.ToBool(v.v), nil
	case e.IsNumber(v.v):
		f, err := e.ToNumber(v.v)
		if err != nil {
			return nil, v.rt.wrap(err)
		}
		return f, nil
	case e.IsString(v.v):
		s, err := e.ToString(v.v)
		if err != nil {
			return nil, v.rt.wrap(err)
		}
		return s, nil
	case e.IsBigInt(v.v):
		if x, ok := e.BigInt(v.v); ok {
			return x, nil
		}
		return nil, nil
	case e.IsSymbol(v.v):
		// A symbol has no Go form. Handing back the Value keeps it usable —
		// as a key, or to pass straight back into the script.
		return v, nil
	}

	if prev, ok := seen[v.v]; ok {
		return prev, nil
	}

	if t, ok := v.Time(); ok {
		return t, nil
	}
	if e.IsByteArray(v.v) {
		// Only a value that unambiguously holds bytes becomes []byte. A
		// Float64Array has bytes too, but they are not what it holds, so it
		// exports as its numbers like any other array-like.
		if b, ok := v.Bytes(); ok {
			return b, nil
		}
	}
	if e.IsFunction(v.v) {
		return v.Function(), nil
	}

	obj := v.object()
	if obj == nil {
		return nil, nil
	}
	if e.IsArray(v.v) || e.IsTypedArray(v.v) {
		n, err := obj.Len()
		if err != nil {
			return nil, err
		}
		out := make([]any, n)
		seen[v.v] = out
		for i := 0; i < n; i++ {
			ev, err := obj.At(i)
			if err != nil {
				return nil, err
			}
			x, err := ev.export(seen)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	}

	keys, err := obj.Keys()
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(keys))
	seen[v.v] = out
	for _, k := range keys {
		ev, err := obj.Get(k)
		if err != nil {
			return nil, err
		}
		x, err := ev.export(seen)
		if err != nil {
			return nil, err
		}
		out[k] = x
	}
	return out, nil
}

// ExportTo converts a JavaScript value into dst, which must be a non-nil
// pointer.
//
// This is the direction most host code wants: you already have the Go type, and
// the conversion should fill it rather than hand back an any to be picked
// apart. Structs are filled by the Runtime's FieldNamer — by default the same
// `json` tags encoding/json uses — and a missing property leaves its field
// alone.
//
//	var cfg struct {
//	    Host string `json:"host"`
//	    Port int    `json:"port"`
//	}
//	v.ExportTo(&cfg)
//
// A *func target binds a JavaScript function to a Go signature, so it can be
// called like any other:
//
//	v, _ := rt.Get("sum")
//
//	var sum func(int, int) int
//	v.ExportTo(&sum)
//	sum(40, 2)
//
// A bound function whose signature ends in error reports a JavaScript throw
// there. One that does not, panics with it — there is nowhere else for it to go.
func (v Value) ExportTo(dst any) error {
	if dst == nil {
		return fmt.Errorf("goant: ExportTo into nil")
	}
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("goant: ExportTo needs a non-nil pointer, got %T", dst)
	}
	return exportInto(v, rv.Elem())
}

// exportInto is the reflective half of ExportTo, also used to build the
// arguments of a bound Go function.
func exportInto(v Value, dst reflect.Value) error {
	dt := dst.Type()

	// The wrapper types are handed over as they are: a host asking for a Value
	// wants the JavaScript value, not a rendering of it.
	switch dt {
	case valueType:
		dst.Set(reflect.ValueOf(v))
		return nil
	case objectPtrType:
		dst.Set(reflect.ValueOf(v.object()))
		return nil
	case functionPtrType:
		dst.Set(reflect.ValueOf(v.Function()))
		return nil
	case promisePtrType:
		dst.Set(reflect.ValueOf(v.Promise()))
		return nil
	case timeType:
		t, ok := v.Time()
		if !ok {
			return fmt.Errorf("goant: %s is not a Date", v.TypeOf())
		}
		dst.Set(reflect.ValueOf(t))
		return nil
	case bigIntPtrType:
		x, ok := v.BigInt()
		if !ok {
			return fmt.Errorf("goant: %s is not a bigint", v.TypeOf())
		}
		dst.Set(reflect.ValueOf(x))
		return nil
	case anyType:
		dst.Set(reflect.ValueOf(v.Export()))
		return nil
	}

	if v.IsNullish() {
		// null and undefined clear the target rather than failing: a property
		// the script did not set should leave a Go field at its zero value, not
		// abort the whole conversion.
		dst.Set(reflect.Zero(dt))
		return nil
	}

	switch dt.Kind() {
	case reflect.Bool:
		dst.SetBool(v.Bool())
		return nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f := v.Float()
		if math.IsNaN(f) {
			return fmt.Errorf("goant: cannot convert %s to %s", v.TypeOf(), dt)
		}
		n := int64(math.Trunc(f))
		if dst.OverflowInt(n) {
			return fmt.Errorf("goant: %v overflows %s", f, dt)
		}
		dst.SetInt(n)
		return nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		f := v.Float()
		if math.IsNaN(f) || f < 0 {
			return fmt.Errorf("goant: cannot convert %v to %s", f, dt)
		}
		n := uint64(math.Trunc(f))
		if dst.OverflowUint(n) {
			return fmt.Errorf("goant: %v overflows %s", f, dt)
		}
		dst.SetUint(n)
		return nil

	case reflect.Float32, reflect.Float64:
		dst.SetFloat(v.Float())
		return nil

	case reflect.String:
		s, err := v.ToString()
		if err != nil {
			return err
		}
		dst.SetString(s)
		return nil

	case reflect.Pointer:
		p := reflect.New(dt.Elem())
		if err := exportInto(v, p.Elem()); err != nil {
			return err
		}
		dst.Set(p)
		return nil

	case reflect.Interface:
		if dt.NumMethod() == 0 {
			dst.Set(reflect.ValueOf(v.Export()))
			return nil
		}
		return fmt.Errorf("goant: cannot export into interface %s", dt)

	case reflect.Slice:
		if dt == byteSliceType {
			if b, ok := v.Bytes(); ok {
				out := make([]byte, len(b))
				copy(out, b)
				dst.SetBytes(out)
				return nil
			}
		}
		return exportSlice(v, dst)

	case reflect.Array:
		return exportArray(v, dst)

	case reflect.Map:
		return exportMap(v, dst)

	case reflect.Struct:
		return exportStruct(v, dst)

	case reflect.Func:
		return exportFunc(v, dst)
	}

	// Anything left may still know how to read itself from JSON, which covers
	// the types that carry their own encoding.
	if reflect.PointerTo(dt).Implements(jsonUnmarshalerType) {
		b, err := v.MarshalJSON()
		if err != nil {
			return err
		}
		p := reflect.New(dt)
		if err := p.Interface().(json.Unmarshaler).UnmarshalJSON(b); err != nil {
			return err
		}
		dst.Set(p.Elem())
		return nil
	}
	return fmt.Errorf("goant: cannot export %s into %s", v.TypeOf(), dt)
}

func exportSlice(v Value, dst reflect.Value) error {
	obj := v.object()
	if obj == nil {
		return fmt.Errorf("goant: cannot export %s into %s", v.TypeOf(), dst.Type())
	}
	n, err := obj.Len()
	if err != nil {
		return err
	}
	out := reflect.MakeSlice(dst.Type(), n, n)
	for i := 0; i < n; i++ {
		ev, err := obj.At(i)
		if err != nil {
			return err
		}
		if err := exportInto(ev, out.Index(i)); err != nil {
			return fmt.Errorf("index %d: %w", i, err)
		}
	}
	dst.Set(out)
	return nil
}

func exportArray(v Value, dst reflect.Value) error {
	obj := v.object()
	if obj == nil {
		return fmt.Errorf("goant: cannot export %s into %s", v.TypeOf(), dst.Type())
	}
	n, err := obj.Len()
	if err != nil {
		return err
	}
	if n > dst.Len() {
		n = dst.Len()
	}
	for i := 0; i < n; i++ {
		ev, err := obj.At(i)
		if err != nil {
			return err
		}
		if err := exportInto(ev, dst.Index(i)); err != nil {
			return fmt.Errorf("index %d: %w", i, err)
		}
	}
	return nil
}

func exportMap(v Value, dst reflect.Value) error {
	obj := v.object()
	if obj == nil {
		return fmt.Errorf("goant: cannot export %s into %s", v.TypeOf(), dst.Type())
	}
	dt := dst.Type()
	if dt.Key().Kind() != reflect.String {
		return fmt.Errorf("goant: map key %s is not a string type", dt.Key())
	}
	keys, err := obj.Keys()
	if err != nil {
		return err
	}
	out := reflect.MakeMapWithSize(dt, len(keys))
	for _, k := range keys {
		ev, err := obj.Get(k)
		if err != nil {
			return err
		}
		elem := reflect.New(dt.Elem()).Elem()
		if err := exportInto(ev, elem); err != nil {
			return fmt.Errorf("key %q: %w", k, err)
		}
		kv := reflect.New(dt.Key()).Elem()
		kv.SetString(k)
		out.SetMapIndex(kv, elem)
	}
	dst.Set(out)
	return nil
}

func exportStruct(v Value, dst reflect.Value) error {
	obj := v.object()
	if obj == nil {
		return fmt.Errorf("goant: cannot export %s into %s", v.TypeOf(), dst.Type())
	}
	namer := v.rt.fieldNamer()
	st := dst.Type()
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if _, ok := embeddedStruct(f, dst.Field(i)); ok {
			ev, err := embeddedTarget(f, dst.Field(i))
			if err != nil || !ev.IsValid() {
				continue
			}
			if err := exportStruct(v, ev); err != nil {
				return err
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		name := namer(f)
		if name == "" {
			continue
		}
		has, err := obj.Has(name)
		if err != nil {
			return err
		}
		if !has {
			// A property the script did not provide leaves the field as it
			// was, so ExportTo can fill in a partially populated struct.
			continue
		}
		pv, err := obj.Get(name)
		if err != nil {
			return err
		}
		if err := exportInto(pv, dst.Field(i)); err != nil {
			return fmt.Errorf("field %s: %w", f.Name, err)
		}
	}
	return nil
}

// exportFunc binds a JavaScript function to a Go function type.
func exportFunc(v Value, dst reflect.Value) error {
	fn := v.Function()
	if fn == nil {
		return fmt.Errorf("goant: %s is not a function", v.TypeOf())
	}
	ft := dst.Type()
	if err := checkResults(ft); err != nil {
		return err
	}
	hasErr := ft.NumOut() > 0 && ft.Out(ft.NumOut()-1) == errorType
	rt := v.rt

	dst.Set(reflect.MakeFunc(ft, func(in []reflect.Value) []reflect.Value {
		args := make([]any, len(in))
		for i, a := range in {
			args[i] = a.Interface()
		}
		res, err := fn.Call(args...)

		out := make([]reflect.Value, ft.NumOut())
		for i := range out {
			out[i] = reflect.Zero(ft.Out(i))
		}
		if err != nil {
			if !hasErr {
				// Nowhere to report it. Panicking is the only alternative to
				// pretending the call succeeded, which would be worse.
				panic(err)
			}
			out[len(out)-1] = reflect.ValueOf(err)
			return out
		}
		if ft.NumOut() > 0 && !(ft.NumOut() == 1 && hasErr) {
			rv := reflect.New(ft.Out(0)).Elem()
			if cerr := exportInto(rt.val(res.v), rv); cerr != nil {
				if !hasErr {
					panic(cerr)
				}
				out[len(out)-1] = reflect.ValueOf(cerr)
				return out
			}
			out[0] = rv
		}
		return out
	}))
	return nil
}
