package jitmem

import (
	"errors"
	"testing"
	"unsafe"
)

// TestExecContextLayout guards the JIT ABI. Generated code addresses these
// fields by the constants, not by name, so a field reordered here without the
// constants moving too would miscompile every block already emitted — silently,
// and only at run time.
func TestExecContextLayout(t *testing.T) {
	var c ExecContext
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Exit", unsafe.Offsetof(c.Exit), CtxOffExit},
		{"Helper", unsafe.Offsetof(c.Helper), CtxOffHelper},
		{"Args", unsafe.Offsetof(c.Args), CtxOffArgs},
		{"Ret", unsafe.Offsetof(c.Ret), CtxOffRet},
		{"Resume", unsafe.Offsetof(c.Resume), CtxOffResume},
		{"Spill", unsafe.Offsetof(c.Spill), CtxOffSpill},
		{"SpillN", unsafe.Offsetof(c.SpillN), CtxOffSpillN},
		{"Pool", unsafe.Offsetof(c.Pool), CtxOffPool},
		{"Host", unsafe.Offsetof(c.Host), CtxOffHost},
		{"This", unsafe.Offsetof(c.This), CtxOffThis},
		{"size", unsafe.Sizeof(c), CtxSize},
	} {
		if tc.got != tc.want {
			t.Errorf("%s at %d, ABI says %d", tc.name, tc.got, tc.want)
		}
	}
}

func TestBlockWriteProtectFree(t *testing.T) {
	b, err := Alloc(64)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	defer b.Free()

	if got := b.Cap(); got < 64 || got%pageSize() != 0 {
		t.Errorf("Cap = %d, want a whole number of %d-byte pages, at least 64", got, pageSize())
	}
	off, err := b.Write([]byte{1, 2, 3, 4})
	if err != nil || off != 0 {
		t.Fatalf("Write = %d, %v", off, err)
	}
	off, err = b.Write([]byte{5, 6})
	if err != nil || off != 4 {
		t.Fatalf("second Write = %d, %v", off, err)
	}
	if b.Len() != 6 {
		t.Errorf("Len = %d, want 6", b.Len())
	}
	if b.AddrAt(4) != b.Addr()+4 {
		t.Error("AddrAt is not Addr plus the offset")
	}
	if b.Executable() {
		t.Error("block reports executable before Protect")
	}
	if err := b.Protect(); err != nil {
		t.Fatalf("Protect: %v", err)
	}
	if !b.Executable() {
		t.Error("block does not report executable after Protect")
	}
	if _, err := b.Write([]byte{7}); !errors.Is(err, ErrSealed) {
		t.Errorf("Write after Protect = %v, want ErrSealed", err)
	}
	if err := b.Protect(); err != nil {
		t.Errorf("second Protect: %v", err)
	}
}

func TestBlockFull(t *testing.T) {
	b, err := Alloc(1)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	defer b.Free()
	// Alloc rounds up to a page, so overflowing takes more than the asked-for
	// size — the point is that Write reports rather than growing, because a
	// block that moved would invalidate every offset already emitted into it.
	if _, err := b.Write(make([]byte, b.Cap()+1)); !errors.Is(err, ErrFull) {
		t.Errorf("oversized Write = %v, want ErrFull", err)
	}
	if b.Len() != 0 {
		t.Errorf("failed Write advanced Len to %d", b.Len())
	}
}

func TestFreeIsIdempotent(t *testing.T) {
	b, err := Alloc(64)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if err := b.Free(); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if err := b.Free(); err != nil {
		t.Errorf("second Free: %v", err)
	}
	if b.Addr() != 0 {
		t.Error("Addr is non-zero after Free")
	}
}

// exec assembles code into a fresh executable block and returns its entry
// address. The block is freed when the test ends.
func exec(t testing.TB, code []byte) (*Block, uintptr) {
	t.Helper()
	b, err := Alloc(len(code))
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	t.Cleanup(func() { b.Free() })
	if _, err := b.Write(code); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := b.Protect(); err != nil {
		t.Fatalf("Protect: %v", err)
	}
	return b, b.Addr()
}

// TestEnterReturnsConstant is the end-to-end proof that this package works:
// bytes written through a writable mapping, flipped to executable, entered from
// Go, and their result observed. If this passes, executable memory, the cache
// flush and the trampoline are all correct on this platform.
func TestEnterReturnsConstant(t *testing.T) {
	_, pc := exec(t, returnConstantCode)
	var ctx ExecContext
	if got := Enter(pc, &ctx); got != 42 {
		t.Errorf("generated code returned %d, want 42", got)
	}
}

// TestEnterPreservesGoroutine checks that the trampoline left R14/R28 alone.
// Allocating forces the runtime to consult g; if the trampoline had clobbered
// it, this crashes rather than fails.
func TestEnterPreservesGoroutine(t *testing.T) {
	_, pc := exec(t, returnConstantCode)
	var ctx ExecContext
	for i := 0; i < 1000; i++ {
		if got := Enter(pc, &ctx); got != 42 {
			t.Fatalf("iteration %d returned %d", i, got)
		}
		if s := make([]byte, 64); len(s) != 64 {
			t.Fatal("unreachable")
		}
	}
}
