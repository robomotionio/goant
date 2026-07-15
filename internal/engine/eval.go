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
	name    string
	kind    evalBindKind
	slot    int // caller local slot (evalBindLocal) or upvalue index (evalBindUpval)
	isConst bool
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
// at the current compile position for a direct eval site.
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
		sc.bindings = append(sc.bindings, evalBinding{name: lv.name, kind: evalBindLocal, slot: i, isConst: lv.isConst})
	}
	for i, u := range c.upvalues {
		if seen[u.name] || !borrowableName(u.name) {
			continue
		}
		seen[u.name] = true
		sc.bindings = append(sc.bindings, evalBinding{name: u.name, kind: evalBindUpval, slot: i, isConst: u.isConst})
	}
	// Context permissions: inside function code (not the top-level script/eval),
	// new.target/arguments are permitted unless the immediately enclosing function
	// is an arrow; super is permitted when the function carries a home object or
	// class-super bindings. These are refined as later commits thread the context.
	sc.inFunction = !c.isScript
	sc.newTargetAllowed = !c.isScript && !c.fn.isArrow
	sc.argumentsAllowed = !c.isScript && !c.fn.isArrow
	sc.superAllowed = !c.isScript
	return sc
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
