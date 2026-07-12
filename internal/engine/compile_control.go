package engine

// Control-flow compilation: loops (while/do-while/C-style for), break/continue
// (with a loop-context stack), switch, and try/catch (ant compiler.c
// compile_while/for/break/continue/switch/try). Labeled statements, for-in/of,
// and finally land as the port continues.

func (c *compiler) pushLoop(label string, isSwitch bool) *loopCtx {
	l := &loopCtx{continueTarget: -1, label: label, isSwitch: isSwitch}
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

func (c *compiler) compileWhile(n *Node) {
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
func (c *compiler) compileForIn(n *Node) { c.compileForArray(n, OpForIn) }

// compileForOf lowers `for (v of iter)` to a lazy loop over a live iterator:
// each step calls iter.next() and, on an abrupt `break`, closes the iterator via
// its return() (IteratorClose). Normal exhaustion does not close (already done).
func (c *compiler) compileForOf(n *Node) {
	// `for (using x of iter)` disposes each element at the end of its iteration.
	// Rewrite to `for (const $tmp of iter) { using x = $tmp; body }` so the body's
	// using-block drives the per-iteration disposal.
	if n.Left != nil && n.Left.Kind == NVar &&
		(n.Left.VarKind == VarUsing || n.Left.VarKind == VarAwaitUsing) &&
		len(n.Left.Args) == 1 && n.Left.Args[0].Left != nil {
		binding := n.Left.Args[0].Left
		loopVar := &Node{Kind: NVar, VarKind: VarLet, Args: []*Node{{Kind: NVarDecl, Left: &Node{Kind: NIdent, Str: "*forusing*"}}}}
		usingVar := &Node{Kind: NVar, VarKind: n.Left.VarKind, Args: []*Node{{Kind: NVarDecl, Left: binding, Right: &Node{Kind: NIdent, Str: "*forusing*"}}}}
		body := n.Body
		if body == nil {
			body = &Node{Kind: NEmpty}
		}
		newBody := &Node{Kind: NBlock, Args: []*Node{usingVar, body}}
		c.compileForOf(&Node{Kind: NForOf, Left: loopVar, Right: n.Right, Body: newBody})
		return
	}
	c.scopeDepth++
	store, lexSlot := c.forInStore(n.Left)
	if store == nil {
		c.errorf("unsupported for-of target (slice)")
		c.scopeDepth--
		return
	}
	c.compileExpr(n.Right)
	c.emit(OpIterCall) // source -> iterator
	c.emitByte(0)      // Size-2 opcode: unused inline operand
	iterSlot := c.addLocal("*foi*", false)
	c.emitOpU16(OpPutLocal, uint16(iterSlot))
	resSlot := c.addLocal("*for*", false)

	// A try-handler covers the loop so a throw in next()/the body closes the
	// iterator before propagating.
	catchHandler := c.emitJump(OpTryPush)

	l := c.pushLoop(c.consumeLabel(), false)
	condStart := len(c.fn.code)
	// result = iter.next()
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emitFieldOp(OpGetField2, "next") // [iter, next]
	c.emit(OpCallMethod)
	c.emitU16(0) // [result]
	c.emitOpU16(OpPutLocal, uint16(resSlot))
	// if result.done: exit without closing (iterator already exhausted)
	c.emitOpU16(OpGetLocal, uint16(resSlot))
	c.emitFieldOp(OpGetField, "done")
	normalExit := c.emitJump(OpJmpTrue)
	// v = result.value
	c.emitOpU16(OpGetLocal, uint16(resSlot))
	c.emitFieldOp(OpGetField, "value")
	store()

	c.compileStmt(n.Body)

	l.continueTarget = len(c.fn.code)
	c.patchContinues(l)
	if lexSlot >= 0 {
		c.emitOpU16(OpCloseUpval, uint16(lexSlot))
	}
	c.emit(OpJmp)
	c.emitU32(uint32(condStart))

	// break lands here (popLoop patches l.breaks here): pop the handler, close
	// the iterator, jump to end.
	c.popLoop()
	c.emit(OpTryPop)
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emit(OpIterClose)
	endBreak := c.emitJump(OpJmp)

	// normal exhaustion: pop the handler (no close — already done), jump to end.
	c.patchJump(normalExit)
	c.emit(OpTryPop)
	endDone := c.emitJump(OpJmp)

	// throw: close the iterator, then re-throw the caught value.
	c.patchJump(catchHandler)
	c.emit(OpCatch)
	c.emitU32(0)
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emit(OpIterClose)
	c.emit(OpThrow)

	c.patchJump(endBreak)
	c.patchJump(endDone)

	c.scopeDepth--
	c.popBlockScope()
}

// compileForAwaitOf lowers `for await (v of src)` to a lazy loop that awaits
// each iter.next() result (GetAsyncIterator + repeated await). Only valid inside
// an async function/generator (OpAwait suspends the coroutine).
func (c *compiler) compileForAwaitOf(n *Node) {
	c.scopeDepth++
	store, lexSlot := c.forInStore(n.Left)
	if store == nil {
		c.errorf("unsupported for-await-of target (slice)")
		c.scopeDepth--
		return
	}
	// iter = GetAsyncIterator(source)
	c.compileExpr(n.Right)
	c.emit(OpForAwaitOf) // source -> asyncIter
	iterSlot := c.addLocal("*fai*", false)
	c.emitOpU16(OpPutLocal, uint16(iterSlot))
	resSlot := c.addLocal("*far*", false)

	l := c.pushLoop(c.consumeLabel(), false)
	condStart := len(c.fn.code)
	// result = await iter.next()
	c.emitOpU16(OpGetLocal, uint16(iterSlot))
	c.emitFieldOp(OpGetField2, "next") // [iter, next]
	c.emit(OpCallMethod)
	c.emitU16(0)    // [promise]
	c.emit(OpAwait) // [result]
	c.emitOpU16(OpPutLocal, uint16(resSlot))
	// if result.done break
	c.emitOpU16(OpGetLocal, uint16(resSlot))
	c.emitFieldOp(OpGetField, "done")
	exit := c.emitJump(OpJmpTrue)
	l.breaks = append(l.breaks, exit)
	// v = result.value
	c.emitOpU16(OpGetLocal, uint16(resSlot))
	c.emitFieldOp(OpGetField, "value")
	store()

	c.compileStmt(n.Body)

	l.continueTarget = len(c.fn.code)
	c.patchContinues(l)
	if lexSlot >= 0 {
		c.emitOpU16(OpCloseUpval, uint16(lexSlot))
	}
	c.emit(OpJmp)
	c.emitU32(uint32(condStart))

	c.popLoop()
	c.scopeDepth--
	c.popBlockScope()
}

// compileForArray implements the shared for-in/for-of lowering: produceOp turns
// the source into an array (keys or values), which is then iterated by index.
func (c *compiler) compileForArray(n *Node, produceOp Opcode) {
	c.scopeDepth++
	store, lexSlot := c.forInStore(n.Left)
	if store == nil {
		c.errorf("unsupported for-in/of target (slice)")
		c.scopeDepth--
		return
	}

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
	if lexSlot >= 0 {
		c.emitOpU16(OpCloseUpval, uint16(lexSlot))
	}
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

func (c *compiler) forInStore(left *Node) (func(), int) {
	var name string
	switch {
	case left.Kind == NVar && len(left.Args) == 1 && left.Args[0].Left != nil:
		binding := left.Args[0].Left
		// Destructuring loop variable: for (var [k,v] of …).
		if binding.Kind == NArray || binding.Kind == NObject {
			kind := left.VarKind
			return func() { c.destructureTarget(binding, kind) }, -1
		}
		if binding.Kind != NIdent {
			return nil, -1
		}
		name = binding.Str
		if !(left.VarKind == VarVar && c.isScript) {
			var slot int
			lexSlot := -1
			if left.VarKind == VarLet || left.VarKind == VarConst {
				slot = c.declareLexical(name, left.VarKind == VarConst)
				lexSlot = slot
			} else {
				slot = c.declareVar(name, false)
			}
			return func() { c.emitOpU16(OpPutLocal, uint16(slot)) }, lexSlot
		}
	case left.Kind == NIdent:
		name = left.Str
	default:
		return nil, -1
	}
	if slot := c.resolveLocal(name); slot >= 0 {
		return func() { c.emitOpU16(OpPutLocal, uint16(slot)) }, -1
	}
	if uv := c.resolveUpvalue(name); uv >= 0 {
		return func() { c.emitOpU16(OpPutUpval, uint16(uv)) }, -1
	}
	return func() { c.emitGlobalPut(name) }, -1
}

func (c *compiler) compileBreak(n *Node) {
	l := c.targetLoop(n.Str)
	if l == nil {
		c.errorf("illegal break statement")
		return
	}
	off := c.emitJump(OpJmp)
	l.breaks = append(l.breaks, off)
}

func (c *compiler) compileContinue(n *Node) {
	l := c.targetLoopForContinue(n.Str)
	if l == nil {
		c.errorf("illegal continue statement")
		return
	}
	off := c.emitJump(OpJmp)
	l.continues = append(l.continues, off)
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
	_ = l
}

func (c *compiler) compileTry(n *Node) {
	// TRY_PUSH marks a catch handler; on throw the interpreter jumps there.
	handler := c.emitJump(OpTryPush)
	c.compileStmt(n.Body)
	c.emit(OpTryPop)
	skip := c.emitJump(OpJmp) // no exception: skip the catch block

	c.patchJump(handler)
	if n.CatchBody != nil {
		// CATCH pushes the thrown value; bind it to the catch parameter local.
		c.emit(OpCatch)
		c.emitU32(0) // finally addr (unused in the slice)
		if n.CatchParam != nil && n.CatchParam.Kind == NIdent {
			c.scopeDepth++
			slot := c.declareVar(n.CatchParam.Str, false)
			c.emitOpU16(OpPutLocal, uint16(slot))
			c.compileStmt(n.CatchBody)
			c.scopeDepth--
		} else {
			c.emit(OpPop) // discard thrown value (optional catch binding)
			c.compileStmt(n.CatchBody)
		}
	}
	c.patchJump(skip)

	if n.FinallyBody != nil {
		c.compileStmt(n.FinallyBody)
	}
}
