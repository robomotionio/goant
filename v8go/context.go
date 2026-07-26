package v8go

import (
	"errors"
	"sync"

	"goant/internal/engine"
)

// Context is an execution context: its own globals and prototypes, sharing the
// isolate's string interning and object pools. That is a realm, and it is the
// same split V8 draws between an isolate and a context.
type Context struct {
	iso *Isolate
	r   *engine.Runtime

	mu     sync.Mutex
	closed bool
}

// NewContext creates a context on iso. If tmpl is non-nil its entries are
// installed on the new global object.
//
// Unlike the V8 binding, iso may not be nil: there is no implicit isolate to
// fall back on, and inventing one would silently give the caller a context that
// shares nothing with the scripts they compiled.
func NewContext(iso *Isolate, tmpl *ObjectTemplate) *Context {
	if iso == nil {
		return nil
	}
	root, err := iso.runtime()
	if err != nil {
		return nil
	}
	c := &Context{iso: iso, r: root.NewRealm()}

	iso.mu.Lock()
	iso.contexts++
	iso.mu.Unlock()

	if tmpl != nil {
		tmpl.installOn(c)
	}
	return c
}

// Close releases the context. The realm becomes collectable once nothing refers
// to it; there is no separate heap to hand back.
func (c *Context) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	already := c.closed
	c.closed = true
	c.mu.Unlock()
	if already {
		return
	}
	c.iso.mu.Lock()
	if c.iso.contexts > 0 {
		c.iso.contexts--
	}
	c.iso.mu.Unlock()
}

// Isolate returns the context's isolate.
func (c *Context) Isolate() *Isolate { return c.iso }

// Global returns the global object.
func (c *Context) Global() *Object {
	if c == nil {
		return nil
	}
	return &Object{Value: wrap(c.r, c.r.Global()), ctx: c}
}

// RunScript compiles and runs src in this context, returning its completion
// value.
func (c *Context) RunScript(src, origin string) (*Value, error) {
	if c == nil {
		return nil, errors.New("v8go: nil context")
	}
	s, err := c.r.CompileScript(origin, src)
	if err != nil {
		return nil, asJSError(c.r, err)
	}
	v, err := c.r.RunScript(s)
	if err != nil {
		return nil, asJSError(c.r, err)
	}
	return wrap(c.r, v), nil
}

// PerformMicrotaskCheckpoint drains the microtask queue. Promise callbacks do
// not run until this is called, so a script whose result depends on a settled
// promise must have a checkpoint between the run and reading the result.
func (c *Context) PerformMicrotaskCheckpoint() {
	if c == nil {
		return
	}
	c.r.DrainJobs()
}

// Object is a JavaScript object with property access.
type Object struct {
	*Value
	ctx *Context
}

// Set writes obj[key]. The value may be a *Value or any Go value NewValue
// accepts.
func (o *Object) Set(key string, val interface{}) error {
	if o == nil || o.Value == nil {
		return errors.New("v8go: nil object")
	}
	v, err := newValueOn(o.Value.r, val)
	if err != nil {
		return err
	}
	if err := o.Value.r.SetProp(o.Value.rt, key, v.rt); err != nil {
		return asJSError(o.Value.r, err)
	}
	return nil
}

// Get reads obj[key].
func (o *Object) Get(key string) (*Value, error) {
	if o == nil || o.Value == nil {
		return nil, errors.New("v8go: nil object")
	}
	v, err := o.Value.r.GetProp(o.Value.rt, key)
	if err != nil {
		return nil, asJSError(o.Value.r, err)
	}
	return wrap(o.Value.r, v), nil
}
