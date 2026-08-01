// Package goant embeds a JavaScript engine in a Go program.
//
// goant is a pure-Go ECMAScript engine: no cgo, no V8, nothing to fetch at
// build time, and one binary that cross-compiles wherever Go does. See the
// README for where it came from and what it replaced.
//
// # Getting started
//
// A Runtime is one JavaScript world — globals, prototypes, the job queue:
//
//	rt := goant.New()
//	defer rt.Close()
//
//	v, err := rt.RunString(`[1, 2, 3].map(n => n * 2).join("-")`)
//	fmt.Println(v.String()) // 2-4-6
//
// Go values go in and JavaScript values come out through ordinary Go types:
//
//	rt.Set("greet", func(name string) string { return "hello " + name })
//	rt.Set("user", map[string]any{"name": "ada", "id": 7})
//
//	v, _ := rt.RunString(`greet(user.name)`)
//
//	var s string
//	v.ExportTo(&s) // s == "hello ada"
//
// # Threading
//
// A Runtime runs one script at a time and is not safe for concurrent use — the
// same constraint every JavaScript engine places on an isolate. Use one Runtime
// per goroutine, or a Pool. Interrupt is the exception: it is safe from any
// goroutine, which is the point of it.
//
// # Errors
//
// A JavaScript exception that reaches the host arrives as *Error, carrying the
// thrown value as well as its message and stack. A script stopped from outside
// reports ErrInterrupted or ErrMemoryLimit instead — being stopped is not
// something the script did, and the two should not be reported the same way.
package goant

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/robomotionio/goant/internal/engine"
)

// Runtime is an embeddable JavaScript runtime: its own globals, prototypes,
// heap and job queue, independent of every other Runtime in the process.
//
// It is not safe for concurrent use. See the package documentation.
type Runtime struct {
	e *engine.Runtime

	// namer decides what a Go struct field is called in JavaScript.
	namer FieldNamer

	// mu guards only what a second goroutine may legitimately touch: the engine
	// pointer, so Close cannot race an Interrupt. It is not a substitute for
	// single-goroutine use.
	mu sync.Mutex
}

// Option configures a Runtime at construction.
type Option func(*Runtime)

// WithMemoryLimit bounds the live heap, in bytes, that a script on this Runtime
// may retain. The limit is checked after a collection: a script holding more
// than this is stopped and the host is told why, via ErrMemoryLimit.
//
// This exists because the alternative is worse than an error. A Go program that
// allocates past what the machine will give it dies in the runtime allocator —
// no panic, no recover, no deferred anything — and takes every other goroutine
// with it. A budget turns "the process is gone" into "this one call failed",
// which a server can report and carry on from.
//
// Pass 0 (the default) for no limit.
func WithMemoryLimit(bytes uint64) Option {
	return func(rt *Runtime) { rt.e.SetHeapLimit(bytes) }
}

// WithModuleDir sets the directory that ES module specifiers resolve against,
// for code run with RunModule and for scripts using import().
func WithModuleDir(dir string) Option {
	return func(rt *Runtime) { rt.e.SetModuleBase(dir) }
}

// WithFieldNamer sets how Go struct fields are named in JavaScript. The default
// is JSONFieldNamer, so a struct crosses into JavaScript under the same names
// encoding/json would give it.
func WithFieldNamer(n FieldNamer) Option {
	return func(rt *Runtime) {
		if n != nil {
			rt.namer = n
		}
	}
}

// WithGC enables or disables the garbage collector. It is on by default.
//
// Turning it off is only sensible for a short-lived Runtime whose whole heap is
// discarded at the end — a one-shot transform, or work done inside a Scope —
// where collecting costs something and reclaims nothing that dropping the
// Runtime would not.
func WithGC(on bool) Option {
	return func(rt *Runtime) { rt.e.SetGCEnabled(on) }
}

// New creates a Runtime with the standard globals installed.
func New(opts ...Option) *Runtime {
	rt := &Runtime{e: engine.New(), namer: JSONFieldNamer}
	for _, o := range opts {
		if o != nil {
			o(rt)
		}
	}
	return rt
}

// Close releases the Runtime. Every Value produced by it becomes invalid.
//
// Go is garbage collected, so this is not the only way to reclaim the heap —
// dropping the last reference works too. Close is how you say "now", and how a
// later use becomes a reported error instead of an unnoticed one.
func (rt *Runtime) Close() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	rt.e = nil
	rt.mu.Unlock()
}

// ErrClosed is returned by a Runtime used after Close.
var ErrClosed = errors.New("goant: runtime is closed")

// engineOf returns the underlying engine, or an error if the Runtime is closed.
func (rt *Runtime) engineOf() (*engine.Runtime, error) {
	if rt == nil {
		return nil, ErrClosed
	}
	rt.mu.Lock()
	e := rt.e
	rt.mu.Unlock()
	if e == nil {
		return nil, ErrClosed
	}
	return e, nil
}

// --- running code -----------------------------------------------------------

// Program is a compiled script. Compiling once and running many times is the
// point of it: parsing and code generation happen here, and nothing about a
// global binding is resolved until the run, so one Program serves every Scope
// on its Runtime.
type Program struct {
	s    *engine.Script
	name string
}

// Name returns the source name the program was compiled under.
func (p *Program) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

// Source returns the text the program was compiled from.
func (p *Program) Source() string {
	if p == nil || p.s == nil {
		return ""
	}
	return p.s.Source()
}

// Compile parses and compiles src without running it. name is what appears in
// stack traces and error locations.
func (rt *Runtime) Compile(name, src string) (*Program, error) {
	e, err := rt.engineOf()
	if err != nil {
		return nil, err
	}
	s, cerr := e.CompileScript(name, src)
	if cerr != nil {
		return nil, rt.wrap(cerr)
	}
	return &Program{s: s, name: name}, nil
}

// RunProgram executes a compiled program and returns its completion value.
//
// Microtasks are not drained. A script whose result depends on a settled
// promise should go through Await, or call RunJobs first — the checkpoint is
// explicit so a host stays in control of when its callbacks run.
func (rt *Runtime) RunProgram(p *Program) (Value, error) {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}, err
	}
	if p == nil || p.s == nil {
		return Value{}, errors.New("goant: nil program")
	}
	v, rerr := e.RunScript(p.s)
	if rerr != nil {
		return Value{}, rt.wrap(rerr)
	}
	return rt.val(v), nil
}

// RunScript compiles and runs src, attributing it to name.
func (rt *Runtime) RunScript(name, src string) (Value, error) {
	p, err := rt.Compile(name, src)
	if err != nil {
		return Value{}, err
	}
	return rt.RunProgram(p)
}

// RunString compiles and runs src as an anonymous script.
func (rt *Runtime) RunString(src string) (Value, error) {
	return rt.RunScript("<eval>", src)
}

// RunFile reads path and runs it as a script.
func (rt *Runtime) RunFile(path string) (Value, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return Value{}, err
	}
	return rt.RunScript(path, string(src))
}

// RunModule runs src as an ES module: strict mode, its own scope, `this` is
// undefined, and import specifiers resolve against the module directory (see
// WithModuleDir). Top-level await is supported, and its jobs are drained before
// this returns.
func (rt *Runtime) RunModule(name, src string) (Value, error) {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}, err
	}
	v, rerr := e.RunModule(name, src)
	if rerr != nil {
		return Value{}, rt.wrap(rerr)
	}
	return rt.val(v), nil
}

// RunJobs drains the microtask and timer queues, running every callback that is
// ready. Promise reactions do not run until something calls this — or Await,
// which calls it for you.
func (rt *Runtime) RunJobs() error {
	e, err := rt.engineOf()
	if err != nil {
		return err
	}
	e.DrainJobs()
	return nil
}

// Await drains the job queue and resolves v if it is a promise.
//
// A non-promise comes back unchanged, so this is safe to wrap around any
// result: it is what you want after running a script whose top level may or may
// not be async. A rejected promise is reported as its rejection reason, in the
// same *Error a throw would have produced. A promise still pending once the
// queue is empty reports ErrPending — nothing left to run can settle it.
func (rt *Runtime) Await(v Value) (Value, error) {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}, err
	}
	e.DrainJobs()
	if !e.IsPromise(v.v) {
		return v, nil
	}
	state, res, ok := e.PromiseState(v.v)
	if !ok {
		return v, nil
	}
	switch state {
	case engine.PromiseFulfilled:
		return rt.val(res), nil
	case engine.PromiseRejected:
		return Value{}, rt.thrown(res)
	}
	return Value{}, ErrPending
}

// ErrPending is returned by Await for a promise no remaining job can settle —
// typically one waiting on host asynchrony the Runtime does not have.
var ErrPending = errors.New("goant: promise is still pending")

// --- globals ----------------------------------------------------------------

// Global returns the global object (globalThis).
func (rt *Runtime) Global() *Object {
	e, err := rt.engineOf()
	if err != nil {
		return nil
	}
	return rt.val(e.Global()).object()
}

// Get reads a global — a property of globalThis.
//
// A name that is not defined reads as undefined. Note that a top-level let,
// const or class declaration is a lexical binding rather than a property of
// globalThis, so it is not visible here; that is what JavaScript does, not a
// limitation of this API. A script that means to publish something should
// assign it (`globalThis.x = ...`) or declare it with var or function.
func (rt *Runtime) Get(name string) (Value, error) {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}, err
	}
	v, gerr := e.GetProp(e.Global(), name)
	if gerr != nil {
		return Value{}, rt.wrap(gerr)
	}
	return rt.val(v), nil
}

// Set defines a global. val may be any Go value ToValue accepts, including a
// function — which becomes callable from JavaScript.
func (rt *Runtime) Set(name string, val any) error {
	e, err := rt.engineOf()
	if err != nil {
		return err
	}
	v, cerr := rt.ToValue(val)
	if cerr != nil {
		return fmt.Errorf("goant: set %q: %w", name, cerr)
	}
	if serr := e.SetProp(e.Global(), name, v.v); serr != nil {
		return rt.wrap(serr)
	}
	return nil
}

// SetAll defines several globals, stopping at the first that fails. Names are
// applied in sorted order so a failure is reported on the same one every time.
func (rt *Runtime) SetAll(vals map[string]any) error {
	for _, k := range sortedNames(vals) {
		if err := rt.Set(k, vals[k]); err != nil {
			return err
		}
	}
	return nil
}

// --- value constructors -----------------------------------------------------

// Undefined returns the undefined primitive.
func (rt *Runtime) Undefined() Value {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}
	}
	return rt.val(e.Undefined())
}

// Null returns the null primitive.
func (rt *Runtime) Null() Value {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}
	}
	return rt.val(e.Null())
}

// NewObject returns an empty JavaScript object.
func (rt *Runtime) NewObject() *Object {
	e, err := rt.engineOf()
	if err != nil {
		return nil
	}
	return rt.val(e.NewObject()).object()
}

// NewArray returns a JavaScript array holding the converted values.
func (rt *Runtime) NewArray(vals ...any) (*Object, error) {
	e, err := rt.engineOf()
	if err != nil {
		return nil, err
	}
	ev := make([]engine.Value, len(vals))
	for i, v := range vals {
		cv, cerr := rt.ToValue(v)
		if cerr != nil {
			return nil, fmt.Errorf("goant: array element %d: %w", i, cerr)
		}
		ev[i] = cv.v
	}
	return rt.val(e.NewArray(ev...)).object(), nil
}

// NewError returns an Error object with the given message, for a host function
// that wants to throw something the script can catch and inspect.
func (rt *Runtime) NewError(msg string) Value {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}
	}
	return rt.val(e.NewError(msg))
}

// NewGoError wraps a Go error as a JavaScript Error carrying its message.
func (rt *Runtime) NewGoError(err error) Value {
	if err == nil {
		return rt.Undefined()
	}
	return rt.NewError(err.Error())
}

// NewBytes wraps b as a Uint8Array without copying it. The array reads and
// writes the caller's slice directly, so b must not be modified while the array
// is alive; pass a copy if that is not what you want.
func (rt *Runtime) NewBytes(b []byte) Value {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}
	}
	return rt.val(e.NewUint8Array(b))
}

// --- interruption -----------------------------------------------------------

// Interrupt stops whatever this Runtime is running, at the script's next check
// point. It is safe from any goroutine — which is the whole point, since the
// caller is usually a deadline on another one.
//
// The Runtime stays interrupted until ClearInterrupt. That is deliberate: a
// host that abandoned a script should not find the Runtime quietly running it
// again on the next call.
func (rt *Runtime) Interrupt() {
	if e, err := rt.engineOf(); err == nil {
		e.Interrupt()
	}
}

// ClearInterrupt clears a pending interruption so the Runtime can be used again.
func (rt *Runtime) ClearInterrupt() {
	if e, err := rt.engineOf(); err == nil {
		e.ClearInterrupt()
	}
}

// Interrupted reports whether an interruption is in flight or still pending.
func (rt *Runtime) Interrupted() bool {
	e, err := rt.engineOf()
	return err == nil && e.Interrupted()
}

// WithContext ties ctx to this Runtime: when ctx is done — cancelled or past
// its deadline — the Runtime is interrupted and the running script stops. The
// returned function detaches the watcher and must be called.
//
//	stop := rt.WithContext(ctx)
//	defer stop()
//	v, err := rt.RunProgram(p)   // errors.Is(err, ErrInterrupted) on timeout
//
// The interruption is left set, so a caller can tell a stopped script from one
// that failed on its own. Call ClearInterrupt before reusing the Runtime.
func (rt *Runtime) WithContext(ctx context.Context) (stop func()) {
	if rt == nil || ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-ctx.Done():
			rt.Interrupt()
		case <-done:
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// --- memory -----------------------------------------------------------------

// Stats reports a Runtime's own memory use — not the process's. An embedder
// running many Runtimes needs to know which one is growing, and Go's
// process-wide MemStats cannot tell it.
type Stats struct {
	// Cells is the number of live heap cells: objects, strings, symbols,
	// closures and bigints.
	Cells int

	// Bytes is what those cells occupy. It counts the cells and their headers,
	// not the payloads hanging off them (string bytes, array backing stores),
	// so it is a floor rather than a total — but it tracks what a script has
	// allocated and not released, which is the number worth retiring on.
	Bytes uint64

	// Interned is how many strings are pinned in the intern table. The table is
	// permanent, so this only rises; watching it separates memory that is merely
	// uncollected from memory that can never be freed. Host data never lands
	// here — only names and literals do.
	Interned int

	// Collections is how many times the collector has run.
	Collections int

	// Limit is the memory limit in bytes, or 0 if there is none.
	Limit uint64
}

// Stats reports this Runtime's memory use.
func (rt *Runtime) Stats() Stats {
	e, err := rt.engineOf()
	if err != nil {
		return Stats{}
	}
	cells, bytes := e.HeapUsage()
	return Stats{
		Cells:       cells,
		Bytes:       bytes,
		Interned:    e.InternedCount(),
		Collections: e.GCCycles(),
		Limit:       e.HeapLimit(),
	}
}

// SetMemoryLimit changes the live-heap budget after construction. See
// WithMemoryLimit.
func (rt *Runtime) SetMemoryLimit(bytes uint64) {
	if e, err := rt.engineOf(); err == nil {
		e.SetHeapLimit(bytes)
	}
}

// Collect runs a garbage collection now. The collector runs on its own during
// execution; this is for a host that wants to reclaim at a known point — after
// a large job, before measuring, or before parking a pooled Runtime.
func (rt *Runtime) Collect() {
	if e, err := rt.engineOf(); err == nil {
		e.Collect()
	}
}

// --- internals --------------------------------------------------------------

// val pairs an engine value with this Runtime.
func (rt *Runtime) val(v engine.Value) Value { return Value{v: v, rt: rt} }

// fieldNamer returns the configured namer, defaulting for a zero Runtime.
func (rt *Runtime) fieldNamer() FieldNamer {
	if rt == nil || rt.namer == nil {
		return JSONFieldNamer
	}
	return rt.namer
}
