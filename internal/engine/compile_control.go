package engine

// Control-flow compilation: loops (while/do-while/C-style for), break/continue
// (with a loop-context stack), switch, and try/catch (ant compiler.c
// compile_while/for/break/continue/switch/try). Labeled statements, for-in/of,
// and finally land as the port continues.

func (c *compiler) pushLoop(label string, isSwitch bool) *loopCtx {
	l := &loopCtx{continueTarget: -1, label: label, isSwitch: isSwitch, unwindDepth: len(c.unwindKinds)}
	c.loops = append(c.loops, l)
	return l
}

func (c *compiler) popLoop() {
	l := c.loops[len(c.loops)-1]
	c.loops = c.loops[:len(c.loops)-1]
	for _, off := range l.breaks {
		c.patchJump(off)
	}
}

// patchContinues resolves all continue jumps to the recorded continue target.
func (c *compiler) patchContinues(l *loopCtx) {
	for _, off := range l.continues {
		c.patchJumpTo(off, l.continueTarget)
	}
}

// patchJumpTo writes an explicit absolute target into a jump operand.
func (c *compiler) patchJumpTo(operandPos, target int) {
	t := uint32(target)
	c.fn.code[operandPos] = byte(t)
	c.fn.code[operandPos+1] = byte(t >> 8)
	c.fn.code[operandPos+2] = byte(t >> 16)
	c.fn.code[operandPos+3] = byte(t >> 24)
}

// compileLabel handles `label: stmt`. For a labelled loop the loop consumes the
// label (so `continue label` works); for other statements it pushes a break-only
// target so `break label` can exit.
func (c *compiler) compileLabel(n *Node) {
	// A label may not be nested inside another statement carrying the same label
	// (sibling labels reusing a name are fine — they aren't in scope together).
	if c.pendingLabel == n.Str {
		c.syntaxErrorf("Label '%s' has already been declared", n.Str)
		return
	}
	for _, l := range c.loops {
		if l.label == n.Str {
			c.syntaxErrorf("Label '%s' has already been declared", n.Str)
			return
		}
	}
	if isLoopNode(n.Body) {
		c.pendingLabel = n.Str
		c.compileStmt(n.Body)
		c.pendingLabel = ""
		return
	}
	l := c.pushLoop(n.Str, true) // break-only (switch-like) target
	c.compileStmt(n.Body)
	c.popLoop()
	_ = l
}

// resetCompletion seeds the script/eval completion slot with undefined. An
// if / switch / iteration statement's value is UpdateEmpty(inner, undefined), so
// it never carries a preceding statement's completion value; its compiler calls
// this before the body may set the slot. (A Block, by contrast, does carry the
// value, so it does not reset.)
func (c *compiler) resetCompletion() {
	if c.isScript {
		c.emit(OpUndef)
		c.emitOpU16(OpPutLocal, uint16(c.completionSlot))
	}
}

func (c *compiler) compileWhile(n *Node) {
	c.resetCompletion()
	l := c.pushLoop(c.consumeLabel(), false)
	loopStart := len(c.fn.code)
	l.continueTarget = loopStart
	c.compileExpr(n.Cond)
	exit := c.emitJump(OpJmpFalse)
	l.breaks = append(l.breaks, exit)
	c.compileStmt(n.Body)
	c.emit(OpJmp)
	c.emitU32(uint32(loopStart))
	c.patchContinues(l)
	c.popLoop()
}

func (c *compiler) compileDoWhile(n *Node) {
	c.resetCompletion()
	l := c.pushLoop(c.consumeLabel(), false)
	loopStart := len(c.fn.code)
	c.compileStmt(n.Body)
	l.continueTarget = len(c.fn.code)
	c.compileExpr(n.Cond)
	c.emit(OpJmpTrue)
	c.emitU32(uint32(loopStart))
	c.patchContinues(l)
	c.popLoop()
}

func (c *compiler) compileFor(n *Node) {
	// `for (using x = r; cond; update) body`: the head's resources bind once in the
	// for-statement's scope and dispose when the whole statement completes. Lower it
	// to `{ using x = r; for (; cond; update) body }` so the block's using-scope
	// drives disposal. A pending label flows through to the inner (real) loop, so
	// `continue`/`break label` still target it.
	if n.Init != nil && n.Init.Kind == NVar &&
		(n.Init.VarKind == VarUsing || n.Init.VarKind == VarAwaitUsing) {
		innerFor := &Node{Kind: NFor, Cond: n.Cond, Update: n.Update, Body: n.Body}
		block := &Node{Kind: NBlock, Args: []*Node{n.Init, innerFor}}
		c.compileStmt(block)
		return
	}
	c.checkForHeadDecl(n.Init, n.Body)
	c.resetCompletion()
	c.scopeDepth++
	// Initializer (a declaration or expression statement).
	lexSlot := -1
	if n.Init != nil {
		if n.Init.Kind == NVar {
			c.compileVarDecl(n.Init)
			// A single let/const init variable gets a fresh per-iteration binding.
			if (n.Init.VarKind == VarLet || n.Init.VarKind == VarConst) &&
				len(n.Init.Args) == 1 && n.Init.Args[0].Left != nil &&
				n.Init.Args[0].Left.Kind == NIdent {
				lexSlot = c.resolveLocal(n.Init.Args[0].Left.Str)
			}
		} else {
			c.compileExpr(n.Init)
			c.emit(OpPop)
		}
	}

	l := c.pushLoop(c.consumeLabel(), false)
	condStart := len(c.fn.code)
	var exit int
	hasExit := false
	if n.Cond != nil {
		c.compileExpr(n.Cond)
		exit = c.emitJump(OpJmpFalse)
		hasExit = true
		l.breaks = append(l.breaks, exit)
	}

	c.compileStmt(n.Body)

	// Continue jumps target the update clause.
	l.continueTarget = len(c.fn.code)
	c.patchContinues(l)
	// Per-iteration lexical binding: close the loop var's captures before the
	// update mutates it for the next iteration.
	if lexSlot >= 0 {
		c.emitOpU16(OpCloseUpval, uint16(lexSlot))
	}
	if n.Update != nil {
		c.compileExpr(n.Update)
		c.emit(OpPop)
	}
	c.emit(OpJmp)
	c.emitU32(uint32(condStart))

	c.popLoop()
	_ = hasExit
	c.scopeDepth--
	c.popBlockScope()
}

// compileForIn lowers `for (v in obj)` to iteration over the enumerable-keys
// array produced by FOR_IN.
func (c *compiler) compileForIn(n *Node) {
	c.checkForHeadDecl(n.Left, n.Body)
	c.compileForArray(n, OpForIn)
}

// compileForOf lowers `for (v of iter)` to a lazy loop over a live iterator:
// each step calls iter.next() and, on an abrupt `break`, closes the iterator via
// its return() (IteratorClose). Normal exhaustion does not close (already done).
func (c *compiler) compileForOf(n *Node) {
	c.checkForHeadDecl(n.Left, n.Body)
	c.resetCompletion()
	// `for (using x of iter)` disposes each element at the end of its iteration.
	// Rewrite to `for (const $tmp of iter) { using x = $tmp; body }` so the body's
	// using-block drives the per-iteration disposal.
	if n.Left != nil && n.Left.Kind == NVar &&
		(n.Left.VarKind == VarUsing || n.Left.VarKind == VarAwaitUsing) &&
		len(n.Left.Args) == 1 && n.Left.Args[0].Left != nil {
		// The declared name stays the LOOP variable, so it keeps the per-iteration
		// fresh binding (and the head's temporal dead zone) that a `const` head
		// already provides; a hidden `using` binding beside it drives the disposal.
		// Binding the name in the body instead would give every iteration's closure
		// the same cell.
		binding := n.Left.Args[0].Left
		loopVar := &Node{Kind: NVar, VarKind: VarConst, Args: []*Node{{Kind: NVarDecl, Left: binding}}}
		usingVar := &Node{Kind: NVar, VarKind: n.Left.VarKind, Args: []*Node{{Kind: NVarDecl,
			Left: &Node{Kind: NIdent, Str: "*forusing*"}, Right: &Node{Kind: NIdent, Str: binding.Str}}}}
		body := n.Body
		if body == nil {
			body = &Node{Kind: NEmpty}
		}
		newBody := &Node{Kind: NBlock, Args: []*Node{usingVar, body}}
		c.compileForOf(&Node{Kind: NForOf, Left: loopVar, Right: n.Right, Body: newBody})
		return
	}
	c.scopeDepth++
	store, lexSlots := c.forInStore(n.Left)
	if store == nil {
		// The head's LeftHandSideExpression is not a simple assignment target
		// (e.g. `for (this of [])`, `for (f() of [])`) — an early SyntaxError.
		c.syntaxErrorf("Invalid left-hand side in for-loop")
		c.scopeDepth--
		return
	}
	// The let/const head binding(s) are in their temporal dead zone while the
	// iterable expression is evaluated; seed with the hole, then detach so the
	// per-iteration bindings use fresh cells (see compileForArray).
	c.seedForHeadHoles(lexSlots)
	c.compileExpr(n.Right)
	c.closeForHeadBindings(lexSlots)
	c.emit(OpIterCall) // source -> iterator
	c.emitByte(0)      // Size-2 opcode: unused inline operand
	iterSlot := c.addLocal("*foi*", false)
	c.emitOpU16(OpPutLocal, uint16(iterSlot))
	// GetIterator reads `next` exactly once (it is part of the Iterator Record);
	// cache it so a later iteration cannot observe a re-read of iterator.next.
	nextSlot := c.addLocal("*fon*", false)
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emitFieldOp(OpGetField, "next")
	c.emitOpU16(OpPutLocal, uint16(nextSlot))
	resSlot := c.addLocal("*for*", false)
	// needsClose gates IteratorClose. It is cleared while the iterator is being
	// stepped — a throw from next(), a done/value getter, etc. must NOT close the
	// iterator (only an abrupt completion of the binding or body does) — and set
	// once a step has fully succeeded, so break/return/throw/labelled-jump out of
	// the body close, while normal exhaustion (done === true) does not.
	closeSlot := c.addLocal("*foc*", false)

	// A try-FINALLY wraps the loop so every abrupt completion routes through the
	// finally via the interpreter's unwind machinery (doReturn / doJump / throw
	// unwind), which then performs IteratorClose when needsClose is still set.
	// This covers `return` and a labelled break/continue leaving the loop, not
	// just the plain break handled by the inline landing below.
	tryJump := c.emitJump(OpTryPushFinally)
	c.unwindPush(unwTryFinally)

	l := c.pushLoop(c.consumeLabel(), false)
	condStart := len(c.fn.code)
	// needsClose = false: stepping the iterator (next(), the done check, reading
	// value) is not covered — those throwing propagates without a close.
	c.emit(OpFalse)
	c.emitOpU16(OpPutLocal, uint16(closeSlot))
	// result = Call(nextMethod, iter) — the cached next, receiver = the iterator.
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emitOpU16(OpGetLocal, uint16(nextSlot)) // [iter, next]
	c.emit(OpCallMethod)
	c.emitU16(0) // [result]
	c.emitOpU16(OpPutLocal, uint16(resSlot))
	// IteratorNext: the result must be an Object, else a TypeError (otherwise a
	// `next()` returning a primitive would read undefined `done`/`value` forever).
	// ToObject returns an Object unchanged, so identity with the original is an
	// allocation-free "is an Object" test; null/undefined throw inside ToObject.
	c.emitOpU16(OpGetLocal, uint16(resSlot))
	c.emit(OpDup)
	c.emit(OpToObject)
	c.emit(OpSeq)
	resOK := c.emitJump(OpJmpTrue)
	c.emit(OpThrowError)
	c.emitU32(uint32(c.constant(c.rt.internString("iterator result is not an object"))))
	c.emitByte(0) // TypeError
	c.patchJump(resOK)
	// if result.done: exit without closing (iterator already exhausted)
	c.emitOpU16(OpGetLocal, uint16(resSlot))
	c.emitFieldOp(OpGetField, "done")
	exhausted := c.emitJump(OpJmpTrue)
	// v = result.value
	c.emitOpU16(OpGetLocal, uint16(resSlot))
	c.emitFieldOp(OpGetField, "value") // [value]
	// The step succeeded: the binding and body now run under close protection.
	c.emit(OpTrue)
	c.emitOpU16(OpPutLocal, uint16(closeSlot)) // [value] (net-neutral)
	store()

	c.compileStmt(n.Body)

	l.continueTarget = len(c.fn.code)
	c.patchContinues(l)
	c.closeForHeadBindings(lexSlots)
	c.emit(OpJmp)
	c.emitU32(uint32(condStart))

	// Normal exhaustion (done === true): needsClose is already false from the top
	// of the iteration, so the shared teardown below skips the close.
	c.patchJump(exhausted)

	// A plain break targeting this loop lands here (needsClose still set): pop
	// the finally handler and fall into the finally, which closes the iterator.
	c.popLoop()
	c.emit(OpTryPop)
	c.unwindPop()

	// The finally block — entered on the normal/break fall-through and, via the
	// unwind machinery, on throw/return/labelled-jump. Close the iterator when
	// needsClose is set, then resume the pending completion (OpFinallyRet).
	c.patchJump(tryJump)
	finallyJump := c.emitJump(OpFinally)
	c.unwindPush(unwFinallyBody)
	c.emitOpU16(OpGetLocal, uint16(closeSlot))
	skipClose := c.emitJump(OpJmpFalse)
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emit(OpIterClose)
	c.patchJump(skipClose)
	c.unwindPop()
	c.emit(OpFinallyRet)
	c.patchJump(finallyJump)

	c.scopeDepth--
	c.popBlockScope()
}

// compileForAwaitOf lowers `for await (v of src)` to a lazy loop that awaits
// each iter.next() result (GetAsyncIterator + repeated await). Only valid inside
// an async function/generator (OpAwait suspends the coroutine).
func (c *compiler) compileForAwaitOf(n *Node) {
	c.checkForHeadDecl(n.Left, n.Body)
	c.resetCompletion()
	c.scopeDepth++
	store, lexSlots := c.forInStore(n.Left)
	if store == nil {
		c.syntaxErrorf("Invalid left-hand side in for-loop")
		c.scopeDepth--
		return
	}
	// iter = GetAsyncIterator(source); the head binding(s) are in TDZ during it.
	c.seedForHeadHoles(lexSlots)
	c.compileExpr(n.Right)
	c.closeForHeadBindings(lexSlots)
	c.emit(OpForAwaitOf) // source -> asyncIter
	iterSlot := c.addLocal("*fai*", false)
	c.emitOpU16(OpPutLocal, uint16(iterSlot))
	// GetAsyncIterator reads `next` once (part of the Iterator Record); cache it.
	nextSlot := c.addLocal("*fan*", false)
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emitFieldOp(OpGetField, "next")
	c.emitOpU16(OpPutLocal, uint16(nextSlot))
	resSlot := c.addLocal("*far*", false)
	// needsClose flag: an abrupt completion (break/return/throw) out of the body
	// closes the iterator; stepping (await next(), the done/value reads) and normal
	// exhaustion do not. Mirrors compileForOf, with an async close in the finally.
	closeSlot := c.addLocal("*fac*", false)

	tryJump := c.emitJump(OpTryPushFinally)
	c.unwindPush(unwTryFinally)

	l := c.pushLoop(c.consumeLabel(), false)
	condStart := len(c.fn.code)
	c.emit(OpFalse)
	c.emitOpU16(OpPutLocal, uint16(closeSlot))
	// result = await Call(nextMethod, iter) — the cached next.
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emitOpU16(OpGetLocal, uint16(nextSlot)) // [iter, next]
	c.emit(OpCallMethod)
	c.emitU16(0)    // [promise]
	c.emit(OpAwait) // [result]
	c.emitOpU16(OpPutLocal, uint16(resSlot))
	// if result.done: exit without closing (already exhausted)
	c.emitOpU16(OpGetLocal, uint16(resSlot))
	c.emitFieldOp(OpGetField, "done")
	exhausted := c.emitJump(OpJmpTrue)
	// v = result.value
	c.emitOpU16(OpGetLocal, uint16(resSlot))
	c.emitFieldOp(OpGetField, "value")
	// The step succeeded: the binding and body now run under close protection.
	c.emit(OpTrue)
	c.emitOpU16(OpPutLocal, uint16(closeSlot))
	store()

	c.compileStmt(n.Body)

	l.continueTarget = len(c.fn.code)
	c.patchContinues(l)
	c.closeForHeadBindings(lexSlots)
	c.emit(OpJmp)
	c.emitU32(uint32(condStart))

	// Normal exhaustion and a plain break both land here; closeSlot decides.
	c.patchJump(exhausted)
	c.popLoop()
	c.emit(OpTryPop)
	c.unwindPop()

	// finally: close the iterator when needsClose is set, then resume the pending
	// completion. Entered on normal/break fall-through and, via unwind, on
	// throw/return/labelled-jump. OpIterClose (as for the sync for-of) invokes
	// return() and — crucially — suppresses its own abrupt result when the pending
	// completion is itself a throw, so a poisoned return does not mask the thrown
	// error. (It does not Await the async return()'s promise; a rejection from that
	// promise is not surfaced — a minor deviation from AsyncIteratorClose.)
	c.patchJump(tryJump)
	finallyJump := c.emitJump(OpFinally)
	c.unwindPush(unwFinallyBody)
	c.emitOpU16(OpGetLocal, uint16(closeSlot))
	skipClose := c.emitJump(OpJmpFalse)
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emit(OpIterClose)
	c.patchJump(skipClose)
	c.unwindPop()
	c.emit(OpFinallyRet)
	c.patchJump(finallyJump)

	c.scopeDepth--
	c.popBlockScope()
}

// compileForArray implements the shared for-in/for-of lowering: produceOp turns
// the source into an array (keys or values), which is then iterated by index.
func (c *compiler) compileForArray(n *Node, produceOp Opcode) {
	c.resetCompletion()
	c.scopeDepth++
	store, lexSlots := c.forInStore(n.Left)
	if store == nil {
		c.syntaxErrorf("Invalid left-hand side in for-loop")
		c.scopeDepth--
		return
	}

	// A let/const for-head binding is in its temporal dead zone while the source
	// expression is evaluated (the head creates a new lexical scope), so seed it
	// with the EMPTY hole first — a closure in the RHS that reads it then throws.
	c.seedForHeadHoles(lexSlots)

	// Annex B.3.6: a `var` loop variable may carry an initializer in a for-in head
	// (non-strict — the parser rejects it in strict mode). Evaluate it once, before
	// the object expression, and seed the loop variable with it.
	if produceOp == OpForIn {
		if init := forInVarInitializer(n.Left); init != nil {
			c.compileExpr(init)
			store()
		}
	}

	c.compileExpr(n.Right)
	// Detach the head binding that a closure in the RHS captured (in its dead
	// zone) so the per-iteration assignments below use a fresh cell — the
	// RHS-scope binding stays permanently uninitialized, so such a closure throws.
	c.closeForHeadBindings(lexSlots)
	c.emit(produceOp) // source -> keys/values array
	keysSlot := c.addLocal("*fik*", false)
	c.emitOpU16(OpPutLocal, uint16(keysSlot))
	iSlot := c.addLocal("*fii*", false)
	c.emit(OpConstI8)
	c.emitByte(0)
	c.emitOpU16(OpPutLocal, uint16(iSlot))

	l := c.pushLoop(c.consumeLabel(), false)
	condStart := len(c.fn.code)
	// if !(i < keys.length) break
	c.emitOpU16(OpGetLocal, uint16(iSlot))
	c.emitOpU16(OpGetLocal, uint16(keysSlot))
	c.emit(OpGetLength)
	c.emit(OpLt)
	exit := c.emitJump(OpJmpFalse)
	l.breaks = append(l.breaks, exit)

	// v = keys[i]
	c.emitOpU16(OpGetLocal, uint16(keysSlot))
	c.emitOpU16(OpGetLocal, uint16(iSlot))
	c.emit(OpGetElem)
	store()

	c.compileStmt(n.Body)

	// continue target: i++
	l.continueTarget = len(c.fn.code)
	c.patchContinues(l)
	// Per-iteration lexical binding: close any closure capture of the loop var
	// before the next iteration reuses its slot.
	c.closeForHeadBindings(lexSlots)
	c.emitOpU16(OpGetLocal, uint16(iSlot))
	c.emit(OpInc)
	c.emitOpU16(OpPutLocal, uint16(iSlot))
	c.emit(OpJmp)
	c.emitU32(uint32(condStart))

	c.popLoop()
	c.scopeDepth--
	c.popBlockScope()
}

// forInStore returns a closure that stores the top-of-stack value into the
// for-in loop binding (declaring it if it is a fresh let/const/local var). The
// second result is the loop variable's local slot when it is lexically block-
// scoped (let/const), so the caller can close per-iteration captures; -1 else.
// forInVarInitializer returns the initializer of a single simple `var`
// declarator in a for-in head (`for (var i = 0 in obj)`), or nil. Only var
// bindings with a plain identifier target qualify (Annex B legacy syntax).
func forInVarInitializer(left *Node) *Node {
	if left != nil && left.Kind == NVar && left.VarKind == VarVar &&
		len(left.Args) == 1 && left.Args[0] != nil {
		decl := left.Args[0]
		if decl.Left != nil && decl.Left.Kind == NIdent {
			return decl.Right
		}
	}
	return nil
}

// checkForHeadDecl enforces the early errors of a for/for-in/for-of head that
// is a lexical (let/const/using/await-using) declaration: its BoundNames must be
// distinct, may not include "let", and may not also be var-declared anywhere in
// the loop body.
func (c *compiler) checkForHeadDecl(head, body *Node) {
	if head == nil || head.Kind != NVar ||
		(head.VarKind != VarLet && head.VarKind != VarConst &&
			head.VarKind != VarUsing && head.VarKind != VarAwaitUsing) {
		return
	}
	var names []string
	for _, d := range head.Args {
		if d != nil {
			collectPatternNames(d.Left, &names)
		}
	}
	seen := map[string]bool{}
	for _, nm := range names {
		if nm == "let" {
			c.syntaxErrorf("let is disallowed as a lexically bound name")
			return
		}
		if seen[nm] {
			c.syntaxErrorf("Identifier '%s' has already been declared", nm)
			return
		}
		seen[nm] = true
	}
	bodyVars := map[string]bool{}
	collectBodyVarNames(body, bodyVars)
	for _, nm := range names {
		if bodyVars[nm] {
			c.syntaxErrorf("Identifier '%s' has already been declared", nm)
			return
		}
	}
}

// seedForHeadHoles seeds each loop-head lexical binding with the EMPTY (TDZ)
// hole: the head bindings are dead while the iterable/enumerated expression is
// evaluated (a closure created there that reads one throws a ReferenceError).
func (c *compiler) seedForHeadHoles(slots []int) {
	for _, s := range slots {
		c.emit(OpEmpty)
		c.emitOpU16(OpPutLocal, uint16(s))
	}
}

// closeForHeadBindings detaches the loop-head lexical bindings so each new
// iteration (and the iterable evaluation) captures fresh cells.
func (c *compiler) closeForHeadBindings(slots []int) {
	for _, s := range slots {
		c.emitOpU16(OpCloseUpval, uint16(s))
	}
}

// forInStore returns a closure that stores the current element/key into the
// loop head, and the lexical binding slots (if any) that need per-iteration TDZ
// seeding and fresh-cell detachment. A nil slice means no lexical head bindings.
func (c *compiler) forInStore(left *Node) (func(), []int) {
	var name string
	switch {
	case left.Kind == NVar && len(left.Args) == 1 && left.Args[0].Left != nil:
		binding := left.Args[0].Left
		// Destructuring loop variable: for (var/let/const [k,v] of …).
		if binding.Kind == NArray || binding.Kind == NObject {
			kind := left.VarKind
			if kind == VarLet || kind == VarConst {
				// Pre-declare the pattern's bound names as lexicals so they get the
				// TDZ hole while the iterable is evaluated and a fresh per-iteration
				// binding. destructureTarget reuses these slots (bindDeclName's
				// lexicalAtCurrentDepth), storing into them rather than redeclaring.
				var names []string
				collectPatternNames(binding, &names)
				var slots []int
				for _, nm := range names {
					slots = append(slots, c.declareLexical(nm, kind == VarConst))
				}
				return func() { c.destructureTarget(binding, kind) }, slots
			}
			return func() { c.destructureTarget(binding, kind) }, nil
		}
		if binding.Kind != NIdent {
			return nil, nil
		}
		name = binding.Str
		// A `var` head binds globally only at true script top level; eval `var`
		// bindings stay frame-local (matching compileVarDecl's asGlobal), so
		// `eval("for (var a in obj) …")` in strict code does not route the store
		// through an unresolvable global assignment.
		if !(left.VarKind == VarVar && c.isScript && !c.isEval) {
			var slot int
			var lexSlots []int
			if left.VarKind == VarLet || left.VarKind == VarConst {
				slot = c.declareLexical(name, left.VarKind == VarConst)
				lexSlots = []int{slot}
			} else {
				slot = c.declareVar(name, false)
			}
			return func() { c.emitOpU16(OpPutLocal, uint16(slot)) }, lexSlots
		}
	case left.Kind == NIdent:
		// A private name is not a valid for-in/of assignment target.
		if len(left.Str) > 0 && left.Str[0] == '#' {
			c.syntaxErrorf("Private field '%s' may not be a for-in/of target", left.Str)
			return nil, nil
		}
		name = left.Str
	case left.Kind == NArray || left.Kind == NObject || left.Kind == NMember:
		// Assignment target with no declaration: for ([a,b] of …), for ({x} of …),
		// for (obj.p of …). The head is a destructuring/member assignment to
		// existing references, evaluated fresh each iteration.
		return func() { c.destructureTarget(left, varAssign) }, nil
	case left.Kind == NCall && !c.fn.isStrict:
		// Annex B web-compat: a CallExpression for-in/of head is a runtime
		// ReferenceError. Each iteration discards the iteration value, evaluates the
		// call for its side effects, and throws before the value is bound/coerced.
		return func() {
			c.emit(OpPop)       // discard the iteration value
			c.compileExpr(left) // evaluate the call
			c.emit(OpPop)
			idx := c.constant(c.rt.internString("Invalid assignment target"))
			c.emit(OpThrowError)
			c.emitU32(uint32(idx))
			c.emitByte(1) // ReferenceError
		}, nil
	default:
		return nil, nil
	}
	if slot := c.resolveLocal(name); slot >= 0 {
		return func() { c.emitOpU16(OpPutLocal, uint16(slot)) }, nil
	}
	if uv := c.resolveUpvalue(name); uv >= 0 {
		return func() { c.emitOpU16(OpPutUpval, uint16(uv)) }, nil
	}
	return func() { c.emitGlobalPut(name) }, nil
}

func (c *compiler) compileBreak(n *Node) {
	l := c.targetLoop(n.Str)
	if l == nil {
		c.syntaxErrorf("Illegal break statement")
		return
	}
	off := c.emitLoopExitJump(l)
	l.breaks = append(l.breaks, off)
}

func (c *compiler) compileContinue(n *Node) {
	l := c.targetLoopForContinue(n.Str)
	if l == nil {
		c.syntaxErrorf("Illegal continue statement")
		return
	}
	off := c.emitLoopExitJump(l)
	l.continues = append(l.continues, off)
}

// emitLoopExitJump emits the jump for a break/continue targeting loop l, running
// any `finally` scopes and popping any plain try/finally handlers the jump exits
// (ant emit_loop_exit_jump). When no finally intervenes it drops handlers with
// TRY_POP / FINALLY_DISCARD then a plain JMP; otherwise it emits UNWIND_JMP so the
// interpreter runs the finallies before landing at the target. Returns the operand
// offset of the branch to patch to the loop exit / continue target.
func (c *compiler) emitLoopExitJump(l *loopCtx) int {
	nPop := len(c.unwindKinds) - l.unwindDepth
	nFin := 0
	for i := len(c.unwindKinds) - 1; i >= l.unwindDepth; i-- {
		if c.unwindKinds[i] == unwTryFinally {
			nFin++
		}
	}
	if nPop <= 0 {
		return c.emitJump(OpJmp)
	}
	if nFin == 0 {
		for i := len(c.unwindKinds) - 1; i >= l.unwindDepth; i-- {
			if c.unwindKinds[i] == unwFinallyBody {
				c.emit(OpFinallyDiscard)
			} else {
				c.emit(OpTryPop)
			}
		}
		return c.emitJump(OpJmp)
	}
	if nFin > 255 {
		nFin = 255
	}
	if nPop > 255 {
		nPop = 255
	}
	off := c.emitJump(OpUnwindJmp)
	c.emitByte(byte(nFin))
	c.emitByte(byte(nPop))
	return off
}

// targetLoop returns the innermost enclosing loop/switch (or the one matching a
// label) for break.
func (c *compiler) targetLoop(label string) *loopCtx {
	for i := len(c.loops) - 1; i >= 0; i-- {
		if label == "" || c.loops[i].label == label {
			return c.loops[i]
		}
	}
	return nil
}

// targetLoopForContinue skips switch contexts (continue only targets loops).
func (c *compiler) targetLoopForContinue(label string) *loopCtx {
	for i := len(c.loops) - 1; i >= 0; i-- {
		if c.loops[i].isSwitch {
			continue
		}
		if label == "" || c.loops[i].label == label {
			return c.loops[i]
		}
	}
	return nil
}

func (c *compiler) compileSwitch(n *Node) {
	// Evaluate discriminant into a temp local, then chain strict-equality tests.
	c.compileExpr(n.Cond)
	discSlot := c.addLocal("*switch*", false)
	c.emitOpU16(OpPutLocal, uint16(discSlot))

	// A switch statement's value is UpdateEmpty(CaseBlockEvaluation, undefined):
	// it never carries a preceding statement's completion value (`1; switch(x){}`
	// completes with undefined, not 1).
	c.resetCompletion()

	// The clauses of a switch share a single lexical (CaseBlock) scope, so their
	// let/const/class bindings are hoisted together — a name declared in two
	// clauses is a duplicate (early SyntaxError), and TDZ spans the whole block.
	c.scopeDepth++
	var caseStmts []*Node
	for _, cas := range n.Args {
		caseStmts = append(caseStmts, cas.Args...)
	}
	c.checkBlockDeclConflicts(caseStmts, true)
	c.hoistLexicals(caseStmts)
	// A CaseBlock is a block scope: its FunctionDeclarations hoist like any other
	// block-level function (lexical binding, plus the Annex B.3.3 var update).
	c.hoistFunctions(caseStmts, true)

	l := c.pushLoop("", true)

	var caseJumps []int
	defaultIdx := -1
	for i, cas := range n.Args {
		if cas.Left == nil { // default
			defaultIdx = i
			caseJumps = append(caseJumps, -1)
			continue
		}
		c.emitOpU16(OpGetLocal, uint16(discSlot))
		c.compileExpr(cas.Left)
		c.emit(OpSeq)
		caseJumps = append(caseJumps, c.emitJump(OpJmpTrue))
	}

	// No case matched → default (or exit).
	afterTests := c.emitJump(OpJmp)

	// Emit case bodies in order, patching the matching test jump to each.
	bodyStarts := make([]int, len(n.Args))
	for i, cas := range n.Args {
		bodyStarts[i] = len(c.fn.code)
		if caseJumps[i] >= 0 {
			c.patchJump(caseJumps[i])
		}
		for _, s := range cas.Args {
			c.compileStmt(s)
		}
	}
	end := len(c.fn.code)

	// Wire the "no match" jump to the default body (or the end).
	if defaultIdx >= 0 {
		c.patchJumpTo(afterTests, bodyStarts[defaultIdx])
	} else {
		c.patchJumpTo(afterTests, end)
	}

	c.popLoop() // patches breaks to `end`
	c.scopeDepth--
	c.popBlockScope()
	_ = l
}

// compileCatchBody binds the catch parameter (if any) and compiles the catch
// body. On entry the thrown value is on the stack (pushed by OP_CATCH).
// checkCatchParamConflict enforces the catch-clause early errors: a catch
// parameter name may not also be lexically (let/const) declared in the catch
// body, nor var-declared there unless the parameter is a single identifier
// (Annex B.3.4 allows `catch(e){ var e }` but not `catch({e}){ var e }`).
func (c *compiler) checkCatchParamConflict(n *Node) {
	if n.CatchParam == nil || n.CatchBody == nil || n.CatchBody.Kind != NBlock {
		return
	}
	var paramNames []string
	collectPatternNames(n.CatchParam, &paramNames)
	if len(paramNames) == 0 {
		return
	}
	pset := map[string]bool{}
	for _, nm := range paramNames {
		// BoundNames of a CatchParameter must be unique: `catch ([x, x])` /
		// `catch ({a: x, b: x})` is an early SyntaxError.
		if pset[nm] {
			c.syntaxErrorf("Duplicate binding '%s' in catch parameter", nm)
			return
		}
		pset[nm] = true
	}
	isPattern := n.CatchParam.Kind == NArray || n.CatchParam.Kind == NObject
	for _, stmt := range n.CatchBody.Args {
		if stmt == nil {
			continue
		}
		// A directly-nested FunctionDeclaration is a LexicallyDeclaredName of the
		// catch Block; if its name is a catch-parameter bound name it is a
		// redeclaration (Annex B relaxes only `var`, not functions).
		if stmt.Kind == NFunc && stmt.Flags&fnArrow == 0 && stmt.Str != "" && pset[stmt.Str] {
			c.syntaxErrorf("Identifier '%s' has already been declared", stmt.Str)
			return
		}
		if stmt.Kind != NVar {
			continue
		}
		lexical := stmt.VarKind == VarLet || stmt.VarKind == VarConst
		if !lexical && !(stmt.VarKind == VarVar && isPattern) {
			continue
		}
		for _, decl := range stmt.Args {
			var names []string
			collectPatternNames(decl.Left, &names)
			for _, nm := range names {
				if pset[nm] {
					c.syntaxErrorf("Identifier '%s' has already been declared", nm)
					return
				}
			}
		}
	}
}

func (c *compiler) compileCatchBody(n *Node) {
	c.checkCatchParamConflict(n)
	if n.CatchParam != nil && n.CatchParam.Kind == NIdent {
		c.scopeDepth++
		// A CatchParameter is lexically scoped and must shadow a same-named outer
		// binding rather than alias it (declareVar reuses an in-scope slot, leaking
		// writes to the outer binding). An Annex B `var e` inside still reuses this
		// slot, since declareVar resolves to this lexical binding.
		slot := c.declareLexical(n.CatchParam.Str, false)
		// A simple identifier catch parameter may coexist with a same-named var
		// (Annex B.3.4), so it does NOT trigger the B.3.3/B.3.4 var-hoisting skip.
		c.locals[slot].catchParam = true
		c.emitOpU16(OpPutLocal, uint16(slot))
		c.compileStmt(n.CatchBody)
		c.scopeDepth--
		c.popBlockScope()
	} else if n.CatchParam != nil && (n.CatchParam.Kind == NArray || n.CatchParam.Kind == NObject) {
		// Destructuring catch binding: bind the pattern from the thrown value.
		c.scopeDepth++
		// Pre-declare each bound name as a fresh block-scoped binding so the pattern
		// shadows (not aliases) a same-named outer binding; destructureTarget then
		// reuses these current-depth slots. (A destructuring catch parameter is not
		// eligible for the Annex B.3.4 var coexistence, so it is not catchParam-flagged
		// — it correctly participates in the B.3.3/B.3.4 var-hoisting shadow check.)
		var names []string
		collectPatternNames(n.CatchParam, &names)
		for _, nm := range names {
			c.declareLexical(nm, false)
		}
		c.destructureTarget(n.CatchParam, VarLet)
		c.compileStmt(n.CatchBody)
		c.scopeDepth--
		c.popBlockScope()
	} else {
		c.emit(OpPop) // discard thrown value (optional binding)
		c.compileStmt(n.CatchBody)
	}
}

// compileFinallyBlock emits the finally body as a subroutine: OP_FINALLY marks it
// (installing a finally handler whose landing is the code after OP_FINALLY_RET),
// the body runs, and OP_FINALLY_RET resumes the pending completion (normal / throw
// / return / break-continue) recorded when the block was entered.
func (c *compiler) compileFinallyBlock(body *Node) {
	finallyJump := c.emitJump(OpFinally)
	c.unwindPush(unwFinallyBody)
	// A finally that completes normally does not contribute its own completion
	// value: `try Block Finally` yields Block's value (step 3, "let F be B"), and
	// `try Block Catch Finally` yields the Block/Catch value. Preserve the
	// completion value across the finally body so the finally's statements don't
	// clobber it (script/eval completion only). An abrupt finally is resumed via
	// OpFinallyRet with its own value, so the restore is harmless there.
	saveSlot := -1
	if c.isScript {
		saveSlot = c.addLocal("*fincmp*", false)
		c.emitOpU16(OpGetLocal, uint16(c.completionSlot))
		c.emitOpU16(OpPutLocal, uint16(saveSlot))
	}
	// The finally Block is in tail position when the TryStatement is: whatever
	// completion it was entered with, a `return f()` here replaces it and leaves
	// the function, so it is a proper tail call. (A tail call from an inner try's
	// body is still excluded — tryDepth only drops back to the enclosing level.)
	c.tryDepth--
	c.compileStmt(body)
	c.tryDepth++
	if saveSlot >= 0 {
		c.emitOpU16(OpGetLocal, uint16(saveSlot))
		c.emitOpU16(OpPutLocal, uint16(c.completionSlot))
	}
	c.unwindPop()
	c.emit(OpFinallyRet)
	c.patchJump(finallyJump)
}

func (c *compiler) compileTry(n *Node) {
	c.resetCompletion()
	c.tryDepth++
	defer func() { c.tryDepth-- }()

	hasCatch := n.CatchBody != nil
	hasFinally := n.FinallyBody != nil

	// The outer handler protects the try (and catch) body. With a finally it is a
	// TRY_PUSH_FINALLY so abrupt completions route through the finally.
	var tryJump int
	if hasFinally {
		tryJump = c.emitJump(OpTryPushFinally)
		c.unwindPush(unwTryFinally)
	} else {
		tryJump = c.emitJump(OpTryPush)
		c.unwindPush(unwTryCatch)
	}

	if hasCatch && hasFinally {
		// A separate inner TRY_PUSH catches into the catch body; the outer
		// TRY_PUSH_FINALLY then still runs the finally on the way out.
		innerJump := c.emitJump(OpTryPush)
		c.unwindPush(unwTryCatch)
		c.compileStmt(n.Body)
		c.emit(OpTryPop)
		c.unwindPop()
		innerEnd := c.emitJump(OpJmp)
		c.patchJump(innerJump)
		catchTag := c.emitJump(OpCatch)
		c.compileCatchBody(n)
		c.patchJump(catchTag)
		c.patchJump(innerEnd)
	} else {
		c.compileStmt(n.Body)
	}

	c.emit(OpTryPop)
	c.unwindPop()

	if !hasFinally {
		endJump := c.emitJump(OpJmp)
		c.patchJump(tryJump)
		catchTag := c.emitJump(OpCatch)
		if hasCatch {
			// With no finally, the try statement is over once the catch body runs, so
			// its Block is in tail position: `catch (e) { return f() }` is a proper
			// tail call. (With a finally it is not — that block still has to run.)
			c.tryDepth--
			c.compileCatchBody(n)
			c.tryDepth++
		} else {
			c.emit(OpPop)
		}
		c.patchJump(catchTag)
		c.patchJump(endJump)
		return
	}

	// Finally: the normal path falls through into OP_FINALLY; abrupt completions
	// (throw/return/break/continue) land here too via the TRY_PUSH_FINALLY handler,
	// with the completion recorded so OP_FINALLY_RET can resume it.
	c.patchJump(tryJump)
	c.compileFinallyBlock(n.FinallyBody)
}
