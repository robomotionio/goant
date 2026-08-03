package engine

// Resolving a free name against a `with` chain.
//
// A function containing a `with` — or a direct eval, which gives the frame a
// dynamic variable object and puts it on the same chain — compiles every free
// name to WITH_GET_VAR, WITH_PUT_VAR or WITH_DEL_VAR rather than to a local, an
// upvalue or a global. The compiler still works out which of those three it
// *would* have been and bakes that in as a fallback, so the resolution is: walk
// the chain, and if no object binds the name, do what the lexical answer says.
//
// The three below are the interpreter's cases with the operand stack taken out,
// so that the compiled tier can call the same code rather than reimplement it.
// They are worth sharing more than most: each is a spec algorithm with an
// observable trap on every step, and the traps are the whole difficulty —
// HasBinding does a HasProperty, then consults @@unscopables only if that said
// yes, and then GetBindingValue does its *own* HasProperty before the Get, which
// a Proxy can observe separately and which an @@unscopables getter can falsify
// in between.

// withFallback names what the compiler resolved a free name to when no
// with-object binds it. It is the low nibble of the flags byte.
const (
	withFallbackGlobal = 0
	withFallbackLocal  = 1
	withFallbackUpval  = 2
)

// Flag bits in the byte that follows a WITH_GET_VAR's operands.
const (
	withFlagRef      = 0x80 // also produce the base, for a paired write
	withFlagLenient  = 0x40 // a `typeof` read: an unresolvable name is undefined
	withFlagBaseOnly = 0x20 // reference mode: resolve the base and read nothing
	withFlagThis     = 0x10 // reference mode: the base is a call's receiver
)

// withScopeOf reports the object in chain that binds name, if any.
//
// This is HasBinding and nothing more: one HasProperty, and the @@unscopables
// Get only if it said yes. The SECOND HasProperty each caller may need belongs
// to GetBindingValue or to SetMutableBinding rather than here, and hoisting it
// up is observable — a Proxy binding object counts one `has` too many, and a
// plain assignment, which resolves its base without ever reading through it,
// would perform a lookup the spec does not ask for.
func (rt *Runtime) withScopeOf(chain []Value, name string) (obj Value, found bool, e *ThrowError) {
	for k := len(chain) - 1; k >= 0; k-- {
		has, e := rt.hasPropE(chain[k], name)
		if e != nil {
			return 0, false, e
		}
		if !has {
			continue
		}
		// Consulted only once the object actually has the name: HasBinding
		// returns before the @@unscopables Get when HasProperty is false.
		unscoped, ue := rt.isUnscopable(chain[k], name)
		if ue != nil {
			return 0, false, ue
		}
		if unscoped {
			continue
		}
		return chain[k], true, nil
	}
	return 0, false, nil
}

// withGetVar reads a free name.
//
// It returns the value and, in reference mode, the base to write back through —
// tEmpty meaning "no with-object bound it, use the lexical fallback", which is
// the marker withPutVar reads. hasBase says whether the caller should make room
// for that base; it is a property of the flags byte, so both tiers know it
// before the call.
func (rt *Runtime) withGetVar(fn *svFunc, cl *closure, locals, chain []Value,
	name string, flags byte, fbIndex int) (base, val Value, e *ThrowError) {

	refMode := flags&withFlagRef != 0
	baseOnly := refMode && flags&withFlagBaseOnly != 0
	thisMode := refMode && flags&withFlagThis != 0

	obj, found, e := rt.withScopeOf(chain, name)
	if e != nil {
		return 0, 0, e
	}
	if found {
		if baseOnly {
			// A plain assignment resolves its base and reads nothing through
			// it, so GetBindingValue never runs and neither does its lookup.
			return obj, mkundef(), nil
		}
		// GetBindingValue performs its OWN HasProperty before the Get: a second
		// observable trap on a Proxy, and the point at which a binding an
		// @@unscopables getter deleted in between is caught.
		still, se := rt.hasPropE(obj, name)
		if se != nil {
			return 0, 0, se
		}
		if !still {
			// The binding went away between HasBinding and GetBindingValue.
			// Strict code throws; sloppy code reads undefined.
			if fn.isStrict {
				return 0, 0, rt.referenceError(name + " is not defined")
			}
			return obj, mkundef(), nil
		}
		v, ge := rt.getField(obj, name)
		if ge != nil {
			return 0, 0, ge
		}
		return obj, v, nil
	}

	// No with-object binds it. In reference mode the base says which fallback a
	// later write should take: undefined for a call's receiver, and the empty
	// marker for everything else.
	base = tEmpty
	if thisMode {
		base = mkundef()
	}
	if baseOnly {
		return base, mkundef(), nil
	}

	switch flags & 0x0f {
	case withFallbackLocal:
		if fbIndex >= len(locals) {
			return 0, 0, rt.typeError("JIT with-fallback slot")
		}
		lv := locals[fbIndex]
		if lv.IsEmpty() {
			return 0, 0, rt.referenceError("Cannot access a lexical binding before initialization")
		}
		return base, lv, nil
	case withFallbackUpval:
		if cl == nil || fbIndex >= len(cl.upvalues) {
			return 0, 0, rt.typeError("JIT with-fallback upvalue")
		}
		uv := cl.upvalues[fbIndex].get()
		if uv.IsEmpty() {
			return 0, 0, rt.referenceError("Cannot access a lexical binding before initialization")
		}
		return base, uv, nil
	default:
		// GetValue on an unresolvable reference throws, exactly as a plain
		// global read would; `typeof` marks itself lenient instead.
		if flags&withFlagLenient == 0 && !rt.hasProp(rt.global, name) {
			return 0, 0, rt.referenceError(name + " is not defined")
		}
		v, ge := rt.getField(rt.global, name)
		if ge != nil {
			return 0, 0, ge
		}
		return base, v, nil
	}
}

// withPutLexical writes through the fallback the compiler baked in.
func (rt *Runtime) withPutLexical(fn *svFunc, cl *closure, locals []Value,
	name string, fbKind byte, fbIndex int, val Value) *ThrowError {

	switch fbKind {
	case withFallbackLocal:
		if fbIndex >= len(locals) {
			return rt.typeError("JIT with-fallback slot")
		}
		locals[fbIndex] = val
		return nil
	case withFallbackUpval:
		if cl == nil || fbIndex >= len(cl.upvalues) {
			return rt.typeError("JIT with-fallback upvalue")
		}
		cl.upvalues[fbIndex].set(val)
		return nil
	default:
		// A strict assignment to a name bound by no with-object and no lexical
		// binding is an assignment to an undeclared global, and a ReferenceError.
		if fn.isStrict && !rt.hasProp(rt.global, name) {
			return rt.referenceError(name + " is not defined")
		}
		if !rt.setProp(rt.global, name, val) && fn.isStrict {
			return rt.typeError("Cannot assign to read only property '" + name + "'")
		}
		return nil
	}
}

// withPutVar writes a free name.
//
// In reference mode base is what the paired read produced, and the write goes
// back through it rather than re-walking the chain — which is what makes a
// compound assignment read and write the same binding.
func (rt *Runtime) withPutVar(fn *svFunc, cl *closure, locals, chain []Value,
	name string, flags byte, fbIndex int, base, val Value) *ThrowError {

	if flags&withFlagRef != 0 {
		if base.IsEmpty() {
			return rt.withPutLexical(fn, cl, locals, name, flags&0x7f, fbIndex, val)
		}
		// SetMutableBinding re-checks HasProperty: if the property was deleted
		// between the reference's read and this write, a strict assignment is a
		// ReferenceError rather than a re-creation.
		has, he := rt.hasPropE(base, name)
		if he != nil {
			return he
		}
		if !has && fn.isStrict {
			return rt.referenceError(name + " is not defined")
		}
		return rt.setField(base, name, val)
	}

	obj, found, e := rt.withScopeOf(chain, name)
	if e != nil {
		return e
	}
	if found {
		// SetMutableBinding re-checks HasProperty before the Set: the second
		// trap, and where a strict write to a binding deleted since HasBinding
		// is caught.
		still, se := rt.hasPropE(obj, name)
		if se != nil {
			return se
		}
		if !still && fn.isStrict {
			return rt.referenceError(name + " is not defined")
		}
		return rt.setField(obj, name, val)
	}
	return rt.withPutLexical(fn, cl, locals, name, flags&0x7f, fbIndex, val)
}

// withDelVar is `delete name` inside a with, which is the only delete whose
// target is a binding rather than a property expression.
func (rt *Runtime) withDelVar(chain []Value, name string) (bool, *ThrowError) {
	obj, found, e := rt.withScopeOf(chain, name)
	if e != nil {
		return false, e
	}
	if found {
		return rt.deleteElement(obj, rt.internString(name))
	}
	// Not bound by a with-object: a global-object delete, which is false for a
	// declared var or function and true for an implicit global or an absent name.
	return rt.deleteElement(rt.global, rt.internString(name))
}
