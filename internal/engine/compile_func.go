package engine

// Function & call compilation (ant compiler.c compile_func_expr / compile_call
// / compile_function_body). The Phase 3 slice supports simple (identifier)
// parameters, block and expression arrow bodies, closures with upvalue capture,
// hoisted function declarations, and ordinary calls. Default/rest/destructuring
// parameters, `arguments`, generators, and async land as the port continues.

// hoistFunctions pre-binds function declarations in a statement list so they are
// callable before their textual position (ant function hoisting).
func (c *compiler) hoistFunctions(list []*Node) {
	for _, stmt := range list {
		if stmt == nil || stmt.Kind != NFunc || stmt.Str == "" || stmt.Flags&fnArrow != 0 {
			continue
		}
		c.compileFunc(stmt)
		c.bindDeclared(stmt.Str)
	}
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

// compileFunc compiles a function/arrow expression into a child function and
// emits CLOSURE, leaving the closure value on the stack.
func (c *compiler) compileFunc(n *Node) {
	child := &compiler{
		rt:        c.rt,
		enclosing: c,
		fn: &svFunc{
			name:        n.Str,
			filename:    c.fn.filename,
			source:      c.fn.source,
			isArrow:     n.Flags&fnArrow != 0,
			isAsync:     n.Flags&fnAsync != 0,
			isGenerator: n.Flags&fnGenerator != 0,
			isClassCtor: n.Flags&fnClassCtor != 0,
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
		slot int
		expr *Node
	}
	type deferredPattern struct {
		slot    int
		pattern *Node
		def     *Node
	}
	var defaults []deferredDefault
	var patterns []deferredPattern
	var restParam *Node
	restIndex := -1
	paramCount := 0
	for i, p := range n.Args {
		switch p.Kind {
		case NIdent:
			c.declareVar(p.Str, false)
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
				defaults = append(defaults, deferredDefault{slot, p.Right})
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
	// Default parameters: if the arg is undefined, evaluate the default.
	for _, d := range defaults {
		c.emitOpU16(OpGetLocal, uint16(d.slot))
		c.emit(OpIsUndef)
		skip := c.emitJump(OpJmpFalse)
		c.compileExpr(d.expr)
		c.emitOpU16(OpPutLocal, uint16(d.slot))
		c.patchJump(skip)
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
		c.hoistFunctions(n.Body.Args)
		c.compileStmts(n.Body.Args)
		c.emit(OpReturnUndef)
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
