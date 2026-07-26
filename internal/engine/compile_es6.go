package engine

import "strconv"

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
			c.compileExpr(prop.Left) // [key]
			c.emit(OpToPropkey)      // [propKey]
			keySlot := c.addLocal("*restkey*", false)
			c.emitOpU16(OpPutLocal, uint16(keySlot)) // []
			computedKeySlots = append(computedKeySlots, keySlot)
			// A member target's reference is evaluated BEFORE the source property is
			// read, so its object and key are observed ahead of the source's getter.
			tObj, tKey, isMember := c.prepareMemberTarget(target)
			c.emitOpU16(OpGetLocal, uint16(src))
			c.emitOpU16(OpGetLocal, uint16(keySlot))
			c.emit(OpGetElem) // [val]
			c.applyDefault(defExpr)
			if isMember {
				c.storeMemberTarget(target, tObj, tKey)
			} else {
				c.destructureTarget(target, kind)
			}
			continue
		}
		priorKeys = append(priorKeys, name)
		tObj, tKey, isMember := c.prepareMemberTarget(target)
		c.emitOpU16(OpGetLocal, uint16(src))
		c.emitFieldOp(OpGetField, name)
		c.applyDefault(defExpr)
		if isMember {
			c.storeMemberTarget(target, tObj, tKey)
			continue
		}
		c.destructureTarget(target, kind)
	}
}

// prepareMemberTarget evaluates a destructuring target that is a member
// reference (`{p: obj[k]} = src`) into temporaries, WITHOUT consuming a value.
// The reference is evaluated before the source property is read — the spec
// evaluates the DestructuringAssignmentTarget first — so `obj` and `k` are seen
// before the source's getter runs. ok is false for any other target shape.
func (c *compiler) prepareMemberTarget(target *Node) (objSlot, keySlot int, ok bool) {
	// A name resolved through an object environment has an observable reference
	// too (the with-object's HasProperty), and it is resolved before the source is
	// read. Leave its base on the stack for storeMemberTarget's paired write.
	if target != nil && target.Kind == NIdent && c.nameIsWithRouted(target.Str) {
		c.emitWithVarBase(target.Str)
		return -1, -1, true
	}
	if target == nil || target.Kind != NMember {
		return 0, 0, false
	}
	objSlot = c.tempLocal()
	c.compileExpr(target.Left)
	c.emitOpU16(OpPutLocal, uint16(objSlot))
	keySlot = -1
	if target.Flags&1 != 0 { // computed
		// The key expression is EVALUATED here, with the reference, but not
		// coerced: EvaluatePropertyAccessWithExpressionKey leaves the raw value as
		// the Reference's [[ReferencedName]], and ToPropertyKey runs in the paired
		// PutValue — after the source property has been read.
		keySlot = c.tempLocal()
		c.compileExpr(target.Right)
		c.emitOpU16(OpPutLocal, uint16(keySlot))
	}
	return objSlot, keySlot, true
}

// storeMemberTarget writes the top-of-stack value through a reference prepared
// by prepareMemberTarget.
func (c *compiler) storeMemberTarget(target *Node, objSlot, keySlot int) {
	if objSlot < 0 { // with-routed identifier: [base, value] on the stack
		c.emitWithVarRef(OpWithPutVar, target.Str)
		c.emit(OpPop)
		return
	}
	vSlot := c.tempLocal()
	c.emitOpU16(OpPutLocal, uint16(vSlot))
	c.emitOpU16(OpGetLocal, uint16(objSlot))
	if keySlot >= 0 {
		c.emitOpU16(OpGetLocal, uint16(keySlot))
		c.emitOpU16(OpGetLocal, uint16(vSlot))
		c.emit(OpPutElem)
		return
	}
	c.emitOpU16(OpGetLocal, uint16(vSlot))
	c.emitFieldOp(OpPutField, target.Right.Str)
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
	// A Module's top-level `var` is a frame local, not a global-object property
	// (same condition as compileVarDecl's asGlobal). Without the isModule guard a
	// destructuring `var` in a module stored to the global — which, module code
	// being strict, threw ReferenceError for the undeclared name.
	if kind == VarVar && c.isScript && !c.isEval && !c.isModule {
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
	// A private name is never a valid assignment target (`for (#x in obj)`,
	// `[#x] = …`); the only legal use is `#x in obj`, handled in compileBinary.
	if len(name) > 0 && name[0] == '#' {
		c.syntaxErrorf("Private field '%s' may not be an assignment target", name)
		return
	}
	// An imported binding is immutable: assigning to it throws a TypeError. The
	// value is consumed first so the destructuring stack stays balanced.
	if _, isImport := c.lookupImport(name); isImport {
		c.emit(OpPop)
		c.emitConstAssignError()
		return
	}
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
	// In a function nested inside a `with`, a captured with-object sits outside
	// this function's own scope but before the enclosing scope, so a free name
	// (not one of our locals, handled above) routes to it ahead of any upvalue.
	if c.inheritedWith {
		c.emitWithVar(OpWithPutVar, name)
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
// superAvailable reports whether a `super` reference is permitted here: the
// nearest enclosing non-arrow function must be an object method, a class element,
// or a class constructor (each carries a [[HomeObject]] / class-super bindings).
// Used to decide whether a direct eval at this site may contain `super`.
func (c *compiler) superAvailable() bool {
	for e := c; e != nil; e = e.enclosing {
		if e.fn == nil || e.fn.isArrow {
			continue
		}
		return e.fn.isMethod || e.fn.isClassElement || e.fn.isClassCtor
	}
	return false
}

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
	if c.methodForInheritedSuper() != nil {
		c.markInheritedSuper()
		c.emit(OpGetSuper)
		return true
	}
	if ec := c.borrowedSuperCompiler(); ec != nil {
		// Direct eval nested in a method/constructor borrows the home object; an
		// arrow written inside that eval code inherits it in turn.
		for e := c; e != nil && e != ec; e = e.enclosing {
			e.fn.capturesHome = true
		}
		c.emit(OpGetSuper)
		return true
	}
	return false
}

// borrowedSuperCompiler returns the direct-eval compiler whose borrowed caller
// scope permits `super`, searching outward past arrows: `eval("() => super.x")`
// in a method resolves through the eval frame's borrowed [[HomeObject]] just as
// the eval body itself does. An ordinary function written inside the eval code
// establishes its own super context and stops the search.
func (c *compiler) borrowedSuperCompiler() *compiler {
	for e := c; e != nil; e = e.enclosing {
		if e.borrowed != nil {
			if e.borrowed.superAllowed {
				return e
			}
			return nil
		}
		if e.fn == nil || !e.fn.isArrow {
			return nil
		}
	}
	return nil
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
	// The synthetic *this* binding, exactly as a `this` expression reads it: an
	// arrow (in a method or in eval code) has none of its own and captures the
	// enclosing one.
	if !c.resolveClassBinding("*this*") {
		c.emit(OpThis)
	}
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
	// [[Construct]] the parent with the DERIVED constructor's new.target, which an
	// arrow nested in the constructor reaches through the *newtarget* binding —
	// its own frame has none.
	if slot := c.resolveLocal("*newtarget*"); slot >= 0 {
		c.emitOpU16(OpGetLocal, uint16(slot))
	} else if uv := c.resolveUpvalue("*newtarget*"); uv >= 0 {
		c.emitOpU16(OpGetUpval, uint16(uv))
	} else {
		c.emit(OpSpecialObj)
		c.emitByte(2)
	}
	// Push the running class's constructor; OpSuperApply takes its [[Prototype]]
	// so a class whose prototype was reassigned calls the new parent.
	if !c.resolveClassBinding("*classctor*") { // [newtarget, classctor]
		c.syntaxErrorf("'super' keyword unexpected here")
		return
	}
	c.buildSpreadArray(n.Args) // [newtarget, classctor, argsArray]
	// BindThisValue: a second super() for the same `this` is a ReferenceError,
	// but only AFTER the parent has been constructed again (observable). Tell
	// OpSuperApply which binding holds `this` so it can make that check itself —
	// reading it raw, since an uninitialised `this` IS the empty value and an
	// ordinary load would report a temporal dead zone. Naming the same binding
	// the store below writes covers a super() nested in an arrow, which reaches
	// the constructor's `this` through an upvalue.
	thisLocal, thisUpval := c.resolveLocal("*this*"), -1
	if thisLocal < 0 {
		thisUpval = c.resolveUpvalue("*this*")
	}
	thisRef := uint16(0)
	switch {
	case thisLocal >= 0 && thisLocal < superThisIndexMax:
		thisRef = superThisLocal | uint16(thisLocal)
	case thisUpval >= 0 && thisUpval < superThisIndexMax:
		thisRef = superThisUpval | uint16(thisUpval)
	}
	c.emit(OpSuperApply) // -> [constructedThis]
	c.emitU16(thisRef)
	// Bind the constructed object as `this`, leaving it as the call's value.
	if thisLocal >= 0 {
		c.emitOpU16(OpSetLocal, uint16(thisLocal))
	} else if thisUpval >= 0 {
		c.emitOpU16(OpSetUpval, uint16(thisUpval))
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
	// emitSuperBase / emitSuperThis are the shared helpers: they also cover a
	// direct eval nested in a method/constructor (c.borrowed.superAllowed), so
	// `super.method()` inside such an eval resolves like it does in the method.
	emitSuperBase := c.emitSuperBase
	emitSuperThis := c.emitSuperThis
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

// privMethodSlotName is the class-scope local that holds an instance private
// method/accessor's SHARED function object. A private accessor may declare a
// getter and a setter under the same name, so the slot is qualified by kind
// (g/s/m) as well as the mangled private key to keep the two distinct.
func (c *compiler) privMethodSlotName(m *Node) string {
	kind := "m"
	if m.Flags&fnGetter != 0 {
		kind = "g"
	} else if m.Flags&fnSetter != 0 {
		kind = "s"
	}
	return "*pm*" + kind + c.privateKey(m.Left.Str)
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
	// (so a field initializer may call them). Each method function was created
	// ONCE at class definition (see the shared-private-method loop in compileClass),
	// so every instance's private environment receives the SAME function object —
	// as the spec requires (a private method is not re-created per instance).
	for _, m := range c.classFields {
		if !isInstancePrivateMethod(m) {
			continue
		}
		flags := byte(0)
		if m.Flags&fnGetter != 0 {
			flags = 1
		} else if m.Flags&fnSetter != 0 {
			flags = 2
		}
		loadThis()
		pmName := c.privMethodSlotName(m)
		if slot := c.resolveLocal(pmName); slot >= 0 {
			c.emitOpU16(OpGetLocal, uint16(slot))
		} else if uvi := c.resolveUpvalue(pmName); uvi >= 0 {
			c.emitOpU16(OpGetUpval, uint16(uvi))
		} else {
			c.emit(OpUndef) // unreachable: the slot is always declared in compileClass
		}
		idx := c.constant(c.rt.internString(c.privateKey(m.Left.Str)))
		c.emit(OpDefineMethod)
		c.emitU32(uint32(idx))
		c.emitByte(flags | 8) // bit 3: function already homed at creation; skip setMethodHome
		c.emit(OpPop)
	}
	// Pass 2: initialize fields in source order.
	for _, m := range c.classFields {
		if isInstancePrivateMethod(m) {
			continue
		}
		if m.Flags&fnComputed != 0 {
			// [this] [key] [value]. The computed key was evaluated ONCE at class
			// definition into a class-scope slot (captured here as an upvalue); read
			// it rather than re-evaluating the key expression per instance.
			loadThis()
			if i, ok := c.fieldKeys[m]; ok {
				name := fieldKeySlotName(i)
				if slot := c.resolveLocal(name); slot >= 0 {
					c.emitOpU16(OpGetLocal, uint16(slot))
				} else if uv := c.resolveUpvalue(name); uv >= 0 {
					c.emitOpU16(OpGetUpval, uint16(uv))
				} else {
					c.compileExpr(m.Left) // unreachable: the slot is declared in compileClass
				}
			} else {
				c.compileExpr(m.Left)
			}
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
			if _, ok := env[name]; ok {
				return true
			}
		}
	}
	// A direct eval carries the enclosing class bodies' private names in its own
	// classPrivateEnvs (captured into the evalScope), checked above — so a private
	// name it does not declare is an early SyntaxError, exactly as outside eval.
	return false
}

// mangleClassPrivates maps each private name of a class body to a storage key
// unique to that body (`#x` -> `#x\x00<id>`), giving `#x` in distinct classes
// distinct identities so a cross-class private access fails the brand check.
func (c *compiler) mangleClassPrivates(names map[string]bool) map[string]string {
	if len(names) == 0 {
		return nil
	}
	c.rt.privClassSeq++
	suffix := "\x00" + strconv.Itoa(c.rt.privClassSeq)
	m := make(map[string]string, len(names))
	for name := range names {
		m[name] = name + suffix
	}
	return m
}

// privateKey resolves a private name (`#x`) to its per-class-body mangled storage
// key (nearest enclosing declaring class wins, matching lexical scope). A public
// name is returned unchanged; an unresolved private name keeps its raw form.
func (c *compiler) privateKey(name string) string {
	if len(name) == 0 || name[0] != '#' {
		return name
	}
	for e := c; e != nil; e = e.enclosing {
		for i := len(e.classPrivateEnvs) - 1; i >= 0; i-- {
			if m, ok := e.classPrivateEnvs[i][name]; ok {
				return m
			}
		}
	}
	return name
}

// fieldKeySlotName is the class-scope local name holding the i-th computed
// instance-field's pre-evaluated property key.
func fieldKeySlotName(i int) string { return "*fk" + strconv.Itoa(i) + "*" }

func (c *compiler) compileClass(n *Node) {
	ctorSlot := c.tempLocal()
	protoSlot := c.tempLocal()
	// All parts of a class definition are strict-mode code — the heritage, the
	// computed keys, and the element initializers — even when the class appears in
	// sloppy code. The parser already enforces that for early errors; this makes
	// the CODE emitted for those parts strict too, so a function expression in the
	// heritage has no own `arguments` / `caller` and its `arguments` object is
	// unmapped (class/strict-mode/arguments-callee).
	savedStrict := c.fn.isStrict
	c.fn.isStrict = true
	defer func() { c.fn.isStrict = savedStrict }()

	// Whether this class body needs a ClassPrivateEnvironment of its own (the
	// OpSpecialObj kind 4/5 pair below): only if it declares private names.
	ownPrivEnv := false

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
	superSlot, superProtoSlot, classCtorSlot := -1, -1, -1
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
		// GetSuperConstructor reads the RUNNING constructor's [[Prototype]] at each
		// super() call, not the heritage captured when the class was defined
		// (`Object.setPrototypeOf(C, X)` redirects it). Declared before the
		// constructor is compiled so its body captures the slot as an upvalue; the
		// slot is filled once the constructor exists, below.
		classCtorSlot = c.addLocal("*classctor*", false)
	}

	// The private environment is now in scope for the constructor, members, field
	// initializers, and static blocks (the heritage above was compiled without it).
	classPrivates := c.mangleClassPrivates(collectClassPrivateNames(n))
	c.classPrivateEnvs = append(c.classPrivateEnvs, classPrivates)
	// Each *evaluation* of a class body creates a fresh set of Private Names, so
	// an instance of one evaluation fails another's brand check even though both
	// were compiled from the same source. The mangled key identifies the body; a
	// runtime tag, allocated here and restored at the end of the definition,
	// identifies the evaluation. Every closure created in between captures it.
	if len(classPrivates) > 0 {
		ownPrivEnv = true
		c.emit(OpSpecialObj)
		c.emitByte(4)
	}

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

	// Declare a class-scope slot for each instance private method's SHARED function
	// (populated below, once the prototype exists) so the constructor captures it
	// as an upvalue and installs the same function object on every instance.
	for _, m := range instanceFields {
		if isInstancePrivateMethod(m) {
			c.addLocal(c.privMethodSlotName(m), false)
		}
	}

	// A computed instance-field key is evaluated ONCE at class definition (in
	// source order, in the element loop below), not per instance. Declare a
	// class-scope slot for each so the constructor captures it as an upvalue and
	// emitInstanceFieldInit reads the pre-evaluated key. (Static-field keys already
	// evaluate once, in the element loop.)
	var computedFieldKeys map[*Node]int
	for i, m := range instanceFields {
		if isInstanceFieldMember(m) && m.Flags&fnComputed != 0 {
			if computedFieldKeys == nil {
				computedFieldKeys = map[*Node]int{}
			}
			computedFieldKeys[m] = i
			c.addLocal(fieldKeySlotName(i), false)
		}
	}
	c.pendingFieldKeys = computedFieldKeys // handed to the constructor by compileFunc

	c.compileFunc(ctorFn) // [ctor]
	c.emitOpU16(OpPutLocal, uint16(ctorSlot))
	if classCtorSlot >= 0 {
		c.emitOpU16(OpGetLocal, uint16(ctorSlot))
		c.emitOpU16(OpPutLocal, uint16(classCtorSlot))
	}

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

	// The constructor gets a [[HomeObject]] too, like every other method. It is
	// only observable through a direct eval — `super.x` written in the
	// constructor's own source resolves against the class's *superproto* binding
	// — but eval code borrows the running closure's home, and without this there
	// was none to borrow. setMethodHome is a no-op unless the function is marked
	// usesSuper, so this costs nothing for a constructor that cannot need it.
	c.emitOpU16(OpGetLocal, uint16(protoSlot))
	c.emitOpU16(OpGetLocal, uint16(ctorSlot))
	c.emit(OpSetHomeObj)
	c.emit(OpPop)
	c.emit(OpPop)

	// ClassDefinitionEvaluation evaluates every element in source order — which
	// for a field means only its computed KEY — and runs the static elements'
	// initializers afterwards, in their own source order. staticElements records
	// them; keySlot >= 0 names the class-scope local holding a pre-evaluated
	// computed key.
	type staticElement struct {
		m       *Node
		keySlot int
		name    string
	}
	var staticElements []staticElement
	// Define methods (skip the constructor; it's already the ctor function).
	for _, m := range n.Args {
		if m.Kind == NStaticBlock {
			// A static initialization block runs with the static field initializers,
			// interleaved with them in source order.
			staticElements = append(staticElements, staticElement{m: m, keySlot: -1})
			continue
		}
		if m.Kind != NMethod {
			continue
		}
		// Instance fields and private methods/accessors are installed per-instance
		// by the constructor, not defined here on the prototype.
		if isInstanceMember(m) {
			// Evaluate a computed instance-field key ONCE, here, in source order
			// (interleaved with static-field keys), into its class-scope slot; the
			// constructor reads it per instance instead of re-evaluating.
			if i, ok := computedFieldKeys[m]; ok {
				c.compileExpr(m.Left)
				c.emit(OpToPropkey)
				c.emitOpU16(OpPutLocal, uint16(c.resolveLocal(fieldKeySlotName(i))))
			}
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
			// A computed STATIC FIELD: only its key is evaluated here, in source
			// order; the initializer runs with the other static elements below.
			if isClassFieldMember(m) {
				slot := c.addLocal("*sfk"+strconv.Itoa(len(staticElements))+"*", false)
				c.compileExpr(m.Left)
				c.emit(OpToPropkey)
				c.emitOpU16(OpPutLocal, uint16(slot))
				staticElements = append(staticElements, staticElement{m: m, keySlot: slot})
				continue
			}
			// Computed data method: a non-enumerable data property whose name is the
			// key (DefineMethod → SetFunctionName), not an ordinary [k]=v.
			c.emitOpU16(OpGetLocal, uint16(target)) // [target]
			c.compileExpr(m.Left)                   // [target, key]
			c.compileFunc(m.Right)
			c.emit(OpDefineMethodComp)
			c.emitByte(0) // data method
			c.emit(OpPop)
			continue
		}
		name, ok := propKeyName(m.Left)
		if !ok {
			c.errorf("unsupported class member key (slice)")
			return
		}
		// Class field with a value (not a method): m.Right is an expression. A field
		// initialized to a function expression or an arrow is still a FIELD — only a
		// concise method carries fnMethod — so it must be an enumerable data
		// property, and a static one must be evaluated in the class-element context
		// where `this` and `super` mean the constructor.
		if m.Right != nil && isClassFieldMember(m) {
			if m.Flags&fnStatic != 0 {
				staticElements = append(staticElements, staticElement{m: m, keySlot: -1, name: name})
				continue
			}
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
		idx := c.constant(c.rt.internString(c.privateKey(name)))
		c.emit(OpDefineMethod)
		c.emitU32(uint32(idx))
		c.emitByte(flags)
		c.emit(OpPop)
	}

	// ClassDefinitionEvaluation initialises the inner class-name binding once every
	// element has been defined and BEFORE the static elements run, so a static
	// block or static field initializer can name the class — while a computed key,
	// evaluated above, still finds it in its temporal dead zone.
	if classNameSlot >= 0 {
		c.emitOpU16(OpGetLocal, uint16(ctorSlot))
		c.emitOpU16(OpPutLocal, uint16(classNameSlot))
	}

	// Static elements: field initializers and static blocks, in source order,
	// after every element's key has been evaluated. Each runs with `this` bound to
	// the constructor, so an arrow inside one captures the class.
	for _, se := range staticElements {
		m := se.m
		if m.Kind == NStaticBlock {
			body := &Node{Kind: NBlock, Args: m.Args}
			blockFn := &Node{Kind: NFunc, Body: body, Flags: fnClassBody}
			c.emitOpU16(OpGetLocal, uint16(ctorSlot)) // this
			c.pendingStaticSuper = true               // a static block's home is the constructor
			c.pendingClassDerived = n.Left != nil
			c.compileFunc(blockFn) // func
			c.emit(OpCallMethod)   // [this, func] -> result
			c.emitU16(0)
			c.emit(OpPop)
			continue
		}
		// A static field initializer is wrapped in a class-body function invoked
		// with the constructor as receiver, like a static block. NamedEvaluation
		// still names an anonymous value from the key.
		if se.keySlot < 0 {
			nameAnonExpr(m.Right, se.name)
		}
		initFn := &Node{Kind: NFunc, Flags: fnClassBody, Body: &Node{Kind: NBlock, Args: []*Node{{Kind: NReturn, Right: m.Right}}}}
		c.emitOpU16(OpGetLocal, uint16(ctorSlot)) // [ctor] (define target)
		if se.keySlot >= 0 {
			c.emitOpU16(OpGetLocal, uint16(se.keySlot)) // [ctor, key]
		}
		c.emitOpU16(OpGetLocal, uint16(ctorSlot)) // [… , this]
		c.pendingStaticSuper = true
		c.pendingClassDerived = n.Left != nil
		c.compileFunc(initFn) // [… , this, initFn]
		c.emit(OpCallMethod)  // [… , value]
		c.emitU16(0)
		if se.keySlot >= 0 {
			// CreateDataPropertyOrThrow on a computed key: a static field named
			// "prototype" is a TypeError rather than a silent failure.
			c.emit(OpDefineMethodComp)
			c.emitByte(3)
		} else {
			c.emitDefineField(se.name)
		}
		c.emit(OpPop)
	}

	// Create each instance private method's SHARED function exactly once, with its
	// [[HomeObject]] set to the prototype, and store it in the class-scope slot the
	// constructor captures. Installing this one function object on every instance
	// (in emitInstanceFieldInit) gives all instances the same private method — the
	// spec creates it once at class definition, not per construction.
	for _, m := range instanceFields {
		if !isInstancePrivateMethod(m) {
			continue
		}
		if m.Right.Str == "" {
			prefix := ""
			if m.Flags&fnGetter != 0 {
				prefix = "get "
			} else if m.Flags&fnSetter != 0 {
				prefix = "set "
			}
			m.Right.Str = prefix + m.Left.Str
			m.Right.Flags |= fnInferredName
		}
		c.emitOpU16(OpGetLocal, uint16(protoSlot)) // [proto]
		c.compileFunc(m.Right)                     // [proto, func]
		c.emit(OpSetHomeObj)                       // [proto, func] (func.[[HomeObject]] = proto)
		slot := c.resolveLocal(c.privMethodSlotName(m))
		c.emitOpU16(OpPutLocal, uint16(slot)) // [proto]
		c.emit(OpPop)                         // []
	}

	// Close the inner class-name scope (the methods have already captured the
	// binding, which was initialised before the static elements ran).
	if classNameSlot >= 0 {
		c.scopeDepth--
		c.popBlockScope()
	}

	// Every closure that belongs to this evaluation has been created; restore the
	// enclosing class's tag (a throw out of the body restores it via the handler).
	if ownPrivEnv {
		c.emit(OpSpecialObj)
		c.emitByte(5)
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

func (c *compiler) compileTaggedTemplate(n *Node, tail bool) {
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

	// tag(strings, ...substitutions). A member tag (obj.tag`...`) is a MemberCall,
	// so it is invoked with this = obj (OpCallMethod); a plain tag with this =
	// undefined (OpCall). In a return's tail position, tail is set and the frame is
	// reused via TAIL_CALL / TAIL_CALL_METHOD.
	member := n.Left
	isMember := member != nil && member.Kind == NMember &&
		!(member.Left != nil && member.Left.Kind == NIdent && member.Left.Str == "super")
	if isMember {
		c.compileExpr(member.Left) // [this]
		if member.Flags&1 != 0 {   // computed key
			c.emit(OpDup)
			c.compileExpr(member.Right)
			c.emit(OpGetElem)
		} else {
			c.emitFieldOp(OpGetField2, member.Right.Str) // [this, fn]
		}
	} else {
		c.compileExpr(n.Left) // [fn]
	}
	c.emitConst(cooked)
	nsubs := 0
	for i := 1; i < len(segs); i += 2 {
		c.compileExpr(segs[i])
		nsubs++
	}
	switch {
	case tail && isMember:
		c.emit(OpTailCallMethod)
	case tail:
		c.emit(OpTailCall)
	case isMember:
		c.emit(OpCallMethod)
	default:
		c.emit(OpCall)
	}
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
