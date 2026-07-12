package engine

// Explicit Resource Management (`using` / `await using`) runtime support. The
// compiler lowers a block containing `using` declarations to a disposal-record
// stack plus the USING_PUSH / USING_DISPOSE / USING_DISPOSE_SUPPRESSED opcodes
// (ant src/silver/ops/using.h). Only the synchronous path is implemented here.

// getDisposeMethod implements GetDisposeMethod(V, hint): [Symbol.dispose] for a
// sync resource, or [Symbol.asyncDispose] (falling back to [Symbol.dispose]) for
// an async one. The method must be callable.
func (rt *Runtime) getDisposeMethod(resource Value, async bool) (Value, *ThrowError) {
	m := mkundef()
	if async && rt.symAsyncDispose != 0 {
		mv, e := rt.getElement(resource, rt.symAsyncDispose)
		if e != nil {
			return mkundef(), e
		}
		m = mv
	}
	if m.IsNullish() && rt.symDispose != 0 {
		mv, e := rt.getElement(resource, rt.symDispose)
		if e != nil {
			return mkundef(), e
		}
		m = mv
	}
	if !rt.isCallable(m) {
		return mkundef(), rt.typeError("resource is not disposable")
	}
	return m, nil
}

// usingPush registers a resource on the block's disposal stack (a JS array of
// [resource, method] records). A nullish resource is a no-op. It returns the
// resource so the compiler can bind it to the `using` variable.
func (rt *Runtime) usingPush(entries, resource Value, async bool) (Value, *ThrowError) {
	if resource.IsNullish() {
		return resource, nil
	}
	m, e := rt.getDisposeMethod(resource, async)
	if e != nil {
		return mkundef(), e
	}
	eo := rt.objPtr(entries)
	if eo == nil {
		return mkundef(), rt.typeError("invalid using disposal stack")
	}
	rec := rt.newArray()
	ro := rt.objPtr(rec)
	rt.arraySet(ro, 0, resource)
	rt.arraySet(ro, 1, m)
	rt.arraySet(eo, eo.arrLen, rec)
	return resource, nil
}

// disposeEntries runs the disposal records in reverse order, folding any
// disposal error into `completion` via SuppressedError, and drains the stack.
func (rt *Runtime) disposeEntries(entries, completion Value) Value {
	eo := rt.objPtr(entries)
	if eo == nil {
		return completion
	}
	n := int(eo.arrLen)
	records := make([]Value, n)
	for i := 0; i < n && i < len(eo.arr); i++ {
		records[i] = eo.arr[i]
	}
	eo.arrLen = 0
	eo.arr = eo.arr[:0]
	for i := n - 1; i >= 0; i-- {
		ro := rt.objPtr(records[i])
		if ro == nil || ro.arrLen < 2 {
			continue
		}
		resource, method := ro.arr[0], ro.arr[1]
		if _, e := rt.callValue(method, resource, nil); e != nil {
			completion = rt.suppressDisposalError(e.Value, completion)
		}
	}
	return completion
}

// suppressDisposalError folds a new disposal error into the running completion:
// the first error stands alone; each later one wraps the previous completion as
// its [[Suppressed]] value.
func (rt *Runtime) suppressDisposalError(err, previous Value) Value {
	if previous.IsUndefined() {
		return err
	}
	return rt.makeSuppressedError(err, previous)
}

// makeSuppressedError builds a SuppressedError with .error / .suppressed set.
func (rt *Runtime) makeSuppressedError(err, suppressed Value) Value {
	obj := rt.newObject(rt.errors.suppressedProto)
	o := rt.objPtr(obj)
	o.setSlot(slotBrand, mknum(brandError))
	o.defineOwn("error", err, attrWritable|attrConfigurable)
	o.defineOwn("suppressed", suppressed, attrWritable|attrConfigurable)
	o.defineOwn("message", rt.newString("An error was suppressed during disposal."), attrWritable|attrConfigurable)
	return obj
}
