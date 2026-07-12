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
		// A pattern with a rest element consumes the whole iterator (done at the
		// end, nothing to close), so materializing is fine; otherwise drive the
		// iterator lazily and close it when not exhausted (7.4.6 / destructuring
		// iterator-closing).
		hasRest := false
		for _, e := range pattern.Args {
			if e != nil && (e.Kind == NRest || e.Kind == NSpread) {
				hasRest = true
				break
			}
		}
		if hasRest {
			src := c.tempLocal()
			c.emit(OpForOf)
			c.emitOpU16(OpPutLocal, uint16(src))
			c.destructureArray(pattern, src, kind)
		} else {
			c.destructureArrayIter(pattern, kind)
		}
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

// destructureArrayIter destructures an array pattern (without a rest element) by
// driving the iterator one value per target, then closing it if it isn't
// already exhausted (spec IteratorBindingInitialization / DestructuringAssignment).
func (c *compiler) destructureArrayIter(pattern *Node, kind VarKind) {
	iterSlot := c.tempLocal()
	c.emit(OpIterCall) // source (on stack) -> iterator
	c.emitByte(0)
	c.emitOpU16(OpPutLocal, uint16(iterSlot))
	resSlot := c.tempLocal()
	doneSlot := c.tempLocal()
	c.emit(OpFalse)
	c.emitOpU16(OpPutLocal, uint16(doneSlot))

	for _, elem := range pattern.Args {
		c.emitIterStep(iterSlot, resSlot, doneSlot) // leaves the value (or undefined)
		if elem.Kind == NEmpty {
			c.emit(OpPop) // hole: consume one step and discard
			continue
		}
		target, defExpr := elem, (*Node)(nil)
		if elem.Kind == NAssignPat || (elem.Kind == NAssign && elem.Op == TokAssign) {
			target, defExpr = elem.Left, elem.Right
		}
		c.applyDefault(defExpr)
		c.destructureTarget(target, kind)
	}
	// if !done: IteratorClose(iter) (the pattern didn't consume everything).
	c.emitOpU16(OpGetLocal, uint16(doneSlot))
	skip := c.emitJump(OpJmpTrue)
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emit(OpIterClose)
	c.patchJump(skip)
}

// emitIterStep leaves the next value from the iterator on the stack, or
// undefined once the iterator is done, updating doneSlot. It never calls next()
// again after the iterator reports done.
func (c *compiler) emitIterStep(iterSlot, resSlot, doneSlot int) {
	c.emitOpU16(OpGetLocal, uint16(doneSlot))
	alreadyDone := c.emitJump(OpJmpTrue) // done -> push undefined
	// r = iter.next(); done = r.done
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emitFieldOp(OpGetField2, "next")
	c.emit(OpCallMethod)
	c.emitU16(0)
	c.emitOpU16(OpPutLocal, uint16(resSlot))
	c.emitOpU16(OpGetLocal, uint16(resSlot))
	c.emitFieldOp(OpGetField, "done")
	c.emitOpU16(OpPutLocal, uint16(doneSlot))
	// push done ? undefined : r.value
	c.emitOpU16(OpGetLocal, uint16(doneSlot))
	nowDone := c.emitJump(OpJmpTrue)
	c.emitOpU16(OpGetLocal, uint16(resSlot))
	c.emitFieldOp(OpGetField, "value")
	haveVal := c.emitJump(OpJmp)
	c.patchJump(alreadyDone)
	c.patchJump(nowDone)
	c.emit(OpUndef)
	c.patchJump(haveVal)
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
	var priorKeys []string
	for _, prop := range pattern.Args {
		if prop.Kind == NSpread {
			// {a, ...rest}: rest is a fresh object with src's enumerable own props
			// minus the already-destructured keys.
			c.emit(OpObject) // [restObj]
			c.emit(OpDup)
			c.emitOpU16(OpGetLocal, uint16(src))
			c.emit(OpCopyDataProps) // [restObj, restObj]
			c.emitByte(0)
			c.emit(OpPop) // [restObj]
			for _, k := range priorKeys {
				c.emit(OpDup)
				c.emitConst(c.rt.internString(k))
				c.emit(OpDelete)
				c.emit(OpPop)
			}
			c.destructureTarget(prop.Right, kind)
			continue
		}
		name, ok := propKeyName(prop.Left)
		computed := prop.Flags&fnComputed != 0 || !ok
		// Binding target and optional default from prop.Right.
		target := prop.Right
		var defExpr *Node
		if target != nil && target.Kind == NAssign && target.Op == TokAssign {
			defExpr = target.Right
			target = target.Left
		}
		if computed {
			// { [expr]: target }: read src[ToPropertyKey(expr)].
			c.emitOpU16(OpGetLocal, uint16(src)) // [src]
			c.compileExpr(prop.Left)             // [src, key]
			c.emit(OpGetElem)                    // [val]
			c.applyDefault(defExpr)
			c.destructureTarget(target, kind)
			continue
		}
		priorKeys = append(priorKeys, name)
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
	var slot int
	if kind == VarLet || kind == VarConst {
		slot = c.declareLexical(name, kind == VarConst)
	} else {
		slot = c.declareVar(name, false)
	}
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
	// super(...) constructs the parent with the derived class's new.target and
	// binds the resulting object as `this` (so subclassing an exotic native like
	// Function/Array/Error yields the native object rather than an ordinary one).
	if !c.resolveClassBinding("*superctor*") { // [superctor]
		c.errorf("'super' keyword unexpected here")
		return
	}
	c.buildSpreadArray(n.Args) // [superctor, argsArray]
	c.emit(OpSuperApply)       // -> [constructedThis]
	c.emitU16(0)
	// Bind the constructed object as `this`, leaving it as the call's value.
	if slot := c.resolveLocal("*this*"); slot >= 0 {
		c.emitOpU16(OpSetLocal, uint16(slot))
	} else if uv := c.resolveUpvalue("*this*"); uv >= 0 {
		c.emitOpU16(OpSetUpval, uint16(uv))
	}
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
	// Function.prototype.toString of a class returns the whole class source, so
	// the constructor function's source span covers the class declaration.
	if n.SrcEnd > n.SrcOff {
		ctorFn.SrcOff = n.SrcOff
		ctorFn.SrcEnd = n.SrcEnd
	}
	ctorFn.Flags |= fnClassCtor // must be invoked with `new`
	// Bind super BEFORE compiling the constructor/methods so their bodies can
	// capture *superctor* / *superproto* as upvalues (for super() / super.x).
	superSlot, superProtoSlot := -1, -1
	if n.Left != nil {
		superSlot = c.declareVar("*superctor*", false)
		c.compileExpr(n.Left)
		c.emitOpU16(OpPutLocal, uint16(superSlot))
		superProtoSlot = c.declareVar("*superproto*", false)
		// superproto = (superctor == null) ? null : superctor.prototype
		c.emitOpU16(OpGetLocal, uint16(superSlot))
		c.emit(OpDup)
		notNull := c.emitJump(OpJmpNotNullish)
		c.emit(OpPop)
		c.emit(OpNull)
		doneP := c.emitJump(OpJmp)
		c.patchJump(notNull)
		c.emitFieldOp(OpGetField, "prototype")
		c.patchJump(doneP)
		c.emitOpU16(OpPutLocal, uint16(superProtoSlot))
	}

	c.compileFunc(ctorFn) // [ctor]
	c.emitOpU16(OpPutLocal, uint16(ctorSlot))

	// Wire the extends prototype chain now that the ctor exists.
	if n.Left != nil {
		// C.prototype.__proto__ = superproto (null for `extends null`).
		c.emitOpU16(OpGetLocal, uint16(ctorSlot))
		c.emitFieldOp(OpGetField, "prototype")
		c.emitOpU16(OpGetLocal, uint16(superProtoSlot))
		c.emit(OpSetProto)
		c.emit(OpPop)
		// C.__proto__ = superctor, but only when not `extends null` (else the
		// constructor keeps Function.prototype).
		c.emitOpU16(OpGetLocal, uint16(superSlot))
		skip := c.emitJump(OpJmpNotNullish)
		doneC := c.emitJump(OpJmp) // null: leave C.__proto__ as Function.prototype
		c.patchJump(skip)
		c.emitOpU16(OpGetLocal, uint16(ctorSlot))
		c.emitOpU16(OpGetLocal, uint16(superSlot))
		c.emit(OpSetProto)
		c.emit(OpPop)
		c.patchJump(doneC)
	}

	// Cache the prototype object.
	c.emitOpU16(OpGetLocal, uint16(ctorSlot))
	c.emitFieldOp(OpGetField, "prototype")
	c.emitOpU16(OpPutLocal, uint16(protoSlot))

	// Define methods (skip the constructor; it's already the ctor function).
	for _, m := range n.Args {
		if m.Kind == NStaticBlock {
			// Static initialization block: run its body with this = the class.
			body := &Node{Kind: NBlock, Args: m.Args}
			blockFn := &Node{Kind: NFunc, Body: body, Flags: fnClassBody}
			c.emitOpU16(OpGetLocal, uint16(ctorSlot)) // this
			c.compileFunc(blockFn)                    // func
			c.emit(OpCallMethod)                      // [this, func] -> result
			c.emitU16(0)
			c.emit(OpPop)
			continue
		}
		if m.Kind != NMethod {
			continue
		}
		if m.Left != nil && m.Left.Kind == NIdent && m.Left.Str == "constructor" && m.Flags&fnStatic == 0 {
			continue
		}
		target := protoSlot
		if m.Flags&fnStatic != 0 {
			target = ctorSlot
		}
		if m.Flags&fnComputed != 0 {
			if m.Flags&(fnGetter|fnSetter) != 0 {
				// Computed accessor: [target, key, func] -> DEFINE_METHOD_COMP.
				c.emitOpU16(OpGetLocal, uint16(target))
				c.compileExpr(m.Left)
				c.compileFunc(m.Right)
				flags := byte(1)
				if m.Flags&fnSetter != 0 {
					flags = 2
				}
				c.emit(OpDefineMethodComp)
				c.emitByte(flags)
				c.emit(OpPop)
				continue
			}
			// Computed data method / field: target[key] = value.
			c.emitOpU16(OpGetLocal, uint16(target)) // [target]
			c.compileExpr(m.Left)                   // [target, key]
			if m.Right != nil && m.Right.Kind == NFunc {
				c.compileFunc(m.Right)
			} else {
				c.compileExpr(m.Right)
			}
			c.emit(OpPutElem)
			continue
		}
		name, ok := propKeyName(m.Left)
		if !ok {
			c.errorf("unsupported class member key (slice)")
			return
		}
		// Class field with a value (not a method): m.Right is an expression.
		if m.Right != nil && m.Right.Kind != NFunc {
			c.emitOpU16(OpGetLocal, uint16(target))
			c.compileExpr(m.Right)
			c.emitDefineField(name) // enumerable field
			c.emit(OpPop)
			continue
		}
		// The method function's .name is its key (get/set prefixed for accessors).
		if m.Right.Str == "" {
			prefix := ""
			if m.Flags&fnGetter != 0 {
				prefix = "get "
			} else if m.Flags&fnSetter != 0 {
				prefix = "set "
			}
			m.Right.Str = prefix + name
			m.Right.Flags |= fnInferredName
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
	// An untagged template with an invalid escape sequence in any cooked segment
	// is an early SyntaxError (only tagged templates tolerate it — the cooked
	// value there is undefined). The segments are the even-indexed args.
	for i := 0; i < len(segs); i += 2 {
		if segs[i].Flags&fnInvalidCooked != 0 {
			c.errorf("Invalid or unexpected token")
			return
		}
	}
	// First cooked segment.
	c.emitConst(c.rt.internString(segs[0].Str))
	for i := 1; i < len(segs); i += 2 {
		// Interpolated expression: ToString via ToPropertyKey (hint "string", so
		// `${obj}` calls toString before valueOf) then concatenate with OpAdd.
		c.compileExpr(segs[i])
		c.emit(OpToPropkey)
		c.emit(OpAdd)
		if i+1 < len(segs) {
			c.emitConst(c.rt.internString(segs[i+1].Str))
			c.emit(OpAdd)
		}
	}
}

// compileTaggedTemplate compiles `tag`...“ : tag(strings, ...substitutions)
// where strings is the frozen cooked-segment array carrying a frozen `raw`.
// containsOptional reports whether a member/call chain contains a `?.` link,
// walking down the chain's left spine.
func containsOptional(n *Node) bool {
	for n != nil {
		switch n.Kind {
		case NOptional:
			return true
		case NMember, NCall:
			n = n.Left
		default:
			return false
		}
	}
	return false
}

// compileOptionalChain compiles an optional chain as a unit: any nullish `?.`
// operand short-circuits the whole chain to undefined.
func (c *compiler) compileOptionalChain(n *Node) {
	var bail []int
	c.compileChainLink(n, &bail)
	done := c.emitJump(OpJmp)
	for _, b := range bail {
		c.patchJump(b)
	}
	c.emit(OpPop) // discard the guarded (nullish) value
	c.emit(OpUndef)
	c.patchJump(done)
}

// emitGuard checks the top-of-stack value: if nullish it jumps to the shared
// bail (leaving that value for the bail handler to pop); else continues.
func (c *compiler) emitGuard(bail *[]int) {
	c.emit(OpDup)
	cont := c.emitJump(OpJmpNotNullish)
	bj := c.emitJump(OpJmp)
	*bail = append(*bail, bj)
	c.patchJump(cont)
}

// compileChainLink emits one link of an optional chain, leaving exactly one
// value (the chain result so far) on the stack.
func (c *compiler) compileChainLink(n *Node, bail *[]int) {
	switch n.Kind {
	case NOptional:
		c.compileChainLink(n.Left, bail)
		c.emitGuard(bail)
		if n.Right == nil {
			return // optional-call base: value is the callee itself
		}
		if n.Flags&1 != 0 {
			c.compileExpr(n.Right)
			c.emit(OpGetElem)
		} else {
			c.emitFieldOp(OpGetField, n.Right.Str)
		}
	case NMember:
		c.compileChainLink(n.Left, bail)
		if n.Flags&1 != 0 {
			c.compileExpr(n.Right)
			c.emit(OpGetElem)
		} else {
			c.emitFieldOp(OpGetField, n.Right.Str)
		}
	case NCall:
		c.compileChainCall(n, bail)
	default:
		c.compileExpr(n)
	}
}

// compileChainCall compiles a call inside an optional chain, preserving method
// this-binding and threading the short-circuit bail.
func (c *compiler) compileChainCall(n *Node, bail *[]int) {
	callee := n.Left
	spread := hasSpread(n.Args)
	isMethod := callee.Kind == NMember || (callee.Kind == NOptional && callee.Right != nil)
	if isMethod {
		c.compileChainLink(callee.Left, bail) // [recv]
		if callee.Kind == NOptional {
			c.emitGuard(bail)
		}
		tSlot := c.tempLocal()
		c.emitOpU16(OpPutLocal, uint16(tSlot))
		loadMethod := func() {
			if callee.Flags&1 != 0 {
				c.compileExpr(callee.Right)
				c.emit(OpGetElem)
			} else {
				c.emitFieldOp(OpGetField, callee.Right.Str)
			}
		}
		if spread {
			// [method, this, argsArray] -> APPLY
			c.emitOpU16(OpGetLocal, uint16(tSlot)) // [recv]
			loadMethod()                           // [method]
			c.emitOpU16(OpGetLocal, uint16(tSlot)) // [method, this]
			c.buildSpreadArray(n.Args)             // [method, this, argsArray]
			c.emit(OpApply)
			c.emitU16(0)
			return
		}
		c.emitOpU16(OpGetLocal, uint16(tSlot)) // [this]
		c.emitOpU16(OpGetLocal, uint16(tSlot)) // [this, recv]
		loadMethod()                           // [this, method]
		for _, a := range n.Args {
			c.compileExpr(a)
		}
		c.emit(OpCallMethod)
		c.emitU16(uint16(len(n.Args)))
		return
	}
	c.compileChainLink(callee, bail) // [fn] (guarded if callee is NOptional)
	if spread {
		c.emit(OpUndef) // [fn, this=undefined]
		c.buildSpreadArray(n.Args)
		c.emit(OpApply) // [fn, this, argsArray] -> result
		c.emitU16(0)
		return
	}
	for _, a := range n.Args {
		c.compileExpr(a)
	}
	c.emit(OpCall)
	c.emitU16(uint16(len(n.Args)))
}

func (c *compiler) compileTaggedTemplate(n *Node) {
	segs := n.Right.Args
	// Build the frozen template-strings array (with frozen .raw) once, at compile
	// time, and store it as a constant: the same object is passed on every
	// evaluation (permanent caching) and it is frozen.
	cooked := c.rt.newArray()
	co := c.rt.objPtr(cooked)
	raw := c.rt.newArray()
	ro := c.rt.objPtr(raw)
	for i := 0; i < len(segs); i += 2 {
		// Template Literal Revision: a segment with an invalid escape sequence has
		// an undefined cooked value in a tagged template (only .raw survives).
		cookedVal := c.rt.internString(segs[i].Str)
		if segs[i].Flags&fnInvalidCooked != 0 {
			cookedVal = mkundef()
		}
		c.rt.arraySet(co, co.arrLen, cookedVal)
		c.rt.arraySet(ro, ro.arrLen, c.rt.internString(segs[i].Aux))
	}
	co.defineOwn("raw", raw, 0)
	c.rt.sealObject(raw, true)
	c.rt.sealObject(cooked, true)

	// tag(strings, ...substitutions)
	c.compileExpr(n.Left)
	c.emitConst(cooked)
	nsubs := 0
	for i := 1; i < len(segs); i += 2 {
		c.compileExpr(segs[i])
		nsubs++
	}
	c.emit(OpCall)
	c.emitU16(uint16(1 + nsubs))
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
	// Lazy delegation: OP_YIELD_STAR_INIT drives the inner iterator and forwards
	// next/throw/return, leaving the delegate's final value on the stack.
	c.compileExpr(n.Right)
	c.emit(OpYieldStarInit)
	c.emitU16(0) // unused inline operand
}
