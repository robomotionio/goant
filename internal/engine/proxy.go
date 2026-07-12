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
	t, e := rt.getField(p.handler, name)
	if e != nil {
		return mkundef(), e
	}
	return t, nil
}

func (rt *Runtime) proxyGet(p *proxyState, key, receiver Value) (Value, *ThrowError) {
	trap, e := p.trap(rt, "get")
	if e != nil {
		return mkundef(), e
	}
	if rt.isCallable(trap) {
		return rt.callValue(trap, p.handler, []Value{p.target, rt.toPropertyKeyValue(key), receiver})
	}
	return rt.getElement(p.target, key)
}

func (rt *Runtime) proxySet(p *proxyState, key, val, receiver Value) *ThrowError {
	trap, e := p.trap(rt, "set")
	if e != nil {
		return e
	}
	if rt.isCallable(trap) {
		_, e := rt.callValue(trap, p.handler, []Value{p.target, rt.toPropertyKeyValue(key), val, receiver})
		return e
	}
	return rt.setElement(p.target, key, val)
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
		return rt.toBoolean(r), nil
	}
	if key.IsSymbol() {
		return rt.hasFieldSymbol(p.target, key.handle()), nil
	}
	name, _ := rt.propKeyString(key)
	return rt.hasProp(p.target, name), nil
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
		return rt.toBoolean(r), nil
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
		return rt.iterableValues(r)
	}
	// Forward to target's own keys.
	var out []Value
	if to := rt.objPtr(p.target); to != nil {
		if p.target.Type() == TArr {
			for i := uint32(0); i < to.arrLen; i++ {
				if int(i) < len(to.arr) && !to.arr[i].IsEmpty() {
					out = append(out, rt.newString(itoaSmall(int(i))))
				}
			}
		}
		for _, k := range to.ownKeys() {
			out = append(out, rt.newString(k))
		}
	}
	return out, nil
}

func (rt *Runtime) proxyGetPrototypeOf(p *proxyState) (Value, *ThrowError) {
	trap, e := p.trap(rt, "getPrototypeOf")
	if e != nil {
		return mkundef(), e
	}
	if rt.isCallable(trap) {
		return rt.callValue(trap, p.handler, []Value{p.target})
	}
	if to := rt.objPtr(p.target); to != nil {
		return to.proto, nil
	}
	return mknull(), nil
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

func (rt *Runtime) proxyConstruct(p *proxyState, args []Value) (Value, *ThrowError) {
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
		return rt.callValue(trap, p.handler, []Value{p.target, argsArr, p.target})
	}
	return rt.construct(p.target, args)
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
