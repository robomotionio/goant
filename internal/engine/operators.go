package engine

// Operator helpers for typeof / delete / in / instanceof (ant ops/unary.h,
// ops/property.h, ops/comparison.h).

// typeofString implements the ECMAScript typeof operator.
func (rt *Runtime) typeofString(v Value) string {
	switch v.Type() {
	case TUndef:
		return "undefined"
	case TNull:
		return "object"
	case TBool:
		return "boolean"
	case TNum:
		return "number"
	case TStr:
		return "string"
	case TSymbol:
		return "symbol"
	case TBigInt:
		return "bigint"
	case TFunc, TCFunc:
		return "function"
	default:
		if o := rt.objPtr(v); o != nil && o.flags.isCallable {
			return "function"
		}
		return "object"
	}
}

// deleteElement implements delete obj[key], returning whether the property was
// removed (or absent).
// isUnscopable reports whether name is excluded from a `with` scope via the
// object's Symbol.unscopables list.
func (rt *Runtime) isUnscopable(obj Value, name string) (bool, *ThrowError) {
	if rt.symUnscopables == 0 {
		return false, nil
	}
	// Get(obj, @@unscopables) through [[Get]] so a Proxy's trap observes it
	// (with-statement HasBinding consults @@unscopables before the name). A throw
	// from the @@unscopables getter — or from reading the blocked name off it —
	// propagates out of HasBinding.
	unsc, e := rt.getElement(obj, rt.symUnscopables)
	if e != nil {
		return false, e
	}
	if !unsc.IsObjectType() {
		return false, nil
	}
	v, e := rt.getField(unsc, name)
	if e != nil {
		return false, e
	}
	return rt.toBoolean(v), nil
}

func (rt *Runtime) deleteElement(obj, key Value) (bool, *ThrowError) {
	if obj.IsNullish() {
		return false, rt.typeError("cannot delete property of " + rt.nullishName(obj))
	}
	if o := rt.objPtr(obj); o != nil && o.proxy != nil {
		return rt.proxyDelete(o.proxy, rt.toPropertyKeyValue(key))
	}
	if key.IsSymbol() {
		if o := rt.objPtr(obj); o != nil {
			return o.deleteOwnSymbol(key.handle()), nil
		}
		return true, nil
	}
	if idx, ok := rt.arrayIndexOf(key); ok && obj.Type() == TArr {
		// arrayIndexOf accepts a string index too ("1"), so `delete a["1"]` clears
		// the element (arrayIndex only matched a numeric key, leaving it in place).
		o := rt.objPtr(obj)
		// An index defined with non-default attributes lives as a named shape
		// property; delete it via deleteOwn (which honors [[Configurable]]).
		if name := numberToString(float64(idx)); o.shape.lookupInterned(name) >= 0 {
			return o.deleteOwn(name), nil
		}
		if int(idx) < len(o.arr) && !o.arr[idx].IsEmpty() {
			// A sealed or frozen array's elements are non-configurable, so
			// [[Delete]] fails; a hole / out-of-range index has nothing to
			// delete and succeeds.
			if o.flags.frozen || o.flags.sealed {
				return false, nil
			}
			o.arr[idx] = tEmpty
		}
		return true, nil
	}
	if idx, ok := rt.arrayIndexOf(key); ok && obj.Type() == TTypedArray {
		// Integer-indexed exotic [[Delete]]: a live element is non-configurable
		// (delete fails); an out-of-range / detached index has nothing to delete
		// (delete succeeds).
		return !rt.taValidIndex(rt.objPtr(obj), int(idx)), nil
	}
	o := rt.objPtr(obj)
	if o == nil {
		return true, nil
	}
	name, e := rt.propKeyString(key)
	if e != nil {
		return false, e
	}
	// An Array's / String exotic object's "length" is a non-configurable own
	// property and cannot be deleted (it is not a shape slot deleteOwn tracks).
	if name == "length" && (obj.Type() == TArr || o.boxed.Type() == TStr) {
		return false, nil
	}
	return o.deleteOwn(name), nil
}

// forInKeys returns the array of enumerable string property keys for a for-in
// loop: own + inherited enumerable keys, deduplicated, integer indices first in
// ascending order, then insertion order (ant js_for_in_keys).
func (rt *Runtime) forInKeys(obj Value) Value {
	arr := rt.newArray()
	if obj.IsNullish() {
		return arr
	}
	ao := rt.objPtr(arr)
	seen := map[string]bool{}
	cur := obj
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		o := rt.objPtr(cur)
		if o == nil {
			break
		}
		if o.proxy != nil {
			keys, _ := rt.proxyOwnKeys(o.proxy)
			for _, kv := range keys {
				if !kv.IsString() {
					continue
				}
				k := string(rt.strBytes(kv))
				if !seen[k] {
					seen[k] = true
					rt.arraySet(ao, ao.arrLen, kv)
				}
			}
			break
		}
		if cur.Type() == TArr {
			for i := uint32(0); i < o.arrLen; i++ {
				if int(i) < len(o.arr) && !o.arr[i].IsEmpty() {
					k := numberToString(float64(i))
					if !seen[k] {
						seen[k] = true
						rt.arraySet(ao, ao.arrLen, rt.newString(k))
					}
				}
			}
		}
		if cur.Type() == TTypedArray {
			// Integer-indexed elements are enumerable own properties of a typed array.
			for i, l := 0, rt.taLength(o); i < l; i++ {
				k := numberToString(float64(i))
				if !seen[k] {
					seen[k] = true
					rt.arraySet(ao, ao.arrLen, rt.newString(k))
				}
			}
		}
		if o.boxed.Type() == TStr {
			// A String wrapper's characters are enumerable own index properties.
			for i, l := 0, utf16Len(rt.strBytes(o.boxed)); i < l; i++ {
				k := numberToString(float64(i))
				if !seen[k] {
					seen[k] = true
					rt.arraySet(ao, ao.arrLen, rt.newString(k))
				}
			}
		}
		// Every own key (enumerable or not) shadows inherited keys of the same name;
		// only enumerable own keys are actually enumerated.
		keys, enum := o.ownKeysForIn()
		for _, k := range keys {
			if seen[k] {
				continue
			}
			seen[k] = true
			if enum[k] {
				rt.arraySet(ao, ao.arrLen, rt.internString(k))
			}
		}
		cur = o.proto
	}
	return arr
}

// jsIn implements the `in` operator: key in obj.
func (rt *Runtime) jsIn(key, obj Value) (bool, *ThrowError) {
	// Private brand check `#x in obj`: the compiler emits the private name as a
	// string key. A non-object receiver simply lacks the brand (no TypeError).
	if key.IsString() {
		if s := string(rt.strBytes(key)); isPrivateKey(s) {
			return rt.hasPrivate(obj, s), nil
		}
	}
	if !obj.IsObjectType() && obj.Type() != TTypedArray {
		return false, rt.typeError("cannot use 'in' operator on a non-object")
	}
	// A proxy dispatches the has trap directly so its [[HasProperty]] invariant
	// errors propagate (hasProp swallows them).
	if o := rt.objPtr(obj); o != nil && o.proxy != nil {
		return rt.proxyHas(o.proxy, rt.toPropertyKeyValue(key))
	}
	if idx, ok := arrayIndex(key); ok && obj.Type() == TArr {
		o := rt.objPtr(obj)
		if idx < o.arrLen && int(idx) < len(o.arr) && !o.arr[idx].IsEmpty() {
			return true, nil
		}
		// A hole (or out-of-range index) is not own, but HasProperty still walks
		// the prototype chain (e.g. an index inherited from Array.prototype).
	}
	// A symbol key walks the prototype chain checking each shape's symbol table
	// (propKeyString below would throw on a symbol).
	pk, ke := rt.toPropertyKey(key)
	if ke != nil {
		return false, ke
	}
	if pk.IsSymbol() {
		sym := pk.handle()
		for cur := obj; ; {
			o := rt.objPtr(cur)
			if o == nil {
				return false, nil
			}
			if o.proxy != nil {
				return rt.proxyHas(o.proxy, pk)
			}
			if o.shape.lookupSymbol(sym) >= 0 {
				return true, nil
			}
			cur = o.proto
		}
	}
	name, e := rt.propKeyString(pk)
	if e != nil {
		return false, e
	}
	if has, isCanon := rt.typedArrayCanonicalHas(obj, name); isCanon {
		return has, nil // integer-indexed exotic [[HasProperty]]: no prototype walk
	}
	// hasPropE (not hasProp) so a Proxy [[HasProperty]] trap's abrupt completion in
	// the prototype chain propagates out of the `in` operator.
	return rt.hasPropE(obj, name)
}

// ordinaryHasInstance implements OrdinaryHasInstance(C, O): a non-callable C is
// false; a bound function chases its [[BoundTargetFunction]]; otherwise O's
// prototype chain is searched for C.prototype (a non-object C.prototype throws).
func (rt *Runtime) ordinaryHasInstance(c, o Value) (bool, *ThrowError) {
	if !rt.isCallable(c) {
		return false, nil
	}
	if co := rt.objPtr(c); co != nil {
		if bt := co.getSlot(slotTargetFunc); bt.IsObjectType() {
			return rt.jsInstanceof(o, bt)
		}
	}
	if !o.IsObjectLike() {
		return false, nil
	}
	protoV, e := rt.getField(c, "prototype")
	if e != nil {
		return false, e
	}
	if !protoV.IsObjectType() {
		return false, rt.typeError("prototype is not an object")
	}
	// Walk O's prototype chain via [[GetPrototypeOf]] so a Proxy in the chain
	// fires its trap (and its invariants) rather than exposing the raw target.
	cur := o
	for depth := 0; depth < maxProtoChainDepth; depth++ {
		next, e := rt.getPrototypeOfValue(cur)
		if e != nil {
			return false, e
		}
		if next.IsNull() {
			return false, nil
		}
		if rt.sameValue(next, protoV) {
			return true, nil
		}
		cur = next
	}
	return false, nil
}

// jsInstanceof implements the `instanceof` operator via the ordinary
// [[HasInstance]] on a callable's .prototype (Symbol.hasInstance lands in
// Phase 5).
func (rt *Runtime) jsInstanceof(l, r Value) (bool, *ThrowError) {
	// A Symbol.hasInstance method on the RHS overrides the ordinary check.
	// getElement routes the lookup through a Proxy's [[Get]] trap (GetMethod).
	if rt.symHasInstance != 0 && r.IsObjectType() {
		hi, e := rt.getElement(r, rt.symHasInstance)
		if e != nil {
			return false, e
		}
		if rt.isCallable(hi) {
			res, e := rt.callValue(hi, r, []Value{l})
			if e != nil {
				return false, e
			}
			return rt.toBoolean(res), nil
		}
	}
	if !rt.isCallable(r) {
		return false, rt.typeError("right-hand side of 'instanceof' is not callable")
	}
	protoV, e := rt.getField(r, "prototype")
	if e != nil {
		return false, e
	}
	if !protoV.IsObjectType() {
		return false, rt.typeError("prototype is not an object")
	}
	if !l.IsObjectLike() {
		return false, nil
	}
	target := rt.objPtr(protoV)
	cur := rt.objPtr(l)
	for depth := 0; depth < maxProtoChainDepth && cur != nil; depth++ {
		if !cur.proto.IsObjectType() {
			break
		}
		next := rt.objPtr(cur.proto)
		if next == target {
			return true, nil
		}
		cur = next
	}
	return false, nil
}
