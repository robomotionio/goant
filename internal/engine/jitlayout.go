package engine

import "unsafe"

// The layout facts compiled code depends on, in one place.
//
// A JIT that reads an object field reads it at a byte offset, and that offset
// has to agree with what Go compiled the struct to. Nothing enforces the
// agreement: a stale number here does not fail to build and does not fail a
// type check, it reads the wrong eight bytes and hands them back as a Value.
// Depending on which field it lands in that is a wrong answer, a pointer the
// collector follows into the middle of something else, or a crash a long way
// from the cause.
//
// So none of these are written down. Every one is derived from the Go
// declaration with unsafe.Offsetof or unsafe.Sizeof, which makes them constants
// the compiler recomputes whenever a struct changes — a field inserted into
// object moves jitOffObjShape by itself, and the emitter follows. What that
// cannot catch is a field changing meaning rather than position, which is what
// the round-trip test is for: it runs emitted code against real objects and
// compares with what Go reads from the same ones.
//
// The one thing that must stay true of every field named here is that it is not
// a Go pointer compiled code retains. Reading a *shape to compare it against a
// cached one is safe because the comparison is over immediately and the shape is
// reachable from the object throughout; parking one in a register across a
// safepoint would not be.

const (
	// Resolving a handle: chunks[h>>shift][h&mask].
	jitPoolChunkShift = poolChunkShift
	jitPoolChunkMask  = poolChunkMask

	// The chunk vector is a Go slice, so its data pointer is behind one more
	// load. The slice header's address is stable — the pool is heap-allocated
	// and Go does not move heap objects — but the data pointer itself is not,
	// because appending a chunk reallocates it. Compiled code therefore loads
	// the header every time rather than baking in the vector.
	jitOffPoolChunks = unsafe.Offsetof(pool[object]{}.chunks)

	jitSizeofPoolCell = unsafe.Sizeof(poolCell[object]{})
	jitOffCellElem    = unsafe.Offsetof(poolCell[object]{}.elem)
	jitOffCellLive    = unsafe.Offsetof(poolCell[object]{}.live)

	// The object header fields a property read touches before the value.
	jitOffObjSelf  = unsafe.Offsetof(object{}.self)
	jitOffObjProto = unsafe.Offsetof(object{}.proto)
	jitOffObjShape = unsafe.Offsetof(object{}.shape)
	jitOffObjInobj = unsafe.Offsetof(object{}.inobj)
	jitOffObjProxy = unsafe.Offsetof(object{}.proxy)

	// A shape's inobj limit, which decides whether a slot is in the object or
	// in its overflow slice.
	jitOffShapeInobjLimit = unsafe.Offsetof(shape{}.inobjLimit)

	// The invocation-dirty pair, which a compiled store has to maintain itself:
	// the handle below which an object predates the running invocation, and the
	// flag saying one was written to. See noteSharedMutation.
	//
	// invDirty is a bool with other bools around it, which is why the store to
	// it is a byte store rather than the word store everything else here uses.
	jitOffRTWatermark = unsafe.Offsetof(Runtime{}.invWatermark)
	jitOffRTDirty     = unsafe.Offsetof(Runtime{}.invDirty)

	// One cache site, and one way within it.
	jitSizeofICWay  = unsafe.Sizeof(icWay{})
	jitOffICWays    = unsafe.Offsetof(propIC{}.ways)
	jitOffICN       = unsafe.Offsetof(propIC{}.n)
	jitSizeofPropIC = unsafe.Sizeof(propIC{})

	jitOffWayShape    = unsafe.Offsetof(icWay{}.shape)
	jitOffWayEpoch    = unsafe.Offsetof(icWay{}.epoch)
	jitOffWaySlot     = unsafe.Offsetof(icWay{}.slot)
	jitOffWayToShape  = unsafe.Offsetof(icWay{}.toShape)
	jitOffWayHolder   = unsafe.Offsetof(icWay{}.holder)
	jitOffWayProtoVal = unsafe.Offsetof(icWay{}.protoVal)
)

// jitInobjSlots is how many properties live in the object itself, which is the
// only case a compiled read serves without a second indirection.
const jitInobjSlots = inobjMaxSlots

// jitObjectPoolAddr is the address of the object pool's slice header.
//
// Taken once per Runtime and passed to compiled code, rather than baked into it,
// because two Runtimes have two pools and code compiled in one must not read the
// other's.
func jitObjectPoolAddr(rt *Runtime) uintptr {
	return uintptr(unsafe.Pointer(rt.objects))
}

// jitEpochAddr is the address of the inline-cache generation counter.
//
// Baked into compiled code as an immediate rather than passed in, because
// unlike the pool it is one counter for the whole process — a cache retired by
// one Runtime is retired for all of them, which the comment on icEpochCounter
// explains is a cost rather than a wrong answer.
//
// Compiled code reads it with an ordinary 32-bit load. That is what
// atomic.LoadUint32 compiles to on amd64, and the value is only ever compared
// for equality against one recorded earlier: reading a stale epoch makes a live
// cache look retired, which costs a slow-path lookup and nothing else.
func jitEpochAddr() uintptr { return uintptr(unsafe.Pointer(&icEpochCounter)) }

// jitICHitAddr is the address of the compiled-hit counter, for the increment
// compiled code emits when GOANT_JIT_STATS is set.
func jitICHitAddr() uintptr { return uintptr(unsafe.Pointer(&jitStats.icHit)) }

// jitICPutHitAddr is the same for the store probe.
func jitICPutHitAddr() uintptr { return uintptr(unsafe.Pointer(&jitStats.putHit)) }

// jitGenericFastAddr is the address of the counter for generic operators whose
// operands turned out to be Numbers after all.
func jitGenericFastAddr() uintptr { return uintptr(unsafe.Pointer(&jitStats.genFast)) }

// jitRuntimeAddr is the address of the Runtime, passed to compiled code for the
// same reason the pool is: a compiled store maintains the invocation-dirty pair
// itself, and two Runtimes have two of them.
func jitRuntimeAddr(rt *Runtime) uintptr { return uintptr(unsafe.Pointer(rt)) }

// jitICWayAddr is the address of a cache site's first way.
//
// Constant for the life of the function: frameICs allocates the array once and
// nothing grows it, and Go does not move what it has allocated. The array is
// reachable from the svFunc that owns the compiled code, so it cannot be
// collected while any of that code can run.
func jitICWayAddr(ics []propIC, i int) uintptr {
	return uintptr(unsafe.Pointer(&ics[i].ways[0]))
}
