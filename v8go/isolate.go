// Package v8go is a drop-in replacement for the v8go binding, implemented on
// the pure-Go goant engine. Swapping the import path takes a program off cgo
// and off V8 without touching call sites.
//
// The mapping is close but not exact, and the differences are deliberate:
//
//   - An Isolate is a goant Runtime. A Context is a realm on it — fresh
//     globals and prototypes, shared string interning and object pools — which
//     is the same isolate/context split V8 has.
//   - There is a compiled tier, off unless IsolateOptions.JIT asks for it, and
//     no separately-managed V8 heap, so the heap and flag controls exist to
//     keep call sites compiling and are documented individually as no-ops. They
//     are not silently ignored: each says so.
//   - Go is garbage collected, so Dispose only drops references.
//
// Anything not needed by a caller is absent rather than stubbed, so a missing
// feature is a compile error rather than a silent wrong answer at runtime.
package v8go

import (
	"errors"
	"runtime"
	"sync"

	"github.com/robomotionio/goant/internal/engine"
)

// IsolateOptions mirrors the V8 heap-sizing options. goant has no separately
// managed heap — allocation goes to the Go heap and the Go GC — so these
// values are recorded and reported back by GetHeapStatistics but do not cap
// anything. A caller that relies on the cap to bound memory should keep its own
// accounting; see HeapStatistics.
type IsolateOptions struct {
	InitialOldSpaceBytes uint64
	MaxOldSpaceBytes     uint64
	MaxYoungSpaceBytes   uint64

	// JIT turns the compiled tier on for this isolate. It has no V8
	// counterpart — V8 is always jitting — and exists because here it is a
	// decision: off, a script runs on the interpreter; on, a function entered
	// often enough is compiled to machine code and run natively thereafter.
	//
	// Worth it for a node that processes many messages, worth nothing for one
	// that runs a script once, so it belongs per isolate rather than per
	// process. See github.com/robomotionio/goant.WithJIT.
	//
	// Setting it false does not turn the tier off — it leaves the process
	// default alone, so GOANT_JIT=1 still means what it says. A caller that
	// wants it off regardless asks for that with Isolate.SetJIT.
	JIT bool
}

// HeapStatistics reports memory use. The field set matches the binding this
// replaces so call sites compile unchanged, but only UsedHeapSize,
// TotalHeapSize, HeapSizeLimit and NumberOfNativeContexts carry a value — the
// rest are V8 bookkeeping with no counterpart here and stay zero.
type HeapStatistics struct {
	TotalHeapSize            uint64
	TotalHeapSizeExecutable  uint64
	TotalPhysicalSize        uint64
	TotalAvailableSize       uint64
	UsedHeapSize             uint64
	HeapSizeLimit            uint64
	MallocedMemory           uint64
	ExternalMemory           uint64
	PeakMallocedMemory       uint64
	NumberOfNativeContexts   uint64
	NumberOfDetachedContexts uint64
}

// Isolate is an independent engine instance.
type Isolate struct {
	mu   sync.Mutex
	rt   *engine.Runtime
	opts IsolateOptions

	// contexts counts live realms, so GetHeapStatistics can report something
	// truthful for NumberOfNativeContexts.
	contexts uint64

	// pending is the exception set by ThrowException from inside a host
	// callback. The callback returns nil to signal "I threw", so the value has
	// to be parked somewhere the callback wrapper can find it on the way out.
	pending *Value

	disposed bool
}

// NewIsolate creates an isolate with default options.
func NewIsolate() *Isolate { return NewIsolateWithOptions(IsolateOptions{}) }

// NewIsolateWithOptions creates an isolate. The options are recorded but do not
// bound memory — see IsolateOptions.
func NewIsolateWithOptions(opts IsolateOptions) *Isolate {
	rt := engine.New()
	// MaxOldSpaceBytes was V8's cap on the old generation. Here it becomes a
	// budget on the live heap, enforced after a collection: a script that
	// retains more than this is stopped and the host is told why, instead of
	// being allowed to run into Go's out-of-memory, which is a runtime throw
	// that no recover can catch and that takes the whole process with it.
	//
	// InitialOldSpaceBytes has no counterpart and is ignored. It existed to
	// pre-commit pages so a fresh V8 isolate would not have to grow at peak
	// load, which was a defence against Windows denying the growth. Go's
	// allocator has no equivalent failure to pre-empt.
	rt.SetHeapLimit(opts.MaxOldSpaceBytes)
	if opts.JIT {
		rt.SetJITEnabled(true)
	}
	return &Isolate{rt: rt, opts: opts}
}

// SetJIT turns the compiled tier on or off for this isolate, including for code
// it has already compiled: off, the next call interprets. Unlike the JIT option
// this goes both ways, so it is the switch to reach for when a host sees
// trouble and wants a live isolate back on the interpreter without a restart.
//
// It affects this isolate and no other.
func (i *Isolate) SetJIT(on bool) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.disposed || i.rt == nil {
		return
	}
	i.rt.SetJITEnabled(on)
}

// JITEnabled reports whether the compiled tier is on for this isolate.
func (i *Isolate) JITEnabled() bool {
	if i == nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return !i.disposed && i.rt != nil && i.rt.JITEnabled()
}

// Dispose drops the isolate's engine reference. Memory is reclaimed by the Go
// GC once nothing else refers to it, so unlike V8 this is not the only way to
// release accumulated state — but calling it is still correct and still what
// makes a pooled isolate collectable.
func (i *Isolate) Dispose() {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rt = nil
	i.disposed = true
}

// TerminateExecution stops whatever this isolate is currently running, as soon
// as the script reaches its next check point. Safe from any goroutine, which is
// the whole point: the caller is usually a timeout on another goroutine.
//
// The isolate stays terminated until ResumeExecution. That is deliberate — a
// host that abandons a script should not find the isolate quietly running it
// again on the next call.
func (i *Isolate) TerminateExecution() {
	if i == nil {
		return
	}
	i.mu.Lock()
	rt := i.rt
	i.mu.Unlock()
	if rt != nil {
		rt.Interrupt()
	}
}

// ResumeExecution clears a termination so the isolate can be used again. V8's
// binding calls this CancelTerminateExecution; both names are provided.
func (i *Isolate) ResumeExecution() {
	if i == nil {
		return
	}
	i.mu.Lock()
	rt := i.rt
	i.mu.Unlock()
	if rt != nil {
		rt.ClearInterrupt()
	}
}

// CancelTerminateExecution is an alias for ResumeExecution.
func (i *Isolate) CancelTerminateExecution() { i.ResumeExecution() }

// Close is an alias for Dispose, matching the binding.
func (i *Isolate) Close() { i.Dispose() }

// HeapLimitExceeded reports that this isolate stopped its script because the
// live heap passed the budget from IsolateOptions.MaxOldSpaceBytes, rather than
// because the host terminated it.
//
// A caller that treats both the same reports a timeout for what is really a
// message too large to process — different problem, different fix, and only one
// of them is the flow designer's to solve.
func (i *Isolate) HeapLimitExceeded() bool {
	if i == nil {
		return false
	}
	i.mu.Lock()
	rt := i.rt
	i.mu.Unlock()
	return rt != nil && rt.HeapLimitExceeded()
}

// IsExecutionTerminating reports whether a termination is in flight or still
// pending.
func (i *Isolate) IsExecutionTerminating() bool {
	i.mu.Lock()
	rt := i.rt
	i.mu.Unlock()
	return rt != nil && rt.Interrupted()
}

// apply lets an Isolate be passed to NewContext as a ContextOption.
func (i *Isolate) apply(o *contextOptions) { o.iso = i }

// GetHeapStatistics reports this isolate's memory use.
//
// UsedHeapSize is *this isolate's* occupancy — the live cells in its own pools —
// not a process-wide figure. That distinction is the whole point: callers use
// this to decide when to retire a pooled isolate, and process-wide Go MemStats
// would make every isolate in a pool see the same number and retire together
// the moment any one of them grew.
//
// The figure counts cells and their headers, so it excludes payloads hanging off
// them (string bytes, array backing stores) and is a floor rather than a total.
// It is still the right signal for retirement, because it tracks how much a
// script has allocated and never released.
//
// It also only rises. goant has no collector yet (PLAN.md Phase 7): cells are
// reclaimed when the whole isolate is dropped and Go collects its pools, so a
// long-lived pooled isolate accumulates everything every script it ran ever
// allocated. Retiring on this number is not a nicety here — it is the only
// reclamation mechanism there is.
//
// TotalHeapSize reports the process figure so a caller that wants overall
// pressure can still see it.
func (i *Isolate) GetHeapStatistics() HeapStatistics {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	i.mu.Lock()
	n := i.contexts
	rt := i.rt
	i.mu.Unlock()

	var used uint64
	if rt != nil {
		_, used = rt.HeapUsage()
	}
	return HeapStatistics{
		TotalHeapSize:          ms.HeapSys,
		UsedHeapSize:           used,
		HeapSizeLimit:          i.opts.MaxOldSpaceBytes,
		NumberOfNativeContexts: n,
	}
}

// ThrowException makes v the pending exception for the host callback currently
// running. It returns nil so a callback can `return iso.ThrowException(v)`,
// which is how the v8go binding is used.
func (i *Isolate) ThrowException(v *Value) *Value {
	if i == nil || v == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.pending = v
	return nil
}

// InternedStrings returns how many strings are pinned in this isolate's intern
// table. The table is permanent, so this only rises; a host can watch it to
// tell memory that is merely uncollected from memory that can never be freed.
func (i *Isolate) InternedStrings() int {
	i.mu.Lock()
	rt := i.rt
	i.mu.Unlock()
	if rt == nil {
		return 0
	}
	return rt.InternedCount()
}

// AddNearHeapLimitCallback is a no-op: there is no V8 heap limit to approach.
// Kept so V8-tuned call sites compile unchanged.
func (i *Isolate) AddNearHeapLimitCallback() {}

// AutomaticallyRestoreInitialHeapLimit is a no-op, for the same reason.
func (i *Isolate) AutomaticallyRestoreInitialHeapLimit(threshold float64) {}

// WarmupOldGenerationHeap is a no-op and always succeeds. Its purpose under V8
// was to pre-commit pages so a later allocation would not have to ask the OS;
// the Go allocator has no equivalent knob worth exposing.
func (i *Isolate) WarmupOldGenerationHeap(bytes uint64) error { return nil }

// SetFlags accepts V8 command-line flags and ignores them. They configure a JIT
// and a heap that do not exist here. Ignoring is the honest behaviour: the
// alternative is to fail on a flag that was only ever a tuning hint.
func SetFlags(flags ...string) {}

// SetNearHeapLimitGrowthBytes is a no-op, matching AddNearHeapLimitCallback.
func SetNearHeapLimitGrowthBytes(bytes uint64) {}

// IsOneByteSafe reports whether b is entirely single-byte (Latin-1) and so can
// be handed over without transcoding. goant stores strings as bytes and does
// not transcode on entry, so this is informational; it is kept because callers
// branch on it to choose an input path.
func IsOneByteSafe(b []byte) bool {
	for _, c := range b {
		if c > 0x7F {
			return false
		}
	}
	return true
}

// ErrDisposed is returned when an isolate is used after Dispose.
var ErrDisposed = errors.New("v8go: isolate has been disposed")

func (i *Isolate) runtime() (*engine.Runtime, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.rt == nil {
		return nil, ErrDisposed
	}
	return i.rt, nil
}

// SetBlobResolver installs the resolver used by ParseJSONBytesLazy for
// envelopes it encounters on this isolate.
func (i *Isolate) SetBlobResolver(r func(ref string) ([]byte, error)) {
	if i == nil {
		return
	}
	i.mu.Lock()
	rt := i.rt
	i.mu.Unlock()
	if rt != nil {
		rt.SetBlobResolver(r)
	}
}

// BlobResolveFailed reports that the isolate stopped its script because a
// lazily parsed envelope named a blob the resolver could not produce.
func (i *Isolate) BlobResolveFailed() bool {
	if i == nil {
		return false
	}
	i.mu.Lock()
	rt := i.rt
	i.mu.Unlock()
	return rt != nil && rt.BlobResolveFailed()
}

// BlobResolveError returns that failure.
func (i *Isolate) BlobResolveError() error {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	rt := i.rt
	i.mu.Unlock()
	if rt == nil {
		return nil
	}
	return rt.BlobResolveError()
}
