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
	// A nullish SYNC resource is skipped entirely. A nullish ASYNC one still gets
	// a record with no method: AddDisposableResource keeps it so that disposal
	// performs its Await, which is what makes the statements after the block run
	// in a later microtask.
	if resource.IsNullish() && !async {
		return resource, nil
	}
	m := mkundef()
	if !resource.IsNullish() {
		mv, e := rt.getDisposeMethod(resource, async)
		if e != nil {
			return mkundef(), e
		}
		m = mv
	}
	eo := rt.objPtr(entries)
	if eo == nil {
		return mkundef(), rt.typeError("invalid using disposal stack")
	}
	rec := rt.newArray()
	ro := rt.objPtr(rec)
	rt.arraySet(ro, 0, resource)
	rt.arraySet(ro, 1, m)
	// Whether the record is an ASYNC one: its disposal result has to be awaited,
	// which is the only way a rejected async disposer is observed at all.
	rt.arraySet(ro, 2, mkbool(async))
	rt.arraySet(eo, eo.arrLen, rec)
	return resource, nil
}

// disposeEntriesAsync is disposeEntries for an async disposal environment (a
// block holding an `await using`): every disposal result is AWAITED, so an async
// disposer that rejects folds into the completion like a synchronous throw. It
// may only run inside a coroutine, which `await using` guarantees.
func (rt *Runtime) disposeEntriesAsync(entries, completion Value) Value {
	for _, rec := range rt.takeDisposalRecords(entries) {
		ro := rt.objPtr(rec)
		resource, method := ro.arr[0], ro.arr[1]
		res := mkundef()
		if rt.isCallable(method) {
			v, e := rt.callValue(method, resource, nil)
			if e != nil {
				completion = rt.suppressDisposalError(e.Value, completion)
				continue
			}
			res = v
		}
		// Only an async record awaits; a plain `[Symbol.dispose]` in an async
		// environment still completes synchronously.
		if ro.arrLen > 2 && rt.toBoolean(ro.arr[2]) {
			if _, inject := rt.suspend(res, true); inject != nil && inject.kind == genThrow {
				completion = rt.suppressDisposalError(inject.val, completion)
			}
		}
	}
	return completion
}

// takeDisposalRecords drains the block's disposal stack, returning its records
// in disposal (reverse) order.
func (rt *Runtime) takeDisposalRecords(entries Value) []Value {
	eo := rt.objPtr(entries)
	if eo == nil {
		return nil
	}
	n := int(eo.arrLen)
	out := make([]Value, 0, n)
	for i := n - 1; i >= 0; i-- {
		if i < len(eo.arr) {
			if ro := rt.objPtr(eo.arr[i]); ro != nil && ro.arrLen >= 2 {
				out = append(out, eo.arr[i])
			}
		}
	}
	eo.arrLen = 0
	eo.arr = eo.arr[:0]
	return out
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
		if !rt.isCallable(method) {
			continue // a methodless record exists only to force an async Await
		}
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
