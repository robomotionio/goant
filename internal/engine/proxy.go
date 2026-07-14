package engine

// Proxy / Reflect-backed exotic objects (ant modules/proxy.c). A Proxy wraps a
// target and a handler; the fundamental object operations ([[Get]], [[Set]],
// [[Has]], [[Delete]], [[OwnPropertyKeys]], …) consult the matching handler trap
// and otherwise forward to the target. Because the interpreter and all builtins
// route element/property access through getField/getElement/setElement/hasProp/
// deleteElement, hooking those points makes "internal calls" (e.g. Array.prototype
// methods invoked on a proxy) trigger the right traps for free.

type proxyState struct {
	target  Value
	handler Value
	revoked bool
}

// newProxy builds a Proxy object over target/handler.
func (rt *Runtime) newProxy(target, handler Value) (Value, *ThrowError) {
	if !target.IsObjectType() || !handler.IsObjectType() {
		return mkundef(), rt.typeError("Cannot create proxy with a non-object as target or handler")
	}
	v := rt.newObject(mknull())
	o := rt.objPtr(v)
	o.proxy = &proxyState{target: target, handler: handler}
	// A proxy is callable/constructable iff its target is.
	if rt.isCallable(target) {
		o.flags.isCallable = true
	}
	return v, nil
}

// toPropertyKeyValue coerces a key to the string/symbol form a trap receives.
func (rt *Runtime) toPropertyKeyValue(key Value) Value {
	if key.IsSymbol() || key.IsString() {
		return key
	}
	s, _ := rt.propKeyString(key)
	return rt.internString(s)
}

// trap returns handler[name], or an error if the proxy is revoked.
func (p *proxyState) trap(rt *Runtime, name string) (Value, *ThrowError) {
	if p.revoked {
		return mkundef(), rt.typeError("Cannot perform '" + name + "' on a proxy that has been revoked")
	}
	// GetMethod(handler, name): a nullish trap means "use the target's default";
	// a present but non-callable trap is a TypeError.
	t, e := rt.getField(p.handler, name)
	if e != nil {
		return mkundef(), e
	}
	if t.IsNullish() {
		return mkundef(), nil
	}
	if !rt.isCallable(t) {
		return mkundef(), rt.typeError("'" + name + "' trap is not a function")
	}
	return t, nil
}

// targetOwnDesc returns target's own property descriptor for key (ordinary
// objects only; array/typed-array indices report a default data descriptor).
func (rt *Runtime) targetOwnDesc(target, key Value) (ownDesc, bool) {
	o := rt.objPtr(target)
	if o == nil {
		return ownDesc{}, false
	}
	if key.IsSymbol() {
		d := o.ownDescriptorSym(key.handle())
		return d, d.exists
	}
	name, _ := rt.propKeyString(key)
	if idx, ok := canonicalIndex(name); ok && rt.hasOwnIndex(target, o, idx) {
		el, _ := rt.getElement(target, key)
		return ownDesc{exists: true, value: el, writable: true, enumerable: true, configable: true}, true
	}
	d := o.ownDescriptor(name)
	return d, d.exists
}

// propKeyId returns a collision-free identity string for a string/symbol key,
// used to detect duplicate ownKeys entries.
func (rt *Runtime) propKeyId(k Value) string {
	if k.IsSymbol() {
		return "y:" + itoaSmall(int(k.handle()))
	}
	return "s:" + string(rt.strBytes(k))
}

// targetExtensible reports the target's [[IsExtensible]] (proxies recurse).
func (rt *Runtime) targetExtensible(v Value) bool {
	o := rt.objPtr(v)
	if o == nil {
		return false
	}
	if o.proxy != nil {
		ext, _ := rt.proxyIsExtensible(o.proxy)
		return ext
	}
	return o.flags.extensible
}

func (rt *Runtime) proxyGet(p *proxyState, key, receiver Value) (Value, *ThrowError) {
	trap, e := p.trap(rt, "get")
	if e != nil {
		return mkundef(), e
	}
	if !rt.isCallable(trap) {
		return rt.getElement(p.target, key)
	}
	res, e := rt.callValue(trap, p.handler, []Value{p.target, rt.toPropertyKeyValue(key), receiver})
	if e != nil {
		return mkundef(), e
	}
	// [[Get]] invariant: a non-configurable target property constrains the trap
	// result — a non-writable data property must report its own value, and a
	// getterless accessor must report undefined.
	if d, ok := rt.targetOwnDesc(p.target, key); ok && !d.configable {
		if !d.isAccessor && !d.writable && !rt.sameValue(res, d.value) {
			return mkundef(), rt.typeError("'get' on proxy: property is a non-writable, non-configurable data property on the target but the trap returned a different value")
		}
		if d.isAccessor && !rt.isCallable(d.getter) && !res.IsUndefined() {
			return mkundef(), rt.typeError("'get' on proxy: property is a non-configurable accessor with an undefined getter but the trap returned a value")
		}
	}
	return res, nil
}

func (rt *Runtime) proxySet(p *proxyState, key, val, receiver Value) (bool, *ThrowError) {
	trap, e := p.trap(rt, "set")
	if e != nil {
		return false, e
	}
	if rt.isCallable(trap) {
		r, e := rt.callValue(trap, p.handler, []Value{p.target, rt.toPropertyKeyValue(key), val, receiver})
		if e != nil {
			return false, e
		}
		if !rt.toBoolean(r) {
			return false, nil // trap reported failure; [[Set]] returns false
		}
		// [[Set]] invariant: cannot successfully assign a value differing from a
		// non-configurable non-writable data property, nor set a non-configurable
		// accessor that lacks a setter.
		if d, ok := rt.targetOwnDesc(p.target, key); ok && !d.configable {
			if !d.isAccessor && !d.writable && !rt.sameValue(val, d.value) {
				return false, rt.typeError("'set' on proxy: trap returned truish for a non-writable, non-configurable property with a different value")
			}
			if d.isAccessor && !rt.isCallable(d.setter) {
				return false, rt.typeError("'set' on proxy: trap returned truish for a non-configurable accessor property with an undefined setter")
			}
		}
		return true, nil
	}
	// No set trap: perform the ordinary [[Set]] on the target, but with the proxy
	// as the receiver so a data-property assignment routes back through the
	// proxy's [[DefineOwnProperty]] / [[GetOwnProperty]] traps (10.5.9).
	return rt.ordinarySet(p.target, key, val, receiver)
}

// ordinarySet implements OrdinarySet(O, P, V, Receiver): walk O's chain for the
// property; an accessor calls its setter with this=Receiver, a Proxy dispatches
// its [[Set]], and a data property (or none) writes to Receiver.
func (rt *Runtime) ordinarySet(o, key, val, receiver Value) (bool, *ThrowError) {
	cur := o
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		oo := rt.objPtr(cur)
		if oo == nil {
			break
		}
		if oo.proxy != nil {
			return rt.proxySet(oo.proxy, key, val, receiver)
		}
		if d, exists := rt.targetOwnDesc(cur, key); exists {
			if d.isAccessor {
				if rt.isCallable(d.setter) {
					_, e := rt.callValue(d.setter, receiver, []Value{val})
					return true, e
				}
				return false, nil
			}
			if !d.writable {
				return false, nil
			}
			return rt.setOnReceiver(receiver, key, val)
		}
		cur = oo.proto
	}
	return rt.setOnReceiver(receiver, key, val)
}

// setOnReceiver performs the final write of OrdinarySetWithOwnDescriptor: it
// creates or updates an own data property on Receiver (via the defineProperty /
// getOwnPropertyDescriptor traps when Receiver is itself a proxy).
func (rt *Runtime) setOnReceiver(receiver, key, val Value) (bool, *ThrowError) {
	ro := rt.objPtr(receiver)
	if ro == nil {
		return false, nil // primitive receiver
	}
	if ro.proxy != nil {
		existing, e := rt.proxyGetOwnPropertyDescriptor(ro.proxy, key)
		if e != nil {
			return false, e
		}
		desc := rt.newPlainObject()
		do := rt.objPtr(desc)
		if existing.IsUndefined() {
			do.defineOwn("value", val, attrDefault)
			do.defineOwn("writable", mktrue(), attrDefault)
			do.defineOwn("enumerable", mktrue(), attrDefault)
			do.defineOwn("configurable", mktrue(), attrDefault)
		} else {
			if g, _ := rt.getField(existing, "get"); rt.isCallable(g) {
				return false, nil
			}
			if s, _ := rt.getField(existing, "set"); rt.isCallable(s) {
				return false, nil
			}
			if w, _ := rt.getField(existing, "writable"); !rt.toBoolean(w) {
				return false, nil
			}
			do.defineOwn("value", val, attrDefault)
		}
		if e := rt.proxyDefineProperty(ro.proxy, key, desc); e != nil {
			return false, e
		}
		return true, nil
	}
	if key.IsSymbol() {
		if d := ro.ownDescriptorSym(key.handle()); d.exists && (d.isAccessor || !d.writable) {
			return false, nil
		}
		return rt.setElement(receiver, key, val) == nil, nil
	}
	name, _ := rt.propKeyString(key)
	return rt.setProp(receiver, name, val), nil
}

func (rt *Runtime) proxyHas(p *proxyState, key Value) (bool, *ThrowError) {
	trap, e := p.trap(rt, "has")
	if e != nil {
		return false, e
	}
	if rt.isCallable(trap) {
		r, e := rt.callValue(trap, p.handler, []Value{p.target, rt.toPropertyKeyValue(key)})
		if e != nil {
			return false, e
		}
		res := rt.toBoolean(r)
		if !res {
			// [[HasProperty]] invariant: an existing non-configurable property (or
			// any own property of a non-extensible target) can't be hidden.
			if d, ok := rt.targetOwnDesc(p.target, key); ok {
				if !d.configable {
					return false, rt.typeError("'has' on proxy: trap returned falsish for a non-configurable property")
				}
				if !rt.targetExtensible(p.target) {
					return false, rt.typeError("'has' on proxy: trap returned falsish for an existing property on a non-extensible target")
				}
			}
		}
		return res, nil
	}
	if key.IsSymbol() {
		return rt.hasFieldSymbol(p.target, key.handle()), nil
	}
	name, _ := rt.propKeyString(key)
	return rt.hasPropE(p.target, name)
}

func (rt *Runtime) proxyDelete(p *proxyState, key Value) (bool, *ThrowError) {
	trap, e := p.trap(rt, "deleteProperty")
	if e != nil {
		return false, e
	}
	if rt.isCallable(trap) {
		r, e := rt.callValue(trap, p.handler, []Value{p.target, rt.toPropertyKeyValue(key)})
		if e != nil {
			return false, e
		}
		res := rt.toBoolean(r)
		if res {
			// [[Delete]] invariant: a non-configurable property can't be reported
			// as deleted, nor any own property of a non-extensible target.
			if d, ok := rt.targetOwnDesc(p.target, key); ok {
				if !d.configable {
					return false, rt.typeError("'deleteProperty' on proxy: trap returned truish for a non-configurable property")
				}
				if !rt.targetExtensible(p.target) {
					return false, rt.typeError("'deleteProperty' on proxy: trap returned truish for a property on a non-extensible target")
				}
			}
		}
		return res, nil
	}
	return rt.deleteElement(p.target, key)
}

// proxyOwnKeys returns the proxy's own string keys (for for-in / ownPropertyNames).
func (rt *Runtime) proxyOwnKeys(p *proxyState) ([]Value, *ThrowError) {
	trap, e := p.trap(rt, "ownKeys")
	if e != nil {
		return nil, e
	}
	if rt.isCallable(trap) {
		r, e := rt.callValue(trap, p.handler, []Value{p.target})
		if e != nil {
			return nil, e
		}
		keys, e := rt.iterableValues(r)
		if e != nil {
			return nil, e
		}
		// Invariant: the trap must report only property keys, with no duplicates.
		seen := make(map[string]bool, len(keys))
		for _, k := range keys {
			if !k.IsString() && !k.IsSymbol() {
				return nil, rt.typeError("'ownKeys' on proxy: trap result must be an array of property keys")
			}
			id := rt.propKeyId(k)
			if seen[id] {
				return nil, rt.typeError("'ownKeys' on proxy: trap result contains duplicate entries")
			}
			seen[id] = true
		}
		// Invariant: every non-configurable target key must appear; on a
		// non-extensible target the result must be exactly the target's keys.
		targetKeys := rt.targetOwnKeyList(p.target)
		ext := rt.targetExtensible(p.target)
		for _, tk := range targetKeys {
			td, _ := rt.targetOwnDesc(p.target, tk)
			inTrap := seen[rt.propKeyId(tk)]
			if !inTrap && td.exists && !td.configable {
				return nil, rt.typeError("'ownKeys' on proxy: trap result did not include a non-configurable property of the proxy target")
			}
			if !inTrap && !ext {
				return nil, rt.typeError("'ownKeys' on proxy: trap result did not include a property of the non-extensible proxy target")
			}
		}
		if !ext {
			tset := make(map[string]bool, len(targetKeys))
			for _, tk := range targetKeys {
				tset[rt.propKeyId(tk)] = true
			}
			for _, k := range keys {
				if !tset[rt.propKeyId(k)] {
					return nil, rt.typeError("'ownKeys' on proxy: trap result included a property not present on the non-extensible proxy target")
				}
			}
		}
		return keys, nil
	}
	// Missing trap: forward to target.[[OwnPropertyKeys]]. When the target is
	// itself a proxy, route through its trap rather than reading the target
	// object's ordinary keys (which would ignore the inner proxy entirely).
	if to := rt.objPtr(p.target); to != nil && to.proxy != nil {
		return rt.proxyOwnKeys(to.proxy)
	}
	return rt.targetOwnKeyList(p.target), nil
}

// targetOwnKeyList returns the target object's own keys (array indices first,
// then string/symbol keys in insertion order) as property-key Values.
func (rt *Runtime) targetOwnKeyList(target Value) []Value {
	var out []Value
	to := rt.objPtr(target)
	if to == nil {
		return out
	}
	if target.Type() == TArr {
		for i := uint32(0); i < to.arrLen; i++ {
			if int(i) < len(to.arr) && !to.arr[i].IsEmpty() {
				out = append(out, rt.newString(itoaSmall(int(i))))
			}
		}
	}
	for _, k := range to.ownKeys() {
		out = append(out, rt.newString(k))
	}
	return out
}

func (rt *Runtime) proxyGetPrototypeOf(p *proxyState) (Value, *ThrowError) {
	trap, e := p.trap(rt, "getPrototypeOf")
	if e != nil {
		return mkundef(), e
	}
	if !rt.isCallable(trap) {
		return rt.getPrototypeOfValue(p.target) // forward to the target (proxy → its trap)
	}
	r, e := rt.callValue(trap, p.handler, []Value{p.target})
	if e != nil {
		return mkundef(), e
	}
	if !r.IsObjectType() && !r.IsNull() {
		return mkundef(), rt.typeError("'getPrototypeOf' on proxy: trap returned neither object nor null")
	}
	// Invariant: a non-extensible target must report its actual prototype.
	extensibleTarget, e := rt.isExtensibleValue(p.target)
	if e != nil {
		return mkundef(), e
	}
	if extensibleTarget {
		return r, nil
	}
	targetProto, e := rt.getPrototypeOfValue(p.target)
	if e != nil {
		return mkundef(), e
	}
	if !rt.sameValue(r, targetProto) {
		return mkundef(), rt.typeError("'getPrototypeOf' on proxy: proxy target is non-extensible but the trap did not return its actual prototype")
	}
	return r, nil
}

func (rt *Runtime) proxyGetOwnPropertyDescriptor(p *proxyState, key Value) (Value, *ThrowError) {
	trap, e := p.trap(rt, "getOwnPropertyDescriptor")
	if e != nil {
		return mkundef(), e
	}
	if rt.isCallable(trap) {
		res, e := rt.callValue(trap, p.handler, []Value{p.target, rt.toPropertyKeyValue(key)})
		if e != nil {
			return mkundef(), e
		}
		if !res.IsObjectType() && !res.IsUndefined() {
			return mkundef(), rt.typeError("'getOwnPropertyDescriptor' on proxy: trap returned neither object nor undefined")
		}
		td, texists := rt.targetOwnDesc(p.target, key)
		if res.IsUndefined() {
			// Invariant: a non-configurable (or non-extensible-target) property
			// can't be reported as absent.
			if texists && !td.configable {
				return mkundef(), rt.typeError("'getOwnPropertyDescriptor' on proxy: trap reported a non-configurable property as non-existent")
			}
			if texists && !rt.targetExtensible(p.target) {
				return mkundef(), rt.typeError("'getOwnPropertyDescriptor' on proxy: trap reported an existing property as non-existent on a non-extensible target")
			}
			return mkundef(), nil
		}
		// Normalize (ToPropertyDescriptor + CompletePropertyDescriptor +
		// FromPropertyDescriptor) so absent fields default per spec.
		completed := rt.completeDescriptor(res)
		// A brand-new property can't be reported on a non-extensible target.
		if !texists && !rt.targetExtensible(p.target) {
			return mkundef(), rt.typeError("'getOwnPropertyDescriptor' on proxy: trap reported a new property on a non-extensible target")
		}
		// A non-configurable report requires an existing non-configurable target
		// property; a non-writable report additionally requires non-writable.
		if cfg, _ := rt.getField(completed, "configurable"); !rt.toBoolean(cfg) {
			if !texists || td.configable {
				return mkundef(), rt.typeError("'getOwnPropertyDescriptor' on proxy: trap reported a property as non-configurable that is not non-configurable on the target")
			}
			if w, _ := rt.getField(completed, "writable"); !td.isAccessor && !rt.toBoolean(w) && td.writable {
				return mkundef(), rt.typeError("'getOwnPropertyDescriptor' on proxy: trap reported a non-writable property that is writable on the target")
			}
		}
		return completed, nil
	}
	// Forward to target.[[GetOwnProperty]]. A proxy target routes through its own
	// trap rather than exposing the (empty) ordinary descriptor beneath it.
	to := rt.objPtr(p.target)
	if to == nil {
		return mkundef(), nil
	}
	if to.proxy != nil {
		return rt.proxyGetOwnPropertyDescriptor(to.proxy, key)
	}
	var d ownDesc
	if key.IsSymbol() {
		d = to.ownDescriptorSym(key.handle())
	} else {
		name, _ := rt.propKeyString(key)
		d = to.ownDescriptor(name)
	}
	if !d.exists {
		return mkundef(), nil
	}
	return rt.descriptorToObject(d), nil
}

// descIsCompatible implements IsCompatiblePropertyDescriptor for an existing
// (non-undefined) target property `cur`: it reports whether the descriptor
// object descVal describes a change the target's current property permits. A
// configurable current property allows anything; a non-configurable one forbids
// becoming configurable, flipping enumerable, changing kind (data↔accessor),
// re-widening a non-writable data value/flag, or swapping accessor get/set.
func (rt *Runtime) descIsCompatible(descVal Value, cur ownDesc) bool {
	if cur.configable {
		return true
	}
	hasConfig := rt.hasProp(descVal, "configurable")
	hasEnum := rt.hasProp(descVal, "enumerable")
	hasWritable := rt.hasProp(descVal, "writable")
	hasValue := rt.hasProp(descVal, "value")
	hasGet := rt.hasProp(descVal, "get")
	hasSet := rt.hasProp(descVal, "set")

	if hasConfig {
		if c, _ := rt.getField(descVal, "configurable"); rt.toBoolean(c) {
			return false // cannot make a non-configurable property configurable
		}
	}
	if hasEnum {
		if e, _ := rt.getField(descVal, "enumerable"); rt.toBoolean(e) != cur.enumerable {
			return false
		}
	}
	descIsAccessor := hasGet || hasSet
	descIsData := hasValue || hasWritable
	if !descIsAccessor && !descIsData {
		return true // generic descriptor: only config/enum, already validated
	}
	if cur.isAccessor != descIsAccessor {
		return false // cannot change between data and accessor
	}
	if !cur.isAccessor {
		if !cur.writable {
			if hasWritable {
				if w, _ := rt.getField(descVal, "writable"); rt.toBoolean(w) {
					return false // cannot re-widen a non-writable data property
				}
			}
			if hasValue {
				if v, _ := rt.getField(descVal, "value"); !rt.sameValue(v, cur.value) {
					return false
				}
			}
		}
		return true
	}
	if hasSet {
		if s, _ := rt.getField(descVal, "set"); !rt.sameValue(s, cur.setter) {
			return false
		}
	}
	if hasGet {
		if g, _ := rt.getField(descVal, "get"); !rt.sameValue(g, cur.getter) {
			return false
		}
	}
	return true
}

func (rt *Runtime) proxyDefineProperty(p *proxyState, key, desc Value) *ThrowError {
	trap, e := p.trap(rt, "defineProperty")
	if e != nil {
		return e
	}
	if rt.isCallable(trap) {
		r, e := rt.callValue(trap, p.handler, []Value{p.target, rt.toPropertyKeyValue(key), desc})
		if e != nil {
			return e
		}
		if !rt.toBoolean(r) {
			// A false trap result rejects: DefinePropertyOrThrow (Object.defineProperty)
			// throws, Reflect.defineProperty returns false.
			return rt.rejectDefine("'defineProperty' on proxy: trap returned falsish")
		}
		// [[DefineOwnProperty]] invariants (10.5.6).
		td, texists := rt.targetOwnDesc(p.target, key)
		ext := rt.targetExtensible(p.target)
		settingConfigFalse := rt.hasProp(desc, "configurable")
		if settingConfigFalse {
			cf, _ := rt.getField(desc, "configurable")
			settingConfigFalse = !rt.toBoolean(cf)
		}
		if !texists {
			if !ext {
				return rt.typeError("'defineProperty' on proxy: trap returned truish for adding a property to the non-extensible proxy target")
			}
			if settingConfigFalse {
				return rt.typeError("'defineProperty' on proxy: trap returned truish for defining a non-configurable property that does not exist on the proxy target")
			}
		} else {
			// IsCompatiblePropertyDescriptor(extensible, Desc, targetDesc): the trap
			// must not claim a change the target's existing property forbids.
			if !rt.descIsCompatible(desc, td) {
				return rt.typeError("'defineProperty' on proxy: trap returned truish for defining an incompatible property on the proxy target")
			}
			if settingConfigFalse && td.configable {
				return rt.typeError("'defineProperty' on proxy: trap returned truish for defining a non-configurable property that is configurable on the proxy target")
			}
			// A non-configurable, writable data property on the target cannot be
			// reported as becoming non-writable.
			if !td.isAccessor && !td.configable && td.writable && rt.hasProp(desc, "writable") {
				if w, _ := rt.getField(desc, "writable"); !rt.toBoolean(w) {
					return rt.typeError("'defineProperty' on proxy: trap returned truish for making a writable, non-configurable target property non-writable")
				}
			}
		}
		return nil
	}
	// Missing trap: forward to the target's own [[DefineOwnProperty]] (a proxy
	// target routes through its own trap).
	if to := rt.objPtr(p.target); to != nil && to.proxy != nil {
		return rt.proxyDefineProperty(to.proxy, key, desc)
	}
	return rt.objectDefinePropertyKey(p.target, rt.toPropertyKeyValue(key), desc)
}

// setPrototypeOfValue dispatches [[SetPrototypeOf]]: a proxy routes through its
// trap, an ordinary object through OrdinarySetPrototypeOf.
func (rt *Runtime) setPrototypeOfValue(v, proto Value) (bool, *ThrowError) {
	o := rt.objPtr(v)
	if o == nil {
		return false, nil
	}
	if o.proxy != nil {
		return rt.proxySetPrototypeOf(o.proxy, proto)
	}
	return rt.ordinarySetProto(o, proto), nil
}

// isExtensibleValue dispatches [[IsExtensible]], propagating a proxy trap's error.
func (rt *Runtime) isExtensibleValue(v Value) (bool, *ThrowError) {
	o := rt.objPtr(v)
	if o == nil {
		return false, nil
	}
	if o.proxy != nil {
		return rt.proxyIsExtensible(o.proxy)
	}
	return o.flags.extensible, nil
}

// getPrototypeOfValue dispatches [[GetPrototypeOf]], propagating a proxy trap's error.
func (rt *Runtime) getPrototypeOfValue(v Value) (Value, *ThrowError) {
	o := rt.objPtr(v)
	if o == nil {
		return mknull(), nil
	}
	if o.proxy != nil {
		return rt.proxyGetPrototypeOf(o.proxy)
	}
	return o.proto, nil
}

func (rt *Runtime) proxySetPrototypeOf(p *proxyState, proto Value) (bool, *ThrowError) {
	trap, e := p.trap(rt, "setPrototypeOf")
	if e != nil {
		return false, e
	}
	if !rt.isCallable(trap) {
		// Missing trap: forward to the target's own [[SetPrototypeOf]].
		return rt.setPrototypeOfValue(p.target, proto)
	}
	r, e := rt.callValue(trap, p.handler, []Value{p.target, proto})
	if e != nil {
		return false, e
	}
	if !rt.toBoolean(r) {
		return false, nil // trap reported failure; [[SetPrototypeOf]] returns false
	}
	// Invariant: if the target is non-extensible, its prototype cannot be changed.
	// IsExtensible / GetPrototypeOf are observable and their abrupt completions
	// (e.g. a proxy target's traps) must propagate.
	extensibleTarget, e := rt.isExtensibleValue(p.target)
	if e != nil {
		return false, e
	}
	if extensibleTarget {
		return true, nil
	}
	targetProto, e := rt.getPrototypeOfValue(p.target)
	if e != nil {
		return false, e
	}
	if !rt.sameValue(proto, targetProto) {
		return false, rt.typeError("'setPrototypeOf' on proxy: trap returned truish for setting a new prototype on the non-extensible proxy target")
	}
	return true, nil
}

func (rt *Runtime) proxyIsExtensible(p *proxyState) (bool, *ThrowError) {
	trap, e := p.trap(rt, "isExtensible")
	if e != nil {
		return false, e
	}
	if !rt.isCallable(trap) {
		return rt.isExtensibleValue(p.target) // forward to the target (proxy → its trap)
	}
	r, e := rt.callValue(trap, p.handler, []Value{p.target})
	if e != nil {
		return false, e
	}
	res := rt.toBoolean(r)
	// Invariant: the trap result must match the target's actual extensibility.
	actual, e := rt.isExtensibleValue(p.target)
	if e != nil {
		return false, e
	}
	if res != actual {
		return false, rt.typeError("'isExtensible' on proxy: trap result does not reflect extensibility of proxy target")
	}
	return res, nil
}

// proxyPreventExtensions implements the proxy [[PreventExtensions]], returning
// the boolean result (the trap's ToBoolean-ed return, or true when no trap) so
// callers can enforce "throw if false" (Object.*) or return it (Reflect.*).
func (rt *Runtime) proxyPreventExtensions(p *proxyState) (bool, *ThrowError) {
	trap, e := p.trap(rt, "preventExtensions")
	if e != nil {
		return false, e
	}
	if rt.isCallable(trap) {
		r, e := rt.callValue(trap, p.handler, []Value{p.target})
		if e != nil {
			return false, e
		}
		ok := rt.toBoolean(r)
		// Invariant: if the trap succeeds, the target must now be non-extensible.
		if ok {
			if to := rt.objPtr(p.target); to != nil && to.flags.extensible {
				return false, rt.typeError("'preventExtensions' on proxy: trap returned truish but the proxy target is extensible")
			}
		}
		return ok, nil
	}
	// No trap: forward to target.[[PreventExtensions]] (through its trap if the
	// target is itself a proxy).
	if to := rt.objPtr(p.target); to != nil {
		if to.proxy != nil {
			return rt.proxyPreventExtensions(to.proxy)
		}
		to.flags.extensible = false
	}
	return true, nil
}

func (rt *Runtime) proxyApply(p *proxyState, thisArg Value, args []Value) (Value, *ThrowError) {
	trap, e := p.trap(rt, "apply")
	if e != nil {
		return mkundef(), e
	}
	if rt.isCallable(trap) {
		argsArr := rt.newArray()
		ao := rt.objPtr(argsArr)
		for _, a := range args {
			rt.arraySet(ao, ao.arrLen, a)
		}
		return rt.callValue(trap, p.handler, []Value{p.target, thisArg, argsArr})
	}
	return rt.callValue(p.target, thisArg, args)
}

func (rt *Runtime) proxyConstruct(p *proxyState, args []Value, newTarget Value) (Value, *ThrowError) {
	// A proxy has a [[Construct]] method only if its target is a constructor.
	if !rt.isCallable(p.target) {
		return mkundef(), rt.typeError("proxy target is not a constructor")
	}
	trap, e := p.trap(rt, "construct")
	if e != nil {
		return mkundef(), e
	}
	if rt.isCallable(trap) {
		argsArr := rt.newArray()
		ao := rt.objPtr(argsArr)
		for _, a := range args {
			rt.arraySet(ao, ao.arrLen, a)
		}
		// The construct trap receives (target, argumentsList, newTarget).
		res, e := rt.callValue(trap, p.handler, []Value{p.target, argsArr, newTarget})
		if e != nil {
			return mkundef(), e
		}
		// Invariant: the construct trap must return an object.
		if !res.IsObjectType() {
			return mkundef(), rt.typeError("'construct' on proxy: trap returned a non-object")
		}
		return res, nil
	}
	// No trap: forward to the target's [[Construct]] preserving newTarget, so
	// GetPrototypeFromConstructor reads "prototype" from the original newTarget
	// (which may itself be this proxy).
	return rt.constructWithTarget(p.target, args, newTarget)
}

// itoaSmall formats a small non-negative int without importing strconv here.
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (rt *Runtime) initProxyBuiltin() {
	ctor := rt.newNativeFunc("Proxy", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		// Proxy is a constructor: a plain call (no `this` object) is a TypeError.
		if rt.objPtr(this) == nil {
			return mkundef(), rt.typeError("Constructor Proxy requires 'new'")
		}
		return rt.newProxy(arg(args, 0), arg(args, 1))
	})
	cobj := rt.objPtr(ctor)
	// Proxy.revocable(target, handler) -> { proxy, revoke }.
	rt.defMethod(cobj, "revocable", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		pv, e := rt.newProxy(arg(args, 0), arg(args, 1))
		if e != nil {
			return mkundef(), e
		}
		p := rt.objPtr(pv).proxy
		revoke := rt.newNativeFunc("", 0, func(rt *Runtime, _ Value, _ []Value) (Value, *ThrowError) {
			p.revoked = true
			return mkundef(), nil
		})
		res := rt.newPlainObject()
		ro := rt.objPtr(res)
		ro.defineOwn("proxy", pv, attrDefault)
		ro.defineOwn("revoke", revoke, attrDefault)
		return res, nil
	})
	rt.defGlobal("Proxy", ctor)
}
