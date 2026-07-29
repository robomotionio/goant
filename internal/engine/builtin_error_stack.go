package engine

import "strings"

// V8's Error stack API: Error.captureStackTrace, Error.stackTraceLimit and
// Error.prepareStackTrace, with the CallSite objects prepareStackTrace receives.
//
// None of this is in ECMA-262 — it is V8's, and it is being standardised as the
// Error Stacks proposal. It is here because real code depends on it: an error
// library that calls Error.captureStackTrace(this, MyError) to hide its own
// constructor from the trace gets a TypeError instead, and that is a crash in
// the library's happy path rather than an edge case.
//
// A trace is captured when the error is constructed, not when .stack is read —
// by then the frames are long gone. The captured call sites are stored in an
// internal slot as an ordinary Array, so the collector traces them like any
// other slot value and nothing has to teach it about a side table.

// A capture is a flat Array of csStride Values per frame — name, file, flags —
// rather than an array of CallSite objects.
//
// Every `new Error` captures, including the ones a program throws and catches in
// a loop and never asks for a stack from, so the capture has to be nearly free.
// Building CallSite objects eagerly made an error cost 2.2x what it did before
// (200k throw/catch: 108ms -> 236ms); a flat array of interned strings is one
// allocation for the whole trace. The objects are built at .stack, and only if
// an Error.prepareStackTrace is installed to receive them.
const (
	csFuncName = iota
	csFileName
	csFlags
	csStride
)

// Flag bits packed into the csFlags element.
const (
	csFlagToplevel = 1 << iota
	csFlagEval
	csFlagConstructor
)

// defaultStackTraceLimit matches V8's: ten frames unless the script says
// otherwise.
const defaultStackTraceLimit = 10

// installErrorStackAPI defines the three Error statics and builds the CallSite
// prototype. errCtor is %Error%.
func (rt *Runtime) installErrorStackAPI(errCtor Value) {
	ec := rt.objPtr(errCtor)

	// Error.stackTraceLimit is an ordinary writable data property: scripts raise
	// it, lower it, and set it to a non-number to mean "capture nothing".
	ec.defineOwn("stackTraceLimit", mknum(defaultStackTraceLimit), attrWritable|attrEnumerable|attrConfigurable)

	// Error.captureStackTrace(target[, constructorOpt]) installs an own "stack"
	// property on target — any object, not just an error. constructorOpt names a
	// function to cut the trace at, so a library can hide its own frames.
	ec.defineOwn("captureStackTrace", rt.newNativeFunc("captureStackTrace", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		target := arg(args, 0)
		if !target.IsObjectLike() {
			return mkundef(), rt.typeError("Error.captureStackTrace requires an object")
		}
		sites := rt.captureCallSites(arg(args, 1))
		rt.objPtr(target).setSlot(slotErrorFrames, sites)
		// The property is an own data property that shadows the inherited
		// accessor, and it is writable: assigning to err.stack after capturing is
		// ordinary code, and V8 allows it.
		str, e := rt.formatStack(target, sites)
		if e != nil {
			return mkundef(), e
		}
		rt.objPtr(target).defineOwn("stack", str, attrWritable|attrConfigurable)
		return mkundef(), nil
	}), attrWritable|attrConfigurable)

	// Error.prepareStackTrace is undefined until a script installs one. Defining
	// it here (rather than leaving it absent) is what V8 does, and it keeps
	// `Error.prepareStackTrace = f; ...; Error.prepareStackTrace = undefined`
	// from adding and removing a property shape on the constructor.
	ec.defineOwn("prepareStackTrace", mkundef(), attrWritable|attrEnumerable|attrConfigurable)

	rt.callSiteProto = rt.makeCallSiteProto()
}

// stackTraceLimit reads Error.stackTraceLimit, clamped to something sane. A
// non-number means capture nothing, which is how scripts turn traces off.
func (rt *Runtime) stackTraceLimit() int {
	v, e := rt.getField(rt.errors.base, "stackTraceLimit")
	if e != nil || !v.IsNumber() {
		return 0
	}
	n := v.Number()
	if n <= 0 {
		return 0
	}
	if n > 1000 {
		return 1000
	}
	return int(n)
}

// captureCallSites walks the live frames innermost-first and returns them as an
// Array of CallSite objects, honouring Error.stackTraceLimit.
//
// skipUntil, when callable, names a function to cut the trace at: every frame up
// to and including it is dropped, which is how Error.captureStackTrace(this,
// MyError) hides a library's own constructor.
func (rt *Runtime) captureCallSites(skipUntil Value) Value {
	limit := rt.stackTraceLimit()
	if limit == 0 {
		// Not an empty array: a capture that recorded nothing must be
		// distinguishable from no capture at all, and allocating one to say so
		// defeats the point of turning traces off.
		return mkundef()
	}
	arr := rt.newArray()
	ao := rt.objPtr(arr)

	skipping := rt.isCallable(skipUntil)
	// Frame d lives at rt.frames[d] and depth counts up from the outermost, so
	// walking down yields innermost-first, which is the order V8 reports.
	for d := rt.frameDepth; d >= 1 && int(ao.arrLen) < limit*csStride; d-- {
		if d >= len(rt.frames) {
			continue
		}
		f := &rt.frames[d]
		// A builtin or a guarded Go recursion spends from frameDepth without
		// publishing; claimNonJSFrame clears fn so those read as empty here.
		if f.fn == nil {
			continue
		}
		if skipping {
			if f.fnVal == skipUntil {
				skipping = false
			}
			continue
		}
		flags := 0
		// A frame whose `this` is undefined is top-level in the sense CallSite
		// means: not invoked as a method of anything.
		if f.thisVal.IsUndefined() {
			flags |= csFlagToplevel
		}
		if f.fn.filename == "<eval>" {
			flags |= csFlagEval
		}
		if !f.newTarget.IsUndefined() {
			flags |= csFlagConstructor
		}
		// Interned: a trace names the same handful of functions and files over and
		// over, so this is a map hit rather than a string allocation.
		rt.arraySet(ao, ao.arrLen, rt.internString(f.fn.name))
		rt.arraySet(ao, ao.arrLen, rt.internString(f.fn.filename))
		rt.arraySet(ao, ao.arrLen, mknum(float64(flags)))
	}
	return arr
}

// newCallSite builds the CallSite for frame i of a capture. Called only when a
// script installed an Error.prepareStackTrace to receive them.
func (rt *Runtime) newCallSite(capture Value, i int) Value {
	fields := rt.newArray()
	fo := rt.objPtr(fields)
	for k := range csStride {
		v, _ := rt.getElement(capture, mknum(float64(i*csStride+k)))
		rt.arraySet(fo, uint32(k), v)
	}
	cs := rt.newObject(rt.callSiteProto)
	rt.objPtr(cs).setSlot(slotCallSite, fields)
	return cs
}

// makeCallSiteProto builds the prototype shared by every CallSite. The methods
// read the receiver's field array; a receiver without one is not a CallSite and
// reports the empty answer rather than throwing, because prepareStackTrace
// implementations routinely call these defensively.
func (rt *Runtime) makeCallSiteProto() Value {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)

	field := func(this Value, i int) Value {
		o := rt.objPtr(this)
		if o == nil {
			return mkundef()
		}
		fields := o.getSlot(slotCallSite)
		if !fields.IsObjectLike() {
			return mkundef()
		}
		v, _ := rt.getElement(fields, mknum(float64(i)))
		return v
	}
	get := func(name string, i int) {
		rt.defMethod(po, name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return field(this, i), nil
		})
	}
	get("getFunctionName", csFuncName)
	get("getMethodName", csFuncName)
	get("getFileName", csFileName)
	get("getScriptNameOrSourceURL", csFileName)

	flag := func(name string, bit int) {
		rt.defMethod(po, name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			v := field(this, csFlags)
			return mkbool(v.IsNumber() && int(v.Number())&bit != 0), nil
		})
	}
	flag("isToplevel", csFlagToplevel)
	flag("isEval", csFlagEval)
	flag("isConstructor", csFlagConstructor)

	// The receiver is not retained past the capture, so the type it was called on
	// cannot be recovered. null is the answer V8 gives for a frame with no type.
	rt.defMethod(po, "getTypeName", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mknull(), nil
	})
	rt.defMethod(po, "isNative", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkbool(false), nil
	})

	// goant records a byte offset per function, not a line table, so a position
	// cannot be reported honestly. null is what V8 returns for a frame it has no
	// position for, and it is what a caller already has to handle.
	null := func(name string) {
		rt.defMethod(po, name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			return mknull(), nil
		})
	}
	null("getLineNumber")
	null("getColumnNumber")
	null("getEvalOrigin")
	null("getThis")
	null("getFunction")
	null("getPromiseIndex")

	rt.defMethod(po, "isAsync", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkbool(false), nil
	})
	rt.defMethod(po, "isPromiseAll", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return mkbool(false), nil
	})
	rt.defMethod(po, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.newString(rt.callSiteText(this)), nil
	})
	return proto
}

// callSiteText renders "name (file)" from a capture's frame i.
func (rt *Runtime) frameText(capture Value, i int) string {
	str := func(k int) string {
		v, _ := rt.getElement(capture, mknum(float64(i*csStride+k)))
		if !v.IsString() {
			return ""
		}
		return rt.strGo(v)
	}
	name, file := str(csFuncName), str(csFileName)
	if name == "" {
		name = "<anonymous>"
	}
	if file == "" {
		return name
	}
	return name + " (" + file + ")"
}

// callSiteText renders one CallSite object the way it appears in a stack string.
func (rt *Runtime) callSiteText(cs Value) string {
	o := rt.objPtr(cs)
	if o == nil {
		return "<anonymous>"
	}
	fields := o.getSlot(slotCallSite)
	if !fields.IsObjectLike() {
		return "<anonymous>"
	}
	return rt.frameText(fields, 0)
}

// formatStack produces the value of `stack`: whatever Error.prepareStackTrace
// returns if a script installed one, otherwise the conventional
// "Name: message\n    at frame" rendering.
func (rt *Runtime) formatStack(err, capture Value) (Value, *ThrowError) {
	n, _ := rt.lengthOf(capture)
	n /= csStride

	prep, e := rt.getField(rt.errors.base, "prepareStackTrace")
	if e == nil && rt.isCallable(prep) {
		// The CallSite objects exist only for this call — the cost of building
		// them is why the capture itself does not.
		sites := rt.newArray()
		so := rt.objPtr(sites)
		for i := range n {
			rt.arraySet(so, so.arrLen, rt.newCallSite(capture, i))
		}
		v, e := rt.callValue(prep, rt.errors.base, []Value{err, sites})
		if e != nil {
			return mkundef(), e
		}
		return v, nil
	}

	var b strings.Builder
	b.WriteString(rt.errorHeadline(err))
	for i := range n {
		b.WriteString("\n    at ")
		b.WriteString(rt.frameText(capture, i))
	}
	return rt.newString(b.String()), nil
}
