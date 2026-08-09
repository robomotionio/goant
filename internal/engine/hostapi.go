package engine

// The host API: the capabilities a conformance runner needs and an embedder
// running someone else's JavaScript must not hand out.
//
// These used to be installed by initBuiltins, which meant every Runtime this
// engine has ever built carried them — including the ones deskbot creates to
// run a customer's Function node. `$262.detachArrayBuffer` let any script
// invalidate any ArrayBuffer's bytes, `$262.createRealm` let it allocate realms
// without bound, and merely READING `$262.IsHTMLDDA` switched the compiled tier
// off for the realm, because the tier cannot represent an object that is falsy.
// None of that is a language feature; all of it is a capability the host grants.
//
// The gate is all-or-nothing on purpose. Splitting it per capability would
// invite a host to grant "just createRealm", and the honest question is not
// which of these a script may have but whether the script is trusted at all.

// EnableHostAPI installs the Test262 host object $262 on this Runtime's global,
// along with the two global capabilities it exposes — evalScript and
// createRealm.
//
// It is off by default. A conformance runner turns it on; an embedder running
// untrusted scripts must not, because $262 is a set of capabilities rather than
// a set of language features:
//
//   - detachArrayBuffer invalidates an ArrayBuffer's bytes, which JavaScript
//     itself can only do by transferring them away;
//   - createRealm allocates a whole realm per call, with nothing bounding how
//     many;
//   - evalScript compiles a Script into this realm, whose top-level var and
//     function declarations become NON-configurable global properties;
//   - reading IsHTMLDDA disables the compiled tier for the realm, for as long
//     as that realm lives.
//
// Calling it twice is harmless: the second call replaces the same properties
// with equivalent ones. See EnableAgents for $262.agent, which is a separate
// grant.
func (rt *Runtime) EnableHostAPI() {
	g := rt.objPtr(rt.global)

	// evalScript(source): evaluate source as a new SCRIPT in this realm. This is a
	// host capability rather than an ECMAScript one, and it is NOT eval: a
	// Script's top-level `var` and function declarations become NON-configurable
	// global properties, and its top-level let/const/class join the global lexical
	// environment where the next Script still sees them.
	g.defineOwn("evalScript", rt.newNativeFunc("evalScript", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sv, e := rt.toStringValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		return rt.evalScriptSource(rt.strGo(sv))
	}), attrWritable|attrConfigurable)

	// createRealm(): a second realm on the same value pools — a fresh global with
	// its own intrinsics, while objects still pass freely between the two. A host
	// capability, like evalScript, and the one Test262's $262.createRealm needs.
	// The returned object carries the new realm's global and an evalScript bound
	// to IT (a native is otherwise handed the CALLING runtime, which would compile
	// into the wrong realm).
	//
	// The new realm gets the host API too. A realm reached through this one was
	// created by a caller that already holds the grant, so withholding it there
	// would only break the cross-realm tests without withholding anything: the
	// grant is on the Runtime, and this call is proof the caller has it.
	g.defineOwn("createRealm", rt.newNativeFunc("createRealm", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		realm := rt.NewRealm()
		realm.EnableHostAPI()
		out := rt.newObject(rt.objectProto)
		oo := rt.objPtr(out)
		oo.defineOwn("global", realm.global, attrWritable|attrEnumerable|attrConfigurable)
		oo.defineOwn("evalScript", rt.newNativeFunc("evalScript", 1, func(caller *Runtime, this Value, args []Value) (Value, *ThrowError) {
			sv, e := caller.toStringValue(arg(args, 0))
			if e != nil {
				return mkundef(), e
			}
			return realm.evalScriptSource(string(caller.strBytes(sv)))
		}), attrWritable|attrEnumerable|attrConfigurable)
		cr, _ := realm.getField(realm.global, "createRealm")
		oo.defineOwn("createRealm", cr, attrWritable|attrEnumerable|attrConfigurable)
		return out, nil
	}), attrWritable|attrConfigurable)

	// $262: the object Test262 expects a host to provide. Every capability it
	// names already exists on the global here; this gathers them under the name
	// the suite looks for, which is what makes the cross-realm tests runnable
	// rather than skippable.
	ho := rt.objPtr(rt.host262Object())
	ho.defineOwn("global", rt.global, attrWritable|attrEnumerable|attrConfigurable)
	for _, name := range []string{"createRealm", "evalScript", "gc"} {
		if v, ok := g.getOwn(name); ok {
			ho.defineOwn(name, v, attrWritable|attrEnumerable|attrConfigurable)
		}
	}
	// IsHTMLDDA: document.all, the [[IsHTMLDDA]] exotic object. The one object
	// the language pretends is undefined -- typeof says so, it is falsy, and it
	// is loosely equal to both null and undefined -- while staying an ordinary
	// Object to everything else, which is what these tests exist to check. It is
	// callable and answers null when called with nothing or with "", so that an
	// algorithm which calls document.all can be told apart from one that does not.
	//
	// Handing one out turns the compiled tier OFF for this realm, and the reason
	// is worth saying plainly. The tier decides truthiness from the Value's tag
	// alone -- an object is truthy, no load, no branch -- and emits `x == null`
	// the same way. Neither can represent an object that is falsy and loosely
	// null, so with one in play the tier and the interpreter give different
	// answers, which is the worst kind of wrong. Teaching them would cost a load
	// and a test on the hottest branch shape in the engine, for an object that
	// exists in a legacy DOM and nowhere else.
	//
	// So it is a getter: a realm that never asks keeps its tier, and a realm that
	// asks has said it cares more about document.all than about speed.
	ho.defineAccessor("IsHTMLDDA", rt.newNativeFunc("get IsHTMLDDA", 0,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if rt.hasHTMLDDA {
				return rt.htmlDDA, nil
			}
			dda := rt.newNativeFunc("", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
				if len(args) == 0 {
					return mknull(), nil
				}
				if a := args[0]; a.IsString() && len(rt.strBytes(a)) == 0 {
					return mknull(), nil
				}
				return mkundef(), nil
			})
			rt.objPtr(dda).extend().isHTMLDDA = true
			rt.htmlDDA, rt.hasHTMLDDA = dda, true
			rt.SetJITEnabled(false)
			return dda, nil
		}), mkundef(), true, false, attrEnumerable|attrConfigurable)

	// detachArrayBuffer(buffer): the suite's way of reaching DetachArrayBuffer,
	// which JavaScript itself can only do by transferring the bytes away.
	ho.defineOwn("detachArrayBuffer", rt.newNativeFunc("detachArrayBuffer", 1,
		func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			o := rt.objPtr(arg(args, 0))
			if o == nil || o.ta != nil || o.dv() != nil {
				return mkundef(), rt.typeError("detachArrayBuffer takes an ArrayBuffer")
			}
			o.abuf = nil
			return mkundef(), nil
		}), attrWritable|attrEnumerable|attrConfigurable)
}

// host262Object returns this realm's $262, creating an empty one if the host has
// not granted the capabilities. EnableAgents needs somewhere to hang
// $262.agent and that grant is separate from this one, so the namespace has to
// be able to exist without anything dangerous on it.
func (rt *Runtime) host262Object() Value {
	g := rt.objPtr(rt.global)
	if v, ok := g.getOwn("$262"); ok && v.IsObjectType() {
		return v
	}
	h262 := rt.newObject(rt.objectProto)
	g.defineOwn("$262", h262, attrWritable|attrConfigurable)
	return h262
}
