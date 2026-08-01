package goant

import (
	"context"
	"errors"
	"sync"

	"github.com/robomotionio/goant/internal/engine"
)

// Scope is one unit of work with its own global state.
//
// A host that runs many small independent scripts — a message arrives, a
// function transforms it, bytes go out — needs each run isolated from the last.
// The obvious way to get that is a fresh Runtime per run, and it is what an
// embedder inherits from engines whose contexts are built from a heap snapshot.
// goant has no snapshot, so a Runtime means constructing every prototype and
// every built-in from nothing: measured at 366 µs, against roughly 6 µs for the
// work a short script actually does. The isolation cost sixty times the job.
//
// Almost none of it needs isolating. The built-ins are identical every time and
// a script that does not modify them cannot tell whether they were rebuilt.
// What genuinely differs per run is what the script installs: properties on
// globalThis, top-level declarations, top-level let and const.
//
// So a Scope keeps the Runtime and gives the run a fresh global object whose
// prototype is the shared one. Built-ins resolve up the chain; anything the
// script assigns lands on the fresh object and goes away at Close. Measured at
// 111 ns — about three thousand times cheaper — and Close reclaims the whole
// run's memory in one step, without tracing anything, because a run allocates a
// graph, produces a result, and everything it made dies together.
//
// Two things follow from sharing rather than copying:
//
// One Scope at a time per Runtime, and no nesting. That is how a pooled host
// already works — a lease per in-flight call — and it is the same constraint
// every engine places on an isolate anyway.
//
// And the shared built-ins are not protected, only watched. A script that
// modifies Array.prototype changes it for the next run too. Polluted reports
// that; such a Runtime must be discarded rather than reused, which is what Pool
// does automatically.
type Scope struct {
	rt  *Runtime
	inv *engine.Invocation

	closed   bool
	polluted bool
}

// NewScope begins a scope on rt. Close it when the run is finished.
//
//	s, err := rt.NewScope()
//	defer s.Close()
//
//	s.Set("input", msg)
//	v, err := s.RunProgram(prog)
//	out, _, err := v.AppendJSON(buf[:0])   // read what you need BEFORE Close
func (rt *Runtime) NewScope() (*Scope, error) {
	e, err := rt.engineOf()
	if err != nil {
		return nil, err
	}
	return &Scope{rt: rt, inv: e.BeginInvocation()}, nil
}

// Runtime returns the Runtime the scope runs on.
func (s *Scope) Runtime() *Runtime {
	if s == nil {
		return nil
	}
	return s.rt
}

// Close ends the scope and, when it is safe, frees everything the run
// allocated — in one step, without tracing.
//
// EVERY Value the scope produced becomes invalid, including the script's
// result. Read what you need first: serialize it, export it, copy it out.
// Reading a Value afterwards reads a recycled cell, which is the one way to get
// a wrong answer out of this API.
//
// Reclamation is skipped when the script wrote to state that predates the
// scope, since something outside the freed region could then point into it.
// Polluted reports that, and such a Runtime should be discarded.
//
// Calling Close twice is harmless.
func (s *Scope) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	s.polluted = s.inv.Dirty()
	s.inv.Release()
}

// Polluted reports whether the run modified state that predates the scope — a
// built-in prototype, most likely.
//
// A Runtime whose scope reports true must not be reused: the next run would
// inherit the change, and its memory could not be reclaimed either. Valid
// before and after Close.
func (s *Scope) Polluted() bool {
	if s == nil {
		return false
	}
	if s.closed {
		return s.polluted
	}
	return s.inv.Dirty()
}

// Global returns the scope's global object — the fresh one, not the Runtime's.
func (s *Scope) Global() *Object {
	if s == nil {
		return nil
	}
	return s.rt.Global()
}

// Get reads a global visible in this scope, which includes the shared ones.
func (s *Scope) Get(name string) (Value, error) {
	if s == nil {
		return Value{}, errors.New("goant: nil scope")
	}
	return s.rt.Get(name)
}

// Set defines a global on the scope's own global object, so it goes away at
// Close rather than accumulating across runs.
func (s *Scope) Set(name string, val any) error {
	if s == nil {
		return errors.New("goant: nil scope")
	}
	return s.rt.Set(name, val)
}

// SetAll defines several scope globals, stopping at the first that fails.
func (s *Scope) SetAll(vals map[string]any) error {
	if s == nil {
		return errors.New("goant: nil scope")
	}
	return s.rt.SetAll(vals)
}

// RunProgram runs a compiled program in this scope. Compiling once on the
// Runtime and running per scope is the pattern this is built for: nothing about
// a global binding is resolved at compile time, so one Program serves every
// scope.
func (s *Scope) RunProgram(p *Program) (Value, error) {
	if s == nil {
		return Value{}, errors.New("goant: nil scope")
	}
	return s.rt.RunProgram(p)
}

// RunString compiles and runs src in this scope.
func (s *Scope) RunString(src string) (Value, error) {
	if s == nil {
		return Value{}, errors.New("goant: nil scope")
	}
	return s.rt.RunString(src)
}

// RunScript compiles and runs src in this scope, attributing it to name.
func (s *Scope) RunScript(name, src string) (Value, error) {
	if s == nil {
		return Value{}, errors.New("goant: nil scope")
	}
	return s.rt.RunScript(name, src)
}

// RunJobs drains the job queue. See Runtime.RunJobs.
func (s *Scope) RunJobs() error {
	if s == nil {
		return errors.New("goant: nil scope")
	}
	return s.rt.RunJobs()
}

// Await drains the job queue and resolves a promise. See Runtime.Await.
func (s *Scope) Await(v Value) (Value, error) {
	if s == nil {
		return Value{}, errors.New("goant: nil scope")
	}
	return s.rt.Await(v)
}

// --- pooling ----------------------------------------------------------------

// Pool keeps warm Runtimes and hands each job a fresh Scope on one.
//
// This is the shape a server wants: building a Runtime is the expensive part
// and running a script is not, so the Runtime is reused and the isolation comes
// from the Scope. Pool does the bookkeeping that goes with that — leasing,
// deadlines, retiring a Runtime that has grown too large or been polluted — so
// a host does not have to write it again.
//
//	pool := goant.NewPool(goant.PoolConfig{
//	    New: func() (*goant.Runtime, error) {
//	        rt := goant.New(goant.WithMemoryLimit(1 << 30))
//	        return rt, rt.Set("log", log.Println)
//	    },
//	    MaxUses:   50_000,
//	    MaxMemory: 512 << 20,
//	})
//	defer pool.Close()
//
//	var out []byte
//	err := pool.Do(ctx, func(s *goant.Scope) error {
//	    s.Set("input", msg)
//	    v, err := s.RunProgram(prog)
//	    if err != nil {
//	        return err
//	    }
//	    out, _, err = v.AppendJSON(nil)
//	    return err
//	})
//
// A Pool is safe for concurrent use. Each job gets its own Runtime for its
// duration, so jobs never share one.
type Pool struct {
	cfg PoolConfig

	mu     sync.Mutex
	free   []*leased
	closed bool

	// live counts Runtimes the pool has built and not yet retired, including
	// those currently leased out.
	live int
}

// PoolConfig configures a Pool. The zero value is usable: it builds plain
// Runtimes and never retires them.
type PoolConfig struct {
	// New builds and configures a Runtime — installing host functions,
	// setting limits, whatever the job needs. It is called on demand, from
	// whichever goroutine needed a Runtime. Leave it nil for a plain
	// goant.New().
	New func() (*Runtime, error)

	// MaxUses retires a Runtime after this many jobs. Nothing here leaks per
	// job, so this is a belt-and-braces bound rather than a necessity; 0 means
	// no limit.
	MaxUses int

	// MaxMemory retires a Runtime whose live heap exceeds this many bytes when
	// a job finishes. It bounds what a pooled Runtime carries between jobs,
	// which is not the same as bounding a single job — for that, set a memory
	// limit on the Runtime in New. 0 means no limit.
	MaxMemory uint64

	// MaxIdle caps how many Runtimes are kept warm. Beyond it, a finished
	// Runtime is dropped rather than parked. 0 means no cap.
	MaxIdle int
}

type leased struct {
	rt   *Runtime
	uses int
}

// NewPool creates a Pool.
func NewPool(cfg PoolConfig) *Pool { return &Pool{cfg: cfg} }

// ErrPoolClosed is returned by Do after Close.
var ErrPoolClosed = errors.New("goant: pool is closed")

// Do leases a Runtime, opens a Scope on it, and runs fn.
//
// The Scope is closed when fn returns, which frees everything the job
// allocated. fn must therefore read out whatever it needs before returning —
// serialize the result, export it, copy it. A Value that outlives the call is
// not valid.
//
// ctx bounds the job: if it is done before or during the run, the script is
// interrupted and the error is ctx.Err(). The Runtime is then retired rather
// than reused, since it was abandoned mid-script.
func (p *Pool) Do(ctx context.Context, fn func(*Scope) error) error {
	if p == nil {
		return errors.New("goant: nil pool")
	}
	if fn == nil {
		return errors.New("goant: nil job")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	l, err := p.acquire()
	if err != nil {
		return err
	}

	// A Runtime goes back to the pool only if the job finished cleanly on it.
	// reusable starts false so that a panic escaping fn — which reaches neither
	// the assignment below nor any of the checks before it — retires the
	// Runtime rather than returning one of unknown state to the next job.
	reusable := false
	defer func() {
		l.uses++
		if !reusable || p.shouldRetire(l) {
			p.discard(l)
			return
		}
		p.release(l)
	}()

	stop := l.rt.WithContext(ctx)
	defer stop()

	scope, serr := l.rt.NewScope()
	if serr != nil {
		return serr
	}
	// Deferred so a panic in the job cannot leave the scope open, which would
	// strand the Runtime's globals in the abandoned run's state. Polluted stays
	// readable after Close.
	defer scope.Close()

	jobErr := fn(scope)
	scope.Close()

	// A context that fired is the real reason, whatever the script reported on
	// the way out: the caller asked for the deadline and should see it.
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	reusable = !scope.Polluted() && !l.rt.Interrupted() &&
		!errors.Is(jobErr, ErrMemoryLimit) && !errors.Is(jobErr, ErrInterrupted)
	return jobErr
}

// Close discards every pooled Runtime. Jobs already running are unaffected;
// their Runtimes are retired when they finish.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	free := p.free
	p.free = nil
	p.closed = true
	p.mu.Unlock()
	for _, l := range free {
		l.rt.Close()
	}
}

// PoolStats reports what a Pool is holding.
type PoolStats struct {
	// Idle is how many Runtimes are parked and ready.
	Idle int
	// Live is how many exist, parked or leased.
	Live int
}

// Stats reports the pool's occupancy.
func (p *Pool) Stats() PoolStats {
	if p == nil {
		return PoolStats{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{Idle: len(p.free), Live: p.live}
}

func (p *Pool) acquire() (*leased, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	if n := len(p.free); n > 0 {
		l := p.free[n-1]
		p.free = p.free[:n-1]
		p.mu.Unlock()
		// A Runtime is never parked while interrupted, but clearing costs
		// nothing and makes a stray Interrupt from a previous job's watcher
		// harmless rather than fatal to the next one.
		l.rt.ClearInterrupt()
		return l, nil
	}
	p.live++
	p.mu.Unlock()

	rt, err := p.build()
	if err != nil {
		p.mu.Lock()
		p.live--
		p.mu.Unlock()
		return nil, err
	}
	return &leased{rt: rt}, nil
}

func (p *Pool) build() (*Runtime, error) {
	if p.cfg.New == nil {
		return New(), nil
	}
	rt, err := p.cfg.New()
	if err != nil {
		return nil, err
	}
	if rt == nil {
		return nil, errors.New("goant: PoolConfig.New returned a nil Runtime")
	}
	return rt, nil
}

func (p *Pool) shouldRetire(l *leased) bool {
	if p.cfg.MaxUses > 0 && l.uses >= p.cfg.MaxUses {
		return true
	}
	if p.cfg.MaxMemory > 0 && l.rt.Stats().Bytes > p.cfg.MaxMemory {
		return true
	}
	return false
}

func (p *Pool) release(l *leased) {
	p.mu.Lock()
	if p.closed || (p.cfg.MaxIdle > 0 && len(p.free) >= p.cfg.MaxIdle) {
		p.live--
		p.mu.Unlock()
		l.rt.Close()
		return
	}
	p.free = append(p.free, l)
	p.mu.Unlock()
}

func (p *Pool) discard(l *leased) {
	p.mu.Lock()
	p.live--
	p.mu.Unlock()
	l.rt.Close()
}
