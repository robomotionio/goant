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
	name      string
	kind      evalBindKind
	slot      int  // caller local slot (evalBindLocal) or upvalue index (evalBindUpval)
	isConst   bool
	isLexical bool // a let/const/class binding — an eval `var` may not shadow it
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

	// inFunction marks that the eval is nested in function code (its `var` and
	// hoisted function declarations bind in the caller's function VariableEnvironment
	// rather than on the global object).
	inFunction bool

	// paramArgsConflict marks a direct eval in the parameter-expression scope of a
	// non-arrow function, where the parameter environment binds `arguments`. A
	// var declaration of `arguments` in the eval body then conflicts:
	// EvalDeclarationInstantiation reports a SyntaxError.
	paramArgsConflict bool
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
	fn, cerr := rt.CompileEval(prog, "<eval>", src)
	if cerr != nil {
		ev, _ := rt.construct(rt.errors.syntaxErr, []Value{rt.newString(cerr.Error())})
		return mkundef(), &ThrowError{Value: ev, rt: rt}
	}
	return rt.runFrame(fn, nil, mkundef(), rt.global, nil)
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
	if c.withDepth != 0 {
		return false
	}
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
		sc.bindings = append(sc.bindings, evalBinding{name: lv.name, kind: evalBindLocal, slot: i, isConst: lv.isConst, isLexical: lv.blockScoped})
	}
	for i, u := range c.upvalues {
		if seen[u.name] || !borrowableName(u.name) {
			continue
		}
		seen[u.name] = true
		sc.bindings = append(sc.bindings, evalBinding{name: u.name, kind: evalBindUpval, slot: i, isConst: u.isConst})
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
				sc.bindings = append(sc.bindings, evalBinding{name: lv.name, kind: evalBindUpval, slot: uv, isConst: c.upvalues[uv].isConst, isLexical: lv.blockScoped})
			}
		}
	}
	// Context permissions: inside function code (not the top-level script/eval),
	// new.target/arguments are permitted unless the immediately enclosing function
	// is an arrow; super is permitted when the function carries a home object or
	// class-super bindings. These are refined as later commits thread the context.
	sc.inFunction = !c.isScript
	sc.newTargetAllowed = !c.isScript && !c.fn.isArrow
	sc.argumentsAllowed = !c.isScript && !c.fn.isArrow
	sc.superAllowed = !c.isScript
	sc.paramArgsConflict = c.inParamExpr && !c.fn.isArrow
	return sc
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
	names := map[string]bool{}
	collectVarFuncNames(stmts, names)
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
	sc := c.captureEvalScope()
	idx := len(c.fn.evalScopes)
	c.fn.evalScopes = append(c.fn.evalScopes, sc)

	c.compileExpr(n.Left) // callee (the `eval` reference) — verified at run time
	for _, arg := range n.Args {
		c.compileExpr(arg)
	}
	c.emit(OpEval)
	c.emitU16(uint16(idx))
	c.emitU16(uint16(len(n.Args)))
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
				index:   b.slot,
				isLocal: b.kind == evalBindLocal,
				isConst: b.isConst,
				name:    name,
			})
			return len(c.upvalues) - 1
		}
	}
	return -1
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
func (rt *Runtime) compileDirectEvalBody(prog *Node, filename, source string, sc *evalScope, strict bool) (*svFunc, error) {
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
			if c.borrowed != nil && c.resolveBorrowed(name) >= 0 {
				continue
			}
			if c.evalVarGlobal {
				if !g.hasOwn(name) {
					g.defineOwn(name, mkundef(), attrWritable|attrEnumerable|attrConfigurable)
				}
				continue
			}
			if c.resolveLocal(name) < 0 {
				c.addLocal(name, false)
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

	strict := rt.frameStrict
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
		funcNames := map[string]bool{}
		topLevelFuncNames(prog.Args, funcNames)
		allNames := evalVarDeclaredNames(prog.Args)
		for f := range funcNames {
			if !rt.canDeclareGlobalFunction(f) {
				return mkundef(), rt.typeError("Cannot declare global function '" + f + "'")
			}
		}
		for v := range allNames {
			if funcNames[v] {
				continue
			}
			if !rt.canDeclareGlobalVar(v) {
				return mkundef(), rt.typeError("Cannot declare global variable '" + v + "'")
			}
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
	if sc.paramArgsConflict {
		names := evalVarDeclaredNames(prog.Args)
		if names["arguments"] {
			ev, _ := rt.construct(rt.errors.syntaxErr,
				[]Value{rt.newString("Cannot declare 'arguments' in this eval context")})
			return mkundef(), &ThrowError{Value: ev, rt: rt}
		}
	}

	fn, cerr := rt.compileDirectEvalBody(prog, "<eval>", src, sc, evalStrict)
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
	evalCl := &closure{fn: fn, upvalues: upvals}
	if callerCl != nil {
		evalCl.home = callerCl.home // object-literal method super in eval
	}

	// Thread the caller's new.target into the eval frame (a non-arrow function
	// direct eval sees its new.target; OpSpecialObj kind 2 reads the frame value).
	rt.pendingNewTarget = newTarget
	return rt.runFrame(fn, evalCl, mkundef(), thisVal, nil)
}
