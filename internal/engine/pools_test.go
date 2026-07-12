package engine

import "testing"

func TestPoolAllocDerefFree(t *testing.T) {
	type box struct{ x int }
	p := newPool[box]()

	h1, e1 := p.alloc()
	e1.x = 111
	h2, e2 := p.alloc()
	e2.x = 222

	if h1 == nullHandle || h2 == nullHandle || h1 == h2 {
		t.Fatalf("bad handles: %d %d", h1, h2)
	}
	if p.get(h1).x != 111 || p.get(h2).x != 222 {
		t.Fatal("deref mismatch")
	}
	if p.len() != 2 {
		t.Fatalf("len=%d want 2", p.len())
	}

	g1 := p.gen(h1)
	p.free(h1)
	if p.get(h1) != nil {
		t.Fatal("freed cell still live")
	}
	if p.alive(h1, g1) {
		t.Fatal("stale (handle,gen) reported alive")
	}
	if p.len() != 1 {
		t.Fatalf("len=%d want 1 after free", p.len())
	}

	// Reallocation reuses the freed handle with a bumped generation.
	h3, e3 := p.alloc()
	e3.x = 333
	if h3 != h1 {
		t.Fatalf("expected handle reuse: got %d want %d", h3, h1)
	}
	if p.gen(h3) == g1 {
		t.Fatal("generation not bumped on reuse")
	}
	if p.alive(h1, g1) {
		t.Fatal("old generation still alive after reuse")
	}
	if p.get(h3).x != 333 {
		t.Fatal("reused cell wrong value")
	}
}

func TestPoolCrossChunk(t *testing.T) {
	p := newPool[int]()
	const n = poolChunkSize*2 + 5
	handles := make([]Handle, n)
	for i := range n {
		h, e := p.alloc()
		*e = i
		handles[i] = h
	}
	if len(p.chunks) < 3 {
		t.Fatalf("expected >=3 chunks, got %d", len(p.chunks))
	}
	for i, h := range handles {
		if got := *p.get(h); got != i {
			t.Fatalf("handle %d: got %d want %d", h, got, i)
		}
	}
}

func TestPoolNullHandle(t *testing.T) {
	p := newPool[int]()
	if p.get(nullHandle) != nil {
		t.Fatal("null handle should not resolve")
	}
	if p.alive(nullHandle, 0) {
		t.Fatal("null handle should not be alive")
	}
}
