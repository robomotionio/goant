//go:build amd64 || arm64

package engine

import (
	"testing"
	"unsafe"

	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// buildResolver assembles a standalone function that does what jitEmitResolve
// emits and nothing else: Args[0] is the pool address, Args[1] a Value, and Ret
// comes back as the address of the object it names.
//
// Standalone because the thing being checked is the arithmetic on the layout
// constants, and running it inside a compiled JavaScript function would make a
// wrong offset show up as a wrong property value several steps later.
func buildResolver(t testing.TB) (*jitmem.Block, uintptr) {
	t.Helper()
	a := jitasm.NewAsm()
	notObject := a.NewLabel()

	pool := jitStackRegs[3]
	val := jitStackRegs[4]
	obj := jitRegExit

	a.MovRegMem(pool, jitasm.RegCtx, jitmem.CtxOffArgs)
	a.MovRegMem(val, jitasm.RegCtx, jitmem.CtxOffArgs+8)
	jitEmitTagCheck(a, val, TObj, notObject)
	jitEmitResolve(a, obj, val, pool)
	a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffRet, obj)
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitReturn))
	a.MovRegImm64(jitRegExit, jitmem.ExitReturn)
	a.Ret()

	// Anything that is not an object comes back as zero, which no live cell is.
	a.Bind(notObject)
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffRet, 0)
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitReturn))
	a.MovRegImm64(jitRegExit, jitmem.ExitReturn)
	a.Ret()

	buf := a.Code()
	b, err := jitmem.Alloc(len(buf))
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if _, err := b.Write(buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := b.Protect(); err != nil {
		t.Fatalf("Protect: %v", err)
	}
	return b, b.Addr()
}

// TestJITResolvesAHandleLikeGoDoes is what makes the layout constants
// trustworthy. Nothing else does: an offset that is merely wrong still compiles,
// still runs, and reads eight bytes belonging to some other field.
//
// So the check is not that the constants equal what unsafe.Offsetof says — they
// are unsafe.Offsetof — but that machine code built from them lands on the same
// object Go lands on, over a spread of handles wide enough to cross a chunk
// boundary.
func TestJITResolvesAHandleLikeGoDoes(t *testing.T) {
	rt := New()
	b, pc := buildResolver(t)
	defer b.Free()

	poolAddr := jitObjectPoolAddr(rt)
	run := func(v Value) uintptr {
		ctx := &jitmem.ExecContext{Args: [4]uint64{uint64(poolAddr), uint64(v)}}
		jitmem.Enter(pc, ctx)
		return uintptr(ctx.Ret)
	}

	// Enough objects to span more than one chunk, so the shift and the mask are
	// both exercised rather than only the low chunk.
	var vals []Value
	for i := 0; i < poolChunkSize+64; i++ {
		vals = append(vals, rt.newObject(rt.objectProto))
	}

	checked := 0
	for i, v := range vals {
		want := uintptr(unsafe.Pointer(rt.objPtr(v)))
		if want == 0 {
			t.Fatalf("object %d did not resolve in Go", i)
		}
		if got := run(v); got != want {
			t.Fatalf("object %d (handle %d): emitted code resolved %#x, Go resolves %#x",
				i, v.Data(), got, want)
		}
		checked++
	}
	if checked < poolChunkSize {
		t.Fatalf("only %d objects checked, want more than one chunk", checked)
	}

	// The guard has to reject everything that is not an object, because the
	// resolution downstream of it does no bounds check of its own.
	for _, v := range []Value{
		mkundef(), mknull(), mkbool(true), mkbool(false),
		tov(0), tov(1), tov(-1), tov(1e308),
		rt.newString("x"),
	} {
		if got := run(v); got != 0 {
			t.Errorf("a %v was resolved to %#x instead of being refused", v.Type(), got)
		}
	}
}

// TestJITObjFieldOffsets reads the header fields a property access needs,
// through emitted code, and compares each with Go.
func TestJITObjFieldOffsets(t *testing.T) {
	rt := New()
	o := rt.newObject(rt.objectProto)
	op := rt.objPtr(o)
	rt.objPtr(o).defineOwn("a", tov(42), attrDefault)

	load := func(disp uintptr) uint64 {
		a := jitasm.NewAsm()
		notObject := a.NewLabel()
		a.MovRegMem(jitStackRegs[3], jitasm.RegCtx, jitmem.CtxOffArgs)
		a.MovRegMem(jitStackRegs[4], jitasm.RegCtx, jitmem.CtxOffArgs+8)
		jitEmitTagCheck(a, jitStackRegs[4], TObj, notObject)
		jitEmitResolve(a, jitRegExit, jitStackRegs[4], jitStackRegs[3])
		a.MovRegMem(jitRegExit, jitRegExit, int32(disp))
		a.Bind(notObject)
		a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffRet, jitRegExit)
		a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitReturn))
		a.MovRegImm64(jitRegExit, jitmem.ExitReturn)
		a.Ret()

		buf := a.Code()
		b, err := jitmem.Alloc(len(buf))
		if err != nil {
			t.Fatal(err)
		}
		defer b.Free()
		b.Write(buf)
		b.Protect()
		ctx := &jitmem.ExecContext{Args: [4]uint64{uint64(jitObjectPoolAddr(rt)), uint64(o)}}
		jitmem.Enter(b.Addr(), ctx)
		return ctx.Ret
	}

	if got, want := load(jitOffObjShape), uint64(uintptr(unsafe.Pointer(op.shape))); got != want {
		t.Errorf("shape: emitted %#x, Go %#x", got, want)
	}
	if got, want := load(jitOffObjProto), uint64(op.proto); got != want {
		t.Errorf("proto: emitted %#x, Go %#x", got, want)
	}
	if got, want := load(jitOffObjInobj), uint64(op.inobj[0]); got != want {
		t.Errorf("inobj[0]: emitted %#x, Go %#x", got, want)
	}
	if got, want := load(jitOffObjSelf)&0xFFFFFFFF, uint64(op.self); got != want {
		t.Errorf("self: emitted %#x, Go %#x", got, want)
	}
}
