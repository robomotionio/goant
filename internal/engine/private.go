package engine

// Private class elements (`#x` fields, `#m()` methods, and `get/set #m`
// accessors). They are stored per-object in object.priv rather than as ordinary
// shape properties, so they are invisible to reflection and never collide with
// an ordinary "#x" string property. Access to a private name the object's class
// did not declare (the "brand check") throws a TypeError.

// isPrivateKey reports whether a member-access key names a private element. A
// syntactic private access is compiled to a per-class mangled key `#x\x00<id>`
// (see compiler.privateKey), so it always contains a NUL; an ordinary string
// property key such as `"#x"` (from `obj["#x"]`) never does and stays public.
func isPrivateKey(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] == 0 {
			return true
		}
	}
	return false
}

// privDisplay strips the per-class mangling suffix (`#x\x00<id>` -> `#x`) so a
// brand-check error message shows the source-level private name.
func privDisplay(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == 0 {
			return name[:i]
		}
	}
	return name
}

type privKind uint8

const (
	privField privKind = iota
	privMethod
	privAccessor
)

// privElem is one private element bound on an object (its name keeps the
// leading '#').
type privElem struct {
	name   string
	kind   privKind
	value  Value // field value or method function
	getter Value
	setter Value
}

// findPriv returns the private element named name (or nil).
func (o *object) findPriv(name string) *privElem {
	for i := range o.priv {
		if o.priv[i].name == name {
			return &o.priv[i]
		}
	}
	return nil
}

// definePrivateField adds a private data field. It reports false (a TypeError to
// the caller) if the object's brand already carries the name — a field may be
// installed on a given object only once.
func (o *object) definePrivateField(name string, v Value) bool {
	if o.findPriv(name) != nil {
		return false
	}
	o.priv = append(o.priv, privElem{name: name, kind: privField, value: v})
	return true
}

// definePrivateMethod adds a private method (a shared function installed in the
// object's private environment). It reports false (a TypeError to the caller) if
// a private element of that name already exists — a private method may be
// installed on a given object only once (e.g. a return-overridden constructor
// re-run on the same object).
func (o *object) definePrivateMethod(name string, fn Value) bool {
	if o.findPriv(name) != nil {
		return false
	}
	o.priv = append(o.priv, privElem{name: name, kind: privMethod, value: fn})
	return true
}

// definePrivateAccessor adds — or, during a single instance's initialization,
// completes — a private get/set accessor pair. It reports false (a TypeError)
// when the half being installed is already present (a second initialization of
// the same object) or the name is bound to a non-accessor element.
func (o *object) definePrivateAccessor(name string, fn Value, isGetter bool) bool {
	if e := o.findPriv(name); e != nil {
		if e.kind != privAccessor {
			return false
		}
		if isGetter {
			if !e.getter.IsUndefined() {
				return false
			}
			e.getter = fn
		} else {
			if !e.setter.IsUndefined() {
				return false
			}
			e.setter = fn
		}
		return true
	}
	pe := privElem{name: name, kind: privAccessor, getter: mkundef(), setter: mkundef()}
	if isGetter {
		pe.getter = fn
	} else {
		pe.setter = fn
	}
	o.priv = append(o.priv, pe)
	return true
}

// getPrivate implements a private member read `obj.#name`: a brand check
// (TypeError if absent), then the field/method value or the getter's result.
func (rt *Runtime) getPrivate(obj Value, name string) (Value, *ThrowError) {
	o := rt.objPtr(obj)
	var e *privElem
	if o != nil {
		e = o.findPriv(name)
	}
	if e == nil {
		return mkundef(), rt.typeError("Cannot read private member " + privDisplay(name) + " from an object whose class did not declare it")
	}
	if e.kind == privAccessor {
		if e.getter.IsUndefined() {
			return mkundef(), rt.typeError("'" + privDisplay(name) + "' was defined without a getter")
		}
		return rt.callValue(e.getter, obj, nil)
	}
	return e.value, nil
}

// setPrivate implements a private member write `obj.#name = v`: a brand check,
// then a field assignment, the setter's invocation, or a TypeError for a method
// / getter-only accessor.
func (rt *Runtime) setPrivate(obj Value, name string, v Value) *ThrowError {
	o := rt.objPtr(obj)
	var e *privElem
	if o != nil {
		e = o.findPriv(name)
	}
	if e == nil {
		return rt.typeError("Cannot write private member " + privDisplay(name) + " to an object whose class did not declare it")
	}
	switch e.kind {
	case privField:
		e.value = v
		return nil
	case privAccessor:
		if e.setter.IsUndefined() {
			return rt.typeError("'" + privDisplay(name) + "' was defined without a setter")
		}
		_, err := rt.callValue(e.setter, obj, []Value{v})
		return err
	default: // method
		return rt.typeError("Cannot write to private method " + privDisplay(name))
	}
}

// hasPrivate implements the private brand check `#name in obj`.
func (rt *Runtime) hasPrivate(obj Value, name string) bool {
	o := rt.objPtr(obj)
	return o != nil && o.findPriv(name) != nil
}
