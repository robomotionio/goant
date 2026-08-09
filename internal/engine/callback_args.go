package engine

// One argument buffer per array iteration call, instead of one per element.
//
// Every generic array algorithm calls its callback as
// `cb(element, index, array)`, and every one of them built that argument list
// with a fresh `[]Value{...}` inside the loop. On a Function node doing
// `msg.records.forEach(...)` over a million records that is a million
// three-word slices, which was the single largest source of allocation in the
// call — V8 does the same work with none.
//
// The buffer can only be shared if nothing the callee does can keep it past its
// own return, and that is a property of the CALLEE, decided once before the
// loop rather than guessed at per element:
//
//   - an ordinary closure copies what it was handed. Parameters go into the
//     frame's locals, a rest parameter is copied into a fresh array, an
//     `arguments` object copies each value into its own property (its parameter
//     map addresses the LOCALS, not this slice), and a compiled frame copies
//     into locals as well. When it returns, nothing refers to the buffer.
//   - an async function or a generator does NOT. newGenState stores the slice
//     itself — `args: args`, no copy — and that state outlives the call, so a
//     shared buffer would still be reachable from a suspended frame long after
//     the loop moved on. `forEach` with an async callback is perfectly legal
//     JavaScript; it simply does not await.
//
//     Refusing them is defence in depth rather than a demonstrated fix, and the
//     distinction is worth writing down. Every attempt to make the corruption
//     VISIBLE failed: `arguments` is materialised during declaration
//     instantiation, at frame entry, before the buffer can be rewritten — even
//     when it is captured by an arrow and only read after the loop, and even
//     for a generator, because newGenerator drives the frame to its body
//     barrier eagerly at call time. So today the values are copied out before
//     anything could disturb them.
//
//     That is an argument about WHEN the arguments object happens to be built,
//     which is not where a rule about lifetimes belongs. The retention is real
//     and in the code; the only thing hiding it is materialisation order, and
//     nothing makes that order a promise. The gate costs an allocation per
//     element for callees nobody puts in a hot loop.
//   - a Proxy copies into a JS array for the apply trap, but its target may be
//     any of the above, so it is refused rather than reasoned about.
//   - a native is refused too. Nothing stops one from retaining the slice it
//     was handed, and "no built-in currently does" is not a property anybody
//     could keep true by accident.
//
// A refusal is not a correctness question, only a lost optimisation: the caller
// allocates per element exactly as it did before.
type callbackArgs struct {
	buf   []Value
	reuse bool
}

// newCallbackArgs decides once whether cb's arguments may share a buffer, and
// allocates that buffer if so. n is how many arguments the algorithm passes.
func (rt *Runtime) newCallbackArgs(cb Value, n int) callbackArgs {
	o := rt.objPtr(cb)
	if o == nil || o.proxy != nil || o.native != nil {
		return callbackArgs{}
	}
	cl := o.clPtr
	if cl == nil || cl.fn == nil || cl.fn.isAsync || cl.fn.isGenerator {
		return callbackArgs{}
	}
	return callbackArgs{buf: make([]Value, n), reuse: true}
}

// three fills the buffer for the `cb(element, index, array)` shape.
//
// Fixed arity rather than variadic: a `...Value` parameter allocates its own
// slice at every call site, which is the allocation this exists to remove.
func (c *callbackArgs) three(a, b, d Value) []Value {
	if !c.reuse {
		return []Value{a, b, d}
	}
	c.buf[0], c.buf[1], c.buf[2] = a, b, d
	return c.buf
}

// four is `cb(accumulator, element, index, array)`, for the reducers.
func (c *callbackArgs) four(a, b, d, e Value) []Value {
	if !c.reuse {
		return []Value{a, b, d, e}
	}
	c.buf[0], c.buf[1], c.buf[2], c.buf[3] = a, b, d, e
	return c.buf
}
