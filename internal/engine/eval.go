package engine

// Direct eval support.
//
// `eval(src)` where the callee is the syntactic `eval` reference (still bound to
// the intrinsic %eval%) is a *direct* eval: it runs in the caller's variable
// environment, inheriting its strictness, `this`, `new.target`, home object, and
// in-scope bindings. The compiler recognises such a call site and emits OpEval
// instead of an ordinary call, attaching an evalScope snapshot of the caller's
// lexical context. At run time OpEval verifies the callee is still %eval% (else
// it degrades to an ordinary call) and, for a string argument, compiles and runs
// the source against that snapshot.
//
// An *indirect* eval — any other way of reaching %eval% — evaluates in global
// scope and sloppy mode; it flows through the native function (performIndirectEval).

// evalBindKind distinguishes how a caller binding visible to a direct eval is
// reached from the running caller frame.
type evalBindKind uint8

const (
	evalBindLocal evalBindKind = iota // a caller frame local slot
	evalBindUpval                     // an entry in the caller closure's upvalue array
)

// evalBinding names one caller binding a direct eval body may resolve to.
type evalBinding struct {
	name       string
	kind       evalBindKind
	slot       int // caller local slot (evalBindLocal) or upvalue index (evalBindUpval)
	isConst    bool
	selfName   bool // a named function expression's immutable self-reference (reassignment: strict TypeError / sloppy no-op)
	isLexical  bool // a let/const/class binding — an eval `var` may not shadow it
	catchParam bool // a simple identifier catch parameter: Annex B.3.4 lets an
	// eval'd `var` (or FunctionDeclaration) of the same name coexist with it
}

// evalScope is the compile-time snapshot of the lexical context at one direct
// eval call site: the caller bindings the eval body may borrow, plus flags
// governing which context-dependent constructs it may contain.
type evalScope struct {
	bindings []evalBinding

	// context-construct permissions (Script static-semantics early errors):
	// eval code may contain new.target/super/arguments only when the enclosing
	// (non-arrow, for new.target/arguments) function code permits them.
	newTargetAllowed bool
	superAllowed     bool
	argumentsAllowed bool

	// strict is the CALLER's strictness, fixed when the call site was compiled.
	// Reading it from the running frame instead is unreliable: a generator or
	// async body runs on its own goroutine, so the Runtime-wide flag can reflect
	// whichever frame last entered rather than the one performing the eval.
	strict bool

	// inFunction marks that the eval is nested in function code (its `var` and
	// hoisted function declarations bind in the caller's function VariableEnvironment
	// rather than on the global object).
	inFunction bool

	// paramNames is the set of formal-parameter names bound in the parameter
	// environment of a direct eval in a parameter-expression scope (and, for a
	// non-arrow function, `arguments`). A var declaration in the eval body that
	// duplicates one of them conflicts with the parameter binding:
	// EvalDeclarationInstantiation reports a SyntaxError. nil when the eval is not
	// in a parameter-expression scope.
	paramNames map[string]bool

	// inFieldInit marks a direct eval whose nearest non-arrow enclosing context is
	// a class field initializer (which has no `arguments` binding). Per the
	// "Additional Early Error Rules for Eval Inside Initializer", the eval body
	// containing an `arguments` reference is a SyntaxError.
	inFieldInit bool

	// privateEnvs are the mangled private-name maps of the class bodies enclosing
	// the eval site (innermost last), so a direct eval's `this.#x` resolves to the
	// same per-class storage key the enclosing class uses (private-name identity).
	privateEnvs []map[string]string
}

// evalInGlobalScope parses, compiles, and runs eval source in global scope. It
// backs both indirect eval and OpEval's pre-scope behaviour. strict seeds the
// parse; an inner "use strict" prologue can still promote a sloppy parse.
func (rt *Runtime) evalInGlobalScope(src string, strict bool) (Value, *ThrowError) {
	prog, perr := parseMode("<eval>", src, strict, false)
	if perr != nil {
		ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString(perr.Error())})
		return mkundef(), &ThrowError{Value: ev, rt: rt}
	}
	// A strict eval keeps its declarations in its own variable environment; only
	// a sloppy one creates global bindings, and then only after every one of them
	// is checked to be definable (EvalDeclarationInstantiation validates the whole
	// set before creating any, so a failure leaves nothing behind).
	if prog.Flags&fnParseStrict == 0 {
		if e := rt.validateGlobalEvalDeclarations(prog); e != nil {
			return mkundef(), e
		}
	}
	fn, cerr := rt.CompileEval(prog, "<eval>", src)
	if cerr != nil {
		ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString(cerr.Error())})
		return mkundef(), &ThrowError{Value: ev, rt: rt}
	}
	if prog.Flags&fnParseStrict == 0 {
		rt.prepareGlobalFuncBindings(prog)
	}
	return rt.runFrame(fn, nil, mkundef(), rt.global, nil)
}

// validateGlobalEvalDeclarations is EvalDeclarationInstantiation's definability
// check for eval code whose variable environment is the global one: an
// undefinable name — a non-configurable global property, or any new name on a
// non-extensible global — is a TypeError raised before a single binding is made.
func (rt *Runtime) validateGlobalEvalDeclarations(prog *Node) *ThrowError {
	funcNames := map[string]bool{}
	topLevelFuncNames(prog.Args, funcNames)
	for f := range funcNames {
		if !rt.canDeclareGlobalFunction(f) {
			return rt.typeError("Cannot declare global function '" + f + "'")
		}
	}
	for v := range evalVarDeclaredNames(prog.Args) {
		if funcNames[v] {
			continue
		}
		if !rt.canDeclareGlobalVar(v) {
			return rt.typeError("Cannot declare global variable '" + v + "'")
		}
	}
	return nil
}

// prepareGlobalFuncBindings applies CreateGlobalFunctionBinding for the top-level
// function declarations of a global-scope (indirect) eval: an absent or
// configurable existing global property is (re)defined with fresh data
// attributes (writable/enumerable/configurable) so the function value and its
// attributes overwrite a prior binding, unlike a plain var.
func (rt *Runtime) prepareGlobalFuncBindings(prog *Node) {
	funcNames := map[string]bool{}
	topLevelFuncNames(prog.Args, funcNames)
	g := rt.objPtr(rt.global)
	for name := range funcNames {
		if d := g.ownDescriptor(name); !d.exists || d.configable {
			g.defineOwn(name, mkundef(), attrWritable|attrEnumerable|attrConfigurable)
		}
	}
}

// performIndirectEval runs an indirect eval: always global scope, always sloppy
// unless the source itself begins with a "use strict" directive.
func (rt *Runtime) performIndirectEval(src string) (Value, *ThrowError) {
	return rt.evalInGlobalScope(src, false)
}

// ---- compile-time: direct-eval detection and scope capture ----

// isDirectEvalCall reports whether call node n is a direct eval: the callee is
// the bare identifier `eval` resolving to a free (global) reference, not a local
// or captured binding and not inside a `with` (where it could resolve dynamically).
func (c *compiler) isDirectEvalCall(n *Node) bool {
	if n.Left == nil || n.Left.Kind != NIdent || n.Left.Str != "eval" {
		return false
	}
	// A with-object may shadow `eval`, but only at RUN time — the call site is
	// still a direct eval when it does not. The callee is read through the with
	// chain (leaving its `this`) and OpEval checks it against the intrinsic,
	// falling back to an ordinary call when the with-object supplied its own.
	return c.resolveLocal("eval") < 0 && c.resolveUpvalue("eval") < 0
}

// captureEvalScope snapshots the caller bindings and context permissions visible
// at the current compile position for a direct eval site. Because the eval body
// may reference any variable reachable from here, every enclosing binding is
// force-captured as an upvalue (an ordinary reference would capture only the
// names it mentions statically).
func (c *compiler) captureEvalScope() *evalScope {
	sc := &evalScope{}
	seen := map[string]bool{}
	// Innermost-first over locals so a shadowing binding wins; skip internal
	// (`*`-prefixed) and private (`#`-prefixed) names — those are threaded
	// through context, not borrowed by name.
	for i := len(c.locals) - 1; i >= 0; i-- {
		lv := c.locals[i]
		if lv.dead || seen[lv.name] || !borrowableName(lv.name) {
			continue
		}
		seen[lv.name] = true
		sc.bindings = append(sc.bindings, evalBinding{name: lv.name, kind: evalBindLocal, slot: i, isConst: lv.isConst, selfName: lv.selfName, isLexical: lv.blockScoped, catchParam: lv.catchParam})
	}
	for i, u := range c.upvalues {
		if seen[u.name] || !borrowableName(u.name) {
			continue
		}
		seen[u.name] = true
		sc.bindings = append(sc.bindings, evalBinding{name: u.name, kind: evalBindUpval, slot: i, isConst: u.isConst, selfName: u.selfName})
	}
	// Enclosing-scope locals the eval may reach: force each into this function's
	// upvalue chain (creating the transitive captures) and borrow it as an upvalue.
	for e := c.enclosing; e != nil; e = e.enclosing {
		for i := len(e.locals) - 1; i >= 0; i-- {
			lv := e.locals[i]
			if lv.dead || seen[lv.name] || !borrowableName(lv.name) {
				continue
			}
			seen[lv.name] = true
			if uv := c.resolveUpvalue(lv.name); uv >= 0 {
				sc.bindings = append(sc.bindings, evalBinding{name: lv.name, kind: evalBindUpval, slot: uv, isConst: c.upvalues[uv].isConst, selfName: c.upvalues[uv].selfName, isLexical: lv.blockScoped, catchParam: lv.catchParam})
			}
		}
	}
	// Context permissions: inside function code (not the top-level script/eval),
	// new.target/arguments are permitted unless the immediately enclosing function
	// is an arrow; super is permitted when the function carries a home object or
	// class-super bindings. These are refined as later commits thread the context.
	sc.inFunction = !c.isScript
	sc.strict = c.fn.isStrict
	sc.newTargetAllowed = !c.isScript && !c.fn.isArrow
	sc.argumentsAllowed = !c.isScript && !c.fn.isArrow
	sc.superAllowed = c.superAvailable()
	if sc.superAllowed {
		// The eval body may reference super. An object-literal method captures its
		// [[HomeObject]] only when its own body reads super (usesSuper → OpSetHomeObj);
		// a super buried in an eval string is invisible to that check, so mark the
		// enclosing method now to force the home capture. (Class elements resolve
		// super via captured *superproto*/*this* bindings, handled separately.)
		for e := c; e != nil; e = e.enclosing {
			if e.fn == nil || e.fn.isArrow {
				continue
			}
			if e.fn.isMethod {
				e.fn.usesSuper = true
			}
			break
		}
	}
	if c.inParamExpr {
		sc.paramNames = c.paramNames
	}
	sc.inFieldInit = c.inClassFieldInitContext()
	if sc.inFieldInit {
		// A class field initializer is invoked (not constructed), so new.target is
		// permitted in a direct eval there and evaluates to undefined (the value is
		// overridden when the eval frame is entered).
		sc.newTargetAllowed = true
	}
	// Snapshot the enclosing class private environments in outermost-first order
	// (each compiler's own stack is already outer-to-inner), so the eval compiler
	// can mangle `#x` to the same key the declaring class uses.
	var chain []*compiler
	for e := c; e != nil; e = e.enclosing {
		chain = append(chain, e)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		sc.privateEnvs = append(sc.privateEnvs, chain[i].classPrivateEnvs...)
	}
	return sc
}

// inClassFieldInitContext reports whether the eval call site's nearest non-arrow
// enclosing function context is a class field initializer (which binds no
// `arguments`). Arrow functions are transparent (they inherit `arguments`), and
// a nested direct eval body inherits its caller's field-init context via the
// borrowed scope.
func (c *compiler) inClassFieldInitContext() bool {
	for e := c; e != nil; e = e.enclosing {
		if e.fn != nil && e.fn.isArrow {
			continue // an arrow does not bind its own `arguments`
		}
		if e.isEval {
			return e.borrowed != nil && e.borrowed.inFieldInit
		}
		return e.inFieldInit
	}
	return false
}

// topLevelFuncNames collects the names of top-level function declarations in an
// eval body (the eval's declaredFunctionNames, which bind via
// CanDeclareGlobalFunction rather than CanDeclareGlobalVar).
func topLevelFuncNames(stmts []*Node, out map[string]bool) {
	for _, n := range stmts {
		s := n
		if s != nil && s.Kind == NLabel {
			s = s.Body
		}
		if s != nil && s.Kind == NFunc && s.Str != "" && s.Flags&fnArrow == 0 {
			out[s.Str] = true
		}
	}
}

// evalVarDeclaredNames returns an eval body's VarDeclaredNames: its var and
// function-declaration names. Top-level class declarations (which collectVarFuncNames
// also gathers) are lexical — they bind in the eval's own declarative environment,
// not the variable environment — so they are excluded here.
func evalVarDeclaredNames(stmts []*Node) map[string]bool {
	return evalVarDeclaredNamesMode(stmts, false)
}

// evalVarDeclaredNamesMode is evalVarDeclaredNames with the Annex B.3.3
// extension switched off for strict code, where a block-level function
// declaration binds only in its block.
func evalVarDeclaredNamesMode(stmts []*Node, strict bool) map[string]bool {
	names := map[string]bool{}
	collectVarFuncNamesMode(stmts, names, strict)
	for _, n := range stmts {
		s := n
		if s != nil && s.Kind == NLabel {
			s = s.Body
		}
		if s != nil && s.Kind == NClass && s.Str != "" {
			delete(names, s.Str) // a class binds lexically, not as a var
		}
	}
	return names
}

// canDeclareGlobalVar mirrors CanDeclareGlobalVar: a new global var binding is
// allowed when the name already exists as an own global property or the global
// object is extensible.
func (rt *Runtime) canDeclareGlobalVar(name string) bool {
	g := rt.objPtr(rt.global)
	if g.hasOwn(name) {
		return true
	}
	return g.flags.extensible
}

// canDeclareGlobalFunction mirrors CanDeclareGlobalFunction: a new global
// function binding is allowed when the name is absent (and the global is
// extensible), the existing property is configurable, or it is an enumerable,
// writable data property.
func (rt *Runtime) canDeclareGlobalFunction(name string) bool {
	g := rt.objPtr(rt.global)
	d := g.ownDescriptor(name)
	if !d.exists {
		return g.flags.extensible
	}
	if d.configable {
		return true
	}
	return !d.isAccessor && d.writable && d.enumerable
}

// borrowableName reports whether a compiler binding name denotes a source-level
// identifier a direct eval can resolve to (excludes engine-internal `*...*` and
// private `#...` names).
func borrowableName(name string) bool {
	if name == "" {
		return false
	}
	switch name[0] {
	case '*', '#':
		return false
	}
	return true
}

// compileDirectEval emits a direct eval call site: the callee reference, the
// argument list, then OpEval carrying the scope-snapshot index and argument count.
func (c *compiler) compileDirectEval(n *Node) {
	c.compileDirectEvalAt(n, false)
}

// compileDirectEvalAt emits a direct eval call site. `tail` marks a call in tail
// position: a direct eval is never itself a tail call, but the SAME syntax is an
// ordinary call when the callee turns out not to be %eval% at run time, and that
// one is — so the flag rides in the scope index's high bit (evalTailFlag) for
// OpEval to act on.
func (c *compiler) compileDirectEvalAt(n *Node, tail bool) {
	sc := c.captureEvalScope()
	idx := len(c.fn.evalScopes)
	c.fn.evalScopes = append(c.fn.evalScopes, sc)
	if tail {
		idx |= evalTailFlag
	}

	if c.nameIsWithRouted("eval") {
		idx |= evalWithThisFlag
		c.emitWithVarCallee("eval") // [this, callee]
	} else {
		c.compileExpr(n.Left) // callee (the `eval` reference) — verified at run time
	}
	for _, arg := range n.Args {
		c.compileExpr(arg)
	}
	c.emit(OpEval)
	c.emitU16(uint16(idx))
	c.emitU16(uint16(len(n.Args)))
}

// evalTailFlag marks an OpEval whose call site is in tail position, and
// evalWithThisFlag one whose callee was read through a `with` chain, so a
// WithBaseObject `this` sits under it. Scope indices are per-function and small,
// so the two high bits are free.
const (
	evalTailFlag     = 0x8000
	evalWithThisFlag = 0x4000
)

// compileDirectEvalSpread emits a direct eval whose argument list contains a
// spread (`eval(...iter)`): build the full argument array (which iterates the
// spread source to completion), then hand OpEval that array's first element (or
// undefined when empty) — eval only consumes its first argument.
func (c *compiler) compileDirectEvalSpread(n *Node) {
	sc := c.captureEvalScope()
	idx := len(c.fn.evalScopes)
	c.fn.evalScopes = append(c.fn.evalScopes, sc)

	if c.nameIsWithRouted("eval") {
		idx |= evalWithThisFlag
		c.emitWithVarCallee("eval") // [this, callee]
	} else {
		c.compileExpr(n.Left) // callee (the `eval` reference), verified at run time
	}
	c.buildSpreadArray(n.Args) // [callee, argsArray]
	c.compileNumberLiteral(0)  // [callee, argsArray, 0]
	c.emit(OpGetElem)          // [callee, argsArray[0] | undefined]
	c.emit(OpEval)
	c.emitU16(uint16(idx))
	c.emitU16(1)
}

// resolveBorrowed resolves name against the caller bindings snapshotted for a
// direct eval body, adding it as an upvalue of the eval function on first use and
// returning that upvalue index (or -1). The eval compiler's upvalues are made up
// exclusively of borrowed bindings, so a name match is an identity match.
func (c *compiler) resolveBorrowed(name string) int {
	if c.borrowed == nil {
		return -1
	}
	for i, u := range c.upvalues {
		if u.name == name {
			return i
		}
	}
	for _, b := range c.borrowed.bindings {
		if b.name == name {
			c.upvalues = append(c.upvalues, upvalDesc{
				index:    b.slot,
				isLocal:  b.kind == evalBindLocal,
				isConst:  b.isConst,
				selfName: b.selfName,
				name:     name,
			})
			return len(c.upvalues) - 1
		}
	}
	return -1
}

// borrowedIsCallerVar reports whether a borrowed name is a binding of the CALLER
// FRAME itself rather than one the caller only reaches through its closure. Only
// the former lies in the eval's VariableEnvironment: EvalDeclarationInstantiation
// asks varEnv.HasBinding, so `var x` naming an ENCLOSING function's binding
// creates a new binding here that shadows it, instead of writing through.
func (c *compiler) borrowedIsCallerVar(name string) bool {
	if c.borrowed == nil {
		return false
	}
	for _, b := range c.borrowed.bindings {
		if b.name == name {
			return b.kind == evalBindLocal
		}
	}
	return false
}

// evalVarUpdatesBorrowed reports whether an eval `var`/function declaration of
// name writes through to the caller binding of that name. With no variable
// object of its own the eval has no alternative; with one, only a binding of the
// caller frame is in its variable environment.
func (c *compiler) evalVarUpdatesBorrowed(name string) bool {
	return !c.evalVarDynamic || c.borrowedIsCallerVar(name)
}

// ---- run time: compiling and executing a direct eval body ----

// parseEvalSource parses direct eval source in the caller's strictness and
// context: new.target is permitted when the enclosing function code allows it.
func parseEvalSource(filename, src string, strict bool, sc *evalScope) (*Node, error) {
	p := &parser{lx: newLexer(src, strict), filename: filename}
	p.newTargetOK = sc.newTargetAllowed
	program := p.mk(NProgram)
	p.parseStmtList(&program.Args, false, true)
	if p.err != nil {
		return nil, p.err
	}
	if strict || programIsStrict(program) {
		program.Flags |= fnParseStrict
	}
	return program, nil
}

// compileDirectEvalBody compiles a parsed direct eval body against a borrowed
// caller scope. Free names resolve to caller bindings (captured as upvalues);
// the eval's own let/const stay frame-local. strict is the eval's effective
// strictness (caller strict or a "use strict" prologue).
func (rt *Runtime) compileDirectEvalBody(prog *Node, filename, source string, sc *evalScope, strict bool, varObj Value) (*svFunc, error) {
	c := &compiler{
		rt:         rt,
		isScript:   true,
		isEval:     true,
		usingStack: -1,
		borrowed:   sc,
		// A sloppy global-scope eval's `var`/function declarations bind on the
		// global object. A strict eval always has its own variable environment
		// (declarations never leak); a function-scope sloppy eval keeps new names
		// frame-local for now (leaking into the caller's function VariableEnvironment
		// is future work).
		evalVarGlobal: !sc.inFunction && !strict,
		fn:            &svFunc{name: "", filename: filename, source: source, isStrict: strict},
	}
	// A direct eval sees the enclosing class private environments, so its `this.#x`
	// mangles to the same per-class key the declaring class uses (privateKey).
	c.classPrivateEnvs = sc.privateEnvs

	// When the calling function carries a dynamic variable object, every free name
	// in the eval body resolves against it (and the rest of the caller's with-chain)
	// before the borrowed static bindings — that is how a `var` an earlier eval
	// created becomes visible. A strict eval keeps its own declarations frame-local
	// but still READS through the chain, so the routing applies either way.
	dynVars := varObj.IsObjectType()
	if dynVars {
		c.inheritedWith = true
		c.fn.evalVarObj = true
		c.evalVarDynamic = !strict
	}

	c.completionSlot = c.addLocal("*completion*", false)
	c.emit(OpUndef)
	c.emitOpU16(OpPutLocal, uint16(c.completionSlot))

	thisSlot := c.addLocal("*this*", false)
	c.emit(OpThis)
	c.emitOpU16(OpPutLocal, uint16(thisSlot))

	// Hoist the eval body's var/function names so a reference before the
	// declaration reads undefined rather than throwing. A name that already exists
	// as a caller binding is updated in place (borrowed); a sloppy global-scope
	// eval binds new names on the global object (configurable, unlike a script's
	// var); otherwise the name is an eval-frame local (a strict eval's own
	// variable environment, or a function-scope eval whose new names stay local —
	// leaking into the caller's function scope is future work).
	{
		names := evalVarDeclaredNames(prog.Args)
		g := rt.objPtr(rt.global)
		for name := range names {
			if c.borrowed != nil && c.evalVarUpdatesBorrowed(name) && c.resolveBorrowed(name) >= 0 {
				continue
			}
			if c.evalVarGlobal {
				if !g.hasOwn(name) {
					g.defineOwn(name, mkundef(), attrWritable|attrEnumerable|attrConfigurable)
				}
				continue
			}
			// A sloppy function-scope eval creates the name in the CALLER's variable
			// environment — CreateMutableBinding(name, true), so unlike a script's
			// `var` the binding is deletable.
			if dynVars && !strict {
				if o := rt.objPtr(varObj); o != nil && !o.hasOwn(name) {
					o.defineOwn(name, mkundef(), attrWritable|attrEnumerable|attrConfigurable)
				}
				continue
			}
			if c.resolveLocal(name) < 0 {
				c.addLocal(name, false)
			}
		}
	}
	// CreateGlobalFunctionBinding: unlike a plain `var` (which leaves an existing
	// global binding untouched), a function declaration at global scope overwrites
	// an existing CONFIGURABLE property with fresh data attributes (writable,
	// enumerable, configurable), so its value and attributes replace the prior
	// binding. Redefine here (absent names were already defined by the var loop)
	// so the compiled value store below is not blocked by a non-writable property.
	if c.evalVarGlobal {
		funcNames := map[string]bool{}
		topLevelFuncNames(prog.Args, funcNames)
		g := rt.objPtr(rt.global)
		for name := range funcNames {
			if c.borrowed != nil && c.resolveBorrowed(name) >= 0 {
				continue
			}
			if d := g.ownDescriptor(name); d.exists && d.configable {
				g.defineOwn(name, mkundef(), attrWritable|attrEnumerable|attrConfigurable)
			}
		}
	}

	c.checkBlockDeclConflicts(prog.Args, false)
	c.hoistLexicals(prog.Args)
	c.hoistFunctions(prog.Args, false)
	c.compileStmts(prog.Args)
	if c.err != nil {
		return nil, c.err
	}
	c.emitOpU16(OpGetLocal, uint16(c.completionSlot))
	c.emit(OpReturn)

	c.fn.maxLocals = len(c.locals)
	if c.fn.maxStack < 8 {
		c.fn.maxStack = 8
	}
	c.fn.upvalDescs = c.upvalues // borrowed bindings are the eval function's upvalues
	return c.fn, nil
}

// performDirectEval evaluates direct eval source in the caller's frame context:
// the caller's strictness, `this`, `new.target`, and in-scope bindings (borrowed
// as upvalues via capture / the caller closure). It is invoked by OpEval with the
// running caller frame's state.
func (rt *Runtime) performDirectEval(src string, sc *evalScope, callerCl *closure,
	thisVal, newTarget Value, capture func(int) *upvalue) (Value, *ThrowError) {

	strict := sc.strict
	prog, perr := parseEvalSource("<eval>", src, strict, sc)
	if perr != nil {
		ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString(perr.Error())})
		return mkundef(), &ThrowError{Value: ev, rt: rt}
	}
	evalStrict := prog.Flags&fnParseStrict != 0

	// EvalDeclarationInstantiation for a sloppy global-scope eval validates that
	// every new global var/function binding is definable BEFORE creating any of
	// them: an undefinable name (a non-extensible global, or a function name
	// colliding with a non-configurable global property) is a TypeError, and no
	// partial bindings are left behind.
	if !sc.inFunction && !evalStrict {
		if e := rt.validateGlobalEvalDeclarations(prog); e != nil {
			return mkundef(), e
		}
	}

	// EvalDeclarationInstantiation: a sloppy direct eval's var/function declaration
	// may not hoist over a like-named lexical (let/const/class) binding of the
	// caller that lies between the eval and its variable environment — a
	// SyntaxError. A strict eval keeps its declarations in its own variable
	// environment, so no such conflict arises.
	if !evalStrict {
		var varNames map[string]bool
		for _, b := range sc.bindings {
			if !b.isLexical {
				continue
			}
			// Annex B.3.4: the Catch clause's environment record is exempt, so
			// `catch (err) { eval("var err;") }` is legal.
			if b.catchParam {
				continue
			}
			if varNames == nil {
				varNames = evalVarDeclaredNames(prog.Args)
			}
			if varNames[b.name] {
				ev, _ := rt.construct(rt.errors.syntaxErr,
					[]Value{rt.newString("Identifier '" + b.name + "' has already been declared")})
				return mkundef(), &ThrowError{Value: ev, rt: rt}
			}
		}
	}

	// EvalDeclarationInstantiation: a `var arguments` (or a function named
	// `arguments`) in a non-arrow function's parameter-expression scope conflicts
	// with the parameter environment's `arguments` binding — a SyntaxError.
	if sc.paramNames != nil {
		for name := range evalVarDeclaredNames(prog.Args) {
			if sc.paramNames[name] {
				ev, _ := rt.construct(rt.errors.syntaxErr,
					[]Value{rt.newString("Cannot declare '" + name + "' in this eval context (it names a parameter)")})
				return mkundef(), &ThrowError{Value: ev, rt: rt}
			}
		}
	}

	// Additional Early Error Rules for Eval Inside Initializer: a direct eval in a
	// class field initializer may not contain an `arguments` reference (the
	// initializer establishes no `arguments` binding). This is a SyntaxError raised
	// before the body executes.
	if sc.inFieldInit {
		for _, st := range prog.Args {
			if nodeContainsArguments(st) {
				ev, _ := rt.construct(rt.errors.syntaxErr,
					[]Value{rt.newString("'arguments' is not allowed in a class field initializer eval")})
				return mkundef(), &ThrowError{Value: ev, rt: rt}
			}
		}
	}

	// The caller's dynamic scope, claimed here so a nested call made while
	// compiling or running this eval cannot overwrite it.
	varObj, callerWith := rt.callerVarObj, rt.callerWithStack

	fn, cerr := rt.compileDirectEvalBody(prog, "<eval>", src, sc, evalStrict, varObj)
	if cerr != nil {
		ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString(cerr.Error())})
		return mkundef(), &ThrowError{Value: ev, rt: rt}
	}

	// Build the eval closure's upvalues from its borrowed-binding descriptors —
	// a caller local is captured live (shared cell), a caller upvalue is aliased.
	upvals := make([]*upvalue, len(fn.upvalDescs))
	for i, d := range fn.upvalDescs {
		if d.isLocal {
			upvals[i] = capture(d.index)
		} else if callerCl != nil && d.index < len(callerCl.upvalues) {
			upvals[i] = callerCl.upvalues[d.index]
		} else {
			upvals[i] = &upvalue{closed: mkundef()}
			upvals[i].location = &upvals[i].closed
		}
	}
	evalCl := &closure{fn: fn, upvalues: upvals, privEnv: rt.callerPrivEnv}
	if callerCl != nil {
		evalCl.home = callerCl.home // object-literal method super in eval
	}
	if fn.evalVarObj {
		// Run in the caller's dynamic scope: the same with-object chain (ending in
		// its variable object), which the eval frame seeds from here.
		evalCl.capturedWith = append([]Value(nil), callerWith...)
		rt.pendingVarObj = varObj
	}

	// Thread the caller's new.target into the eval frame (a non-arrow function
	// direct eval sees its new.target; OpSpecialObj kind 2 reads the frame value).
	if sc.inFieldInit {
		newTarget = mkundef() // a field initializer is called, not constructed
	}
	rt.pendingNewTarget = newTarget
	return rt.runFrame(fn, evalCl, mkundef(), thisVal, nil)
}
