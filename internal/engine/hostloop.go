package engine

// The host half of the event loop.
//
// Everything the loop ran until now was work JavaScript had already created:
// a promise reaction, a timer the script itself scheduled. That is enough for a
// program whose asynchrony is internal, and not enough for anything that waits
// on the world — a fetch, a file read, a subprocess. Those finish on some other
// goroutine, at some real time, and the answer has to reach a promise that was
// handed to JavaScript long before.
//
// Three things make that possible, and all three are here:
//
//   - an external job queue a second goroutine may push onto (Post), because the
//     Runtime itself is single-goroutine and a host callback must not touch the
//     heap directly;
//   - a live-work count (HostRef/HostUnref), because "no jobs left" no longer
//     means "nothing more can happen" — a request in flight will produce a job
//     that does not exist yet;
//   - a real clock (SetRealTimers), because a setTimeout that orders tasks but
//     never elapses is fine for a deterministic script and wrong for a retry
//     backoff.
//
// The virtual-clock loop stays exactly as it was, and is still the default. A
// host opts into this one.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// hostQueue is the cross-goroutine half of the Runtime, alongside interruptState.
//
// It is a separate struct, held by pointer and shared with every realm of the
// isolate, for the same reason the interrupt flag is: there is one loop, and a
// host that posts to a realm has posted to the loop.
type hostQueue struct {
	mu     sync.Mutex
	jobs   []func()
	closed bool

	// wake is how a posting goroutine tells a sleeping loop it has work. Buffered
	// by one and written non-blockingly: a wake that finds the buffer full has
	// already been delivered, since the loop re-checks the queue after every
	// wake it receives.
	wake chan struct{}

	// refs counts host operations that have started and not yet posted their
	// result. It is what keeps the loop from concluding it is idle while a
	// goroutine is still on its way back with an answer.
	refs atomic.Int64
}

func newHostQueue() *hostQueue {
	return &hostQueue{wake: make(chan struct{}, 1)}
}

// signal wakes a sleeping loop, without blocking if it is already awake.
func (h *hostQueue) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// Post schedules fn to run on the Runtime's own goroutine, at the next point the
// loop is between jobs. It is safe from any goroutine — that is the whole point
// of it — and it is the ONLY safe way for another goroutine to reach a Runtime.
//
// A closure passed here must not capture a Value: the collector cannot see
// inside a func, and a Value the queue is holding on behalf of a job that has
// not run yet is a Value nothing is keeping alive. Capture Go data, convert it
// inside fn, or root what you must keep with HoldValue.
//
// Returns ErrHostClosed once the Runtime has been closed, so a goroutine
// finishing after shutdown learns that its answer went nowhere instead of
// blocking forever or panicking.
func (rt *Runtime) Post(fn func()) error {
	if rt == nil || rt.host == nil || fn == nil {
		return ErrHostClosed
	}
	h := rt.host
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrHostClosed
	}
	h.jobs = append(h.jobs, fn)
	h.mu.Unlock()
	h.signal()
	return nil
}

// HostRef records that a host operation is in flight, so the loop stays alive
// waiting for it. Every HostRef must be matched by exactly one HostUnref,
// including on the failure path — an unbalanced ref hangs the loop until its
// context is cancelled, which is a deadlock wearing a timeout.
func (rt *Runtime) HostRef() {
	if rt != nil && rt.host != nil {
		rt.host.refs.Add(1)
	}
}

// HostUnref records that an in-flight host operation is done.
func (rt *Runtime) HostUnref() {
	if rt == nil || rt.host == nil {
		return
	}
	if rt.host.refs.Add(-1) <= 0 {
		// The loop may be asleep with nothing else to wait for. Waking it lets it
		// discover it is idle and return, rather than sleeping out its deadline.
		rt.host.signal()
	}
}

// HostPending reports whether anything outside JavaScript is still expected:
// a posted job not yet run, or an operation that has taken a ref.
func (rt *Runtime) HostPending() bool {
	if rt == nil || rt.host == nil {
		return false
	}
	h := rt.host
	if h.refs.Load() > 0 {
		return true
	}
	h.mu.Lock()
	n := len(h.jobs)
	h.mu.Unlock()
	return n > 0
}

// CloseHost stops the queue accepting work and drops what it is holding. A
// later Post reports ErrHostClosed rather than queueing into a Runtime that
// will never run again.
func (rt *Runtime) CloseHost() {
	if rt == nil || rt.host == nil {
		return
	}
	h := rt.host
	h.mu.Lock()
	h.closed = true
	h.jobs = nil
	h.mu.Unlock()
	h.signal()
}

// runExternalJobs runs everything posted so far, and only that: a job that posts
// another does not extend this pass, so one slow producer cannot starve the
// timers. Reports whether it ran anything.
func (rt *Runtime) runExternalJobs() bool {
	if rt.host == nil {
		return false
	}
	h := rt.host
	h.mu.Lock()
	batch := h.jobs
	h.jobs = nil
	h.mu.Unlock()
	if len(batch) == 0 {
		return false
	}
	for _, fn := range batch {
		if rt.exitCode != nil {
			return true
		}
		fn()
		rt.drainMicrotasks()
	}
	return true
}

// --- host roots --------------------------------------------------------------

// HoldValue roots v until the returned function is called.
//
// A promise handed to JavaScript and then dropped by it — `void fetch(url)` —
// has no reference the collector can find, and the answer still has to arrive
// somewhere. This is how the host says "I am holding this", and it is a
// reference count rather than a flag because the same value may be held twice.
//
// Call it on the Runtime's goroutine, like everything else that touches the
// heap. The release function has the same rule; post it if you are elsewhere.
func (rt *Runtime) HoldValue(v Value) (release func()) {
	if rt == nil {
		return func() {}
	}
	if rt.hostRoots == nil {
		rt.hostRoots = map[Value]int{}
	}
	rt.hostRoots[v]++
	var once sync.Once
	return func() {
		once.Do(func() {
			if n := rt.hostRoots[v]; n <= 1 {
				delete(rt.hostRoots, v)
			} else {
				rt.hostRoots[v] = n - 1
			}
		})
	}
}

// --- host promises -----------------------------------------------------------

// NewHostPromise returns a pending promise and the two functions that settle it.
//
// The promise is rooted and the loop is ref'd from here until one of them runs,
// so a host may hand the promise to JavaScript, forget it, and still settle it
// later. Settling releases both.
//
// Call this on the Runtime's goroutine — typically from inside the host function
// that is returning the promise. The settle functions have the same rule: reach
// them from another goroutine through Post.
func (rt *Runtime) NewHostPromise() (promise Value, resolve, reject func(Value)) {
	p, o := rt.makePromise()
	release := rt.HoldValue(p)
	rt.HostRef()
	var once sync.Once
	settle := func(fn func(Value)) func(Value) {
		return func(v Value) {
			once.Do(func() {
				fn(v)
				release()
				rt.HostUnref()
			})
		}
	}
	return p,
		settle(func(v Value) { rt.resolvePromise(p, o, v) }),
		settle(func(v Value) { rt.rejectPromise(o, v) })
}

// --- the real clock ----------------------------------------------------------

// SetRealTimers switches the loop from the virtual clock to the wall clock: a
// timer fires when its delay has actually elapsed, and the loop sleeps in
// between instead of running the queue down as fast as it can.
//
// Off by default, because determinism is worth more to a test than elapsed time
// is, and a script that only orders its callbacks gets the same answer either
// way. It is worth turning on for anything that waits on the world — a poll
// interval, a retry backoff, a timeout racing a request — where firing
// immediately means not waiting at all.
func (rt *Runtime) SetRealTimers(on bool) { rt.realTimers = on }

// RealTimers reports whether the wall clock is in use.
func (rt *Runtime) RealTimers() bool { return rt.realTimers }

// RunLoop drives the event loop until there is nothing left that could produce
// more work — no microtasks, no timers, no posted jobs, no host operation in
// flight — or until ctx is done.
//
// This is DrainJobs for a host that has asynchrony of its own. The difference is
// what "nothing left" means: with a fetch outstanding, the queues are empty and
// the program is very much not finished, and only the ref count knows.
func (rt *Runtime) RunLoop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	prev := rt.loopCtx
	rt.loopCtx = ctx
	defer func() { rt.loopCtx = prev }()
	for {
		// Whichever clock is in use: this drains everything runnable now, and
		// under real timers it also waits for a timer that is not due yet.
		rt.runEventLoop()
		if rt.exitCode != nil || rt.Interrupted() {
			break
		}
		if err := ctx.Err(); err != nil {
			break
		}
		if !rt.HostPending() {
			return nil
		}
		// Empty queues with a host operation outstanding: the program is not
		// finished, it is waiting. Waiting is the whole difference between this
		// and DrainJobs, and it is why the ref count exists.
		rt.waitForWork(ctx.Done(), time.Time{}, false)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if rt.Interrupted() {
		return ErrTerminated
	}
	return nil
}

// loopContext is the context the current RunLoop is bounded by, or Background
// when the loop was entered any other way.
func (rt *Runtime) loopContext() context.Context {
	if rt.loopCtx != nil {
		return rt.loopCtx
	}
	return context.Background()
}

// runRealEventLoop is runEventLoop against the wall clock and the host queue.
//
// The shape is the same — microtasks, then the earliest macrotask, then
// microtasks again — with two differences that only a real clock creates: a
// timer that is not due yet is not runnable, so the loop has to wait rather than
// fire it; and waiting is only correct if something can wake it, which is what
// the ref count and the wake channel are for.
func (rt *Runtime) runRealEventLoop() {
	ctx := rt.loopContext()
	done := ctx.Done()
	for {
		if rt.exitCode != nil || rt.Interrupted() {
			return
		}
		select {
		case <-done:
			return
		default:
		}

		rt.runExternalJobs()
		rt.drainMicrotasks()
		if rt.exitCode != nil || rt.Interrupted() {
			return
		}
		if rt.settleReadyAsyncWaits() {
			rt.drainMicrotasks()
			continue
		}
		if rt.fireDueTimers() {
			continue
		}

		next, hasTimer := rt.nextTimerDeadline()
		if !hasTimer && !rt.HostPending() {
			// Genuinely out of work — except for the one kind of work that lives
			// on neither queue.
			if rt.serviceAsyncWaits() {
				rt.drainMicrotasks()
				continue
			}
			return
		}
		rt.waitForWork(done, next, hasTimer)
	}
}

// fireDueTimers runs every timer whose deadline has passed, earliest first,
// draining microtasks after each. Reports whether it fired anything.
//
// One pass, re-picking the earliest each time: a callback may schedule, cancel
// or re-arm timers, so the list it was chosen from is not the list that is there
// afterwards.
func (rt *Runtime) fireDueTimers() bool {
	fired := false
	for {
		if rt.exitCode != nil || rt.Interrupted() {
			return fired
		}
		now := time.Now()
		best := -1
		for i := range rt.macrotasks {
			t := &rt.macrotasks[i]
			if t.cancelled || t.deadline.After(now) {
				continue
			}
			if best < 0 || t.deadline.Before(rt.macrotasks[best].deadline) ||
				(t.deadline.Equal(rt.macrotasks[best].deadline) && t.seq < rt.macrotasks[best].seq) {
				best = i
			}
		}
		if best < 0 {
			rt.dropCancelledTimers()
			return fired
		}
		t := rt.macrotasks[best]
		if t.period > 0 {
			// setInterval re-arms from the deadline it was due at, not from now, so
			// a callback that takes longer than the period does not push the whole
			// series later and later. A run that fell far behind is not replayed:
			// the next deadline is at least one period from now.
			rt.timerSeq++
			d := t.deadline.Add(time.Duration(t.period) * time.Millisecond)
			if !d.After(now) {
				d = now.Add(time.Duration(t.period) * time.Millisecond)
			}
			rt.macrotasks[best].deadline = d
			rt.macrotasks[best].seq = rt.timerSeq
		} else {
			rt.macrotasks = append(rt.macrotasks[:best], rt.macrotasks[best+1:]...)
		}
		hold := rt.holdInFlight(t.args, t.fn)
		rt.callValue(t.fn, mkundef(), t.args)
		hold()
		rt.drainMicrotasks()
		rt.runExternalJobs()
		fired = true
	}
}

// dropCancelledTimers removes cleared timers from the list. Under the virtual
// clock a cancelled entry is skipped and forgotten when the loop ends; a
// long-lived real-clock loop would otherwise accumulate them for its whole life.
func (rt *Runtime) dropCancelledTimers() {
	live := rt.macrotasks[:0]
	for _, t := range rt.macrotasks {
		if !t.cancelled {
			live = append(live, t)
		}
	}
	clear(rt.macrotasks[len(live):])
	rt.macrotasks = live
}

// nextTimerDeadline returns when the earliest live timer is due.
func (rt *Runtime) nextTimerDeadline() (time.Time, bool) {
	var best time.Time
	found := false
	for i := range rt.macrotasks {
		t := &rt.macrotasks[i]
		if t.cancelled {
			continue
		}
		if !found || t.deadline.Before(best) {
			best, found = t.deadline, true
		}
	}
	return best, found
}

// waitForWork blocks until a posted job arrives, the next timer is due, or the
// loop's context is cancelled. It is the only place the engine sleeps.
func (rt *Runtime) waitForWork(done <-chan struct{}, next time.Time, hasTimer bool) {
	h := rt.host
	if h == nil {
		return
	}
	if !hasTimer {
		select {
		case <-h.wake:
		case <-done:
		}
		return
	}
	d := time.Until(next)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-h.wake:
	case <-t.C:
	case <-done:
	}
}
