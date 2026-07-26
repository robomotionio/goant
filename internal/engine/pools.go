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

// gcPoison is the GOANT_GC_POISON debug mode: a swept cell is not returned to
// the free list, and the first use of one panics instead of quietly reading
// whatever was allocated over it. That turns a missing collector root — which
// otherwise surfaces as an unrelated value going wrong much later — into a Go
// stack trace at the exact read. See collect.go.
var gcPoison = osGetenvGCPoison()

// pool is a chunked non-moving arena of T.
type pool[T any] struct {
	// chunks holds pointers to fixed-size arrays rather than slices. Resolving
	// a handle is the single hottest indirection in the engine — every property
	// access on an object goes through it — and an array pointer makes it one
	// load and one bounds check instead of a slice header plus two, because the
	// in-chunk index comes from a mask and is provably in range.
	chunks   []*[poolChunkSize]poolCell[T]
	freeList []Handle // stack of freed handles for reuse
	next     Handle   // next never-used handle (starts at 1)
	liveN    int

	// poisoned records cells the collector freed, under gcPoison only.
	poisoned map[Handle]bool
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
		p.chunks = append(p.chunks, new([poolChunkSize]poolCell[T]))
	}
}

// get returns a pointer to the element for a live handle, or nil.
func (p *pool[T]) get(h Handle) *T {
	cl := p.cell(h)
	if cl == nil || !cl.live {
		if gcPoison && p.poisoned[h] {
			panic(poisonError{h})
		}
		return nil
	}
	return &cl.elem
}

// poisonError is what a gcPoison build panics with when something dereferences
// a handle the collector freed.
type poisonError struct{ h Handle }

func (e poisonError) Error() string { return "goant: use of collected handle" }

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

// ---- tracing support ----
//
// A handle is not a pointer, so the Go collector cannot tell a live cell from a
// dead one: the chunk holds every cell it ever allocated, and from Go's side
// they are all reachable. Reclaiming a cell is therefore the engine's job, and
// these are the two halves of it — a mark bitset keyed by handle, and a sweep
// that frees everything the mark phase did not reach.
//
// Freeing zeroes the cell, so everything hanging off it (a string's bytes, an
// object's overflow slots and shape) becomes unreachable to the Go collector
// and is reclaimed by it. Only the cells themselves are managed here.

// markSet is a bitset over handles, sized to the pool's high-water mark.
type markSet []uint64

func (m markSet) has(h Handle) bool {
	i := uint32(h) >> 6
	return int(i) < len(m) && m[i]&(1<<(uint32(h)&63)) != 0
}

// set records h and reports whether it was already recorded, which is what
// stops a trace from following a cycle forever.
func (m markSet) set(h Handle) (already bool) {
	i := uint32(h) >> 6
	if int(i) >= len(m) {
		return true // outside the snapshot: allocated after marking began
	}
	bit := uint64(1) << (uint32(h) & 63)
	if m[i]&bit != 0 {
		return true
	}
	m[i] |= bit
	return false
}

// newMarks returns a zeroed bitset covering every handle allocated so far,
// reusing the previous cycle's storage.
func (p *pool[T]) newMarks(prev markSet) markSet {
	n := int(p.next>>6) + 1
	if cap(prev) >= n {
		m := prev[:n]
		clear(m)
		return m
	}
	return make(markSet, n)
}

// sweep frees every live cell whose handle is not in m, and reports how many it
// released.
func (p *pool[T]) sweep(m markSet) int {
	freed := 0
	for c := range p.chunks {
		base := Handle(c << poolChunkShift)
		for s := 0; s < poolChunkSize; s++ {
			h := base + Handle(s)
			if h == nullHandle {
				continue
			}
			if h >= p.next {
				break
			}
			if !p.chunks[c][s].live || m.has(h) {
				continue
			}
			if gcPoison {
				// Free without recycling: the handle stays dead so a dangling
				// reference to it is caught rather than silently repointed at
				// the next allocation.
				cl := &p.chunks[c][s]
				cl.live = false
				cl.gen++
				var zero T
				cl.elem = zero
				p.liveN--
				if p.poisoned == nil {
					p.poisoned = map[Handle]bool{}
				}
				p.poisoned[h] = true
				freed++
				continue
			}
			p.free(h)
			freed++
		}
	}
	return freed
}
