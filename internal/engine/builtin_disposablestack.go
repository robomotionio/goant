package engine

// DisposableStack / AsyncDisposableStack (Explicit Resource Management). A stack
// records disposers (use / adopt / defer) and runs them in reverse on dispose,
// folding any disposal error into a SuppressedError chain.

const (
	dispKindDefer = 0 // onDispose()            — no receiver, no argument
	dispKindAdopt = 1 // onDispose(value)       — no receiver, value argument
	dispKindUse   = 2 // value[@@dispose]()     — value receiver, no argument
)

// stackState returns a DisposableStack's records array, or an error if `this`
// is not a (live) stack. disposed reports the disposed flag.
func (rt *Runtime) stackState(this Value) (o *object, entries Value, disposed bool, err *ThrowError) {
	o = rt.objPtr(this)
	if o == nil {
		return nil, mkundef(), false, rt.typeError("not a DisposableStack")
	}
	entries = o.getSlot(slotEntries)
	if entries == 0 || !entries.IsObjectType() {
		return nil, mkundef(), false, rt.typeError("not a DisposableStack")
	}
	return o, entries, rt.toBoolean(o.getSlot(slotData)), nil
}

// pushDisposer appends a [kind, value, method] record onto a stack's entries.
func (rt *Runtime) pushDisposer(entries Value, kind int, value, method Value) {
	rec := rt.newArray()
	ro := rt.objPtr(rec)
	rt.arraySet(ro, 0, mknum(float64(kind)))
	rt.arraySet(ro, 1, value)
	rt.arraySet(ro, 2, method)
	eo := rt.objPtr(entries)
	rt.arraySet(eo, eo.arrLen, rec)
}

// callDisposer invokes one record according to its kind.
func (rt *Runtime) callDisposer(rec Value) *ThrowError {
	ro := rt.objPtr(rec)
	if ro == nil || ro.arrLen < 3 {
		return nil
	}
	kind := int(ro.arr[0].Number())
	value, method := ro.arr[1], ro.arr[2]
	var e *ThrowError
	switch kind {
	case dispKindUse:
		_, e = rt.callValue(method, value, nil)
	case dispKindAdopt:
		_, e = rt.callValue(method, mkundef(), []Value{value})
	default:
		_, e = rt.callValue(method, mkundef(), nil)
	}
	return e
}

// drainRecords snapshots and clears a stack's entries (reverse-iteration order).
func (rt *Runtime) drainRecords(entries Value) []Value {
	eo := rt.objPtr(entries)
	if eo == nil {
		return nil
	}
	n := int(eo.arrLen)
	recs := make([]Value, n)
	for i := 0; i < n && i < len(eo.arr); i++ {
		recs[i] = eo.arr[i]
	}
	eo.arrLen = 0
	eo.arr = eo.arr[:0]
	return recs
}

func (rt *Runtime) initDisposableStack() {
	rt.defineDisposableStack("DisposableStack", false)
	rt.defineDisposableStack("AsyncDisposableStack", true)
}

func (rt *Runtime) defineDisposableStack(name string, async bool) {
	proto := rt.newObject(rt.objectProto)
	po := rt.objPtr(proto)
	rt.setStringTag(proto, name)

	ctor := rt.newNativeFunc(name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !rt.constructing() {
			return mkundef(), rt.typeError("Constructor " + name + " requires 'new'")
		}
		o := rt.objPtr(this)
		if o == nil {
			return mkundef(), rt.typeError("Constructor " + name + " requires 'new'")
		}
		o.setSlot(slotEntries, rt.newArray())
		o.setSlot(slotData, mkbool(false))
		return this, nil
	})
	rt.objPtr(ctor).defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)

	// use(value): register value's own [@@dispose]/[@@asyncDispose]; returns value.
	rt.defMethod(po, "use", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, entries, disposed, e := rt.stackState(this)
		if e != nil {
			return mkundef(), e
		}
		_ = o
		if disposed {
			return mkundef(), rt.referenceError(name + " already disposed")
		}
		value := arg(args, 0)
		if value.IsNullish() {
			return value, nil
		}
		m, e := rt.getDisposeMethod(value, async)
		if e != nil {
			return mkundef(), e
		}
		rt.pushDisposer(entries, dispKindUse, value, m)
		return value, nil
	})

	// adopt(value, onDispose): register onDispose(value); returns value.
	rt.defMethod(po, "adopt", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, entries, disposed, e := rt.stackState(this)
		if e != nil {
			return mkundef(), e
		}
		if disposed {
			return mkundef(), rt.referenceError(name + " already disposed")
		}
		value := arg(args, 0)
		cb := arg(args, 1)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("onDispose is not callable")
		}
		rt.pushDisposer(entries, dispKindAdopt, value, cb)
		return value, nil
	})

	// defer(onDispose): register onDispose(); returns undefined.
	rt.defMethod(po, "defer", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		_, entries, disposed, e := rt.stackState(this)
		if e != nil {
			return mkundef(), e
		}
		if disposed {
			return mkundef(), rt.referenceError(name + " already disposed")
		}
		cb := arg(args, 0)
		if !rt.isCallable(cb) {
			return mkundef(), rt.typeError("onDispose is not callable")
		}
		rt.pushDisposer(entries, dispKindDefer, mkundef(), cb)
		return mkundef(), nil
	})

	// move(): transfer resources to a new stack; the old stack is emptied+disposed.
	rt.defMethod(po, "move", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, entries, disposed, e := rt.stackState(this)
		if e != nil {
			return mkundef(), e
		}
		if disposed {
			return mkundef(), rt.referenceError(name + " already disposed")
		}
		ns := rt.newObject(proto)
		no := rt.objPtr(ns)
		no.setSlot(slotEntries, entries)
		no.setSlot(slotData, mkbool(false))
		o.setSlot(slotEntries, rt.newArray())
		o.setSlot(slotData, mkbool(true))
		return ns, nil
	})

	// disposed getter.
	getter := rt.newNativeFunc("get disposed", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil {
			return mkundef(), rt.typeError("not a " + name)
		}
		return mkbool(rt.toBoolean(o.getSlot(slotData))), nil
	})
	po.defineAccessor("disposed", getter, mkundef(), true, false, attrConfigurable)

	if async {
		disposeAsync := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, entries, disposed, e := rt.stackState(this)
			if e != nil {
				return rt.rejectedPromise(e.Value), nil
			}
			if disposed {
				return rt.resolvedPromise(mkundef()), nil
			}
			o.setSlot(slotData, mkbool(true))
			return rt.disposeStackAsync(entries), nil
		}
		rt.defMethod(po, "disposeAsync", 0, disposeAsync)
		if rt.symAsyncDispose != 0 {
			po.defineOwnSymbol(rt.symAsyncDispose.handle(), rt.newNativeFunc("[Symbol.asyncDispose]", 0, disposeAsync), attrWritable|attrConfigurable)
		}
	} else {
		dispose := func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o, entries, disposed, e := rt.stackState(this)
			if e != nil {
				return mkundef(), e
			}
			if disposed {
				return mkundef(), nil
			}
			o.setSlot(slotData, mkbool(true))
			recs := rt.drainRecords(entries)
			completion := mkundef()
			for i := len(recs) - 1; i >= 0; i-- {
				if de := rt.callDisposer(recs[i]); de != nil {
					completion = rt.suppressDisposalError(de.Value, completion)
				}
			}
			if !completion.IsUndefined() {
				return mkundef(), &ThrowError{Value: completion, rt: rt}
			}
			return mkundef(), nil
		}
		rt.defMethod(po, "dispose", 0, dispose)
		if rt.symDispose != 0 {
			po.defineOwnSymbol(rt.symDispose.handle(), rt.newNativeFunc("[Symbol.dispose]", 0, dispose), attrWritable|attrConfigurable)
		}
	}

	rt.defGlobal(name, ctor)
}

// disposeStackAsync runs an AsyncDisposableStack's disposers in reverse,
// awaiting any promise a disposer returns, and settles the returned promise
// (rejecting with the aggregated completion if any disposer failed).
func (rt *Runtime) disposeStackAsync(entries Value) Value {
	recs := rt.drainRecords(entries)
	result, ro := rt.makePromise()
	var step func(i int, completion Value)
	step = func(i int, completion Value) {
		for i >= 0 {
			rec := recs[i]
			i--
			res, e := rt.callDisposerValue(rec)
			if e != nil {
				completion = rt.suppressDisposalError(e.Value, completion)
				continue
			}
			if rt.isPromise(res) {
				idx, comp := i, completion
				onF := rt.newNativeFunc("", 1, func(rt *Runtime, this Value, a []Value) (Value, *ThrowError) {
					step(idx, comp)
					return mkundef(), nil
				})
				onR := rt.newNativeFunc("", 1, func(rt *Runtime, this Value, a []Value) (Value, *ThrowError) {
					step(idx, rt.suppressDisposalError(arg(a, 0), comp))
					return mkundef(), nil
				})
				rt.promiseThen(onF, onR, rt.objPtr(res))
				return
			}
		}
		if completion.IsUndefined() {
			rt.resolvePromise(result, ro, mkundef())
		} else {
			rt.rejectPromise(ro, completion)
		}
	}
	step(len(recs)-1, mkundef())
	return result
}

// callDisposerValue is callDisposer but returns the disposer's result value
// (for the async path, so a returned promise can be awaited).
func (rt *Runtime) callDisposerValue(rec Value) (Value, *ThrowError) {
	ro := rt.objPtr(rec)
	if ro == nil || ro.arrLen < 3 {
		return mkundef(), nil
	}
	kind := int(ro.arr[0].Number())
	value, method := ro.arr[1], ro.arr[2]
	switch kind {
	case dispKindUse:
		return rt.callValue(method, value, nil)
	case dispKindAdopt:
		return rt.callValue(method, mkundef(), []Value{value})
	default:
		return rt.callValue(method, mkundef(), nil)
	}
}
