// Package v8go is a drop-in replacement for the v8go binding, implemented on
// the pure-Go goant engine. Swapping the import path takes a program off cgo
// and off V8 without touching call sites.
//
// The mapping is close but not exact, and the differences are deliberate:
//
//   - An Isolate is a goant Runtime. A Context is a realm on it — fresh
//     globals and prototypes, shared string interning and object pools — which
//     is the same isolate/context split V8 has.
//   - There is no JIT and no separately-managed V8 heap, so the heap and flag
//     controls exist to keep call sites compiling and are documented
//     individually as no-ops. They are not silently ignored: each says so.
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
	MaxOldSpaceBytes   uint64
	MaxYoungSpaceBytes uint64
}

// HeapStatistics reports memory use. Only UsedHeapSize is meaningful, and it is
// this isolate's own accounting of live engine objects — not a Go-runtime-wide
// figure, and not directly comparable to V8's.
type HeapStatistics struct {
	TotalHeapSize          uint64
	UsedHeapSize           uint64
	HeapSizeLimit          uint64
	ExternalMemory         uint64
	NumberOfNativeContexts uint64
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
	return &Isolate{rt: engine.New(), opts: opts}
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

// GetHeapStatistics reports this isolate's memory use. UsedHeapSize is derived
// from Go's own accounting of heap in use, which for a single-isolate process
// is a good proxy and for a many-isolate process over-reports — every isolate
// sees the whole process. Callers using it to decide when to retire a pooled
// isolate should read it as "memory pressure", not "this isolate's footprint".
func (i *Isolate) GetHeapStatistics() HeapStatistics {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	i.mu.Lock()
	n := i.contexts
	i.mu.Unlock()
	return HeapStatistics{
		TotalHeapSize:          ms.HeapSys,
		UsedHeapSize:           ms.HeapAlloc,
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
