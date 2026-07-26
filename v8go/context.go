package v8go

import (
	"errors"
	"sync"

	"github.com/robomotionio/goant/internal/engine"
)

// Context is an execution context: its own globals, sharing the isolate's
// builtins, string interning and object pools.
//
// Under V8 this is a fresh realm, because V8 builds one from a heap snapshot
// and it is cheap. goant has no snapshot, so a realm costs 366 µs and 885
// allocations — against roughly 6 µs for the work a short script actually does.
// Rebuilding the universe to isolate a message transform is the wrong trade.
//
// So a Context is an Invocation: a fresh global object whose prototype is the
// shared one. Builtins resolve up the chain, everything the script installs
// lands on the fresh global and is dropped at Close. Measured at 111 ns.
//
// The one thing this does not isolate is modification of the shared builtins.
// That is detected rather than prevented: Dirty reports it, and a host pooling
// isolates must not reuse one whose Context reported true.
//
// Consequence of sharing the isolate rather than forking it: one Context at a
// time per Isolate. That is how a pooled-isolate host already uses this — a
// lease per in-flight call — and it is the same constraint V8 places on an
// isolate anyway.
type Context struct {
	iso *Isolate
	r   *engine.Runtime
	inv *engine.Invocation

	mu     sync.Mutex
	closed bool
}

// contextOptions accumulates what NewContext was given.
type contextOptions struct {
	iso  *Isolate
	tmpl *ObjectTemplate
}

// ContextOption configures a new Context. Both *Isolate and *ObjectTemplate
// satisfy it, which is what lets NewContext(iso, globals) read naturally while
// either argument stays optional.
type ContextOption interface {
	apply(*contextOptions)
}

// NewContext creates a context. Passing an *Isolate puts the context on that
// isolate; passing an *ObjectTemplate installs its entries on the new global.
// With no isolate a fresh one is created, matching the binding.
func NewContext(opt ...ContextOption) *Context {
	var opts contextOptions
	for _, o := range opt {
		if o != nil {
			o.apply(&opts)
		}
	}
	iso := opts.iso
	if iso == nil {
		iso = NewIsolate()
	}
	root, err := iso.runtime()
	if err != nil {
		return nil
	}
	c := &Context{iso: iso, r: root, inv: root.BeginInvocation()}

	iso.mu.Lock()
	iso.contexts++
	iso.mu.Unlock()

	// Host functions are installed on the invocation's global, so they go away
	// with it rather than accumulating across calls.
	if opts.tmpl != nil {
		opts.tmpl.installOn(c)
	}
	return c
}

// Close ends the invocation and, when it is safe, frees everything the script
// allocated — in one step, without tracing.
//
// A run allocates a message graph, produces a result, and every object it made
// dies together, so the allocator simply rewinds. Memory stays flat across any
// number of messages instead of accumulating until the isolate is retired.
//
// It is skipped when the script wrote to state that predates the Context, since
// something outside the freed region could then point into it. Dirty reports
// that case and such an isolate should be disposed anyway.
//
// Values do not outlive their Context. That is already true under V8, where a
// Value is context-scoped, so this does not change what a correct caller may
// do — but it does mean a caller must read what it needs before closing.
func (c *Context) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	already := c.closed
	c.closed = true
	inv := c.inv
	c.mu.Unlock()
	if already {
		return
	}
	inv.Release()
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

// Dirty reports whether the script modified state that predates this Context —
// a builtin prototype, most likely. Such an isolate must be disposed rather
// than returned to a pool: the next script on it would inherit the change.
//
// Valid until Close, and after it.
func (c *Context) Dirty() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	inv := c.inv
	c.mu.Unlock()
	return inv.Dirty()
}
