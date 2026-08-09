package engine

// Agents: more than one thread of JavaScript, sharing bytes and nothing else.
//
// An ECMAScript agent is one thread of control with its own execution stack and
// its own heap. Agents that can share memory form an agent cluster, and what
// they share is exactly the backing store of a SharedArrayBuffer -- never an
// object, never a function, never a realm. So an agent here is a separate
// *Runtime with separate pools, and the only thing that crosses between two of
// them is a []byte.
//
// That is what keeps this feature from reaching the rest of the engine. The
// collector, the shapes, the inline caches and the compiled code all stay
// exactly as single-threaded as they were; nothing in them ever sees a second
// agent. Compare the alternative -- a shared object heap -- which would want a
// concurrent collector and a lock on every shape transition.
//
// # One at a time
//
// Agents in a cluster take turns. The cluster holds a mutex and an agent must
// have it to run, so no two ever execute at once. Three things follow:
//
//   - There are no data races on the shared bytes, and none on any Go state the
//     engine keeps, because the mutex orders every access. The race detector
//     agrees, which it would not if unordered writes to a SharedArrayBuffer were
//     issued from two goroutines at once -- the ECMAScript memory model permits
//     those, and Go's does not.
//   - The engine did not have to become thread-safe to gain agents.
//   - Real parallelism is not available, and no test262 test asks for it: a
//     test has to be deterministic, so what it checks is the ORDER agents
//     observe, never that two of them ran at the same instant.
//
// # Being taken off the CPU
//
// Taking turns cannot be voluntary. The harness spins:
//
//	while ((agents = Atomics.load(typedArray, index)) !== expected) { }
//
// with nothing inside the loop, so an agent that only yields when it chooses to
// would sit there forever. The scheduler therefore takes the turn away, and it
// does so through the interrupt flag the interpreter already tests on every
// function entry and every 1024th loop back edge -- the same mechanism the host
// interrupt and the heap limit use, for the same reason: the hot path cannot
// afford a second check beside the one it already has. An agent that wants the
// mutex raises interruptYield on whoever holds it; the holder's next safepoint
// hands it over and takes it back. A process with one agent never sets the flag
// and so pays exactly nothing.

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// maxWaiters bounds Atomics.notify's count when it is +∞: there cannot be more
// waiters than this, and it keeps an unbounded number out of an int.
const maxWaiters = 1 << 30

// agentEpoch is the origin for $262.agent.monotonicNow. One process-wide value
// so two agents' clocks are comparable, which is the whole point of the tests
// that read it.
var agentEpoch = time.Now()

// agentCluster is a set of agents that share memory. One exists per cluster,
// created by the first $262.agent.start.
type agentCluster struct {
	// holder is the Runtime currently running, so an agent that wants the turn
	// knows whose interrupt flag to raise.
	holder atomic.Pointer[Runtime]

	// queued is len(queue), readable without the lock so an agent can tell in one
	// atomic load whether giving the turn up would accomplish anything.
	queued atomic.Int32

	mu sync.Mutex
	// held says someone has the turn. Note that this is NOT a mutex: the turn is
	// handed from one agent to the next by closing the successor's channel, so
	// there is no window in which it is free and every agent gets it in the order
	// it asked. A mutex here livelocks -- Go's is barging, and an agent spinning
	// in a `while (Atomics.load(...) !== n);` loop re-takes it before the waiter
	// it just woke is ever scheduled.
	held bool
	// queue is the agents waiting for the turn, oldest first.
	queue []chan struct{}
	// waiters is the Atomics.wait wait-list, in arrival order: notify wakes the
	// oldest first, which is the order the spec's [[WaiterList]] specifies.
	waiters []*agentWaiter
	// reports is $262.agent.report's FIFO, drained by $262.agent.getReport.
	reports []string
	// inboxes are the not-yet-delivered broadcast channels, one per started
	// agent. A broadcast empties the list: an agent started afterwards is not a
	// recipient of a broadcast that already happened.
	inboxes []chan []byte
	// started counts agents launched, and caps them.
	started int
}

// agentWaiter is one agent parked in Atomics.wait. The pair (block, offset)
// identifies the memory location: block is the backing array, which is shared,
// and not the SharedArrayBuffer object, which is per agent.
type agentWaiter struct {
	block  *byte
	offset int
	wake   chan struct{}
	// woken records that a notify claimed this waiter, so a timeout racing with
	// a notify reports "ok" -- the notify already counted it.
	woken bool
}

// takeTurn blocks until this agent may run. It asks whoever is running to give
// the turn up rather than waiting for them to finish, because they may never
// finish -- a spin loop is exactly what the harness does.
func (c *agentCluster) takeTurn(rt *Runtime) {
	if ch := c.join(); ch != nil {
		<-ch
	}
	c.claim(rt)
}

// claim records who is running and, if anyone is already queued behind them,
// asks them to hand over.
//
// The second half is not belt and braces. The turn passes from one agent to the
// next through a channel, and for that instant holder is nil -- so an agent
// joining the queue right then finds nobody to ask, and if the incoming agent
// goes on to spin (`while (Atomics.load(i32a, 0) !== n);` is the harness's
// idiom) it never learns anyone is waiting, and the cluster stops. Whoever
// takes the turn therefore looks at the queue itself.
func (c *agentCluster) claim(rt *Runtime) {
	c.holder.Store(rt)
	if c.queued.Load() > 0 {
		rt.requestYield()
	}
}

// join puts this agent at the back of the queue, or gives it the turn at once
// if nobody has it. A non-nil channel is the ticket to wait on.
func (c *agentCluster) join() chan struct{} {
	c.mu.Lock()
	if !c.held {
		c.held = true
		c.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	c.queue = append(c.queue, ch)
	c.queued.Store(int32(len(c.queue)))
	h := c.holder.Load()
	c.mu.Unlock()
	if h != nil {
		h.requestYield()
	}
	return ch
}

// giveTurn passes the turn to the next agent in line, or marks it free. The
// successor is handed the turn directly -- held stays true across the gap --
// so nobody can barge in front of a queue that is already formed.
func (c *agentCluster) giveTurn(rt *Runtime) {
	c.holder.CompareAndSwap(rt, nil)
	c.mu.Lock()
	if len(c.queue) > 0 {
		next := c.queue[0]
		c.queue = c.queue[1:]
		c.queued.Store(int32(len(c.queue)))
		c.mu.Unlock()
		close(next)
		return
	}
	c.held = false
	c.mu.Unlock()
}

// enter takes the turn on behalf of an agent whose Runtime does not exist yet,
// and builds it holding the turn: constructing a realm is engine work like any
// other and must not run beside another agent's.
func (c *agentCluster) enter(build func() *Runtime) *Runtime {
	if ch := c.join(); ch != nil {
		<-ch
	}
	rt := build()
	c.claim(rt)
	return rt
}

// pause gives the turn up for the duration of f, which is anything that blocks
// without executing JavaScript: a sleep, a wait, a join. Another agent runs
// meanwhile, which is the only way the thing being waited for can happen.
func (c *agentCluster) pause(rt *Runtime, f func()) {
	c.giveTurn(rt)
	f()
	c.takeTurn(rt)
}

// requestYield asks a running agent to hand over at its next safepoint. It only
// ever raises the flag from zero: a real interrupt outranks a turn.
func (rt *Runtime) requestYield() {
	if rt.interrupt != nil {
		rt.interrupt.flag.CompareAndSwap(interruptNone, interruptYield)
	}
}

// yieldTurn is the safepoint's response to interruptYield: put the turn back,
// let whoever asked for it take it, then take it again. Called from
// interruptPending, so every existing check point -- interpreted back edges,
// compiled back edges, function entry -- is a place an agent can be switched
// out, and no new check was added to any of them.
func (rt *Runtime) yieldTurn() {
	c := rt.cluster
	if c == nil {
		return
	}
	rt.interrupt.flag.CompareAndSwap(interruptYield, interruptNone)
	if c.queued.Load() == 0 {
		// Whoever asked has already been served. Nothing to hand over.
		return
	}
	c.giveTurn(rt)
	c.takeTurn(rt)
}

// ---- the wait list ----

// waitBlock identifies the shared memory a typed array views: the address of
// the backing array's first byte. Two agents' SharedArrayBuffer objects are
// different objects over the same array, and this is what they have in common.
func waitBlock(o *object) (*byte, bool) {
	b := o.ta.bufPtr
	if b == nil || !b.abShared() || len(b.abuf) == 0 {
		return nil, false
	}
	return &b.abuf[0], true
}

// atomicsPark is the blocking half of Atomics.wait. The value has already been
// compared and matched, so this agent is going to sleep; the question is only
// whether it is woken or times out.
//
// timeout is in milliseconds and may be +Inf, which is a wait with no deadline
// -- legitimate here because another agent can always run and notify us, which
// is exactly what the turn-taking guarantees.
func (rt *Runtime) atomicsPark(o *object, index int, timeout float64) string {
	c := rt.cluster
	block, ok := waitBlock(o)
	if !ok || c == nil {
		// Nobody else can see these bytes, so nobody can ever change them: the
		// wait is a sleep with a known answer.
		if c != nil && timeout > 0 {
			rt.agentSleep(timeout)
		}
		return "timed-out"
	}
	w := &agentWaiter{
		block:  block,
		offset: o.ta.byteOffset + index*o.ta.size(),
		wake:   make(chan struct{}),
	}
	c.mu.Lock()
	c.waiters = append(c.waiters, w)
	c.mu.Unlock()

	result := "ok"
	c.pause(rt, func() {
		if timeout <= 0 {
			// A zero timeout still has to be published to the list, because a
			// notify racing with it may legitimately claim this waiter.
			select {
			case <-w.wake:
			default:
				result = "timed-out"
			}
			return
		}
		if math.IsInf(timeout, 1) {
			<-w.wake
			return
		}
		t := time.NewTimer(time.Duration(timeout * float64(time.Millisecond)))
		defer t.Stop()
		select {
		case <-w.wake:
		case <-t.C:
			result = "timed-out"
		}
	})

	c.mu.Lock()
	for i, x := range c.waiters {
		if x == w {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			break
		}
	}
	// A notify that reached this waiter counted it, so it must report "ok" even
	// if the timer fired in the same instant. Counting it and then telling it it
	// timed out would lose a wakeup.
	if w.woken {
		result = "ok"
	}
	c.mu.Unlock()
	return result
}

// atomicsWake is Atomics.notify: wake up to count waiters on this location, in
// arrival order, and report how many were woken.
func (rt *Runtime) atomicsWake(o *object, index int, count int) int {
	c := rt.cluster
	if c == nil || count <= 0 {
		return 0
	}
	block, ok := waitBlock(o)
	if !ok {
		return 0
	}
	offset := o.ta.byteOffset + index*o.ta.size()

	c.mu.Lock()
	n := 0
	for _, w := range c.waiters {
		if n >= count {
			break
		}
		if w.block != block || w.offset != offset || w.woken {
			continue
		}
		w.woken = true
		close(w.wake)
		n++
	}
	c.mu.Unlock()
	return n
}

// agentSleep blocks this agent for ms milliseconds, giving up the turn so the
// agents it is waiting for can run. That is the only reason to sleep.
func (rt *Runtime) agentSleep(ms float64) {
	if ms <= 0 {
		return
	}
	d := time.Duration(ms * float64(time.Millisecond))
	if c := rt.cluster; c != nil {
		c.pause(rt, func() { time.Sleep(d) })
		return
	}
	time.Sleep(d)
}

// ---- reports ----

func (c *agentCluster) report(msg string) {
	c.mu.Lock()
	c.reports = append(c.reports, msg)
	c.mu.Unlock()
}

// takeReport pops the oldest report, or reports that there is none. getReport
// answers null in that case, which is how the harness spins until an agent has
// something to say.
func (c *agentCluster) takeReport() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reports) == 0 {
		return "", false
	}
	s := c.reports[0]
	c.reports = c.reports[1:]
	return s, true
}

// ---- the host surface: $262.agent ----

// maxAgents caps how many agents one cluster may start. test262's tests use a
// handful; the cap is here because each agent is a goroutine holding a whole
// realm, and a script that asks for a million of them should be told no rather
// than take the process down.
const maxAgents = 64

// EnableAgents installs $262.agent on this Runtime, letting a script start
// other agents and share memory with them.
//
// It is off by default and a host must ask, because starting an agent is a
// capability, not a language feature: each one is a goroutine with a realm of
// its own, and a script that could start them at will could exhaust the
// process. The conformance runner turns it on; an embedder running untrusted
// scripts should not.
func (rt *Runtime) EnableAgents() {
	rt.installAgentHost(false)
}

// installAgentHost defines $262.agent. `child` picks which half of the API is
// present: an agent that was started receives broadcasts and reports, and the
// one that started it broadcasts and collects.
func (rt *Runtime) installAgentHost(child bool) {
	// $262 is only there when the host granted EnableHostAPI, and agents are a
	// separate grant: host262Object makes an empty namespace when it has to, so
	// asking for agents does not also hand out detachArrayBuffer.
	ho := rt.objPtr(rt.host262Object())
	if ho == nil {
		return
	}
	agent := rt.newPlainObject()
	ao := rt.objPtr(agent)

	// Both halves can measure time and step aside, and both need to: an agent
	// that never gives the turn up starves whoever it is waiting for.
	rt.defMethod(ao, "monotonicNow", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mknum(float64(time.Since(agentEpoch).Nanoseconds()) / 1e6), nil
	})
	rt.defMethod(ao, "sleep", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		ms, e := rt.toNumber(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		rt.agentSleep(ms)
		return mkundef(), nil
	})

	if child {
		rt.defMethod(ao, "receiveBroadcast", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			fn := arg(args, 0)
			if !rt.isCallable(fn) {
				return mkundef(), rt.typeError("$262.agent.receiveBroadcast expects a function")
			}
			var block []byte
			var got bool
			// Waiting for the broadcast is the first thing a started agent does,
			// and it must not hold the turn while it does: the agent that will
			// send it is the one that cannot run.
			rt.cluster.pause(rt, func() {
				block, got = <-rt.agentInbox, true
			})
			if !got {
				return mkundef(), nil
			}
			_, e := rt.callValue(fn, mkundef(), []Value{rt.newSharedBufOver(block)})
			return mkundef(), e
		})
		rt.defMethod(ao, "report", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			s, e := rt.toStringValue(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			rt.cluster.report(rt.strGo(s))
			return mkundef(), nil
		})
		// leaving() says this agent is done. Nothing has to happen: the script
		// ends, the goroutine returns, and the turn goes back on its own.
		rt.defMethod(ao, "leaving", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return mkundef(), nil
		})
	} else {
		rt.defMethod(ao, "start", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			src, e := rt.toStringValue(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			return mkundef(), rt.agentStart(rt.strGo(src))
		})
		rt.defMethod(ao, "broadcast", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(arg(args, 0))
			if o == nil || !o.abObj || !o.abShared() {
				return mkundef(), rt.typeError("$262.agent.broadcast expects a SharedArrayBuffer")
			}
			rt.agentBroadcast(o.abuf)
			return mkundef(), nil
		})
		// getReport answers null when no agent has said anything yet, which is
		// what the harness polls on.
		rt.defMethod(ao, "getReport", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			c := rt.cluster
			if c == nil {
				return mknull(), nil
			}
			if s, ok := c.takeReport(); ok {
				return rt.newString(s), nil
			}
			return mknull(), nil
		})
	}
	ho.defineOwn("agent", agent, attrWritable|attrEnumerable|attrConfigurable)
}

// ensureCluster joins this Runtime to a cluster, creating one on first use. The
// Runtime that calls it is the agent already running, so it holds the turn from
// the start and never blocks taking it.
func (rt *Runtime) ensureCluster() *agentCluster {
	if rt.cluster == nil {
		c := &agentCluster{held: true}
		c.holder.Store(rt)
		rt.cluster = c
	}
	return rt.cluster
}

// agentStart launches an agent running src. It returns as soon as the goroutine
// exists; the new agent takes its first turn when this one next reaches a
// safepoint or blocks.
func (rt *Runtime) agentStart(src string) *ThrowError {
	c := rt.ensureCluster()
	c.mu.Lock()
	if c.started >= maxAgents {
		c.mu.Unlock()
		return rt.rangeError("$262.agent.start: too many agents")
	}
	c.started++
	inbox := make(chan []byte, 1)
	c.inboxes = append(c.inboxes, inbox)
	c.mu.Unlock()

	go func() {
		// The realm is built holding the turn. Constructing one is engine work
		// like any other and must not run beside another agent's.
		child := c.enter(func() *Runtime {
			ch := New()
			ch.cluster = c
			ch.agentInbox = inbox
			return ch
		})
		defer c.giveTurn(child)
		child.installAgentHost(true)
		sc, err := child.CompileScript("agent.js", src)
		if err != nil {
			return
		}
		_, _ = child.RunScript(sc)
		child.DrainJobs()
	}()
	return nil
}

// agentBroadcast hands the bytes to every agent started so far. Each gets the
// same backing array -- that sharing is the whole point -- and wraps it in a
// SharedArrayBuffer of its own.
func (rt *Runtime) agentBroadcast(block []byte) {
	c := rt.cluster
	if c == nil {
		return
	}
	c.mu.Lock()
	boxes := append([]chan []byte(nil), c.inboxes...)
	c.inboxes = nil
	c.mu.Unlock()
	for _, ch := range boxes {
		ch <- block
	}
}

// newSharedBufOver wraps bytes this agent did not allocate in a
// SharedArrayBuffer of its own. The object is this realm's; the bytes are not.
func (rt *Runtime) newSharedBufOver(block []byte) Value {
	v := rt.newObject(rt.sabProto)
	o := rt.objPtr(v)
	o.abObj, o.extend().abShared = true, true
	o.abuf = block
	o.extend().abMax = len(block)
	return v
}

// ---- waitAsync ----

// asyncWaiter is a waitAsync that has not settled: the wait-list entry, the
// deadline, and the resolve function of the promise it answered with.
type asyncWaiter struct {
	w        *agentWaiter
	deadline time.Time
	noExpiry bool
	resolve  Value
}

// parkAsync puts a waitAsync on the list without blocking, and reports whether
// there was anyone who could ever wake it. False means the location is not
// really shared, so the caller should answer "timed-out" now rather than hold a
// promise nothing can settle.
func (rt *Runtime) parkAsync(p atomicsWaitPlan, resolve Value) bool {
	// A cluster of one is a real cluster: waitAsync does not block its agent, so
	// the agent can go on and notify itself, and a single-agent
	// waitAsync-then-notify is a test in its own right.
	c := rt.ensureCluster()
	block, ok := waitBlock(p.view)
	if !ok {
		return false
	}
	w := &agentWaiter{
		block:  block,
		offset: p.view.ta.byteOffset + p.index*p.view.ta.size(),
		wake:   make(chan struct{}),
	}
	c.mu.Lock()
	c.waiters = append(c.waiters, w)
	c.mu.Unlock()

	aw := &asyncWaiter{w: w, resolve: resolve, noExpiry: math.IsInf(p.timeout, 1)}
	if !aw.noExpiry {
		aw.deadline = time.Now().Add(time.Duration(p.timeout * float64(time.Millisecond)))
	}
	// rt.asyncWaits is a Runtime field, so the collector's reflective walk finds
	// the resolve function inside it. That is the whole reason the walk exists
	// rather than a hand-written root list: a new field like this one is rooted
	// by being declared, and cannot be forgotten.
	rt.asyncWaits = append(rt.asyncWaits, aw)
	return true
}

// settleReadyAsyncWaits settles every waitAsync that can be answered WITHOUT
// waiting: one that has been notified, or whose deadline has passed. It reports
// whether it settled any.
//
// The event loop calls this on every turn, not only when it runs dry, and the
// difference matters. A test that polls with `$262.agent.setTimeout(wait, 0)`
// keeps a timer ready forever, and under a virtual clock a zero-delay timer is
// always due -- so the queue never empties and a waiter serviced only at the
// end is never serviced at all. The deadline is real time even though the
// timers are not, which is what lets the two coexist.
func (rt *Runtime) settleReadyAsyncWaits() bool {
	now := time.Now()
	settled := false
	for i := 0; i < len(rt.asyncWaits); {
		aw := rt.asyncWaits[i]
		ready := false
		select {
		case <-aw.w.wake:
			ready = true
		default:
			ready = !aw.noExpiry && !now.Before(aw.deadline)
		}
		if !ready {
			i++
			continue
		}
		rt.asyncWaits = append(rt.asyncWaits[:i], rt.asyncWaits[i+1:]...)
		rt.finishAsyncWait(aw, "")
		settled = true
	}
	return settled
}

// finishAsyncWait takes the waiter off the cluster list and resolves its
// promise. An empty `result` means work it out: a notify that claimed this
// waiter is "ok" whatever the clock says, because the notify already counted it.
func (rt *Runtime) finishAsyncWait(aw *asyncWaiter, result string) {
	c := rt.cluster
	c.mu.Lock()
	for i, x := range c.waiters {
		if x == aw.w {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			break
		}
	}
	if aw.w.woken {
		result = "ok"
	} else if result == "" {
		result = "timed-out"
	}
	c.mu.Unlock()
	rt.callValue(aw.resolve, mkundef(), []Value{rt.newString(result)})
}

// serviceAsyncWaits settles the oldest outstanding waitAsync, blocking until it
// is notified or its deadline passes, and reports whether it did anything. The
// event loop calls it when it has run out of jobs: a promise from waitAsync is
// the one thing a queue can be empty and still not be finished.
func (rt *Runtime) serviceAsyncWaits() bool {
	if len(rt.asyncWaits) == 0 {
		return false
	}
	// A waiter with no deadline can only be settled by a notify, so it is worth
	// blocking on only if one can still arrive: either it has already been
	// woken, or some other agent is alive to do the waking. Otherwise it is a
	// promise that will never settle -- which is the correct outcome, and is
	// reached by leaving it on the list and letting the event loop finish, not
	// by waiting forever for it.
	best := -1
	for i, a := range rt.asyncWaits {
		if !a.settleable(rt) {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		b := rt.asyncWaits[best]
		// Earliest deadline first, so timeouts settle in the order they expire.
		// A woken waiter beats any deadline: it is ready now.
		switch {
		case b.noExpiry && !a.noExpiry:
			best = i
		case !b.noExpiry && !a.noExpiry && a.deadline.Before(b.deadline):
			best = i
		}
	}
	if best < 0 {
		return false
	}
	aw := rt.asyncWaits[best]
	rt.asyncWaits = append(rt.asyncWaits[:best], rt.asyncWaits[best+1:]...)

	result := "ok"
	c := rt.cluster
	c.pause(rt, func() {
		if aw.noExpiry {
			<-aw.w.wake
			return
		}
		t := time.NewTimer(time.Until(aw.deadline))
		defer t.Stop()
		select {
		case <-aw.w.wake:
		case <-t.C:
			result = "timed-out"
		}
	})
	rt.finishAsyncWait(aw, result)
	return true
}

// settleable reports whether waiting on this entry can ever end. A deadline
// always ends; without one, only a notify does, and a notify needs either to
// have happened already or to have somebody left who could send it.
func (a *asyncWaiter) settleable(rt *Runtime) bool {
	if !a.noExpiry {
		return true
	}
	select {
	case <-a.w.wake:
		return true
	default:
	}
	c := rt.cluster
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started > 0
}
