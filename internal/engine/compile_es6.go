package engine

// ES6 expression/statement compilation (Phase 5): template literals, spread in
// array literals and calls, and for-of iteration. More ES6 (destructuring,
// default/rest params, classes, generators) lands incrementally.

// ---- destructuring (array/object patterns in declarations) ----

// compileDestructureDecl binds a destructuring pattern from a value expression
// (ant compile_destructure_binding).
func (c *compiler) compileDestructureDecl(pattern, valueExpr *Node, kind VarKind) {
	if valueExpr == nil {
		c.emit(OpUndef)
	} else {
		c.compileExpr(valueExpr)
	}
	c.destructureTarget(pattern, kind)
}

// destructureTarget consumes the value on top of the stack, binding it to the
// pattern (an identifier or a nested array/object pattern).
func (c *compiler) destructureTarget(pattern *Node, kind VarKind) {
	switch pattern.Kind {
	case NIdent:
		c.bindDeclName(pattern.Str, kind)
	case NArray:
		src := c.tempLocal()
		c.emit(OpForOf) // materialize the iterable into an array
		c.emitOpU16(OpPutLocal, uint16(src))
		c.destructureArray(pattern, src, kind)
	case NObject:
		src := c.tempLocal()
		c.emitOpU16(OpPutLocal, uint16(src))
		c.destructureObject(pattern, src, kind)
	case NMember:
		// Assignment to a member reference (e.g. [obj.x] = …); value on top.
		computed := pattern.Flags&1 != 0
		vSlot := c.tempLocal()
		c.emitOpU16(OpPutLocal, uint16(vSlot))
		c.compileExpr(pattern.Left)
		if computed {
			c.compileExpr(pattern.Right)
			c.emitOpU16(OpGetLocal, uint16(vSlot))
			c.emit(OpPutElem)
		} else {
			c.emitOpU16(OpGetLocal, uint16(vSlot))
			c.emitFieldOp(OpPutField, pattern.Right.Str)
		}
	case NEmpty:
		c.emit(OpPop)
	default:
		c.errorf("unsupported destructuring target (slice)")
	}
}

func (c *compiler) destructureArray(pattern *Node, src int, kind VarKind) {
	for i, elem := range pattern.Args {
		if elem.Kind == NEmpty {
			continue
		}
		if elem.Kind == NRest || elem.Kind == NSpread {
			// rest: src.slice(i) (array patterns spell rest as NSpread)
			c.emitOpU16(OpGetLocal, uint16(src))
			c.emitFieldOp(OpGetField2, "slice")
			c.compileNumberLiteral(float64(i))
			c.emit(OpCallMethod)
			c.emitU16(1)
			c.destructureTarget(elem.Right, kind)
			return
		}
		target, defExpr := elem, (*Node)(nil)
		// A default spells as NAssignPat in declaration patterns and as a plain
		// NAssign (`=`) when an array literal is reinterpreted as an assignment
		// target.
		if elem.Kind == NAssignPat || (elem.Kind == NAssign && elem.Op == TokAssign) {
			target, defExpr = elem.Left, elem.Right
		}
		c.emitOpU16(OpGetLocal, uint16(src))
		c.compileNumberLiteral(float64(i))
		c.emit(OpGetElem) // src[i]
		c.applyDefault(defExpr)
		c.destructureTarget(target, kind)
	}
}

func (c *compiler) destructureObject(pattern *Node, src int, kind VarKind) {
	for _, prop := range pattern.Args {
		if prop.Kind == NSpread {
			c.errorf("object rest destructuring not yet supported (slice)")
			return
		}
		name, ok := propKeyName(prop.Left)
		if !ok {
			c.errorf("computed destructuring keys not yet supported (slice)")
			return
		}
		// Binding target and optional default from prop.Right.
		target := prop.Right
		var defExpr *Node
		if target != nil && target.Kind == NAssign && target.Op == TokAssign {
			defExpr = target.Right
			target = target.Left
		}
		c.emitOpU16(OpGetLocal, uint16(src))
		c.emitFieldOp(OpGetField, name)
		c.applyDefault(defExpr)
		c.destructureTarget(target, kind)
	}
}

// applyDefault replaces the top-of-stack value with defExpr if it is undefined.
func (c *compiler) applyDefault(defExpr *Node) {
	if defExpr == nil {
		return
	}
	c.emit(OpDup)
	c.emit(OpIsUndef)
	keep := c.emitJump(OpJmpFalse)
	c.emit(OpPop)
	c.compileExpr(defExpr)
	c.patchJump(keep)
}

// bindDeclName stores the top-of-stack value into a freshly declared binding.
func (c *compiler) bindDeclName(name string, kind VarKind) {
	if kind == varAssign {
		// Assign to an existing binding (destructuring assignment leaf).
		c.compileIdentStore(name)
		return
	}
	if kind == VarVar && c.isScript && !c.isEval {
		c.emitGlobalPut(name)
		return
	}
	slot := c.declareVar(name, kind == VarConst)
	c.emitOpU16(OpPutLocal, uint16(slot))
}

// compileIdentStore stores the top-of-stack value into an existing identifier
// binding (local/upvalue/with/global), consuming it.
func (c *compiler) compileIdentStore(name string) {
	switch {
	case c.resolveLocal(name) >= 0:
		c.emitOpU16(OpPutLocal, uint16(c.resolveLocal(name)))
	case c.resolveUpvalue(name) >= 0:
		c.emitOpU16(OpPutUpval, uint16(c.resolveUpvalue(name)))
	case c.withDepth > 0:
		c.emitWithVar(OpWithPutVar, name)
	default:
		c.emitGlobalPut(name)
	}
}

// hasSpread reports whether any element in the list is a spread.
func hasSpread(elems []*Node) bool {
	for _, e := range elems {
		if e != nil && e.Kind == NSpread {
			return true
		}
	}
	return false
}

// buildSpreadArray compiles elements (some possibly spreads) into a fresh array,
// leaving that array on the stack.
func (c *compiler) buildSpreadArray(elems []*Node) {
	tSlot := c.tempLocal()
	c.emit(OpArray)
	c.emitU16(0)
	c.emitOpU16(OpPutLocal, uint16(tSlot))
	for _, el := range elems {
		if el.Kind == NSpread {
			c.emitOpU16(OpGetLocal, uint16(tSlot)) // arr
			c.compileExpr(el.Right)                // iterable
			c.emit(OpSpread)                       // arr iterable -> (appends)
			continue
		}
		c.emitOpU16(OpGetLocal, uint16(tSlot)) // obj
		c.emitOpU16(OpGetLocal, uint16(tSlot))
		c.emit(OpGetLength) // key = length
		if el.Kind == NEmpty {
			c.emit(OpEmpty)
		} else {
			c.compileExpr(el) // val
		}
		c.emit(OpPutElem)
	}
	c.emitOpU16(OpGetLocal, uint16(tSlot))
}

// resolveClassBinding emits a load of a captured class binding (*superctor* /
// *superproto* / *this*), returning false if it's not in scope.
func (c *compiler) resolveClassBinding(name string) bool {
	if slot := c.resolveLocal(name); slot >= 0 {
		c.emitOpU16(OpGetLocal, uint16(slot))
		return true
	}
	if uv := c.resolveUpvalue(name); uv >= 0 {
		c.emitOpU16(OpGetUpval, uint16(uv))
		return true
	}
	return false
}

// compileSuperCall compiles `super(...)` in a derived constructor: it invokes
// the parent constructor with the current `this`.
func (c *compiler) compileSuperCall(n *Node) {
	// CALL_METHOD / APPLY expect the receiver (this) beneath the callee.
	if hasSpread(n.Args) {
		if !c.resolveClassBinding("*superctor*") {
			c.errorf("'super' keyword unexpected here")
			return
		}
		if !c.resolveClassBinding("*this*") {
			c.emit(OpUndef)
		}
		c.buildSpreadArray(n.Args)
		c.emit(OpApply) // [superctor, this, args] -> result
		c.emitU16(0)
		return
	}
	if !c.resolveClassBinding("*this*") { // this (receiver)
		c.emit(OpUndef)
	}
	if !c.resolveClassBinding("*superctor*") { // func
		c.errorf("'super' keyword unexpected here")
		return
	}
	for _, a := range n.Args {
		c.compileExpr(a)
	}
	c.emit(OpCallMethod) // [this, superctor, args...] -> result
	c.emitU16(uint16(len(n.Args)))
}

// compileSuperMethodCall compiles `super.method(...)`: it invokes the parent
// prototype's method with the current `this`.
func (c *compiler) compileSuperMethodCall(n *Node) {
	member := n.Left
	// this = current this
	if !c.resolveClassBinding("*this*") {
		c.emit(OpUndef)
	}
	// method = *superproto*.name
	if !c.resolveClassBinding("*superproto*") {
		c.errorf("'super' keyword unexpected here")
		return
	}
	if member.Flags&1 != 0 {
		c.compileExpr(member.Right)
		c.emit(OpGetElem)
	} else {
		c.emitFieldOp(OpGetField, member.Right.Str)
	}
	for _, a := range n.Args {
		c.compileExpr(a)
	}
	c.emit(OpCallMethod) // [this, method, args...] -> result
	c.emitU16(uint16(len(n.Args)))
}

// compileClass compiles a class declaration/expression into a constructor
// function with prototype methods and static members, wiring the extends
// prototype chain. super() / super.method are a later refinement.
func (c *compiler) compileClass(n *Node) {
	ctorSlot := c.tempLocal()
	protoSlot := c.tempLocal()

	// Find the constructor method, if any.
	var ctorFn *Node
	for _, m := range n.Args {
		if m.Kind == NMethod && m.Left != nil && m.Left.Kind == NIdent &&
			m.Left.Str == "constructor" && m.Flags&fnStatic == 0 {
			ctorFn = m.Right
		}
	}
	if ctorFn == nil {
		if n.Left != nil {
			// Derived default constructor: `constructor(...args){ super(...args); }`.
			argsRef := &Node{Kind: NIdent, Str: "*ctorargs*"}
			ctorFn = &Node{Kind: NFunc, Str: n.Str,
				Args: []*Node{{Kind: NRest, Right: argsRef}},
				Body: &Node{Kind: NBlock, Args: []*Node{
					{Kind: NCall, Left: &Node{Kind: NIdent, Str: "super"},
						Args: []*Node{{Kind: NSpread, Right: argsRef}}},
				}},
			}
		} else {
			ctorFn = &Node{Kind: NFunc, Body: &Node{Kind: NBlock}, Str: n.Str}
		}
	} else {
		ctorFn.Str = n.Str
	}
	// Bind super BEFORE compiling the constructor/methods so their bodies can
	// capture *superctor* / *superproto* as upvalues (for super() / super.x).
	superSlot, superProtoSlot := -1, -1
	if n.Left != nil {
		superSlot = c.declareVar("*superctor*", false)
		c.compileExpr(n.Left)
		c.emitOpU16(OpPutLocal, uint16(superSlot))
		superProtoSlot = c.declareVar("*superproto*", false)
		c.emitOpU16(OpGetLocal, uint16(superSlot))
		c.emitFieldOp(OpGetField, "prototype")
		c.emitOpU16(OpPutLocal, uint16(superProtoSlot))
	}

	c.compileFunc(ctorFn) // [ctor]
	c.emitOpU16(OpPutLocal, uint16(ctorSlot))

	// Wire the extends prototype chain now that the ctor exists.
	if n.Left != nil {
		c.emitOpU16(OpGetLocal, uint16(ctorSlot))
		c.emitFieldOp(OpGetField, "prototype")
		c.emitOpU16(OpGetLocal, uint16(superProtoSlot))
		c.emit(OpSetProto)
		c.emit(OpPop)
		c.emitOpU16(OpGetLocal, uint16(ctorSlot))
		c.emitOpU16(OpGetLocal, uint16(superSlot))
		c.emit(OpSetProto)
		c.emit(OpPop)
	}

	// Cache the prototype object.
	c.emitOpU16(OpGetLocal, uint16(ctorSlot))
	c.emitFieldOp(OpGetField, "prototype")
	c.emitOpU16(OpPutLocal, uint16(protoSlot))

	// Define methods (skip the constructor; it's already the ctor function).
	for _, m := range n.Args {
		if m.Kind != NMethod {
			continue
		}
		if m.Left != nil && m.Left.Kind == NIdent && m.Left.Str == "constructor" && m.Flags&fnStatic == 0 {
			continue
		}
		if m.Flags&fnComputed != 0 {
			c.errorf("computed class member names not yet supported (slice)")
			return
		}
		name, ok := propKeyName(m.Left)
		if !ok {
			c.errorf("unsupported class member key (slice)")
			return
		}
		target := protoSlot
		if m.Flags&fnStatic != 0 {
			target = ctorSlot
		}
		// Class field with a value (not a method): m.Right is an expression.
		if m.Right != nil && m.Right.Kind != NFunc {
			c.emitOpU16(OpGetLocal, uint16(target))
			c.compileExpr(m.Right)
			c.emitDefineField(name) // enumerable field
			c.emit(OpPop)
			continue
		}
		c.emitOpU16(OpGetLocal, uint16(target))
		c.compileFunc(m.Right)
		flags := byte(0) // data method
		if m.Flags&fnGetter != 0 {
			flags = 1
		} else if m.Flags&fnSetter != 0 {
			flags = 2
		}
		idx := c.constant(c.rt.internString(name))
		c.emit(OpDefineMethod)
		c.emitU32(uint32(idx))
		c.emitByte(flags)
		c.emit(OpPop)
	}

	// The class value is the constructor.
	c.emitOpU16(OpGetLocal, uint16(ctorSlot))
}

// compileTemplate compiles a template literal `a${x}b` into string concatenation
// (ant compile_template). Args interleave cooked string segments and expressions:
// [str0, expr0, str1, expr1, …, strN].
func (c *compiler) compileTemplate(n *Node) {
	segs := n.Args
	if len(segs) == 0 {
		c.emitConst(c.rt.internString(""))
		return
	}
	// First cooked segment.
	c.emitConst(c.rt.internString(segs[0].Str))
	for i := 1; i < len(segs); i += 2 {
		// Interpolated expression, coerced to string via `+` (left is a string).
		c.compileExpr(segs[i])
		c.emit(OpAdd)
		if i+1 < len(segs) {
			c.emitConst(c.rt.internString(segs[i+1].Str))
			c.emit(OpAdd)
		}
	}
}

// compileYield emits a yield expression. Plain `yield [expr]` suspends with the
// operand and resumes with the injected value; `yield* expr` (Flags==1) delegates
// to another iterable, yielding each of its values in turn.
func (c *compiler) compileYield(n *Node) {
	if n.Flags == 1 {
		c.compileYieldStar(n)
		return
	}
	if n.Right != nil {
		c.compileExpr(n.Right)
	} else {
		c.emit(OpUndef)
	}
	c.emit(OpYield)
}

// compileYieldStar lowers `yield* expr` to iteration over expr's values,
// yielding each. Values are materialized eagerly (like for-of); the delegated
// iterator's return value is not propagated (yield* evaluates to undefined).
func (c *compiler) compileYieldStar(n *Node) {
	c.scopeDepth++
	c.compileExpr(n.Right)
	c.emit(OpForOf) // iterable -> values array
	arrSlot := c.addLocal("*yss*", false)
	c.emitOpU16(OpPutLocal, uint16(arrSlot))
	iSlot := c.addLocal("*ysi*", false)
	c.emit(OpConstI8)
	c.emitByte(0)
	c.emitOpU16(OpPutLocal, uint16(iSlot))

	condStart := len(c.fn.code)
	c.emitOpU16(OpGetLocal, uint16(iSlot))
	c.emitOpU16(OpGetLocal, uint16(arrSlot))
	c.emit(OpGetLength)
	c.emit(OpLt)
	exit := c.emitJump(OpJmpFalse)

	// yield arr[i]; discard the resume value (not forwarded to inner in eager mode)
	c.emitOpU16(OpGetLocal, uint16(arrSlot))
	c.emitOpU16(OpGetLocal, uint16(iSlot))
	c.emit(OpGetElem)
	c.emit(OpYield)
	c.emit(OpPop)

	// i++
	c.emitOpU16(OpGetLocal, uint16(iSlot))
	c.emit(OpInc)
	c.emitOpU16(OpPutLocal, uint16(iSlot))
	c.emit(OpJmp)
	c.emitU32(uint32(condStart))

	c.patchJump(exit)
	c.scopeDepth--
	c.emit(OpUndef) // completion value of yield*
}
