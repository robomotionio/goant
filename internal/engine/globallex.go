package engine

// The global lexical environment (9.1.1.4). A Script's top-level `let`, `const`
// and `class` do NOT become properties of the global object: they live in the
// global environment record's declarative part, where the next Script — and any
// eval — still sees them, and where they shadow a same-named global property.
//
// goant keeps them as ordinary frame locals of the Script that declared them, so
// that Script's own code, its closures and its temporal dead zone all work
// unchanged. What makes them global is this registry: it holds the declaring
// frame's locals slice, which is allocated once and never moves, so a later
// reader sees the live binding. It is the same aliasing modules rely on.

// globalLexBinding is one Script-level lexical binding, addressed in the frame
// that declared it. Its value is the empty Value while in its temporal dead zone.
type globalLexBinding struct {
	locals  []Value
	slot    int
	isConst bool
}

func (b *globalLexBinding) get() Value { return b.locals[b.slot] }

// globalLexDecl is the compile-time half: which slot of the Script frame holds
// the binding, recorded on the svFunc so frame entry can register it.
type globalLexDecl struct {
	slot    int
	isConst bool
}

// lookupGlobalLex finds a global lexical binding by name.
func (rt *Runtime) lookupGlobalLex(name string) *globalLexBinding {
	if rt.globalLex == nil {
		return nil
	}
	return rt.globalLex[name]
}

// registerGlobalLex publishes a Script frame's top-level lexical bindings, which
// is what makes them visible outside the frame. Called at frame entry, before
// any of the bindings is initialised, so a reference from a nested call reaches
// the temporal dead zone rather than nothing at all.
func (rt *Runtime) registerGlobalLex(fn *svFunc, locals []Value) {
	if rt.globalLex == nil {
		rt.globalLex = map[string]*globalLexBinding{}
	}
	for name, d := range fn.globalLex {
		rt.globalLex[name] = &globalLexBinding{locals: locals, slot: d.slot, isConst: d.isConst}
	}
	// GET_GLOBAL caches the global object's own slot for names with no lexical
	// binding, and shadowing is not visible in that object's shape. A binding
	// appearing here can shadow a name some site already cached, so those
	// entries have to go. This runs once per Script frame, not per access.
	if len(fn.globalLex) > 0 {
		icEpochBump()
	}
}

// globalLexRead is GetBindingValue for a global lexical: reading it before its
// declaration has run is a ReferenceError, not undefined.
func (rt *Runtime) globalLexRead(b *globalLexBinding, name string) (Value, *ThrowError) {
	v := b.get()
	if v.IsEmpty() {
		return mkundef(), rt.referenceError("Cannot access '" + name + "' before initialization")
	}
	return v, nil
}

// globalLexWrite is SetMutableBinding for a global lexical.
func (rt *Runtime) globalLexWrite(b *globalLexBinding, name string, v Value) *ThrowError {
	if b.get().IsEmpty() {
		return rt.referenceError("Cannot access '" + name + "' before initialization")
	}
	if b.isConst {
		return rt.typeError("Assignment to constant variable '" + name + "'")
	}
	b.locals[b.slot] = v
	return nil
}

// hasRestrictedGlobalProperty implements HasRestrictedGlobalProperty: a Script's
// top-level lexical declaration may not shadow a NON-configurable own property
// of the global object (`let undefined`, `let NaN`), which is an early error.
func (rt *Runtime) hasRestrictedGlobalProperty(name string) bool {
	d := rt.objPtr(rt.global).ownDescriptor(name)
	return d.exists && !d.configable
}

// GlobalDeclError is a TypeError raised by GlobalDeclarationInstantiation before
// a Script runs: one of its top-level declarations names a binding the global
// environment cannot create (a non-configurable property, or any new name on a
// non-extensible global). It is returned from Compile because it is decided
// there, but it is a runtime TypeError, not an early SyntaxError.
type GlobalDeclError struct{ Msg string }

func (e *GlobalDeclError) Error() string { return "TypeError: " + e.Msg }

// topLevelLexicalNames returns the names a Script's top-level let / const /
// class declarations bind — the ones that belong to the global environment's
// declarative record.
func topLevelLexicalNames(stmts []*Node) []string {
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, s := range stmts {
		if s == nil {
			continue
		}
		if s.Kind == NClass {
			add(s.Str)
			continue
		}
		if s.Kind != NVar || (s.VarKind != VarLet && s.VarKind != VarConst) {
			continue
		}
		for _, decl := range s.Args {
			var names []string
			collectPatternNames(decl.Left, &names)
			for _, n := range names {
				add(n)
			}
		}
	}
	return out
}

// globalDeclarationInstantiation performs the checks and bindings a Script's
// top level needs before it runs (16.1.7): every lexical name must be free of a
// restricted global property and of an existing global lexical binding, every
// var and function name must be definable, and only once ALL of that holds are
// any bindings created — a failing Script leaves the global environment
// untouched.
func (rt *Runtime) globalDeclarationInstantiation(prog *Node, strict bool) error {
	lexNames := topLevelLexicalNames(prog.Args)
	for _, name := range lexNames {
		if rt.hasRestrictedGlobalProperty(name) || rt.lookupGlobalLex(name) != nil {
			return &SyntaxError{Msg: "Identifier '" + name + "' has already been declared"}
		}
	}
	varNames := evalVarDeclaredNamesMode(prog.Args, strict)
	for name := range varNames {
		if rt.lookupGlobalLex(name) != nil {
			return &SyntaxError{Msg: "Identifier '" + name + "' has already been declared"}
		}
	}
	funcNames := map[string]bool{}
	topLevelFuncNames(prog.Args, funcNames)
	for name := range funcNames {
		if !rt.canDeclareGlobalFunction(name) {
			return &GlobalDeclError{Msg: "Cannot declare global function '" + name + "'"}
		}
	}
	for name := range varNames {
		if funcNames[name] {
			continue
		}
		if !rt.canDeclareGlobalVar(name) {
			return &GlobalDeclError{Msg: "Cannot declare global variable '" + name + "'"}
		}
	}
	g := rt.objPtr(rt.global)
	// CreateGlobalFunctionBinding: unlike a plain var, a function declaration
	// REPLACES a configurable existing property with fresh attributes. A Script's
	// bindings are non-configurable (an eval's are not).
	for name := range funcNames {
		if d := g.ownDescriptor(name); !d.exists || d.configable {
			g.defineOwn(name, mkundef(), attrWritable|attrEnumerable)
		}
	}
	for name := range varNames {
		if funcNames[name] {
			continue
		}
		if !g.hasOwn(name) {
			g.defineOwn(name, mkundef(), attrWritable|attrEnumerable)
		}
	}
	return nil
}
