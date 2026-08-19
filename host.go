package goant

// Host asynchrony: the API a program uses when the answer comes from somewhere
// the Runtime cannot see.
//
// A Runtime on its own is a closed world. Everything it awaits, it also
// produces: a promise settles because some other JavaScript settled it, and a
// timer fires because the loop decided it was that task's turn. Embedding one
// in a Go program means the interesting answers arrive from outside it — an
// HTTP response, a file read, a subprocess exiting — on another goroutine, at a
// real time, long after the call that promised them returned.
//
// Three pieces cover that, and they are meant to be used together:
//
//	p, resolve, _ := rt.NewPromise()      // hand JavaScript a promise now
//	go func() {
//		body, err := http.Get(url)        // ... work on another goroutine ...
//		if err != nil { reject(err); return }
//		resolve(body)                     // ... and settle it from there
//	}()
//	return p
//
// then, once the script has been started, rt.RunLoop(ctx) drives the loop until
// everything it is waiting for has arrived — including the work above, which
// neither queue knows about but the Runtime is counting.

import (
	"context"
	"fmt"
	"sync"

	"github.com/robomotionio/goant/internal/engine"
)

// ModuleResolver answers where an import specifier lives.
//
// It receives the specifier exactly as written and the path of the module doing
// the importing (empty at an entry point), and returns the module's source and
// the path to key it under. Returning an empty source means "the file at that
// path", so a resolver can rewrite specifiers without also taking over reading
// them; returning source is how a module that is not a file at all — an embedded
// bundle entry, a generated shim — gets loaded.
//
// The path is the registry key. Two importers whose specifiers resolve to the
// same path share one module instance, which is what makes a shared dependency
// shared rather than duplicated; a resolver that returns a different path each
// time will load a fresh copy each time.
type ModuleResolver func(specifier, referrer string) (source, path string, err error)

// WithModuleResolver installs the host's module resolution.
//
// Without one, a specifier is resolved as a path relative to the importing
// module. That covers relative imports and nothing else: a bare
// `import "@scope/pkg"` becomes a file beside the importer, which is not there.
// What a bare specifier means is a property of the host — a node_modules layout,
// an import map, a bundle compiled into the binary — so the host is asked.
func WithModuleResolver(r ModuleResolver) Option {
	return func(rt *Runtime) {
		if r == nil {
			return
		}
		rt.e.SetModuleResolver(func(spec, referrer string) (string, string, error) {
			return r(spec, referrer)
		})
	}
}

// WithRealTimers switches the Runtime from the virtual clock to the wall clock.
//
// Under the default virtual clock a delay only orders callbacks: setTimeout(f,
// 5000) runs f as soon as everything shorter has run, and no time passes. That
// is what a deterministic test wants, and it is wrong for anything whose delay
// means something — a retry backoff that retries instantly, a poll interval
// that spins, a timeout that fires before the request it was racing.
//
// With this on, a timer fires when its delay has actually elapsed and the loop
// sleeps in between. Note that it makes the loop blocking: RunJobs on a Runtime
// with a five-second timer pending takes five seconds. That is the point, but it
// is a change in what the call costs.
func WithRealTimers(on bool) Option {
	return func(rt *Runtime) { rt.e.SetRealTimers(on) }
}

// NewPromise returns a pending promise and the two functions that settle it.
//
// This is how a host function returns an answer it does not have yet: create
// the promise, hand it to JavaScript, and settle it from whichever goroutine
// eventually produces the value.
//
//	rt.Set("readFile", func(path string) (Value, error) {
//		p, resolve, reject := rt.NewPromise()
//		go func() {
//			b, err := os.ReadFile(path)
//			if err != nil { reject(err); return }
//			resolve(string(b))
//		}()
//		return p, nil
//	})
//
// Call NewPromise on the Runtime's own goroutine — inside the host function,
// as above. The settle functions are the exception: they are safe from any
// goroutine, because that is what they are for. Each schedules the settlement
// onto the Runtime's queue rather than performing it, so the value is converted
// where every other conversion happens; nothing takes effect until the loop next
// runs, which means RunLoop, RunJobs or Await.
//
// Only the first of the two to be called has any effect, as with the executor's
// resolve and reject. Both report an error if the Runtime has been closed, and
// resolve reports one for a value it cannot convert — in which case the promise
// is rejected with a TypeError, since a promise that was handed out and then
// silently never settled is the worst of the available outcomes.
//
// Until it settles, the promise is held: the loop stays alive waiting for it and
// the collector will not sweep it, even if JavaScript has dropped every
// reference. An abandoned promise therefore keeps a RunLoop from returning,
// which is a leak with a clear cause — settle it, on the failure path too.
func (rt *Runtime) NewPromise() (p Value, resolve, reject func(any) error) {
	e, err := rt.engineOf()
	if err != nil {
		fail := func(any) error { return err }
		return Value{}, fail, fail
	}
	pv, res, rej := e.NewHostPromise()
	var once sync.Once
	settle := func(ok bool) func(any) error {
		return func(x any) error {
			var outcome error
			called := false
			once.Do(func() {
				called = true
				outcome = rt.Post(func() {
					v, cerr := rt.ToValue(x)
					if cerr != nil {
						// The host meant to fulfil and cannot. Rejecting says so
						// inside JavaScript; the error below says so in Go.
						rej(e.NewError(fmt.Sprintf("goant: cannot convert settled value: %v", cerr)))
						return
					}
					if ok {
						res(v.v)
					} else {
						rej(rt.toRejection(x, v))
					}
				})
			})
			if !called {
				return ErrSettled
			}
			return outcome
		}
	}
	return rt.val(pv), settle(true), settle(false)
}

// toRejection picks what a rejection actually throws. A Go error rejects with a
// JavaScript Error carrying its message, which is what a script's catch expects;
// anything else rejects with itself, because a host rejecting with a value has
// chosen that value deliberately.
func (rt *Runtime) toRejection(x any, converted Value) engine.Value {
	if err, ok := x.(error); ok {
		return rt.NewGoError(err).v
	}
	return converted.v
}

// ErrSettled is returned by a settle function called after the promise has
// already been settled — by the other function, or by an earlier call to this
// one. It is informational: nothing changed, and nothing was going to.
var ErrSettled = fmt.Errorf("goant: promise is already settled")

// Post schedules fn to run on the Runtime's goroutine, at the next point the
// event loop is between jobs.
//
// It is safe from any goroutine, and it is the only thing about a Runtime that
// is apart from Interrupt. A Go callback that touches the heap from elsewhere —
// setting a global, resolving a promise, calling a JavaScript function — is a
// data race, whatever it looks like; this is how such work gets to the right
// goroutine.
//
// Posting does not by itself keep the loop alive: a job posted while nothing is
// running waits for the next RunLoop, RunJobs or Await. Reports an error once
// the Runtime is closed, so a goroutine finishing after shutdown learns that its
// answer went nowhere.
func (rt *Runtime) Post(fn func()) error {
	e, err := rt.engineOf()
	if err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("goant: nil job")
	}
	if perr := e.Post(fn); perr != nil {
		return ErrClosed
	}
	return nil
}

// HostRef tells the Runtime that a host operation is in flight, so RunLoop
// keeps running until it lands. NewPromise does this for you; use it directly
// for work that will finish by posting rather than by settling a promise — a
// background write, a subscription delivering into a callback.
//
// Every HostRef needs exactly one HostUnref, on the failure path too. An
// unbalanced ref is a loop that never returns; an early unref is a loop that
// returns while the answer is still in the post.
func (rt *Runtime) HostRef() {
	if e, err := rt.engineOf(); err == nil {
		e.HostRef()
	}
}

// HostUnref records that an in-flight host operation is done. See HostRef.
func (rt *Runtime) HostUnref() {
	if e, err := rt.engineOf(); err == nil {
		e.HostUnref()
	}
}

// UnrefTimer marks a timer as not keeping the loop alive, and RefTimer undoes
// it. This is Node's timer.unref(), and it exists for one shape: a watchdog
// armed alongside some work, which must fire if the loop is running and must not
// be the reason it keeps running.
//
// id is what setTimeout or setInterval returned. An id that no longer names a
// live timer is ignored — a timer that has fired or been cleared has nothing
// left to unref.
func (rt *Runtime) UnrefTimer(id float64) {
	if e, err := rt.engineOf(); err == nil {
		e.UnrefTimer(id, true)
	}
}

// RefTimer restores a timer's hold on the loop. See UnrefTimer.
func (rt *Runtime) RefTimer(id float64) {
	if e, err := rt.engineOf(); err == nil {
		e.UnrefTimer(id, false)
	}
}

// RunLoop drives the event loop until nothing more can happen, or ctx is done.
//
// "Nothing more" means more than an empty queue once a host is involved: no
// microtasks, no timers, no posted jobs, and no promise from NewPromise still
// waiting to be settled. A program with a request in flight has empty queues and
// is not finished, and this is the call that knows the difference — which is why
// it, rather than RunJobs, is what an embedder with host asynchrony should run.
//
// A cancelled or expired ctx stops the loop between jobs and is reported as
// ctx.Err(). That does NOT stop a script already running: a callback in an
// endless loop is stopped by Interrupt (or by WithContext, which does both).
func (rt *Runtime) RunLoop(ctx context.Context) error {
	e, err := rt.engineOf()
	if err != nil {
		return err
	}
	if err := e.RunLoop(ctx); err != nil {
		return rt.wrap(err)
	}
	return nil
}

// AwaitContext is Await with a deadline and host asynchrony: it drives the loop
// until v settles, ctx is done, or nothing is left that could settle it.
//
// Await is the right call for a promise the script itself will settle. This is
// the right one for a promise a host operation will settle, because it keeps the
// loop running while that operation is outstanding instead of concluding the
// queues are empty.
func (rt *Runtime) AwaitContext(ctx context.Context, v Value) (Value, error) {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}, err
	}
	if !e.IsPromise(v.v) {
		if err := rt.RunLoop(ctx); err != nil {
			return Value{}, err
		}
		return v, nil
	}
	if err := rt.RunLoop(ctx); err != nil {
		return Value{}, err
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
