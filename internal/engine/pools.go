package engine

// Chunked, non-moving handle pools (PLAN.md §Architecture; TODO 0.2).
//
// Heap-resident Values carry a 32-bit handle instead of a raw pointer. A
// handle indexes a [pool]: a slice of fixed-size chunks that are never moved
// or reallocated, so a handle stays valid for the lifetime of its cell. This
// keeps Values pointer-free (Go GC never sees them) while still giving O(1)
// deref via &chunks[h>>shift][h&mask].
//
// Each cell carries a generation counter bumped on free, giving weak refs and
// FinalizationRegistry a way to detect stale handles (ABA safety) without the
// engine ever reusing a (handle, generation) pair within a GC cycle.

const (
	poolChunkShift = 12 // 4096 cells per chunk (power of two)
	poolChunkSize  = 1 << poolChunkShift
	poolChunkMask  = poolChunkSize - 1
)

// Handle is a 32-bit index into a pool. Handle 0 is reserved as "null handle";
// real cells start at 1 so a zeroed Value payload never aliases a live cell.
type Handle uint32

const nullHandle Handle = 0

// poolCell wraps a stored element with its liveness generation.
type poolCell[T any] struct {
	elem T
	gen  uint32 // bumped on free; even == live is NOT assumed, see alloc
	live bool
}

// pool is a chunked non-moving arena of T.
type pool[T any] struct {
	chunks   [][]poolCell[T]
	freeList []Handle // stack of freed handles for reuse
	next     Handle   // next never-used handle (starts at 1)
	liveN    int
}

func newPool[T any]() *pool[T] {
	return &pool[T]{next: 1}
}

// locate returns the chunk/slot indices for a handle.
func (p *pool[T]) locate(h Handle) (chunk, slot int) {
	idx := uint32(h)
	return int(idx >> poolChunkShift), int(idx & poolChunkMask)
}

// cell returns a pointer to the backing cell for h (nil if out of range).
func (p *pool[T]) cell(h Handle) *poolCell[T] {
	if h == nullHandle {
		return nil
	}
	c, s := p.locate(h)
	if c >= len(p.chunks) {
		return nil
	}
	return &p.chunks[c][s]
}

// alloc reserves a cell and returns its handle plus a pointer to the element.
// Reused handles get a bumped generation.
func (p *pool[T]) alloc() (Handle, *T) {
	var h Handle
	if n := len(p.freeList); n > 0 {
		h = p.freeList[n-1]
		p.freeList = p.freeList[:n-1]
	} else {
		h = p.next
		p.next++
		p.ensure(h)
	}
	cl := p.cell(h)
	cl.live = true
	p.liveN++
	var zero T
	cl.elem = zero
	return h, &cl.elem
}

// ensure grows the chunk vector so that h is addressable.
func (p *pool[T]) ensure(h Handle) {
	c, _ := p.locate(h)
	for len(p.chunks) <= c {
		p.chunks = append(p.chunks, make([]poolCell[T], poolChunkSize))
	}
}

// get returns a pointer to the element for a live handle, or nil.
func (p *pool[T]) get(h Handle) *T {
	cl := p.cell(h)
	if cl == nil || !cl.live {
		return nil
	}
	return &cl.elem
}

// gen returns the current generation of a handle's cell (0 if absent).
func (p *pool[T]) gen(h Handle) uint32 {
	cl := p.cell(h)
	if cl == nil {
		return 0
	}
	return cl.gen
}

// alive reports whether (h, gen) still names a live cell — the weak-ref /
// ABA-safety check.
func (p *pool[T]) alive(h Handle, gen uint32) bool {
	cl := p.cell(h)
	return cl != nil && cl.live && cl.gen == gen
}

// free releases a cell, bumping its generation so stale handles are detectable.
// The stored element is zeroed so any Go-owned payload slices become
// unreachable and the Go GC can reclaim them (hybrid ownership, PLAN.md §GC).
func (p *pool[T]) free(h Handle) {
	cl := p.cell(h)
	if cl == nil || !cl.live {
		return
	}
	cl.live = false
	cl.gen++
	var zero T
	cl.elem = zero
	p.freeList = append(p.freeList, h)
	p.liveN--
}

// truncate frees every cell from h upward and rewinds the allocator to h.
//
// This is region reclamation: instead of tracing which cells are still
// reachable, the caller asserts that nothing below h can reach anything above
// it, and the whole range goes at once. Cells are zeroed so their Go-owned
// payloads become collectable, and generations are bumped so a stale handle is
// detectable by anything that checks.
//
// The chunks themselves are kept. Reusing them is the point: the next
// invocation allocates into memory that is already there.
func (p *pool[T]) truncate(h Handle) {
	if h < 1 {
		h = 1
	}
	for i := h; i < p.next; i++ {
		cl := p.cell(i)
		if cl == nil {
			continue
		}
		if cl.live {
			p.liveN--
		}
		cl.live = false
		cl.gen++
		var zero T
		cl.elem = zero
	}
	// A free-list entry at or above the watermark would hand out a handle the
	// allocator is about to hand out anyway.
	kept := p.freeList[:0]
	for _, fh := range p.freeList {
		if fh < h {
			kept = append(kept, fh)
		}
	}
	p.freeList = kept
	p.next = h
}

// len returns the number of live cells.
func (p *pool[T]) len() int { return p.liveN }
