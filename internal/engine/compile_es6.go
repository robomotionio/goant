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
		// Drive the iterator lazily (one next() per element, a rest element draining
		// the remainder into a fresh array) and close it on an abrupt or trailing
		// completion when it isn't already exhausted (7.4.6 iterator-closing).
		c.destructureArrayIter(pattern, kind)
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
		c.syntaxErrorf("Invalid destructuring assignment target")
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

	// Protect the element extraction with a try-FINALLY: any abrupt completion —
	// a throwing target/default/value getter, or a `return` unwinding through a
	// `yield` in a default initializer — must close the iterator (unless it is
	// already done) before propagating. A finally (not a catch) is needed so a
	// return, not only a throw, routes through the close.
	tryJump := c.emitJump(OpTryPushFinally)
	c.unwindPush(unwTryFinally)
	for _, elem := range pattern.Args {
		if elem.Kind == NEmpty {
			c.emitIterStep(iterSlot, resSlot, doneSlot)
			c.emit(OpPop) // hole: consume one step and discard
			continue
		}
		if elem.Kind == NRest || elem.Kind == NSpread {
			// Rest element (always last): drain the remaining values into a fresh
			// array — arr[arr.length] = value until the iterator is done — then
			// assign it. Draining leaves the iterator done, so no close follows.
			restTarget := elem.Right
			// AssignmentRestElement evaluates its DestructuringAssignmentTarget
			// reference BEFORE iterating (step 1a). For an assignment to a member
			// (`[...obj[key()]] = it`), pre-evaluate the object and computed key so
			// their side effects — including a `yield` in the key — happen before
			// next() is called; a `return` unwinding through that yield then closes
			// the not-yet-drained iterator.
			memberObjSlot, memberKeySlot := -1, -1
			if kind == varAssign && restTarget != nil && restTarget.Kind == NMember {
				memberObjSlot = c.tempLocal()
				c.compileExpr(restTarget.Left)
				c.emitOpU16(OpPutLocal, uint16(memberObjSlot))
				if restTarget.Flags&1 != 0 { // computed key
					memberKeySlot = c.tempLocal()
					c.compileExpr(restTarget.Right)
					c.emitOpU16(OpPutLocal, uint16(memberKeySlot))
				}
			}
			restSlot := c.tempLocal()
			c.emit(OpArray)
			c.emitU16(0)
			c.emitOpU16(OpPutLocal, uint16(restSlot))
			vSlot := c.tempLocal()
			loopStart := len(c.fn.code)
			c.emitIterStep(iterSlot, resSlot, doneSlot) // [value | undefined]
			c.emitOpU16(OpGetLocal, uint16(doneSlot))
			restDone := c.emitJump(OpJmpTrue) // done: [undefined] left on the stack
			c.emitOpU16(OpPutLocal, uint16(vSlot))
			c.emitOpU16(OpGetLocal, uint16(restSlot))
			c.emitOpU16(OpGetLocal, uint16(restSlot))
			c.emit(OpGetLength)
			c.emitOpU16(OpGetLocal, uint16(vSlot))
			c.emit(OpPutElem)
			c.emit(OpJmp)
			c.emitU32(uint32(loopStart))
			c.patchJump(restDone)
			c.emit(OpPop) // discard the trailing undefined
			if memberObjSlot >= 0 {
				// Store the drained array into the pre-evaluated member reference.
				c.emitOpU16(OpGetLocal, uint16(memberObjSlot))
				if memberKeySlot >= 0 {
					c.emitOpU16(OpGetLocal, uint16(memberKeySlot))
					c.emitOpU16(OpGetLocal, uint16(restSlot))
					c.emit(OpPutElem)
				} else {
					c.emitOpU16(OpGetLocal, uint16(restSlot))
					c.emitFieldOp(OpPutField, restTarget.Right.Str)
				}
			} else {
				c.emitOpU16(OpGetLocal, uint16(restSlot))
				c.destructureTarget(restTarget, kind)
			}
			continue
		}
		target, defExpr := elem, (*Node)(nil)
		if elem.Kind == NAssignPat || (elem.Kind == NAssign && elem.Op == TokAssign) {
			target, defExpr = elem.Left, elem.Right
		}
		// AssignmentElement evaluates its target reference BEFORE stepping the
		// iterator; for a member target that reference has observable side effects
		// (`[ obj[key()] ] = iter` evaluates key() before next()), so pre-evaluate
		// it, then step, apply the default, and store.
		if kind == varAssign && target.Kind == NMember {
			objSlot := c.tempLocal()
			c.compileExpr(target.Left)
			c.emitOpU16(OpPutLocal, uint16(objSlot))
			keySlot := -1
			if target.Flags&1 != 0 { // computed key
				keySlot = c.tempLocal()
				c.compileExpr(target.Right)
				c.emitOpU16(OpPutLocal, uint16(keySlot))
			}
			c.emitIterStep(iterSlot, resSlot, doneSlot)
			c.applyDefault(defExpr)
			vSlot := c.tempLocal()
			c.emitOpU16(OpPutLocal, uint16(vSlot))
			c.emitOpU16(OpGetLocal, uint16(objSlot))
			if keySlot >= 0 {
				c.emitOpU16(OpGetLocal, uint16(keySlot))
				c.emitOpU16(OpGetLocal, uint16(vSlot))
				c.emit(OpPutElem)
			} else {
				c.emitOpU16(OpGetLocal, uint16(vSlot))
				c.emitFieldOp(OpPutField, target.Right.Str)
			}
			continue
		}
		c.emitIterStep(iterSlot, resSlot, doneSlot) // leaves the value (or undefined)
		nameDefaultTarget(target, defExpr)
		c.applyDefault(defExpr)
		c.destructureTarget(target, kind)
	}
	c.emit(OpTryPop)
	c.unwindPop()
	// The finally — entered on normal fall-through and, via the unwind machinery,
	// on throw/return/labelled-jump out of a default's yield. Close the iterator
	// if the pattern did not exhaust it, then resume the pending completion. When
	// that completion is a throw, OpIterClose suppresses any error from the close.
	c.patchJump(tryJump)
	finallyJump := c.emitJump(OpFinally)
	c.unwindPush(unwFinallyBody)
	c.emitCloseIfNotDone(iterSlot, doneSlot)
	c.unwindPop()
	c.emit(OpFinallyRet)
	c.patchJump(finallyJump)
}

// emitCloseIfNotDone emits `if !done: IteratorClose(iter)` — the iterator is
// closed only when it has not already reported (or been marked) done.
func (c *compiler) emitCloseIfNotDone(iterSlot, doneSlot int) {
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
	// Optimistically mark the iterator done before calling next(): a throw from
	// next() itself records the iterator as done (spec IteratorStep), so an
	// enclosing IteratorClose is correctly skipped. Reset to r.done on success.
	c.emit(OpTrue)
	c.emitOpU16(OpPutLocal, uint16(doneSlot))
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
		nameDefaultTarget(target, defExpr)
		c.emitOpU16(OpGetLocal, uint16(src))
		c.compileNumberLiteral(float64(i))
		c.emit(OpGetElem) // src[i]
		c.applyDefault(defExpr)
		c.destructureTarget(target, kind)
	}
}

func (c *compiler) destructureObject(pattern *Node, src int, kind VarKind) {
	// RequireObjectCoercible(src): object destructuring of null/undefined throws a
	// TypeError before any binding — even for an empty pattern (`{} = null`).
	c.emitOpU16(OpGetLocal, uint16(src))
	c.emit(OpIsUndefOrNull)
	coercible := c.emitJump(OpJmpFalse)
	c.emit(OpThrowError)
	c.emitU32(uint32(c.constant(c.rt.internString("Cannot destructure null or undefined"))))
	c.emitByte(0) // TypeError
	c.patchJump(coercible)

	var priorKeys []string
	var computedKeySlots []int
	for _, prop := range pattern.Args {
		if prop.Kind == NSpread {
			// {a, ...rest}: rest is a fresh object with src's enumerable own props
			// minus the already-destructured keys — both the static names and the
			// runtime property keys of any computed properties.
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
			for _, slot := range computedKeySlots {
				c.emit(OpDup)
				c.emitOpU16(OpGetLocal, uint16(slot))
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
		nameDefaultTarget(target, defExpr)
		if computed {
			// { [expr]: target }: read src[ToPropertyKey(expr)]. Evaluate the key
			// once and remember the resulting property key so a trailing ...rest
			// excludes it (a computed key may have side effects — no re-evaluation).
			c.emitOpU16(OpGetLocal, uint16(src)) // [src]
			c.compileExpr(prop.Left)             // [src, key]
			c.emit(OpToPropkey)                  // [src, propKey]
			keySlot := c.addLocal("*restkey*", false)
			c.emit(OpDup)                            // [src, propKey, propKey]
			c.emitOpU16(OpPutLocal, uint16(keySlot)) // [src, propKey]
			computedKeySlots = append(computedKeySlots, keySlot)
			c.emit(OpGetElem) // [val]
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
		// Reuse the slot pre-declared by hoistLexicals (leaving the binding in TDZ
		// until this store) so a hoisted nested function captures the same slot;
		// else declare it now.
		if s := c.lexicalAtCurrentDepth(name); s >= 0 {
			slot = s
		} else {
			slot = c.declareLexical(name, kind == VarConst)
		}
	} else {
		slot = c.declareVar(name, false)
	}
	c.emitOpU16(OpPutLocal, uint16(slot))
}

// compileIdentStore stores the top-of-stack value into an existing identifier
// binding (local/upvalue/with/global), consuming it. Used for destructuring
// assignment leaves.
func (c *compiler) compileIdentStore(name string) {
	if slot := c.resolveLocal(name); slot >= 0 {
		// Assignment to a const binding throws a TypeError (strict & sloppy),
		// mirroring compileAssign's storeVar. Consume the value first so the
		// destructuring stack stays balanced, then throw.
		if c.locals[slot].isConst {
			c.emit(OpPop)
			c.emitConstAssignError()
			return
		}
		// TDZ: destructuring-assigning to a let binding still in its dead zone is a
		// ReferenceError — probe with a checked read (throws on the EMPTY hole).
		if c.locals[slot].blockScoped {
			c.emitOpU16(OpGetLocal, uint16(slot))
			c.emit(OpPop)
		}
		c.emitOpU16(OpPutLocal, uint16(slot))
		return
	}
	switch {
	case c.resolveUpvalue(name) >= 0:
		uv := c.resolveUpvalue(name)
		// Assigning to a const captured from an enclosing scope throws a TypeError.
		if c.upvalues[uv].isConst {
			c.emit(OpPop)
			c.emitConstAssignError()
			return
		}
		// TDZ probe for a captured let (harmless for a var cell, never a hole).
		c.emitOpU16(OpGetUpval, uint16(uv))
		c.emit(OpPop)
		c.emitOpU16(OpPutUpval, uint16(uv))
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

// superHomeBinding returns the captured binding whose value is the home object's
// [[Prototype]] for a `super` reference: the parent constructor (*superctor*) in
// a static context, otherwise the parent prototype (*superproto*). It looks
// through enclosing arrows to the nearest method/block boundary.
func (c *compiler) superHomeBinding() string {
	for e := c; e != nil; e = e.enclosing {
		if e.fn == nil || e.fn.isArrow {
			continue
		}
		if e.staticSuper {
			return "*superctor*"
		}
		break
	}
	return "*superproto*"
}

// hasClassSuper reports whether a `super` reference resolves to a class's home
// binding (*superproto* / *superctor*) rather than an object-literal method's
// runtime [[HomeObject]]. The nearest enclosing non-arrow function decides:
// a class element (isClassElement) captures the class binding, an object method
// does not. Arrows are transparent — they inherit the enclosing method's super.
// (A plain local lookup would misfire, since a *superproto* local left in an
// enclosing scope by a sibling class is not this method's super binding.)
func (c *compiler) hasClassSuper() bool {
	for e := c; e != nil; e = e.enclosing {
		if e.fn == nil || e.fn.isArrow {
			continue
		}
		// Only a *derived* class element captures *superproto* / *superctor*. A
		// base class element (no heritage) has neither, so — like an object
		// method — its super resolves via the element's [[HomeObject]] (the
		// class prototype for an instance element, the class for a static one).
		return e.fn.isClassElement && e.fn.classIsDerived
	}
	return false
}

// emitSuperBase pushes the object a super reference looks up on: a class home
// binding (*superproto* / *superctor*) inside a class element, or an object
// method's [[HomeObject]].[[Prototype]] (via OpGetSuper). Returns false when
// super is not available in the current context.
func (c *compiler) emitSuperBase() bool {
	if c.hasClassSuper() {
		return c.resolveClassBinding(c.superHomeBinding())
	}
	if c.fn != nil && c.fn.isMethod && !c.fn.isArrow {
		c.fn.usesSuper = true
		c.emit(OpGetSuper)
		return true
	}
	return false
}

// emitSuperThis pushes the `this` a super reference uses as its accessor
// receiver: the captured *this* in a class element, the dynamic `this` in an
// object-literal method.
func (c *compiler) emitSuperThis() {
	if c.hasClassSuper() {
		if !c.resolveClassBinding("*this*") {
			c.emit(OpUndef)
		}
		return
	}
	c.emit(OpThis)
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

// inDerivedCtor reports whether the innermost enclosing non-arrow function is a
// derived class constructor (arrows are transparent, inheriting the super
// binding lexically), which is the only place a SuperCall is allowed.
func (c *compiler) inDerivedCtor() bool {
	for e := c; e != nil; e = e.enclosing {
		if e.fn == nil || e.fn.isArrow {
			continue
		}
		return e.fn.isDerivedCtor
	}
	return false
}

// compileSuperCall compiles `super(...)` in a derived constructor: it invokes
// the parent constructor with the current `this`.
func (c *compiler) compileSuperCall(n *Node) {
	// A SuperCall is only legal inside a derived class constructor (arrows are
	// transparent to super, so a super() nested in an arrow of the constructor is
	// fine); in any other method or function it is an early SyntaxError, even
	// though *superctor* is visible there as an upvalue.
	if !c.inDerivedCtor() {
		c.syntaxErrorf("'super' keyword unexpected here")
		return
	}
	// super(...) constructs the parent with the derived class's new.target and
	// binds the resulting object as `this` (so subclassing an exotic native like
	// Function/Array/Error yields the native object rather than an ordinary one).
	if !c.resolveClassBinding("*superctor*") { // [superctor]
		c.syntaxErrorf("'super' keyword unexpected here")
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
	// A derived class initializes its instance fields immediately after super()
	// binds `this` (InitializeInstanceElements). classFields is set only on the
	// constructor's own compiler, so a super() call nested in an arrow is a no-op
	// here (a rare case left for later).
	c.emitInstanceFieldInit()
}

// compileSuperMethodCall compiles `super.method(...)`: it invokes the parent
// prototype's method with the current `this`. The lookup base is a class home
// binding (*superproto* / *superctor*) inside a class element, or the object
// method's [[HomeObject]].[[Prototype]] (via OpGetSuper) inside an object
// literal method.
func (c *compiler) compileSuperMethodCall(n *Node) {
	member := n.Left
	// emitSuperBase pushes the object to look the method up on; false → error.
	emitSuperBase := func() bool {
		if c.hasClassSuper() {
			return c.resolveClassBinding(c.superHomeBinding())
		}
		if c.fn != nil && c.fn.isMethod && !c.fn.isArrow {
			c.fn.usesSuper = true
			c.emit(OpGetSuper)
			return true
		}
		return false
	}
	// emitSuperThis pushes the receiver: the captured *this* in a class element,
	// the dynamic `this` in an object method.
	emitSuperThis := func() {
		if c.hasClassSuper() {
			if !c.resolveClassBinding("*this*") {
				c.emit(OpUndef)
			}
			return
		}
		c.emit(OpThis)
	}
	emitMethod := func() {
		if member.Flags&1 != 0 {
			c.compileExpr(member.Right)
			c.emit(OpGetElem)
		} else {
			c.emitFieldOp(OpGetField, member.Right.Str)
		}
	}
	// Spread args: emit [method, this, argsArray] for APPLY.
	if hasSpread(n.Args) {
		if !emitSuperBase() {
			c.syntaxErrorf("'super' keyword unexpected here")
			return
		}
		emitMethod()   // [method]
		emitSuperThis() // [method, this]
		c.buildSpreadArray(n.Args) // [method, this, argsArray]
		c.emit(OpApply)
		c.emitU16(0)
		return
	}
	emitSuperThis() // [this]
	if !emitSuperBase() {
		c.syntaxErrorf("'super' keyword unexpected here")
		return
	}
	emitMethod() // [this, method]
	for _, a := range n.Args {
		c.compileExpr(a)
	}
	c.emit(OpCallMethod) // [this, method, args...] -> result
	c.emitU16(uint16(len(n.Args)))
}

// compileClass compiles a class declaration/expression into a constructor
// function with prototype methods and static members, wiring the extends
// prototype chain. super() / super.method are a later refinement.
// isInstanceFieldMember reports whether a class element is an instance field
// (as opposed to a method, accessor, constructor, static member, or static
// block). Methods/accessors/constructor carry fnMethod on their function node;
// a field's value is a plain expression (or NUndef for `x;`), possibly even a
// non-method function (`x = function(){}`).
func isInstanceFieldMember(m *Node) bool {
	if m == nil || m.Kind != NMethod {
		return false
	}
	if m.Flags&(fnStatic|fnGetter|fnSetter) != 0 {
		return false
	}
	if m.Flags&fnComputed == 0 && m.Left != nil && m.Left.Kind == NIdent && m.Left.Str == "constructor" {
		return false
	}
	if m.Right != nil && m.Right.Kind == NFunc && m.Right.Flags&fnMethod != 0 {
		return false
	}
	return true
}

// isInstancePrivateMethod reports whether m is a non-static private method or
// accessor. These are installed per-instance in the object's private
// environment (unlike public methods, which live on the prototype).
func isInstancePrivateMethod(m *Node) bool {
	if m == nil || m.Kind != NMethod || m.Flags&fnStatic != 0 || !isPrivateMemberProp(m.Left) {
		return false
	}
	if m.Flags&(fnGetter|fnSetter) != 0 {
		return true
	}
	return m.Right != nil && m.Right.Kind == NFunc && m.Right.Flags&fnMethod != 0
}

// isInstanceMember reports whether m is installed per-instance (a field or a
// private method/accessor) rather than on the prototype.
func isInstanceMember(m *Node) bool {
	return isInstanceFieldMember(m) || isInstancePrivateMethod(m)
}

// emitInstanceFieldInit initializes a class's instance elements on `this`: first
// all private methods/accessors are added to the instance's private
// environment, then each field's initializer is evaluated with `this` bound to
// the new instance and defined (DefineField — not [[Set]]). Called at base-ctor
// entry and right after super() in a derived ctor.
func (c *compiler) emitInstanceFieldInit() {
	loadThis := func() {
		if slot := c.resolveLocal("*this*"); slot >= 0 {
			c.emitOpU16(OpGetLocal, uint16(slot))
		} else if uv := c.resolveUpvalue("*this*"); uv >= 0 {
			c.emitOpU16(OpGetUpval, uint16(uv))
		} else {
			c.emit(OpUndef)
		}
	}
	// Pass 1: install private methods/accessors before any field initializer runs
	// (so a field initializer may call them).
	for _, m := range c.classFields {
		if !isInstancePrivateMethod(m) {
			continue
		}
		flags := byte(0)
		prefix := ""
		if m.Flags&fnGetter != 0 {
			flags, prefix = 1, "get "
		} else if m.Flags&fnSetter != 0 {
			flags, prefix = 2, "set "
		}
		if m.Right.Str == "" {
			m.Right.Str = prefix + m.Left.Str
			m.Right.Flags |= fnInferredName
		}
		loadThis()
		c.compileFunc(m.Right)
		idx := c.constant(c.rt.internString(m.Left.Str))
		c.emit(OpDefineMethod)
		c.emitU32(uint32(idx))
		c.emitByte(flags)
		c.emit(OpPop)
	}
	// Pass 2: initialize fields in source order.
	for _, m := range c.classFields {
		if isInstancePrivateMethod(m) {
			continue
		}
		if m.Flags&fnComputed != 0 {
			// [this] [key] [value] — computed key (evaluated per-instance; a v1
			// simplification of the once-at-definition rule).
			loadThis()
			c.compileExpr(m.Left)
			if m.Right == nil {
				c.emit(OpUndef)
			} else {
				c.inFieldInit = true
				c.compileExpr(m.Right)
				c.inFieldInit = false
			}
			c.emit(OpPutElem)
			continue
		}
		name, ok := propKeyName(m.Left)
		if !ok {
			c.errorf("unsupported class field key (slice)")
			return
		}
		loadThis()
		if m.Right == nil {
			c.emit(OpUndef)
		} else {
			nameAnonExpr(m.Right, name) // NamedEvaluation for `field = () => {}`
			c.inFieldInit = true
			c.compileExpr(m.Right)
			c.inFieldInit = false
		}
		c.emitDefineField(name)
		c.emit(OpPop)
	}
}

// collectClassPrivateNames returns the set of private names (with leading '#')
// declared directly in a class body.
func collectClassPrivateNames(n *Node) map[string]bool {
	var names map[string]bool
	for _, m := range n.Args {
		if m == nil || m.Kind != NMethod || !isPrivateMemberProp(m.Left) {
			continue
		}
		if names == nil {
			names = map[string]bool{}
		}
		names[m.Left.Str] = true
	}
	return names
}

// privateNameDeclared reports whether a private name is declared in the class
// body currently being compiled or in any enclosing one. An eval body compiles
// without the enclosing class's private environment, yet a direct eval may
// legitimately reference an enclosing private name, so the check is skipped
// there (the reference is resolved at runtime instead).
func (c *compiler) privateNameDeclared(name string) bool {
	for e := c; e != nil; e = e.enclosing {
		for _, env := range e.classPrivateEnvs {
			if env[name] {
				return true
			}
		}
		if e.isEval {
			return true
		}
	}
	return false
}

func (c *compiler) compileClass(n *Node) {
	ctorSlot := c.tempLocal()
	protoSlot := c.tempLocal()

	// This class's private environment is pushed after the heritage is compiled
	// (below) and popped on exit, so the heritage, siblings, and enclosing code
	// cannot see these private names — while a nested class expression keeps the
	// enclosing environment visible beneath its own on the same compiler's stack.
	privDepth := len(c.classPrivateEnvs)
	defer func() { c.classPrivateEnvs = c.classPrivateEnvs[:privDepth] }()

	// A named class has an inner, immutable binding of its own name scoped to the
	// class body: methods close over it, and it is unaffected by reassignment of
	// any outer binding of the same name (class.lexical-name).
	classNameSlot := -1
	if n.Str != "" {
		c.scopeDepth++
		classNameSlot = c.declareLexical(n.Str, true)
		// The binding starts uninitialized (TDZ) so a computed member key that
		// reads the class name throws (class.computed-names-tdz); it is filled with
		// the constructor only after all members are defined.
		c.emit(OpEmpty)
		c.emitOpU16(OpPutLocal, uint16(classNameSlot))
	}

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
		// Each class needs its OWN *superctor* / *superproto* slot: declareVar
		// reuses a same-named binding in scope, so two sibling derived classes
		// would share one slot and the later class's parent would clobber the
		// earlier's — turning the earlier's super() into infinite self-recursion.
		superSlot = c.addLocal("*superctor*", false)
		c.compileExpr(n.Left)
		c.emitOpU16(OpPutLocal, uint16(superSlot))
		// The heritage must be null or a constructor, checked at class definition.
		c.emitOpU16(OpGetLocal, uint16(superSlot))
		c.emit(OpChkCtor)
		superProtoSlot = c.addLocal("*superproto*", false)
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
		// The protoParent (superclass.prototype) must be an Object or null.
		c.emitOpU16(OpGetLocal, uint16(superProtoSlot))
		c.emit(OpChkProto)
	}

	// The private environment is now in scope for the constructor, members, field
	// initializers, and static blocks (the heritage above was compiled without it).
	c.classPrivateEnvs = append(c.classPrivateEnvs, collectClassPrivateNames(n))

	// Collect instance fields and hand them to the constructor so it initializes
	// them per-instance (base: at entry; derived: after super()). They are NOT
	// defined on the prototype.
	var instanceFields []*Node
	for _, m := range n.Args {
		if isInstanceMember(m) {
			instanceFields = append(instanceFields, m)
		}
	}
	c.pendingClassFields = instanceFields
	c.pendingClassDerived = n.Left != nil

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
			c.pendingStaticSuper = true               // a static block's home is the constructor
			c.pendingClassDerived = n.Left != nil
			c.compileFunc(blockFn) // func
			c.emit(OpCallMethod)                      // [this, func] -> result
			c.emitU16(0)
			c.emit(OpPop)
			continue
		}
		if m.Kind != NMethod {
			continue
		}
		// Instance fields and private methods/accessors are installed per-instance
		// by the constructor, not defined here on the prototype.
		if isInstanceMember(m) {
			continue
		}
		if m.Left != nil && m.Left.Kind == NIdent && m.Left.Str == "constructor" && m.Flags&fnStatic == 0 {
			continue
		}
		target := protoSlot
		c.pendingStaticSuper = false
		c.pendingClassDerived = n.Left != nil
		if m.Flags&fnStatic != 0 {
			target = ctorSlot
			c.pendingStaticSuper = true // a static method's home is the constructor
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
			if m.Right != nil && m.Right.Kind == NFunc && m.Right.Flags&fnMethod != 0 {
				// A computed method is a non-enumerable data property whose name is
				// the key (DefineMethod → SetFunctionName), not an ordinary [k]=v.
				c.compileFunc(m.Right)
				c.emit(OpDefineMethodComp)
				c.emitByte(0) // data method
				c.emit(OpPop)
				continue
			}
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

	// Now that all members (including computed keys) are defined, initialize the
	// inner class-name binding to the constructor so method bodies see the class,
	// then close its scope (the methods have already captured the binding).
	if classNameSlot >= 0 {
		c.emitOpU16(OpGetLocal, uint16(ctorSlot))
		c.emitOpU16(OpPutLocal, uint16(classNameSlot))
		c.scopeDepth--
		c.popBlockScope()
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
			c.syntaxErrorf("Invalid or unexpected token in template literal")
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
		// A super-property base (`super.a?.b`) reads through the super binding, not
		// an ordinary receiver on the stack.
		if n.Left != nil && n.Left.Kind == NIdent && n.Left.Str == "super" {
			c.compileSuperMember(n) // leaves the super-property value
			return
		}
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
	// A super constructor call `super(...)` as a chain base (super()?.x) leaves the
	// constructed `this` on the stack.
	if callee.Kind == NIdent && callee.Str == "super" {
		c.compileSuperCall(n)
		return
	}
	// An optional call `base?.(...)` wraps its callee in NOptional with Right==nil.
	// The callee VALUE is what gets guarded (not a member of it), so unwrap to the
	// real callee and remember to guard the loaded value before calling — a member
	// callee still binds `this` to its base.
	guardCallee := false
	if callee.Kind == NOptional && callee.Right == nil {
		callee = callee.Left
		guardCallee = true
	}
	// A super method callee (`super.m?.()`) reads the method through the super
	// binding and calls it with the method's `this`, not an ordinary receiver.
	if callee.Kind == NMember && callee.Left != nil && callee.Left.Kind == NIdent && callee.Left.Str == "super" {
		if !c.emitSuperBase() { // [base]
			c.syntaxErrorf("'super' keyword unexpected here")
			return
		}
		if callee.Flags&1 != 0 { // method = base[key]
			c.compileExpr(callee.Right)
			c.emit(OpGetElem)
		} else {
			c.emitFieldOp(OpGetField, callee.Right.Str)
		} // [method]
		if guardCallee {
			c.emitGuard(bail) // [method] (balanced on bail)
		}
		mSlot := c.tempLocal()
		c.emitOpU16(OpPutLocal, uint16(mSlot))
		if spread {
			c.emitOpU16(OpGetLocal, uint16(mSlot)) // [method]
			c.emitSuperThis()                      // [method, this]
			c.buildSpreadArray(n.Args)             // [method, this, argsArray]
			c.emit(OpApply)
			c.emitU16(0)
			return
		}
		c.emitSuperThis()                      // [this]
		c.emitOpU16(OpGetLocal, uint16(mSlot)) // [this, method]
		for _, a := range n.Args {
			c.compileExpr(a)
		}
		c.emit(OpCallMethod)
		c.emitU16(uint16(len(n.Args)))
		return
	}
	isMethod := callee.Kind == NMember || (callee.Kind == NOptional && callee.Right != nil)
	if isMethod {
		c.compileChainLink(callee.Left, bail) // [recv]
		if callee.Kind == NOptional {
			c.emitGuard(bail) // a?.b(...): guard the member base
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
		// For `base?.(...)` the method value must be guarded, but only while it is
		// alone on the stack (a bail pops exactly one value). Load it, guard it,
		// and stash it so `this` can be re-established below.
		mSlot := -1
		if guardCallee {
			c.emitOpU16(OpGetLocal, uint16(tSlot)) // [recv]
			loadMethod()                           // [method]
			c.emitGuard(bail)                      // [method] (balanced on bail)
			mSlot = c.tempLocal()
			c.emitOpU16(OpPutLocal, uint16(mSlot))
		}
		emitMethodValue := func() {
			if mSlot >= 0 {
				c.emitOpU16(OpGetLocal, uint16(mSlot))
			} else {
				c.emitOpU16(OpGetLocal, uint16(tSlot))
				loadMethod()
			}
		}
		if spread {
			emitMethodValue()                      // [method]
			c.emitOpU16(OpGetLocal, uint16(tSlot)) // [method, this]
			c.buildSpreadArray(n.Args)             // [method, this, argsArray]
			c.emit(OpApply)
			c.emitU16(0)
			return
		}
		c.emitOpU16(OpGetLocal, uint16(tSlot)) // [this]
		emitMethodValue()                      // [this, method]
		for _, a := range n.Args {
			c.compileExpr(a)
		}
		c.emit(OpCallMethod)
		c.emitU16(uint16(len(n.Args)))
		return
	}
	c.compileChainLink(callee, bail) // [fn]
	if guardCallee {
		c.emitGuard(bail) // base?.(...) with a non-member callee: guard the value
	}
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
