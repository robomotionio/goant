package engine

// Port of ant src/silver/compiler.c (AST → bytecode). The Phase 3 vertical
// slice implements a coherent subset — literals, arithmetic/comparison/logical
// operators, local variables (var/let/const as frame slots), assignment,
// if/while/do-while/blocks, and script completion value — proving the full
// parse→compile→execute pipeline. The remaining node kinds (functions, closures/
// upvalues, classes, calls, member access, try/for-of, destructuring, TDZ,
// IC/feedback tables) are layered on as the port continues.

import "fmt"

// CompileError is a compile-time (semantic) error.
type CompileError struct{ Msg string }

func (e *CompileError) Error() string { return "CompileError: " + e.Msg }

type localVar struct {
	name        string
	depth       int
	isConst     bool
	captured    bool
	blockScoped bool // let/const: hidden once its block is exited
	dead        bool // scope exited; no longer resolvable
}

type compiler struct {
	rt         *Runtime
	fn         *svFunc
	enclosing  *compiler
	locals     []localVar
	upvalues   []upvalDesc
	scopeDepth int
	curStack   int
	err        error

	// isScript marks the top-level script compilation: top-level `var` and
	// function declarations bind on the global object rather than as locals.
	isScript bool

	// isEval marks eval-body compilation: top-level `var` bindings stay in the
	// eval frame (do not leak to the global object), matching strict eval.
	isEval bool

	// completionSlot holds the running script/eval completion value.
	completionSlot int

	// loops is the enclosing-loop stack for break/continue resolution
	// (ant sv_loop_t).
	loops []*loopCtx

	// withDepth > 0 inside a `with` block: unqualified names that would be
	// globals must resolve dynamically against the with-object chain.
	withDepth int

	// pendingLabel is a label awaiting the loop/statement it prefixes.
	pendingLabel string

	// usingStack is the local slot holding the current block's disposal-record
	// array (for `using` declarations), or -1 when not inside a using block.
	usingStack int
}

func (c *compiler) consumeLabel() string {
	s := c.pendingLabel
	c.pendingLabel = ""
	return s
}

func isLoopNode(n *Node) bool {
	return n != nil && (n.Kind == NWhile || n.Kind == NDoWhile || n.Kind == NFor ||
		n.Kind == NForIn || n.Kind == NForOf)
}

// loopCtx tracks break/continue jump sites for one loop (ant sv_loop_t).
type loopCtx struct {
	breaks         []int // JMP operand offsets to patch to the loop exit
	continues      []int // JMP operand offsets to patch to the continue target
	continueTarget int   // resolved continue target (-1 until known)
	label          string
	isSwitch       bool
}

// Compile compiles a parsed program to a bytecode function (script mode).
func (rt *Runtime) Compile(prog *Node, filename, source string) (*svFunc, error) {
	return rt.compileProgram(prog, filename, source, false)
}

// CompileEval compiles an eval body: `var` bindings stay frame-local.
func (rt *Runtime) CompileEval(prog *Node, filename, source string) (*svFunc, error) {
	return rt.compileProgram(prog, filename, source, true)
}

func (rt *Runtime) compileProgram(prog *Node, filename, source string, isEval bool) (*svFunc, error) {
	c := &compiler{
		rt:         rt,
		isScript:   true,
		isEval:     isEval,
		usingStack: -1,
		fn:         &svFunc{name: "", filename: filename, source: source, isStrict: prog.Flags&fnParseStrict != 0},
	}
	// Reserve slot 0 for the completion value.
	c.completionSlot = c.addLocal("*completion*", false)
	c.emit(OpUndef)
	c.emitOpU16(OpPutLocal, uint16(c.completionSlot))

	// Bind the script's `this` (the global object) for lexical-this capture.
	thisSlot := c.addLocal("*this*", false)
	c.emit(OpThis)
	c.emitOpU16(OpPutLocal, uint16(thisSlot))

	// Global instantiation: pre-create var/function bindings so declarations
	// don't trip the strict "assignment to unresolvable" check, and undeclared
	// assignments in strict mode correctly throw ReferenceError. (Eval bodies
	// keep their vars frame-local, so no global pre-creation.)
	if !c.isEval {
		names := map[string]bool{}
		collectVarFuncNames(prog.Args, names)
		g := rt.objPtr(rt.global)
		for name := range names {
			if !g.hasOwn(name) {
				g.defineOwn(name, mkundef(), attrWritable|attrEnumerable)
			}
		}
	}

	c.hoistFunctions(prog.Args, false)
	c.compileStmts(prog.Args)
	if c.err != nil {
		return nil, c.err
	}
	// Return the completion value.
	c.emitOpU16(OpGetLocal, uint16(c.completionSlot))
	c.emit(OpReturn)

	c.fn.maxLocals = len(c.locals)
	if c.fn.maxStack < 8 {
		c.fn.maxStack = 8
	}
	return c.fn, nil
}

func (c *compiler) errorf(format string, args ...any) {
	if c.err == nil {
		c.err = &CompileError{Msg: fmt.Sprintf(format, args...)}
	}
}

// ---- locals ----

func (c *compiler) addLocal(name string, isConst bool) int {
	c.locals = append(c.locals, localVar{name: name, depth: c.scopeDepth, isConst: isConst})
	return len(c.locals) - 1
}

// resolveLocal returns the slot for name, searching innermost-first. Bindings
// whose block has been exited (dead) are skipped.
func (c *compiler) resolveLocal(name string) int {
	for i := len(c.locals) - 1; i >= 0; i-- {
		if c.locals[i].dead {
			continue
		}
		if c.locals[i].name == name {
			return i
		}
	}
	return -1
}

// declareLexical creates a fresh block-scoped (let/const) binding that shadows
// any outer binding and is hidden when its block exits.
func (c *compiler) declareLexical(name string, isConst bool) int {
	c.locals = append(c.locals, localVar{name: name, depth: c.scopeDepth, isConst: isConst, blockScoped: true})
	return len(c.locals) - 1
}

// popBlockScope hides the block-scoped bindings declared at the just-exited depth.
func (c *compiler) popBlockScope() {
	for i := len(c.locals) - 1; i >= 0; i-- {
		if c.locals[i].depth > c.scopeDepth && c.locals[i].blockScoped {
			c.locals[i].dead = true
		}
	}
}

// resolveUpvalue resolves name as a capture from an enclosing function,
// returning the upvalue index (or -1). It marks the captured enclosing local.
func (c *compiler) resolveUpvalue(name string) int {
	if c.enclosing == nil {
		return -1
	}
	if slot := c.enclosing.resolveLocal(name); slot >= 0 {
		c.enclosing.locals[slot].captured = true
		return c.addUpvalue(slot, true)
	}
	if uv := c.enclosing.resolveUpvalue(name); uv >= 0 {
		return c.addUpvalue(uv, false)
	}
	return -1
}

func (c *compiler) addUpvalue(index int, isLocal bool) int {
	for i, u := range c.upvalues {
		if u.index == index && u.isLocal == isLocal {
			return i
		}
	}
	c.upvalues = append(c.upvalues, upvalDesc{index: index, isLocal: isLocal})
	return len(c.upvalues) - 1
}

// declareVar declares (or reuses) a local binding, returning its slot.
func (c *compiler) declareVar(name string, isConst bool) int {
	// var reuses an existing binding in scope; let/const shadow.
	if slot := c.resolveLocal(name); slot >= 0 {
		return slot
	}
	return c.addLocal(name, isConst)
}

// ---- statements ----

func (c *compiler) compileStmts(list []*Node) {
	for _, stmt := range list {
		c.compileStmt(stmt)
		if c.err != nil {
			return
		}
	}
}

func (c *compiler) compileStmt(n *Node) {
	if n == nil {
		return
	}
	switch n.Kind {
	case NEmpty, NDebugger:
		return
	case NFunc:
		// Function declarations are hoisted (bound before the body runs). A
		// parenthesized function *expression* statement contributes a completion
		// value (used by eval); a bare one is a no-op.
		if n.Flags&fnParen != 0 {
			c.compileExpr(n)
			if c.isScript {
				c.emitOpU16(OpSetLocal, uint16(c.completionSlot))
			}
			c.emit(OpPop)
		}
		return
	case NClass:
		if n.Flags&fnParen != 0 {
			// Parenthesized class *expression* statement (completion value).
			c.compileClass(n)
			if c.isScript {
				c.emitOpU16(OpSetLocal, uint16(c.completionSlot))
			}
			c.emit(OpPop)
			return
		}
		// Class declaration: compile and bind to the class name.
		c.compileClass(n)
		c.bindDeclared(n.Str)
		return
	case NVar:
		c.compileVarDecl(n)
	case NBlock:
		if blockHasUsing(n.Args) {
			c.compileBlockWithUsing(n)
			return
		}
		c.scopeDepth++
		c.hoistFunctions(n.Args, true)
		c.compileStmts(n.Args)
		c.scopeDepth--
		c.popBlockScope()
	case NIf:
		c.compileIf(n)
	case NWhile:
		c.compileWhile(n)
	case NDoWhile:
		c.compileDoWhile(n)
	case NFor:
		c.compileFor(n)
	case NForIn:
		c.compileForIn(n)
	case NForOf:
		c.compileForOf(n)
	case NForAwaitOf:
		c.compileForAwaitOf(n)
	case NBreak:
		c.compileBreak(n)
	case NContinue:
		c.compileContinue(n)
	case NSwitch:
		c.compileSwitch(n)
	case NThrow:
		c.compileExpr(n.Right)
		c.emit(OpThrow)
	case NWith:
		c.compileWith(n)
	case NLabel:
		c.compileLabel(n)
	case NTry:
		c.compileTry(n)
	case NReturn:
		if n.Right != nil {
			c.compileExpr(n.Right)
		} else {
			c.emit(OpUndef)
		}
		c.emit(OpReturn)
	default:
		// Expression statement: evaluate; in a script the value updates the
		// completion value, in a function it is simply discarded.
		if canBeExpressionStatement(n) || n.Kind == NAssign || n.Kind == NBinary {
			c.compileExpr(n)
			if c.isScript {
				c.emitOpU16(OpSetLocal, uint16(c.completionSlot))
			}
			c.emit(OpPop)
			return
		}
		c.errorf("unsupported statement kind %v (slice)", n.Kind)
	}
}

// blockHasUsing reports whether a statement list declares a `using` or
// `await using` resource directly (so the block needs a disposal scope).
func blockHasUsing(stmts []*Node) bool {
	for _, s := range stmts {
		d := s
		if s != nil && s.Kind == NExport && s.Left != nil {
			d = s.Left
		}
		if d != nil && d.Kind == NVar && (d.VarKind == VarUsing || d.VarKind == VarAwaitUsing) {
			return true
		}
	}
	return false
}

// compileBlockWithUsing compiles a block whose direct statements include a
// `using` declaration. Resources register on a disposal-record array; a
// try-handler runs the disposal on both normal and abrupt completion (the latter
// folding disposal errors into the thrown value via SuppressedError). Disposal
// on `break`/`continue`/`return` out of the block is not yet handled — matching
// goant's existing try/finally limitation.
func (c *compiler) compileBlockWithUsing(n *Node) {
	c.scopeDepth++
	c.hoistFunctions(n.Args, true)

	c.emit(OpArray)
	c.emitU16(0)
	stackLocal := c.addLocal("*using*", false)
	c.emitOpU16(OpPutLocal, uint16(stackLocal))
	errLocal := c.addLocal("*usingerr*", false)
	savedUsing := c.usingStack
	c.usingStack = stackLocal

	catchHandler := c.emitJump(OpTryPush)
	c.compileStmts(n.Args)
	c.emit(OpTryPop)

	// Normal completion: dispose all resources, discard the completion value.
	c.emitOpU16(OpGetLocal, uint16(stackLocal))
	c.emit(OpUsingDispose)
	c.emit(OpPop)
	endJump := c.emitJump(OpJmp)

	// Abrupt completion (throw): capture the error, dispose-suppressed, re-throw.
	c.patchJump(catchHandler)
	c.emit(OpCatch)
	c.emitU32(0)
	c.emitOpU16(OpPutLocal, uint16(errLocal))
	c.emitOpU16(OpGetLocal, uint16(stackLocal))
	c.emitOpU16(OpGetLocal, uint16(errLocal))
	c.emit(OpUsingDisposeSuppressed)
	c.emit(OpThrow)

	c.patchJump(endJump)
	c.usingStack = savedUsing
	c.scopeDepth--
	c.popBlockScope()
}

func (c *compiler) compileVarDecl(n *Node) {
	// `using` / `await using`: bind the resource and register its disposer on the
	// enclosing block's disposal stack.
	if (n.VarKind == VarUsing || n.VarKind == VarAwaitUsing) && c.usingStack >= 0 {
		for _, decl := range n.Args {
			if decl.Left == nil || decl.Left.Kind != NIdent {
				c.errorf("unsupported using declaration target (slice)")
				return
			}
			slot := c.declareLexical(decl.Left.Str, false)
			c.emitOpU16(OpGetLocal, uint16(c.usingStack)) // entries
			if decl.Right != nil {
				c.compileExpr(decl.Right)
			} else {
				c.emit(OpUndef)
			}
			if n.VarKind == VarAwaitUsing {
				c.emit(OpUsingPushAsync)
			} else {
				c.emit(OpUsingPush)
			}
			c.emitOpU16(OpPutLocal, uint16(slot))
		}
		return
	}
	// Top-level `var` binds globally; `let`/`const` (and any binding inside a
	// function) are frame locals.
	asGlobal := n.VarKind == VarVar && c.isScript && !c.isEval
	for _, decl := range n.Args {
		if decl.Left != nil && (decl.Left.Kind == NArray || decl.Left.Kind == NObject) {
			c.compileDestructureDecl(decl.Left, decl.Right, n.VarKind)
			continue
		}
		if decl.Left == nil || decl.Left.Kind != NIdent {
			c.errorf("unsupported declaration target (slice)")
			return
		}
		name := decl.Left.Str
		nameAnonExpr(decl.Right, name)
		if asGlobal {
			if decl.Right != nil {
				c.compileExpr(decl.Right)
				c.emitGlobalPut(name)
			}
			// A bare `var x;` at top level leaves any existing global intact.
			continue
		}
		var slot int
		if n.VarKind == VarLet || n.VarKind == VarConst {
			slot = c.declareLexical(name, n.VarKind == VarConst)
		} else {
			slot = c.declareVar(name, false)
		}
		if decl.Right != nil {
			c.compileExpr(decl.Right)
		} else {
			c.emit(OpUndef)
		}
		c.emitOpU16(OpPutLocal, uint16(slot))
	}
}

func (c *compiler) compileIf(n *Node) {
	c.compileExpr(n.Cond)
	elseJump := c.emitJump(OpJmpFalse)
	c.compileStmt(n.Left)
	if n.Right != nil {
		endJump := c.emitJump(OpJmp)
		c.patchJump(elseJump)
		c.compileStmt(n.Right)
		c.patchJump(endJump)
	} else {
		c.patchJump(elseJump)
	}
}

// ---- expressions ----

func (c *compiler) compileExpr(n *Node) {
	if n == nil {
		c.emit(OpUndef)
		return
	}
	switch n.Kind {
	case NNumber:
		c.compileNumberLiteral(n.Num)
	case NString:
		c.emitConst(c.rt.internString(n.Str))
	case NBigInt:
		c.compileBigIntLiteral(n.Str)
	case NBool:
		if n.Num != 0 {
			c.emit(OpTrue)
		} else {
			c.emit(OpFalse)
		}
	case NNull:
		c.emit(OpNull)
	case NUndef:
		c.emit(OpUndef)
	case NRegexp:
		c.emitConst(c.rt.internString(n.Str))
		c.emitConst(c.rt.internString(n.Aux))
		c.emit(OpRegexp)
	case NGlobalThis:
		c.emit(OpGlobal)
	case NNewTarget:
		// An arrow resolves new.target as the enclosing function's *newtarget*
		// binding (lexical); a non-arrow reads its own frame's new.target.
		if slot := c.resolveLocal("*newtarget*"); slot >= 0 {
			c.emitOpU16(OpGetLocal, uint16(slot))
		} else if uv := c.resolveUpvalue("*newtarget*"); uv >= 0 {
			c.emitOpU16(OpGetUpval, uint16(uv))
		} else {
			c.emit(OpSpecialObj)
			c.emitByte(2)
		}
	case NThis:
		// `this` reads the synthetic *this* binding; arrows resolve it as an
		// upvalue, giving them the enclosing function's `this` (lexical this).
		if slot := c.resolveLocal("*this*"); slot >= 0 {
			c.emitOpU16(OpGetLocal, uint16(slot))
		} else if uv := c.resolveUpvalue("*this*"); uv >= 0 {
			c.emitOpU16(OpGetUpval, uint16(uv))
		} else {
			c.emit(OpThis)
		}
	case NIdent:
		c.compileIdentLoad(n)
	case NBinary:
		c.compileBinary(n)
	case NUnary:
		c.compileUnary(n)
	case NAssign:
		c.compileAssign(n)
	case NUpdate:
		c.compileUpdate(n)
	case NCall:
		c.compileCall(n)
	case NNew:
		c.compileNew(n)
	case NFunc:
		c.compileFunc(n)
	case NClass:
		c.compileClass(n)
	case NObject:
		c.compileObject(n)
	case NArray:
		c.compileArray(n)
	case NMember:
		if containsOptional(n) {
			c.compileOptionalChain(n)
		} else {
			c.compileMember(n)
		}
	case NOptional:
		c.compileOptionalChain(n)
	case NTemplate:
		c.compileTemplate(n)
	case NTaggedTemplate:
		c.compileTaggedTemplate(n)
	case NTypeof:
		c.compileExpr(n.Right)
		c.emit(OpTypeof)
	case NVoid:
		c.compileExpr(n.Right)
		c.emit(OpVoid)
	case NDelete:
		c.compileDelete(n)
	case NThrow:
		c.compileExpr(n.Right)
		c.emit(OpThrow)
	case NTernary:
		c.compileTernary(n)
	case NSequence:
		c.compileExpr(n.Left)
		c.emit(OpPop)
		c.compileExpr(n.Right)
	case NYield:
		c.compileYield(n)
	case NAwait:
		c.compileExpr(n.Right)
		c.emit(OpAwait)
	default:
		c.errorf("unsupported expression kind %v (slice)", n.Kind)
	}
}

func (c *compiler) compileNumberLiteral(d float64) {
	// Small integers use the compact CONST_I8 form.
	if d == float64(int8(d)) && d == float64(int64(d)) {
		c.emit(OpConstI8)
		c.emitByte(byte(int8(d)))
		return
	}
	c.emitConst(mknum(d))
}

func (c *compiler) compileIdentLoad(n *Node) {
	if slot := c.resolveLocal(n.Str); slot >= 0 {
		c.emitOpU16(OpGetLocal, uint16(slot))
		return
	}
	if uv := c.resolveUpvalue(n.Str); uv >= 0 {
		c.emitOpU16(OpGetUpval, uint16(uv))
		return
	}
	if c.withDepth > 0 {
		c.emitWithVar(OpWithGetVar, n.Str)
		return
	}
	c.emitGlobalGet(n.Str)
}

// compileWith compiles a `with (obj) stmt`: unqualified names inside resolve
// dynamically against obj (then the global) via WITH_GET_VAR/WITH_PUT_VAR.
func (c *compiler) compileWith(n *Node) {
	c.compileExpr(n.Left)
	c.emit(OpEnterWith)
	c.withDepth++
	c.compileStmt(n.Body)
	c.withDepth--
	c.emit(OpExitWith)
}

// emitWithVar emits a with-scoped variable access (op + u32 name + 3 pad bytes,
// matching the generated size).
func (c *compiler) emitWithVar(op Opcode, name string) {
	idx := c.constant(c.rt.internString(name))
	c.emit(op)
	c.emitU32(uint32(idx))
	c.emitU16(0)
	c.emitByte(0)
}

func (c *compiler) compileBinary(n *Node) {
	// Private brand check `#x in obj`: the LHS is a private name, keyed as its
	// string form rather than loaded as a variable.
	if n.Op == TokIn && n.Left != nil && n.Left.Kind == NIdent &&
		len(n.Left.Str) > 0 && n.Left.Str[0] == '#' {
		c.emitConst(c.rt.internString(n.Left.Str))
		c.compileExpr(n.Right)
		c.emit(OpIn)
		return
	}
	// Logical operators short-circuit.
	switch n.Op {
	case TokLand:
		c.compileExpr(n.Left)
		c.emit(OpDup)
		jmp := c.emitJump(OpJmpFalse)
		c.emit(OpPop)
		c.compileExpr(n.Right)
		c.patchJump(jmp)
		return
	case TokLor:
		c.compileExpr(n.Left)
		c.emit(OpDup)
		jmp := c.emitJump(OpJmpTrue)
		c.emit(OpPop)
		c.compileExpr(n.Right)
		c.patchJump(jmp)
		return
	case TokNullish:
		c.compileExpr(n.Left)
		c.emit(OpDup)
		jmp := c.emitJump(OpJmpNotNullish)
		c.emit(OpPop)
		c.compileExpr(n.Right)
		c.patchJump(jmp)
		return
	}
	c.compileExpr(n.Left)
	c.compileExpr(n.Right)
	op, ok := binaryOpcode(n.Op)
	if !ok {
		c.errorf("unsupported binary operator %v (slice)", n.Op)
		return
	}
	c.emit(op)
	if op == OpInstanceof {
		c.emitU16(0) // ic slot (INSTANCEOF is size 3)
	}
}

func binaryOpcode(t Token) (Opcode, bool) {
	switch t {
	case TokPlus:
		return OpAdd, true
	case TokMinus:
		return OpSub, true
	case TokMul:
		return OpMul, true
	case TokDiv:
		return OpDiv, true
	case TokRem:
		return OpMod, true
	case TokExp:
		return OpExp, true
	case TokLt:
		return OpLt, true
	case TokLe:
		return OpLe, true
	case TokGt:
		return OpGt, true
	case TokGe:
		return OpGe, true
	case TokEq:
		return OpEq, true
	case TokNe:
		return OpNe, true
	case TokSeq:
		return OpSeq, true
	case TokSne:
		return OpSne, true
	case TokAnd:
		return OpBand, true
	case TokOr:
		return OpBor, true
	case TokXor:
		return OpBxor, true
	case TokShl:
		return OpShl, true
	case TokShr:
		return OpShr, true
	case TokZShr:
		return OpUshr, true
	case TokIn:
		return OpIn, true
	case TokInstanceof:
		return OpInstanceof, true
	}
	return OpInvalid, false
}

// compileDelete compiles `delete obj.x` / `delete obj[e]`; other forms
// (delete of a variable) evaluate to true without effect in sloppy mode.
func (c *compiler) compileDelete(n *Node) {
	target := n.Right
	if target != nil && target.Kind == NMember {
		c.compileExpr(target.Left)
		if target.Flags&1 != 0 {
			c.compileExpr(target.Right)
		} else {
			c.emitConst(c.rt.internString(target.Right.Str))
		}
		c.emit(OpDelete)
		return
	}
	// `delete x`, `delete 1`, etc. → true (no binding removed).
	c.emit(OpTrue)
}

// compileUpdate compiles ++/-- (prefix and postfix) on an identifier target.
// Member-expression targets (obj.x++) are added with the read-modify-write
// helper later.
func (c *compiler) compileUpdate(n *Node) {
	target := n.Right
	if target != nil && target.Kind == NMember {
		c.compileMemberUpdate(n)
		return
	}
	if target == nil || target.Kind != NIdent {
		c.errorf("only identifier increment/decrement is supported (slice)")
		return
	}
	prefix := n.Flags == 1
	incOp := OpInc
	if n.Op == TokPostDec {
		incOp = OpDec
	}
	name := target.Str
	slot := c.resolveLocal(name)
	uv := -1
	if slot < 0 {
		uv = c.resolveUpvalue(name)
	}

	load := func() {
		switch {
		case slot >= 0:
			c.emitOpU16(OpGetLocal, uint16(slot))
		case uv >= 0:
			c.emitOpU16(OpGetUpval, uint16(uv))
		default:
			c.emitGlobalGet(name)
		}
	}
	storeKeep := func() { // leaves the stored value on the stack
		switch {
		case slot >= 0:
			c.emitOpU16(OpSetLocal, uint16(slot))
		case uv >= 0:
			c.emitOpU16(OpSetUpval, uint16(uv))
		default:
			c.emit(OpDup)
			c.emitGlobalPut(name)
		}
	}
	storeConsume := func() { // consumes the stored value
		switch {
		case slot >= 0:
			c.emitOpU16(OpPutLocal, uint16(slot))
		case uv >= 0:
			c.emitOpU16(OpPutUpval, uint16(uv))
		default:
			c.emitGlobalPut(name)
		}
	}

	if prefix {
		// ++x: x = ToNumber(x) + 1; result = new value.
		load()
		c.emit(incOp)
		storeKeep()
		return
	}
	// x++: result = ToNumber(x); x = that + 1.
	load()
	c.emit(OpUplus) // ToNumber(old)
	c.emit(OpDup)
	c.emit(incOp)
	storeConsume()
}

func (c *compiler) compileUnary(n *Node) {
	c.compileExpr(n.Right)
	switch n.Op {
	case TokUMinus:
		c.emit(OpNeg)
	case TokUPlus:
		c.emit(OpUplus)
	case TokNot:
		c.emit(OpNot)
	case TokTilda:
		c.emit(OpBnot)
	default:
		c.errorf("unsupported unary operator %v (slice)", n.Op)
	}
}

// nameAnonExpr implements NamedEvaluation: an anonymous function/class on the
// RHS of a binding or assignment takes the target's name (mutating the AST node
// so compileFunc/compileClass stamp it as the .name).
// isAnonFuncDef reports whether n is an anonymous function or class expression —
// i.e. an IsAnonymousFunctionDefinition target for NamedEvaluation.
func isAnonFuncDef(n *Node) bool {
	return n != nil && (n.Kind == NFunc || n.Kind == NClass) && n.Str == ""
}

func nameAnonExpr(rhs *Node, name string) {
	if rhs == nil || name == "" {
		return
	}
	if (rhs.Kind == NFunc || rhs.Kind == NClass) && rhs.Str == "" {
		rhs.Str = name
		rhs.Flags |= fnInferredName
	}
}

func (c *compiler) compileAssign(n *Node) {
	// Destructuring assignment: [a,b]=rhs / ({x}=rhs). Yields the RHS value.
	if n.Op == TokAssign && n.Left != nil && (n.Left.Kind == NArray || n.Left.Kind == NObject) {
		c.compileExpr(n.Right)
		c.emit(OpDup)
		c.destructureTarget(n.Left, varAssign)
		return
	}
	if n.Left != nil && n.Left.Kind == NMember {
		c.compileMemberAssign(n)
		return
	}
	// Assignment to a non-reference literal (undefined/null/this/true/…) is a
	// no-op that yields the RHS (sloppy mode); strict-mode rejection is a
	// parser concern handled elsewhere.
	if n.Left != nil && (n.Left.Kind == NUndef || n.Left.Kind == NNull ||
		n.Left.Kind == NThis || n.Left.Kind == NBool || n.Left.Kind == NGlobalThis) {
		c.compileExpr(n.Right)
		return
	}
	if n.Left == nil || n.Left.Kind != NIdent {
		c.errorf("only simple assignment targets supported (slice)")
		return
	}
	name := n.Left.Str
	slot := c.resolveLocal(name)
	uv := -1
	if slot < 0 {
		uv = c.resolveUpvalue(name)
	}

	loadVar := func() {
		switch {
		case slot >= 0:
			c.emitOpU16(OpGetLocal, uint16(slot))
		case uv >= 0:
			c.emitOpU16(OpGetUpval, uint16(uv))
		case c.withDepth > 0:
			c.emitWithVar(OpWithGetVar, name)
		default:
			c.emitGlobalGet(name)
		}
	}
	storeVar := func() {
		switch {
		case slot >= 0:
			c.emitOpU16(OpSetLocal, uint16(slot))
		case uv >= 0:
			c.emitOpU16(OpSetUpval, uint16(uv))
		case c.withDepth > 0:
			c.emit(OpDup)
			c.emitWithVar(OpWithPutVar, name)
		default:
			c.emit(OpDup)
			c.emitGlobalPut(name)
		}
	}

	// Logical assignment (&&= ||= ??=): short-circuit — the RHS (and the store)
	// runs only when the current value requires it.
	if jmpOp, ok := logicalAssignJmp(n.Op); ok {
		loadVar()
		c.emit(OpDup)
		skip := c.emitJump(jmpOp)
		c.emit(OpPop)
		c.compileExpr(n.Right)
		storeVar()
		c.patchJump(skip)
		return
	}

	// Evaluate the value to assign, leaving it on the stack.
	if n.Op == TokAssign {
		nameAnonExpr(n.Right, name)
		c.compileExpr(n.Right)
	} else {
		op, ok := compoundOpcode(n.Op)
		if !ok {
			c.errorf("unsupported compound assignment %v (slice)", n.Op)
			return
		}
		switch {
		case slot >= 0:
			c.emitOpU16(OpGetLocal, uint16(slot))
		case uv >= 0:
			c.emitOpU16(OpGetUpval, uint16(uv))
		case c.withDepth > 0:
			c.emitWithVar(OpWithGetVar, name)
		default:
			c.emitGlobalGet(name)
		}
		c.compileExpr(n.Right)
		c.emit(op)
	}

	// SET_* keeps the value on the stack (assignment is an expression).
	switch {
	case slot >= 0:
		c.emitOpU16(OpSetLocal, uint16(slot))
	case uv >= 0:
		c.emitOpU16(OpSetUpval, uint16(uv))
	case c.withDepth > 0:
		c.emit(OpDup)
		c.emitWithVar(OpWithPutVar, name)
	default:
		c.emit(OpDup)
		c.emitGlobalPut(name)
	}
}

// logicalAssignJmp maps a logical-assignment operator to the branch that skips
// the assignment when the current value already short-circuits it.
func logicalAssignJmp(t Token) (Opcode, bool) {
	switch t {
	case TokLandAssign:
		return OpJmpFalse, true // a &&= b: skip when a is falsy
	case TokLorAssign:
		return OpJmpTrue, true // a ||= b: skip when a is truthy
	case TokNullishAssign:
		return OpJmpNotNullish, true // a ??= b: skip when a is non-nullish
	}
	return 0, false
}

func compoundOpcode(t Token) (Opcode, bool) {
	switch t {
	case TokPlusAssign:
		return OpAdd, true
	case TokMinusAssign:
		return OpSub, true
	case TokMulAssign:
		return OpMul, true
	case TokDivAssign:
		return OpDiv, true
	case TokRemAssign:
		return OpMod, true
	case TokExpAssign:
		return OpExp, true
	case TokAndAssign:
		return OpBand, true
	case TokOrAssign:
		return OpBor, true
	case TokXorAssign:
		return OpBxor, true
	case TokShlAssign:
		return OpShl, true
	case TokShrAssign:
		return OpShr, true
	case TokZShrAssign:
		return OpUshr, true
	}
	return OpInvalid, false
}

func (c *compiler) compileTernary(n *Node) {
	c.compileExpr(n.Cond)
	elseJump := c.emitJump(OpJmpFalse)
	c.compileExpr(n.Left)
	endJump := c.emitJump(OpJmp)
	c.patchJump(elseJump)
	c.compileExpr(n.Right)
	c.patchJump(endJump)
}
