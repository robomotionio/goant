package engine

// `import defer * as ns from "m"`: a module linked with the rest of the graph
// and then left unrun. What runs it is the first question anyone asks its
// namespace about a string key.
//
// The deferred namespace is a separate object from the module's ordinary one --
// a module imported both ways has two namespaces, and they are not equal -- and
// it is built at link time, because the keys are known then. Only the VALUES
// need the module to have run, which is why asking about a key is the trigger
// and asking about the object is not: the prototype, the extensibility and the
// key list are all answerable without running anything.
//
// "then" is exempt. A deferred namespace has to be safe to hand to `await` and
// to promise resolution, both of which read `.then` off whatever they are given,
// and neither is asking for the module.

// deferredNamespace is the deferred namespace object of a module, built once and
// kept on the record. A module already evaluated still gets one: the namespace a
// deferred import hands back is a deferred namespace whatever state the module
// is in, which is how a JSON module -- evaluated before it is even linked --
// still answers "Deferred Module".
func (rt *Runtime) deferredNamespace(m *moduleRecord) Value {
	if m.hasDeferredNS {
		return m.deferredNS
	}
	ns := rt.newObject(mknull())
	no := rt.objPtr(ns)
	m.deferredNS, m.hasDeferredNS = ns, true
	for _, name := range rt.exportNames(m, nil) {
		target, ambiguous := rt.resolveExport(m, name, nil)
		if ambiguous || !target.found() {
			continue // an ambiguous name is simply absent from the namespace
		}
		t, exported := target, name
		get := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			// A module may export something called "then", and reading it is
			// still not a question about the module: until it has run of its own
			// accord, the answer is undefined rather than its value.
			if exported == "then" && !m.status.settled() {
				return mkundef(), nil
			}
			if e := rt.runDeferred(m); e != nil {
				return mkundef(), e
			}
			if t.namespaceOf != nil {
				if t.deferred {
					return rt.deferredNamespace(t.namespaceOf), nil
				}
				return rt.moduleNamespace(t.namespaceOf), nil
			}
			v, _ := t.owner.exportValue(t.localName)
			if v.IsEmpty() {
				return mkundef(), rt.referenceError("Cannot access '" + exported + "' before initialization")
			}
			return v, nil
		})
		no.defineAccessor(name, get, mkundef(), true, false, attrEnumerable)
	}
	if rt.symToStringTag != 0 {
		no.defineOwnSymbol(rt.symToStringTag.handle(), rt.internString("Deferred Module"), 0)
	}
	no.flags.extensible = false
	// Registered as a module namespace as well: everything that makes one exotic
	// -- the null prototype that cannot be changed, the descriptors that report
	// writable while refusing to be written, the define that only succeeds when
	// it changes nothing -- is the same here.
	if rt.moduleNamespaces == nil {
		rt.moduleNamespaces = map[*object]bool{}
	}
	rt.moduleNamespaces[no] = true
	if rt.deferredNamespaces == nil {
		rt.deferredNamespaces = map[*object]*moduleRecord{}
	}
	rt.deferredNamespaces[no] = m
	return ns
}

// touchDeferred is the trigger. Every operation that asks a namespace about a
// string key runs the module first: [[Get]], [[Set]], [[Delete]],
// [[HasProperty]], [[GetOwnProperty]], [[DefineOwnProperty]], and
// [[OwnPropertyKeys]], which asks about all of them at once and so passes the
// empty key.
//
// A symbol is not a question about the module -- nothing a module exports can be
// reached by one -- and neither is "then", which the language itself reads off
// values that are merely passing through.
func (rt *Runtime) touchDeferred(o *object, key string, isSymbol bool) *ThrowError {
	if rt.deferredNamespaces == nil || isSymbol || key == "then" {
		return nil
	}
	m, ok := rt.deferredNamespaces[o]
	if !ok {
		return nil
	}
	return rt.runDeferred(m)
}

// deferredAt is the trigger as a prototype walk meets it: an object reached on
// the way to answering a question about a string key. A deferred namespace can
// be someone's prototype, and a question asked of the child is still a question
// about the module.
func (rt *Runtime) deferredAt(o *object, key string) *ThrowError {
	if rt.deferredNamespaces == nil || key == "then" {
		return nil
	}
	if m, ok := rt.deferredNamespaces[o]; ok {
		return rt.runDeferred(m)
	}
	return nil
}

// deferredOnChain triggers a deferred namespace anywhere on a receiver's
// prototype chain. [[Set]] walks that chain looking for a setter, so a write
// through `super` to an object whose prototype is a deferred namespace is a
// question about the module even though the write lands somewhere else.
func (rt *Runtime) deferredOnChain(obj Value, key string) *ThrowError {
	if rt.deferredNamespaces == nil || key == "then" {
		return nil
	}
	cur := obj
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		o := rt.objPtr(cur)
		if o == nil {
			return nil
		}
		if m, ok := rt.deferredNamespaces[o]; ok {
			return rt.runDeferred(m)
		}
		cur = o.proto
	}
	return nil
}

// touchDeferredValue is touchDeferred for callers holding a Value and a property
// key that may be either a string or a symbol.
func (rt *Runtime) touchDeferredValue(v Value, key Value) *ThrowError {
	if rt.deferredNamespaces == nil {
		return nil
	}
	o := rt.objPtr(v)
	if o == nil {
		return nil
	}
	if key.IsSymbol() {
		return nil
	}
	return rt.touchDeferred(o, rt.strGo(key), false)
}

// runDeferred evaluates a deferred module, now and synchronously. Its
// asynchronous dependencies were evaluated when the importer was -- a top-level
// await cannot be started from inside a property read, which has nowhere to wait
// -- so what is left below this module can only be synchronous, and it either
// finishes or throws before this returns.
//
// A module that has already run, or already failed, is answered from what it
// left behind: the failure is remembered and rethrown at every later touch.
func (rt *Runtime) runDeferred(m *moduleRecord) *ThrowError {
	if m == nil {
		return nil
	}
	if m.status == modErrored {
		return m.evalErr
	}
	if m.status == modEvaluated {
		return nil
	}
	// Mid-evaluation. The module is on the stack -- its own, or another in its
	// cycle, or one suspended at a top-level await -- so running it now would
	// run it twice, and waiting for it is not something a property read can do.
	// Neither is an answer, so there is no answer.
	if m.status == modEvaluating || m.status == modEvaluatingAsync {
		return rt.typeError("cannot access a deferred module while it is being evaluated")
	}
	_, _, err := rt.innerModuleEvaluation(m, nil, 0)
	if err != nil {
		// Remember it on the module itself, so the second touch reports the same
		// failure rather than trying to run the body again.
		if m.status != modErrored {
			m.status, m.evalErr = modErrored, err
		}
		return err
	}
	return nil
}
