package engine

// Function & call compilation (ant compiler.c compile_func_expr / compile_call
// / compile_function_body). The Phase 3 slice supports simple (identifier)
// parameters, block and expression arrow bodies, closures with upvalue capture,
// hoisted function declarations, and ordinary calls. Default/rest/destructuring
// parameters, `arguments`, generators, and async land as the port continues.

// hoistFunctions pre-binds function declarations in a statement list so they are
// callable before their textual position (ant function hoisting). blockScoped
// marks a nested block: in strict mode its function declarations are lexically
// scoped to the block rather than the enclosing function.
func (c *compiler) hoistFunctions(list []*Node, blockScoped bool) {
	for _, stmt := range list {
		fn := stmt
		// Annex B (sloppy): a labeled function declaration `label: function f(){}`
		// hoists like a plain one.
		for !c.fn.isStrict && fn != nil && fn.Kind == NLabel {
			fn = fn.Body
		}
		if fn == nil || fn.Kind != NFunc || fn.Str == "" || fn.Flags&fnArrow != 0 {
			continue
		}
		c.compileFunc(fn)
		if blockScoped && c.fn.isStrict {
			slot := c.declareLexical(fn.Str, false)
			c.emitOpU16(OpPutLocal, uint16(slot))
		} else {
			c.bindDeclared(fn.Str)
		}
	}
	// Annex B B.3.4 (sloppy): a function declaration that is the body of an
	// if-statement branch hoists its name (undefined) to the enclosing scope; the
	// assignment happens when the branch executes (see compileIfBranch).
	if !c.fn.isStrict {
		for _, stmt := range list {
			c.hoistAnnexBIf(stmt)
		}
	}
}

// hoistAnnexBIf declares (as undefined) the names of any function declarations
// that are direct if-statement branch bodies, recursing through nested ifs.
func (c *compiler) hoistAnnexBIf(stmt *Node) {
	if stmt == nil || stmt.Kind != NIf {
		return
	}
	for _, b := range []*Node{stmt.Left, stmt.Right} {
		for b != nil && b.Kind == NLabel {
			b = b.Body
		}
		if b == nil {
			continue
		}
		if b.Kind == NFunc && b.Str != "" && b.Flags&fnArrow == 0 {
			c.declareAnnexBName(b.Str)
		} else if b.Kind == NIf {
			c.hoistAnnexBIf(b)
		}
	}
}

// declareAnnexBName creates an enclosing-scope binding (initialized to undefined)
// for an Annex B if-body function, unless one already exists.
func (c *compiler) declareAnnexBName(name string) {
	if c.isScript {
		g := c.rt.objPtr(c.rt.global)
		if !g.hasOwn(name) {
			g.defineOwn(name, mkundef(), attrWritable|attrEnumerable)
		}
		return
	}
	if c.resolveLocal(name) >= 0 {
		return
	}
	slot := c.declareVar(name, false)
	c.emit(OpUndef)
	c.emitOpU16(OpPutLocal, uint16(slot))
}

// compileIfBranch compiles an if-statement branch. In sloppy mode a bare function
// declaration branch (Annex B) assigns the hoisted enclosing binding.
func (c *compiler) compileIfBranch(n *Node) {
	if !c.fn.isStrict && n != nil && n.Kind == NFunc && n.Str != "" &&
		n.Flags&fnArrow == 0 && n.Flags&fnParen == 0 {
		c.compileFunc(n)
		switch {
		case c.resolveLocal(n.Str) >= 0:
			c.emitOpU16(OpPutLocal, uint16(c.resolveLocal(n.Str)))
		case c.resolveUpvalue(n.Str) >= 0:
			c.emitOpU16(OpPutUpval, uint16(c.resolveUpvalue(n.Str)))
		default:
			c.emitGlobalPut(n.Str)
		}
		return
	}
	c.compileStmt(n)
}

// hoistLexicals pre-declares the simple let/const bindings of a statement list at
// the current scope, initializing each to an EMPTY hole. Running before function
// hoisting lets nested functions capture the binding, and a read before the
// declaration's initializer runs hits the hole and throws (temporal dead zone).
// Destructuring targets are left to inline declaration (no TDZ) to stay
// conservative. Top-level script `var`-scoped code is unaffected.
func (c *compiler) hoistLexicals(list []*Node) {
	for _, stmt := range list {
		if stmt == nil || stmt.Kind != NVar {
			continue
		}
		if stmt.VarKind != VarLet && stmt.VarKind != VarConst {
			continue
		}
		for _, decl := range stmt.Args {
			if decl.Left == nil || decl.Left.Kind != NIdent {
				continue
			}
			if c.lexicalAtCurrentDepth(decl.Left.Str) >= 0 {
				continue // already hoisted (duplicate — inline decl will store)
			}
			slot := c.declareLexical(decl.Left.Str, stmt.VarKind == VarConst)
			c.emit(OpEmpty)
			c.emitOpU16(OpPutLocal, uint16(slot))
		}
	}
}

// lexicalAtCurrentDepth returns the slot of a live block-scoped binding declared
// at the current scope depth, or -1.
func (c *compiler) lexicalAtCurrentDepth(name string) int {
	for i := len(c.locals) - 1; i >= 0; i-- {
		lv := &c.locals[i]
		if !lv.dead && lv.blockScoped && lv.depth == c.scopeDepth && lv.name == name {
			return i
		}
	}
	return -1
}

// bindDeclared binds the value on top of the stack to a declared name (global
// for the top-level script, otherwise a frame local), consuming it.
func (c *compiler) bindDeclared(name string) {
	if c.isScript {
		c.emitGlobalPut(name)
		return
	}
	slot := c.declareVar(name, false)
	c.emitOpU16(OpPutLocal, uint16(slot))
}

// bindClassDecl binds a class declaration's name, consuming the class on the
// stack. Class declarations are lexically (block) scoped: inside a nested block a
// class binding gets a fresh slot that shadows any outer binding of the same name
// and is hidden when the block exits (class.block-scoped). At function/script top
// level it binds like any other declaration.
func (c *compiler) bindClassDecl(name string) {
	if c.scopeDepth > 0 {
		slot := c.declareLexical(name, false)
		c.emitOpU16(OpPutLocal, uint16(slot))
		return
	}
	c.bindDeclared(name)
}

// compileFunc compiles a function/arrow expression into a child function and
// emits CLOSURE, leaving the closure value on the stack.
func (c *compiler) compileFunc(n *Node) {
	child := &compiler{
		rt:         c.rt,
		enclosing:  c,
		usingStack: -1,
		fn: &svFunc{
			name:        n.Str,
			filename:    c.fn.filename,
			source:      c.fn.source,
			isArrow:     n.Flags&fnArrow != 0,
			isAsync:     n.Flags&fnAsync != 0,
			isGenerator: n.Flags&fnGenerator != 0,
			isClassCtor: n.Flags&fnClassCtor != 0,
			isMethod:    n.Flags&(fnMethod|fnGetter|fnSetter) != 0 && n.Flags&fnClassCtor == 0,
			isStrict:    c.fn.isStrict,
			srcStart:    int(n.SrcOff),
			srcEnd:      int(n.SrcEnd),
		},
	}
	child.compileFunctionBody(n)
	if child.err != nil {
		if c.err == nil {
			c.err = child.err
		}
		return
	}
	child.fn.upvalDescs = child.upvalues
	idx := len(c.fn.childFuncs)
	c.fn.childFuncs = append(c.fn.childFuncs, child.fn)

	c.emit(OpClosure)
	c.emitU32(uint32(idx))
}

// compileFunctionBody compiles a function's parameters and body into c.fn.
func (c *compiler) compileFunctionBody(n *Node) {
	// Class bodies (constructors, methods, accessors) are always strict.
	if n.Flags&fnClassBody != 0 || n.Flags&fnClassCtor != 0 {
		c.fn.isStrict = true
	}
	// A function with its own "use strict" directive is strict (as is any
	// function nested in strict code, inherited via c.fn.isStrict).
	if !c.fn.isStrict && n.Body != nil && n.Body.Kind == NBlock {
		for _, s := range n.Body.Args {
			if s == nil || s.Kind == NEmpty {
				continue
			}
			if s.Kind != NString {
				break
			}
			if s.Str == "use strict" {
				c.fn.isStrict = true
				break
			}
		}
	}

	// Parameters become the first local slots. Simple/default params get one
	// slot each (and are arg-copied); a rest param collects the remaining args.
	type deferredDefault struct {
		slot   int
		expr   *Node
		argIdx int
	}
	type deferredPattern struct {
		slot    int
		pattern *Node
		def     *Node
	}
	// orderedParam records a simple / default NIdent parameter in source order for
	// left-to-right TDZ binding.
	type orderedParam struct {
		slot   int
		argIdx int
		def    *Node // nil for a plain parameter
	}
	var defaults []deferredDefault
	var patterns []deferredPattern
	var ordered []orderedParam
	var restParam *Node
	restIndex := -1
	paramCount := 0
	for i, p := range n.Args {
		switch p.Kind {
		case NIdent:
			slot := c.declareVar(p.Str, false)
			ordered = append(ordered, orderedParam{slot, i, nil})
			paramCount++
		case NArray, NObject:
			slot := c.addLocal("*param*", false)
			patterns = append(patterns, deferredPattern{slot, p, nil})
			paramCount++
		case NAssignPat:
			slot := c.addLocal("*param*", false)
			if p.Left != nil && p.Left.Kind == NIdent {
				// Rename the slot to the parameter name for direct reference.
				c.locals[slot].name = p.Left.Str
				defaults = append(defaults, deferredDefault{slot, p.Right, i})
				ordered = append(ordered, orderedParam{slot, i, p.Right})
			} else {
				patterns = append(patterns, deferredPattern{slot, p.Left, p.Right})
			}
			paramCount++
		case NRest:
			if p.Right == nil || p.Right.Kind != NIdent {
				c.errorf("destructuring rest parameters not yet supported (slice)")
				return
			}
			restParam = p
			restIndex = i
		default:
			c.errorf("unsupported parameter form (slice)")
			return
		}
	}
	c.fn.paramCount = paramCount

	// Bind `this` / self-name / arguments BEFORE evaluating parameter defaults,
	// since default expressions may reference them (e.g. `m(o = this)`).
	//
	// Non-arrow functions bind their own `this` in *this* (arrows inherit the
	// enclosing one lexically via upvalue capture).
	if n.Flags&fnArrow == 0 {
		slot := c.declareVar("*this*", false)
		c.emit(OpThis)
		c.emitOpU16(OpPutLocal, uint16(slot))
		// Bind new.target in *newtarget* so a nested arrow can capture it lexically
		// (arrows have no new.target of their own). Only when referenced (in the
		// body or a nested arrow) to avoid the extra slot in the common case.
		if referencesNewTarget(n.Body) {
			ntSlot := c.declareVar("*newtarget*", false)
			c.emit(OpSpecialObj)
			c.emitByte(2)
			c.emitOpU16(OpPutLocal, uint16(ntSlot))
		}
	}
	// A named function is self-bound: its name refers to itself inside the body
	// (named function expressions; also enables recursion for declarations). A
	// name assigned by NamedEvaluation does NOT create this binding.
	if n.Str != "" && n.Flags&fnArrow == 0 && n.Flags&fnInferredName == 0 {
		slot := c.declareVar(n.Str, false)
		// A named function EXPRESSION's self-reference is immutable (assigning to it
		// is a strict-mode TypeError, a sloppy no-op); a declaration's name is the
		// mutable outer binding.
		if n.Flags&fnFuncExpr != 0 {
			c.locals[slot].selfName = true
		}
		c.emit(OpSpecialObj)
		c.emitByte(1) // current function value
		c.emitOpU16(OpPutLocal, uint16(slot))
	}
	// If a non-arrow function references `arguments`, bind it at entry.
	if n.Flags&fnArrow == 0 && referencesArguments(n.Body) {
		slot := c.declareVar("arguments", false)
		c.emit(OpSpecialObj)
		c.emitByte(0)
		c.emitOpU16(OpPutLocal, uint16(slot))
	}

	// Rest parameter: collect args[restIndex:] into an array.
	if restParam != nil {
		slot := c.declareVar(restParam.Right.Str, false)
		c.emit(OpRest)
		c.emitU16(uint16(restIndex))
		c.emitOpU16(OpPutLocal, uint16(slot))
	}
	// Default parameters. When the list is all simple/default NIdent parameters
	// (no destructuring), bind them left-to-right through the temporal dead zone:
	// every parameter slot starts as an EMPTY hole, and each is initialized from
	// its raw argument (or default) in turn, so a default referencing the same or a
	// later parameter throws (default-params.tdz). Otherwise fall back to the
	// simpler frame-prefill + default-fixup path.
	if len(defaults) > 0 && len(patterns) == 0 {
		for _, o := range ordered {
			c.emit(OpEmpty)
			c.emitOpU16(OpPutLocal, uint16(o.slot))
		}
		for _, o := range ordered {
			c.emit(OpGetArg)
			c.emitU16(uint16(o.argIdx))
			if o.def != nil {
				c.emit(OpDup)
				c.emit(OpIsUndef)
				useArg := c.emitJump(OpJmpFalse)
				c.emit(OpPop)
				c.compileExpr(o.def)
				c.patchJump(useArg)
			}
			c.emitOpU16(OpPutLocal, uint16(o.slot))
		}
	} else {
		for _, d := range defaults {
			c.emitOpU16(OpGetLocal, uint16(d.slot))
			c.emit(OpIsUndef)
			skip := c.emitJump(OpJmpFalse)
			c.compileExpr(d.expr)
			c.emitOpU16(OpPutLocal, uint16(d.slot))
			c.patchJump(skip)
		}
	}
	// Destructuring parameters: bind the pattern from the (defaulted) arg slot.
	for _, dp := range patterns {
		c.emitOpU16(OpGetLocal, uint16(dp.slot))
		if dp.def != nil {
			c.applyDefault(dp.def)
		}
		c.destructureTarget(dp.pattern, VarLet)
	}

	if n.Body == nil {
		c.emit(OpReturnUndef)
	} else if n.Body.Kind == NBlock {
		// Pre-declare all function-scoped var/function/class names as locals so
		// that hoisted function bodies capture them as upvalues (their cells)
		// rather than resolving to globals.
		names := map[string]bool{}
		collectVarFuncNames(n.Body.Args, names)
		for name := range names {
			if c.resolveLocal(name) < 0 {
				c.addLocal(name, false)
			}
		}
		c.hoistLexicals(n.Body.Args)
		c.hoistFunctions(n.Body.Args, false)
		c.compileStmts(n.Body.Args)
		// A class constructor's implicit completion returns its (possibly
		// super-rebound) `this`, so subclassing an exotic native yields the object
		// super() constructed rather than the pre-allocated ordinary one.
		if c.fn.isClassCtor {
			if slot := c.resolveLocal("*this*"); slot >= 0 {
				c.emitOpU16(OpGetLocal, uint16(slot))
				c.emit(OpReturn)
			} else {
				c.emit(OpReturnUndef)
			}
		} else {
			c.emit(OpReturnUndef)
		}
	} else {
		// Concise arrow body: the expression is the return value.
		c.compileExpr(n.Body)
		c.emit(OpReturn)
	}
	c.fn.maxLocals = len(c.locals)
	if c.fn.maxStack < 16 {
		c.fn.maxStack = 16
	}
}

// simpleParamName extracts an identifier parameter name (slice restriction).
func simpleParamName(p *Node) (string, bool) {
	if p != nil && p.Kind == NIdent {
		return p.Str, true
	}
	return "", false
}

// compileNew compiles a `new F(args)` constructor call.
func (c *compiler) compileNew(n *Node) {
	c.compileExpr(n.Left) // constructor
	for _, arg := range n.Args {
		if arg.Kind == NSpread {
			c.errorf("spread arguments in new not yet supported (slice)")
			return
		}
		c.compileExpr(arg)
	}
	c.emit(OpNew)
	c.emitU16(uint16(len(n.Args)))
}

// compileTailCall emits a proper-tail-call form of `return f(args)` (OpTailCall /
// OpTailCallMethod, which reuse the current frame). It returns false — so the
// caller falls back to a normal call+return — for forms that are not eligible:
// spread arguments, optional chains, or super calls.
func (c *compiler) compileTailCall(n *Node) bool {
	if hasSpread(n.Args) || containsOptional(n) {
		return false
	}
	if n.Left != nil && n.Left.Kind == NIdent && n.Left.Str == "super" {
		return false
	}
	if n.Left != nil && n.Left.Kind == NMember && n.Left.Left != nil &&
		n.Left.Left.Kind == NIdent && n.Left.Left.Str == "super" {
		return false
	}
	if n.Left != nil && n.Left.Kind == NMember {
		member := n.Left
		c.compileExpr(member.Left) // [this]
		if member.Flags&1 != 0 {   // computed
			c.emit(OpDup)
			c.compileExpr(member.Right)
			c.emit(OpGetElem)
		} else {
			c.emitFieldOp(OpGetField2, member.Right.Str)
		}
		for _, a := range n.Args {
			c.compileExpr(a)
		}
		c.emit(OpTailCallMethod)
		c.emitU16(uint16(len(n.Args)))
		return true
	}
	c.compileExpr(n.Left) // [fn]
	for _, a := range n.Args {
		c.compileExpr(a)
	}
	c.emit(OpTailCall)
	c.emitU16(uint16(len(n.Args)))
	return true
}

// compileCall compiles a function or method call.
func (c *compiler) compileCall(n *Node) {
	// Optional-chain calls (a?.b(), a?.(), a?.b.c()) short-circuit as a unit.
	if containsOptional(n) {
		c.compileOptionalChain(n)
		return
	}
	// super(...) and super.method(...) calls.
	if n.Left != nil && n.Left.Kind == NIdent && n.Left.Str == "super" {
		c.compileSuperCall(n)
		return
	}
	if n.Left != nil && n.Left.Kind == NMember && n.Left.Left != nil &&
		n.Left.Left.Kind == NIdent && n.Left.Left.Str == "super" {
		c.compileSuperMethodCall(n)
		return
	}

	spread := hasSpread(n.Args)

	// Method call: the receiver becomes `this` (CALL_METHOD / APPLY for spread).
	if n.Left != nil && n.Left.Kind == NMember {
		member := n.Left
		// Spread: emit [func, this, argsArray] for APPLY.
		if spread {
			tSlot := c.tempLocal()
			c.compileExpr(member.Left)
			c.emitOpU16(OpPutLocal, uint16(tSlot)) // receiver -> temp
			c.emitOpU16(OpGetLocal, uint16(tSlot))
			if member.Flags&1 != 0 {
				c.compileExpr(member.Right)
				c.emit(OpGetElem)
			} else {
				c.emitFieldOp(OpGetField, member.Right.Str)
			} // [method]
			c.emitOpU16(OpGetLocal, uint16(tSlot)) // [method, this]
			c.buildSpreadArray(n.Args)             // [method, this, argsArray]
			c.emit(OpApply)
			c.emitU16(0)
			return
		}
		c.compileExpr(member.Left)
		if member.Flags&1 != 0 { // obj[expr](...)
			c.emit(OpDup)
			c.compileExpr(member.Right)
			c.emit(OpGetElem)
		} else {
			c.emitFieldOp(OpGetField2, member.Right.Str)
		}
		for _, arg := range n.Args {
			c.compileExpr(arg)
		}
		c.emit(OpCallMethod)
		c.emitU16(uint16(len(n.Args)))
		return
	}

	// Plain call: `this` is undefined.
	if spread {
		c.compileExpr(n.Left) // [func]
		c.emit(OpUndef)       // [func, this=undefined]
		c.buildSpreadArray(n.Args)
		c.emit(OpApply) // [func, this, argsArray] -> result
		c.emitU16(0)
		return
	}
	c.compileExpr(n.Left)
	for _, arg := range n.Args {
		c.compileExpr(arg)
	}
	c.emit(OpCall)
	c.emitU16(uint16(len(n.Args)))
}
