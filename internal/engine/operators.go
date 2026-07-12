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
func (rt *Runtime) deleteElement(obj, key Value) (bool, *ThrowError) {
	if obj.IsNullish() {
		return false, rt.typeError("cannot delete property of " + rt.nullishName(obj))
	}
	if idx, ok := arrayIndex(key); ok && obj.Type() == TArr {
		o := rt.objPtr(obj)
		if int(idx) < len(o.arr) {
			o.arr[idx] = tEmpty
		}
		return true, nil
	}
	o := rt.objPtr(obj)
	if o == nil {
		return true, nil
	}
	name, e := rt.propKeyString(key)
	if e != nil {
		return false, e
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
		for _, k := range o.ownKeysEnumerable() {
			if !seen[k] {
				seen[k] = true
				rt.arraySet(ao, ao.arrLen, rt.internString(k))
			}
		}
		cur = o.proto
	}
	return arr
}

// jsIn implements the `in` operator: key in obj.
func (rt *Runtime) jsIn(key, obj Value) (bool, *ThrowError) {
	if !obj.IsObjectType() && obj.Type() != TTypedArray {
		return false, rt.typeError("cannot use 'in' operator on a non-object")
	}
	if idx, ok := arrayIndex(key); ok && obj.Type() == TArr {
		o := rt.objPtr(obj)
		return idx < o.arrLen && int(idx) < len(o.arr) && !o.arr[idx].IsEmpty(), nil
	}
	name, e := rt.propKeyString(key)
	if e != nil {
		return false, e
	}
	return rt.hasProp(obj, name), nil
}

// jsInstanceof implements the `instanceof` operator via the ordinary
// [[HasInstance]] on a callable's .prototype (Symbol.hasInstance lands in
// Phase 5).
func (rt *Runtime) jsInstanceof(l, r Value) (bool, *ThrowError) {
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
	if !l.IsObjectType() {
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
