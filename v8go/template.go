package v8go

import (
	"errors"

	"goant/internal/engine"
)

// FunctionCallback is a Go function callable from JavaScript. Returning nil
// means undefined — or, if the callback called Isolate.ThrowException first,
// that it threw.
type FunctionCallback func(info *FunctionCallbackInfo) *Value

// FunctionCallbackInfo is the argument list of a call into Go.
type FunctionCallbackInfo struct {
	ctx  *Context
	this *Value
	args []*Value
}

// Args returns the call's arguments.
func (i *FunctionCallbackInfo) Args() []*Value { return i.args }

// This returns the call's this-binding.
func (i *FunctionCallbackInfo) This() *Value { return i.this }

// Context returns the context the call happened in.
func (i *FunctionCallbackInfo) Context() *Context { return i.ctx }

// FunctionTemplate produces a callable object per context.
type FunctionTemplate struct {
	iso *Isolate
	cb  FunctionCallback
}

// NewFunctionTemplate creates a template that instantiates to a function
// calling cb.
func NewFunctionTemplate(iso *Isolate, cb FunctionCallback) *FunctionTemplate {
	if iso == nil || cb == nil {
		return nil
	}
	return &FunctionTemplate{iso: iso, cb: cb}
}

// instantiate builds the callable for a specific context. The bridge from the
// engine's calling convention to the binding's is here: the engine hands us
// values and expects (value, throw); the callback returns a value and signals a
// throw out of band via Isolate.ThrowException.
func (t *FunctionTemplate) instantiate(c *Context) engine.Value {
	iso := t.iso
	cb := t.cb
	return c.r.NewFunction("", 0, func(r *engine.Runtime, this engine.Value, args []engine.Value) (engine.Value, *engine.ThrowError) {
		info := &FunctionCallbackInfo{
			ctx:  c,
			this: wrap(r, this),
			args: make([]*Value, len(args)),
		}
		for i, a := range args {
			info.args[i] = wrap(r, a)
		}

		// Clear any exception left over from an earlier callback before running
		// this one, so a stale value cannot be mistaken for a fresh throw.
		iso.mu.Lock()
		iso.pending = nil
		iso.mu.Unlock()

		ret := cb(info)

		iso.mu.Lock()
		thrown := iso.pending
		iso.pending = nil
		iso.mu.Unlock()

		if thrown != nil {
			return r.Undefined(), r.Throw(thrown.rt)
		}
		if ret == nil {
			return r.Undefined(), nil
		}
		return ret.rt, nil
	})
}

// ObjectTemplate describes an object to be created once per context — in
// practice the set of host functions installed on a fresh global.
type ObjectTemplate struct {
	iso     *Isolate
	entries []templateEntry
}

type templateEntry struct {
	key string
	val interface{}
}

// NewObjectTemplate creates an empty template.
func NewObjectTemplate(iso *Isolate) *ObjectTemplate {
	if iso == nil {
		return nil
	}
	return &ObjectTemplate{iso: iso}
}

// Set records a property. The value may be a *FunctionTemplate, an
// *ObjectTemplate, or anything NewValue accepts. Entries are applied in the
// order they were set, so a later Set of the same key wins.
func (t *ObjectTemplate) Set(key string, val interface{}, attrs ...interface{}) error {
	if t == nil {
		return errors.New("v8go: nil template")
	}
	t.entries = append(t.entries, templateEntry{key: key, val: val})
	return nil
}

// installOn materialises the template's entries on c's global object.
func (t *ObjectTemplate) installOn(c *Context) {
	g := c.r.Global()
	for _, e := range t.entries {
		v, ok := t.materialise(c, e.val)
		if !ok {
			continue
		}
		_ = c.r.SetProp(g, e.key, v)
	}
}

func (t *ObjectTemplate) materialise(c *Context, val interface{}) (engine.Value, bool) {
	switch x := val.(type) {
	case *FunctionTemplate:
		if x == nil {
			return c.r.Undefined(), false
		}
		return x.instantiate(c), true
	case *ObjectTemplate:
		if x == nil {
			return c.r.Undefined(), false
		}
		obj := c.r.NewObject()
		for _, e := range x.entries {
			v, ok := x.materialise(c, e.val)
			if !ok {
				continue
			}
			_ = c.r.SetProp(obj, e.key, v)
		}
		return obj, true
	}
	v, err := newValueOn(c.r, val)
	if err != nil {
		return c.r.Undefined(), false
	}
	return v.rt, true
}
