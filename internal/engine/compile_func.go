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
// functionSelfNameShadowed reports whether a same-named parameter or a top-level
// body `var` shadows the function's self-name binding. Both reuse the name's slot
// in the flat-local model, so the self-name must not be created there — the
// parameter / var binding is a normal mutable one. A body `let`/`const`/`class`
// shadows via a fresh (block-scoped) slot and so is not considered here.
func functionSelfNameShadowed(n *Node) bool {
	name := n.Str
	for _, p := range n.Args {
		var names []string
		collectPatternNames(p, &names)
		for _, nm := range names {
			if nm == name {
				return true
			}
		}
	}
	bodyVars := map[string]bool{}
	collectBodyVarNames(n.Body, bodyVars)
	return bodyVars[name]
}

// annexBVarShadowed reports whether name is lexically bound (let/const/class,
// catch parameter, ...) in a block enclosing the current one within this
// function. Such a binding makes the equivalent `var name` an early error, so the
// Annex B.3.3 block-function var-hoisting extension is skipped and the
// declaration binds only lexically. Mirrors ast.go's shadow set so the compiler
// and the global pre-creation (collectVarFuncNames) agree.
func (c *compiler) annexBVarShadowed(name string) bool {
	// A formal parameter (or the implicit `arguments`) already binds the name in
	// the function scope, so the extension is skipped (B.3.3.1: "F is not an
	// element of parameterNames").
	if c.paramNames[name] {
		return true
	}
	for i := len(c.locals) - 1; i >= 0; i-- {
		lv := c.locals[i]
		if lv.dead || !lv.blockScoped || lv.catchParam || lv.name != name {
			continue
		}
		if lv.depth < c.scopeDepth {
			return true
		}
	}
	return false
}

func (c *compiler) hoistFunctions(list []*Node, blockScoped bool) {
	// A block FunctionDeclaration that qualifies for Annex B.3.3 gets TWO
	// bindings: the block-scoped one it actually denotes, and the function-scope
	// var the extension copies its value into. The lexical slot is declared here,
	// before any body is compiled, so that a reference to the name from inside
	// the function captures the block binding rather than the var — otherwise
	// `{ function f(){ f = 1 } } f()` would overwrite the var and the outer name
	// would stop being callable.
	annexBLex := map[string]int{}
	if blockScoped && !c.fn.isStrict {
		for _, stmt := range list {
			fn := stmt
			for fn != nil && fn.Kind == NLabel {
				fn = fn.Body
			}
			if fn == nil || fn.Kind != NFunc || fn.Str == "" ||
				fn.Flags&(fnArrow|fnAsync|fnGenerator) != 0 || c.annexBVarShadowed(fn.Str) {
				continue
			}
			if _, dup := annexBLex[fn.Str]; !dup {
				annexBLex[fn.Str] = c.declareLexical(fn.Str, false)
			}
		}
	}
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
		if blockScoped && (c.fn.isStrict || fn.Flags&(fnAsync|fnGenerator) != 0 || c.annexBVarShadowed(fn.Str)) {
			// A block FunctionDeclaration binds only lexically (no enclosing var) when:
			// strict mode; it is async/generator (never eligible for the Annex B.3.3
			// web-compat extension); or B.3.3 is skipped because an enclosing-block
			// let/const/class shadows the name (the equivalent `var` would be an early
			// error).
			slot := c.declareLexical(fn.Str, false)
			c.emitOpU16(OpPutLocal, uint16(slot))
		} else if blockScoped {
			// Annex B.3.3: the value is stored in the block binding the declaration
			// denotes, and a copy goes to the function-scope var the extension
			// creates — targeting it past any intervening catch parameter of the same
			// name rather than the nearest binding bindDeclared would pick. Later
			// writes inside the block hit only the block binding.
			if lex, ok := annexBLex[fn.Str]; ok {
				c.emit(OpDup)
				c.emitOpU16(OpPutLocal, uint16(lex))
			}
			if slot := c.resolveFunctionVar(fn.Str); slot >= 0 {
				c.emitOpU16(OpPutLocal, uint16(slot))
			} else {
				c.bindDeclared(fn.Str)
			}
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

// annexBIfVarShadowed reports whether the Annex B.3.4 var-hoisting extension for
// an if-branch FunctionDeclaration named `name` is skipped: when a formal
// parameter or a lexical binding in the same-or-enclosing scope shares the name
// (the equivalent `var name` would be an early error). Unlike a block-level
// function (annexBVarShadowed), an if-branch declaration's var target is the
// enclosing scope itself, so a sibling let/const at the current depth shadows it.
func (c *compiler) annexBIfVarShadowed(name string) bool {
	if c.paramNames[name] {
		return true
	}
	for i := len(c.locals) - 1; i >= 0; i-- {
		lv := c.locals[i]
		if lv.dead || !lv.blockScoped || lv.catchParam || lv.name != name {
			continue
		}
		if lv.depth <= c.scopeDepth {
			return true
		}
	}
	return false
}

// declareAnnexBName creates an enclosing-scope binding (initialized to undefined)
// for an Annex B if-body function, unless one already exists or the extension is
// skipped because a parameter/lexical shadows the name.
func (c *compiler) declareAnnexBName(name string) {
	if c.annexBIfVarShadowed(name) {
		return
	}
	if c.isScript && !c.evalVarDynamic {
		g := c.rt.objPtr(c.rt.global)
		if !g.hasOwn(name) {
			g.defineOwn(name, mkundef(), attrWritable|attrEnumerable)
		}
		return
	}
	// A direct eval's B.3.4 name binds in the caller's variable environment; the
	// binding itself was created by EvalDeclarationInstantiation, so there is
	// nothing to declare here.
	if c.evalVarDynamic {
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
		// When the B.3.4 extension is skipped (a parameter/lexical shadows the
		// name), the declaration must not update the enclosing binding: compile the
		// function for its early errors, then discard its value.
		if c.annexBIfVarShadowed(n.Str) {
			c.compileFunc(n)
			c.emit(OpPop)
			return
		}
		// The declaration has its own binding in the branch's scope — that is what
		// a reference from inside the function denotes — and the var receives only
		// a copy, so a later write inside the function does not reach it and the
		// outer name stays callable. The scope is entered and the binding declared
		// BEFORE the body is compiled, or the closure would capture the var
		// instead (the same ordering hoistFunctions needs for B.3.3).
		c.scopeDepth++
		lex := c.declareLexical(n.Str, false)
		c.compileFunc(n)
		c.emit(OpDup)
		c.emitOpU16(OpPutLocal, uint16(lex))
		// The B.3.4 assignment targets the enclosing var-scope binding (the one
		// declareAnnexBName created), not an intervening catch parameter: a simple
		// catch parameter may coexist with the var, so `try{}catch(f){if(1)function
		// f(){}}` updates the outer function-scope (or global) `f`, leaving the catch
		// parameter alone. resolveFunctionVar skips the catch param to find that var.
		if slot := c.resolveFunctionVar(n.Str); slot >= 0 {
			c.emitOpU16(OpPutLocal, uint16(slot))
		} else if uv := c.resolveUpvalue(n.Str); uv >= 0 {
			c.emitOpU16(OpPutUpval, uint16(uv))
		} else if c.evalVarDynamic {
			// In a direct eval the var-scope binding lives on the caller's variable
			// object, not on the global.
			c.emitWithVar(OpWithPutVar, n.Str)
		} else {
			c.emitGlobalPut(n.Str)
		}
		c.scopeDepth--
		c.popBlockScope()
		return
	}
	c.compileStmt(n)
}

// checkParamLexicalConflict enforces the early error that a lexical declaration
// (let/const/class) at the top level of a function body may not redeclare a
// parameter name (BoundNames of FormalParameters ∩ LexicallyDeclaredNames of the
// body must be empty). A body `var` of the same name is allowed. Body-level
// function declarations are var-scoped here, so they do not participate.
func (c *compiler) checkParamLexicalConflict(n *Node) {
	if n.Body == nil || n.Body.Kind != NBlock || len(n.Args) == 0 {
		return
	}
	paramNames := map[string]bool{}
	for _, param := range n.Args {
		switch param.Kind {
		case NAssignPat:
			collectBindingNames(param.Left, paramNames)
		case NRest:
			collectBindingNames(param.Right, paramNames)
		default:
			collectBindingNames(param, paramNames)
		}
	}
	if len(paramNames) == 0 {
		return
	}
	for _, stmt := range n.Body.Args {
		if stmt == nil {
			continue
		}
		var names []string
		switch {
		case stmt.Kind == NVar && (stmt.VarKind == VarLet || stmt.VarKind == VarConst):
			for _, d := range stmt.Args {
				if d != nil {
					collectPatternNames(d.Left, &names)
				}
			}
		case stmt.Kind == NClass && stmt.Str != "":
			names = append(names, stmt.Str)
		}
		for _, nm := range names {
			if paramNames[nm] {
				c.syntaxErrorf("Identifier '%s' has already been declared", nm)
				return
			}
		}
	}
}

// checkBlockDeclConflicts enforces the early errors on the combined declared
// names of a StatementList. `blockScope` distinguishes a genuine Block or switch
// CaseBlock — where every FunctionDeclaration is a lexical binding — from a
// Script or function-body top level, where FunctionDeclarations are var-scoped
// and only let/const/class contribute lexical names.
//
// The rules (matching V8):
//   - lexically-declared names must be pairwise distinct;
//   - a lexical name may not also be var-declared; and
//   - a lexical name may not also be declared by a sloppy plain FunctionDeclaration.
//
// In a block, sloppy plain FunctionDeclarations are exempt from the first and
// third rules among themselves (Annex B.3.3): two `function f(){}` may coexist
// and may shadow a var, so they are tracked apart from `lexical`. Async and
// generator declarations never receive that relaxation, and in strict mode a
// plain FunctionDeclaration is an ordinary lexical binding.
func (c *compiler) checkBlockDeclConflicts(list []*Node, blockScope bool) {
	lexical := map[string]bool{} // let/const/class + (block-scope) async/gen/strict fns
	plainFn := map[string]bool{} // sloppy plain FunctionDeclaration names (block scope)
	varNames := map[string]bool{}

	fail := func(name string) {
		c.syntaxErrorf("Identifier '%s' has already been declared", name)
	}
	addLexical := func(name string) {
		if lexical[name] {
			fail(name)
		}
		lexical[name] = true
	}

	for _, stmt := range list {
		if c.err != nil {
			return
		}
		if stmt == nil {
			continue
		}
		switch stmt.Kind {
		case NVar:
			var names []string
			for _, decl := range stmt.Args {
				collectPatternNames(decl.Left, &names)
			}
			if stmt.VarKind == VarLet || stmt.VarKind == VarConst ||
				stmt.VarKind == VarUsing || stmt.VarKind == VarAwaitUsing {
				// `using` / `await using` bindings are block-scoped lexical names, so
				// they collide with a same-named var/let/const/using (`{ using f = …;
				// var f; }` is a redeclaration SyntaxError).
				for _, nm := range names {
					addLexical(nm)
				}
			} else {
				for _, nm := range names {
					varNames[nm] = true
				}
			}
		case NClass:
			if stmt.Str != "" {
				addLexical(stmt.Str)
			}
		case NFunc:
			if stmt.Str == "" || stmt.Flags&(fnArrow|fnFuncExpr) != 0 {
				continue
			}
			switch {
			case !blockScope:
				// Script / function body: FunctionDeclarations are var-scoped.
				varNames[stmt.Str] = true
			case stmt.Flags&(fnAsync|fnGenerator) != 0 || c.fn.isStrict:
				addLexical(stmt.Str)
			default:
				plainFn[stmt.Str] = true
			}
		}
	}
	if c.err != nil {
		return
	}
	// VarDeclaredNames of the StatementList includes `var`s hoisted out of nested
	// blocks and statements (but not nested functions), so a lexical name here
	// conflicts with a deeper var too: `{ { var f; } let f; }`.
	for _, stmt := range list {
		collectBodyVarNames(stmt, varNames)
	}
	for nm := range lexical {
		if varNames[nm] || plainFn[nm] {
			fail(nm)
			return
		}
	}
	// A block-scoped (Annex B) FunctionDeclaration is also a LexicallyDeclaredName
	// of the block, so its name colliding with a VarDeclaredName is an early error
	// too: `{ function f(){} var f; }` / `switch(x){case 1: function f(){}
	// default: var f}`. (Two function declarations of the same name are allowed.)
	for nm := range plainFn {
		if varNames[nm] {
			fail(nm)
			return
		}
	}
}

// hoistLexicals pre-declares the let/const bindings of a statement list at the
// current scope, initializing each to an EMPTY hole. Running before function
// hoisting lets nested functions capture the binding, and a read before the
// declaration's initializer runs hits the hole and throws (temporal dead zone).
// Names bound inside a destructuring pattern are hoisted too (via
// collectPatternNames), so `const {x}=o; function f(){return x}` captures x.
// Top-level script `var`-scoped code is unaffected.
func (c *compiler) hoistLexicals(list []*Node) {
	// A lexical name may not also be var-declared in the same scope. Collect the
	// var names declared directly in this statement list (the common same-level
	// `let x; var x;` conflict; var hoisting through nested blocks is not modeled
	// here, so this only ever reports a true conflict — never a false one).
	varNames := map[string]bool{}
	for _, stmt := range list {
		if stmt != nil && stmt.Kind == NVar && stmt.VarKind == VarVar {
			for _, decl := range stmt.Args {
				var names []string
				collectPatternNames(decl.Left, &names)
				for _, nm := range names {
					varNames[nm] = true
				}
			}
		}
	}
	// A name may be lexically declared at most once per scope; `seen` tracks the
	// let/const names declared by THIS statement list (a genuine duplicate) so a
	// pre-existing binding from an outer/enclosing construct isn't misread as one.
	seen := map[string]bool{}
	for _, stmt := range list {
		if stmt == nil {
			continue
		}
		// A class declaration is lexically scoped: pre-declare its name as an
		// empty TDZ hole so a read before the declaration throws (bindClassDecl
		// reuses the slot). Only at the same lexical depth (a nested block's class
		// is hoisted by that block).
		if stmt.Kind == NClass && stmt.Str != "" {
			if seen[stmt.Str] || varNames[stmt.Str] {
				c.syntaxErrorf("Identifier '%s' has already been declared", stmt.Str)
				return
			}
			seen[stmt.Str] = true
			if c.lexicalAtCurrentDepth(stmt.Str) < 0 {
				slot := c.declareLexical(stmt.Str, false)
				c.emit(OpEmpty)
				c.emitOpU16(OpPutLocal, uint16(slot))
			}
			continue
		}
		if stmt.Kind != NVar {
			continue
		}
		if stmt.VarKind != VarLet && stmt.VarKind != VarConst {
			continue
		}
		for _, decl := range stmt.Args {
			var names []string
			collectPatternNames(decl.Left, &names)
			for _, name := range names {
				if name == "let" {
					// `let`/`const` may not bind the name "let" (LexicalDeclaration).
					c.syntaxErrorf("let is disallowed as a lexically bound name")
					return
				}
				if seen[name] || varNames[name] {
					c.syntaxErrorf("Identifier '%s' has already been declared", name)
					return
				}
				seen[name] = true
				if c.lexicalAtCurrentDepth(name) >= 0 {
					continue // already hoisted (e.g. class-name binding); inline decl will store
				}
				slot := c.declareLexical(name, stmt.VarKind == VarConst)
				c.emit(OpEmpty)
				c.emitOpU16(OpPutLocal, uint16(slot))
			}
		}
	}
}

// collectPatternNames appends the identifier names bound by a declaration target
// — a plain NIdent, or a nested array/object binding pattern — to out. Defaults
// (NAssignPat / `x = d`) and rest elements are unwrapped to their target;
// member targets (which appear only in destructuring *assignment*, never a
// let/const declaration) bind no name and are skipped.
// paramsBindArguments reports whether any parameter binds the name "arguments".
// Such a parameter suppresses the arguments object (FunctionDeclarationInstantiation
// step: argumentsObjectNeeded is false when "arguments" is a parameter name).
func paramsBindArguments(params []*Node) bool {
	var names []string
	for _, p := range params {
		if p == nil {
			continue
		}
		target := p
		switch {
		case p.Kind == NRest || p.Kind == NSpread:
			target = p.Right
		case p.Kind == NAssignPat || (p.Kind == NAssign && p.Op == TokAssign):
			target = p.Left
		}
		collectPatternNames(target, &names)
	}
	for _, nm := range names {
		if nm == "arguments" {
			return true
		}
	}
	return false
}

func collectPatternNames(pattern *Node, out *[]string) {
	if pattern == nil {
		return
	}
	switch pattern.Kind {
	case NIdent:
		*out = append(*out, pattern.Str)
	case NArray:
		for _, elem := range pattern.Args {
			if elem == nil || elem.Kind == NEmpty {
				continue
			}
			switch {
			case elem.Kind == NRest || elem.Kind == NSpread:
				collectPatternNames(elem.Right, out)
			case elem.Kind == NAssignPat || (elem.Kind == NAssign && elem.Op == TokAssign):
				collectPatternNames(elem.Left, out)
			default:
				collectPatternNames(elem, out)
			}
		}
	case NObject:
		for _, prop := range pattern.Args {
			if prop == nil {
				continue
			}
			if prop.Kind == NSpread {
				collectPatternNames(prop.Right, out)
				continue
			}
			target := prop.Right
			if target != nil && target.Kind == NAssign && target.Op == TokAssign {
				target = target.Left
			}
			collectPatternNames(target, out)
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
	// A direct eval's hoisted function declaration binds per the eval's variable
	// environment: an existing caller binding is updated in place, a sloppy
	// global-scope eval binds on the global object, and any other case is an
	// eval-frame local (a strict eval keeps its declarations to itself; a
	// function-scope eval keeps new names local — leaking into the caller's
	// function scope is future work).
	if c.borrowed != nil {
		if c.evalVarUpdatesBorrowed(name) {
			if uv := c.resolveBorrowed(name); uv >= 0 {
				c.emitOpU16(OpPutUpval, uint16(uv))
				return
			}
		}
		if c.evalVarGlobal {
			c.emitGlobalPut(name)
			return
		}
		// The caller's variable object already holds the name (created by
		// EvalDeclarationInstantiation); store the function value there. The test is
		// for a function-scope slot, not any slot: an Annex B.3.3 block function has
		// a block-scoped binding of the same name, and its var-scope copy must not
		// land back in that one.
		if c.evalVarDynamic && c.resolveFunctionVar(name) < 0 {
			c.emitWithVar(OpWithPutVar, name)
			return
		}
		slot := c.declareVar(name, false)
		c.emitOpU16(OpPutLocal, uint16(slot))
		return
	}
	// Script code binds on the global object, and so does a SLOPPY indirect eval
	// (its variable environment is the global one). A STRICT eval has its own
	// variable environment, so its declarations stay frame-local and never leak.
	if c.isScript && !c.isModule && (!c.isEval || c.evalVarGlobal) {
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
	// hoistLexicals pre-declared the class name as an empty TDZ slot at this
	// lexical depth; initialize that slot in place so a read before the class
	// declaration hit the temporal dead zone.
	if slot := c.lexicalAtCurrentDepth(name); slot >= 0 {
		c.emitOpU16(OpPutLocal, uint16(slot))
		return
	}
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
			isClassCtor:    n.Flags&fnClassCtor != 0,
			isClassElement: n.Flags&(fnClassBody|fnClassCtor) != 0,
			classIsDerived: c.pendingClassDerived,
			isMethod:       n.Flags&(fnMethod|fnGetter|fnSetter) != 0 && n.Flags&fnClassCtor == 0,
			isStrict:    c.fn.isStrict,
			metaCell:    c.fn.metaCell,
			srcStart:    int(n.SrcOff),
			srcEnd:      int(n.SrcEnd),
		},
	}
	// Hand a class constructor its instance-field initializers (set by
	// compileClass immediately before this call; cleared so no other function
	// inherits them).
	// A function nested (lexically) inside a `with` block resolves its free names
	// against the captured with-objects; propagate that through further nesting.
	child.inheritedWith = c.withDepth > 0 || c.inheritedWith
	child.realWith = c.withDepth > 0 || c.realWith
	child.fn.capturesWith = child.inheritedWith
	child.classFields = c.pendingClassFields
	child.classDerived = c.pendingClassDerived
	child.fieldKeys = c.pendingFieldKeys
	child.fn.isDerivedCtor = child.classDerived && child.fn.isClassCtor
	child.staticSuper = c.pendingStaticSuper
	c.pendingClassFields = nil
	c.pendingClassDerived = false
	c.pendingFieldKeys = nil
	c.pendingStaticSuper = false
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
			if s == nil {
				continue
			}
			if s.Kind != NString { // a non-string statement (incl. `;`) ends the prologue
				break
			}
			if s.Str == "use strict" && s.Flags&fnStrHadEscape == 0 {
				c.fn.isStrict = true
				break
			}
		}
	}

	// A sloppy direct eval's `var` and function declarations bind in the calling
	// function's variable environment, which has no compile-time slot for a name
	// the function never mentions. Give such a function a dynamic variable object
	// (allocated at frame entry) and route its free names through it, reusing the
	// with-object machinery — the resolution rule is the same: check an object at
	// run time, else fall back to the static binding. A STRICT eval has its own
	// variable environment, so a strict function needs none of this.
	if !c.fn.isStrict && !c.fn.dynamicVars {
		has := nodeHasDirectEval(n.Body)
		for _, p := range n.Args {
			has = has || nodeHasDirectEval(p)
		}
		if has {
			c.fn.dynamicVars = true
			c.inheritedWith = true
		}
	}

	// Record the formal-parameter names (plus the implicit `arguments` binding of
	// an ordinary function) so Annex B.3.3 can skip var-hoisting a block-level
	// function whose name is already a parameter (see annexBVarShadowed).
	c.paramNames = map[string]bool{}
	for _, param := range n.Args {
		switch param.Kind {
		case NAssignPat:
			collectBindingNames(param.Left, c.paramNames)
		case NRest:
			collectBindingNames(param.Right, c.paramNames)
		default:
			collectBindingNames(param, c.paramNames)
		}
	}
	if n.Flags&fnArrow == 0 {
		c.paramNames["arguments"] = true
	}

	// Duplicate parameter names are forbidden for an arrow, a method/accessor, any
	// strict-mode function, or one with a non-simple (rest/default/destructuring)
	// parameter list — only a sloppy simple-parameter regular function may repeat
	// a name.
	if n.Flags&(fnArrow|fnMethod|fnGetter|fnSetter) != 0 || c.fn.isStrict || hasNonSimpleParams(n) {
		seen := map[string]bool{}
		for _, param := range n.Args {
			var names []string
			switch param.Kind {
			case NAssignPat:
				collectPatternNames(param.Left, &names)
			case NRest:
				collectPatternNames(param.Right, &names)
			default:
				collectPatternNames(param, &names)
			}
			for _, nm := range names {
				if seen[nm] {
					c.syntaxErrorf("Duplicate parameter name not allowed in this context")
					return
				}
				seen[nm] = true
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
	// A sloppy function may repeat a simple parameter name; the repeats SHARE one
	// binding, so each position has to copy its own argument in order for the last
	// occurrence to win. dupParams records that, and paramCount then counts the
	// distinct slots the frame prefill may fill.
	dupParams := false
	seenParam := map[string]bool{}
	for i, p := range n.Args {
		switch p.Kind {
		case NIdent:
			if seenParam[p.Str] {
				dupParams = true
			}
			seenParam[p.Str] = true
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
			if p.Right == nil || (p.Right.Kind != NIdent && p.Right.Kind != NArray && p.Right.Kind != NObject) {
				c.errorf("unsupported rest parameter form (slice)")
				return
			}
			restParam = p
			restIndex = i
		default:
			c.errorf("unsupported parameter form (slice)")
			return
		}
	}
	if dupParams {
		paramCount = len(seenParam)
	}
	c.fn.paramCount = paramCount
	// A mapped `arguments` needs formal i to own frame slot i: only a simple
	// parameter list of distinct names lays out that way (declareVar reuses the
	// slot of a repeated name), and only sloppy non-arrow functions map at all.
	if !c.fn.isStrict && n.Flags&fnArrow == 0 {
		distinct := map[string]bool{}
		c.fn.mappedArgs = true
		for _, p := range n.Args {
			if p.Kind != NIdent || distinct[p.Str] {
				c.fn.mappedArgs = false
				break
			}
			distinct[p.Str] = true
		}
	}
	// Function.length (ExpectedArgumentCount): the number of parameters before
	// the first one with a default initializer or the rest parameter. A
	// destructuring parameter without a default still counts.
	c.fn.fnLength = 0
	for _, p := range n.Args {
		if p.Kind == NAssignPat || p.Kind == NRest {
			break
		}
		c.fn.fnLength++
	}

	// Bind `this` / self-name / arguments BEFORE evaluating parameter defaults,
	// since default expressions may reference them (e.g. `m(o = this)`).
	//
	// Non-arrow functions bind their own `this` in *this* (arrows inherit the
	// enclosing one lexically via upvalue capture).
	if n.Flags&fnArrow == 0 {
		slot := c.declareVar("*this*", false)
		c.fn.thisSlot = slot
		if c.fn.isDerivedCtor {
			// A derived constructor's `this` is in its temporal dead zone until
			// super() binds it: reading it before then (or an implicit/undefined
			// return with no super()) throws a ReferenceError.
			c.emit(OpEmpty)
		} else {
			c.emit(OpThis)
		}
		c.emitOpU16(OpPutLocal, uint16(slot))
		// Bind new.target in *newtarget* so a nested arrow can capture it lexically
		// (arrows have no new.target of their own). Only when referenced (in the
		// body or a nested arrow) to avoid the extra slot in the common case.
		// A derived constructor always binds it: super() needs new.target, and a
		// super() nested in an arrow reaches it only through this binding.
		if referencesNewTarget(n.Body) || c.fn.isDerivedCtor {
			ntSlot := c.declareVar("*newtarget*", false)
			c.emit(OpSpecialObj)
			c.emitByte(2)
			c.emitOpU16(OpPutLocal, uint16(ntSlot))
		}
	}
	// A named function is self-bound: its name refers to itself inside the body
	// (named function expressions; also enables recursion for declarations). A
	// name assigned by NamedEvaluation does NOT create this binding. The self-name
	// lives in an outer environment, so a same-named parameter or a top-level body
	// `var` (both of which reuse the slot here) SHADOWS it with a mutable binding —
	// skip the self-name entirely in that case (`function g(g){}`,
	// `function g(){ var g }`). A body `let`/`const` shadows via a fresh slot.
	// Only a function EXPRESSION gets this binding. A declaration's name is the
	// mutable binding in the ENCLOSING scope, and giving the body its own slot for
	// it shadowed that: `function f(){ f = 2 } f()` left the outer f untouched,
	// and an exported function reassigning its own name never updated the export.
	if n.Str != "" && n.Flags&fnArrow == 0 && n.Flags&fnInferredName == 0 &&
		n.Flags&fnFuncExpr != 0 && !functionSelfNameShadowed(n) {
		slot := c.declareVar(n.Str, false)
		// A named function expression's self-reference is immutable: assigning to
		// it is a strict-mode TypeError and a sloppy no-op.
		c.locals[slot].selfName = true
		c.emit(OpSpecialObj)
		c.emitByte(1) // current function value
		c.emitOpU16(OpPutLocal, uint16(slot))
	}
	// If a non-arrow function references `arguments` — in its body or a parameter
	// default (evaluated after the arguments object exists) — bind it at entry,
	// before the defaults run.
	if n.Flags&fnArrow == 0 && (referencesArguments(n.Body) || paramsReferenceArguments(n.Args)) &&
		!paramsBindArguments(n.Args) {
		slot := c.declareVar("arguments", false)
		c.emit(OpSpecialObj)
		c.emitByte(0)
		c.emitOpU16(OpPutLocal, uint16(slot))
	}

	// Rest parameter: collect args[restIndex:] into an array, then bind it (a
	// plain name) or destructure it (a `...[a,b]` / `...{a}` pattern).
	if restParam != nil {
		c.emit(OpRest)
		c.emitU16(uint16(restIndex))
		if restParam.Right.Kind == NIdent {
			slot := c.declareVar(restParam.Right.Str, false)
			c.emitOpU16(OpPutLocal, uint16(slot))
		} else {
			c.destructureTarget(restParam.Right, VarLet)
		}
	}
	// Default parameters. When the list is all simple/default NIdent parameters
	// (no destructuring), bind them left-to-right through the temporal dead zone:
	// every parameter slot starts as an EMPTY hole, and each is initialized from
	// its raw argument (or default) in turn, so a default referencing the same or a
	// later parameter throws (default-params.tdz). Otherwise fall back to the
	// simpler frame-prefill + default-fixup path.
	// Parameter default / destructuring expressions evaluate in the parameter
	// environment (a direct eval there is governed by that scope).
	savedInParam := c.inParamExpr
	c.inParamExpr = true
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
		if dupParams {
			// Repeated names share a slot, which the frame prefill can only fill once;
			// copying each argument in source order gives the last occurrence.
			for _, o := range ordered {
				c.emit(OpGetArg)
				c.emitU16(uint16(o.argIdx))
				c.emitOpU16(OpPutLocal, uint16(o.slot))
			}
		}
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
	c.inParamExpr = savedInParam

	if n.Body == nil {
		c.emit(OpReturnUndef)
	} else if n.Body.Kind == NBlock {
		// Pre-declare all function-scoped var/function/class names as locals so
		// that hoisted function bodies capture them as upvalues (their cells)
		// rather than resolving to globals.
		// In STRICT code a block-level function declaration binds only in its block,
		// so it contributes no function-scope name: pre-declaring one would make a
		// reference from outside the block resolve to undefined instead of throwing.
		names := map[string]bool{}
		collectVarFuncNamesMode(n.Body.Args, names, c.fn.isStrict)
		for name := range names {
			if c.resolveLocal(name) < 0 {
				c.addLocal(name, false)
			}
		}
		c.checkBlockDeclConflicts(n.Body.Args, false)
		c.checkParamLexicalConflict(n)
		c.hoistLexicals(n.Body.Args)
		c.hoistFunctions(n.Body.Args, false)
		// A base class constructor initializes its instance fields on `this`
		// before the body runs. (A derived ctor does it after super() instead —
		// see compileSuperCall.)
		if c.fn.isClassCtor && !c.classDerived {
			c.emitInstanceFieldInit()
		}
		if c.fn.isGenerator {
			// Generator body barrier: parameters (above) are instantiated eagerly at
			// call time, then the coroutine suspends here so the body proper does not
			// run until the first resume. newGenerator drives to this point once; the
			// tEmpty sentinel marks the barrier yield and its resume value is dropped.
			c.emit(OpEmpty)
			c.emit(OpYield)
			c.emit(OpPop)
		}
		// A `using`/`await using` at the top level of the function body disposes its
		// resources when the body's declarative environment is torn down (function
		// return / fall-through / throw), the same scaffolding a nested block uses.
		bodyUsing := blockHasUsing(n.Body.Args)
		bodyDispose, bodyDisposeSuppressed := OpUsingDispose, OpUsingDisposeSuppressed
		if blockHasAwaitUsing(n.Body.Args) {
			bodyDispose, bodyDisposeSuppressed = OpUsingDisposeAsync, OpUsingDisposeAsyncSuppressed
		}
		var usingStackLocal, usingErrLocal, usingCatch, usingEnd int
		savedUsingStack := c.usingStack
		if bodyUsing {
			c.emit(OpArray)
			c.emitU16(0)
			usingStackLocal = c.addLocal("*using*", false)
			c.emitOpU16(OpPutLocal, uint16(usingStackLocal))
			usingErrLocal = c.addLocal("*usingerr*", false)
			c.usingStack = usingStackLocal
			usingCatch = c.emitJump(OpTryPush)
		}
		c.compileStmts(n.Body.Args)
		if bodyUsing {
			// Normal completion (fall-through): dispose, then re-thread into the
			// function's implicit-return handling below.
			c.emit(OpTryPop)
			c.emitOpU16(OpGetLocal, uint16(usingStackLocal))
			c.emit(bodyDispose)
			c.emit(OpPop)
			usingEnd = c.emitJump(OpJmp)
			// Abrupt completion (throw): dispose-suppressed and re-throw.
			c.patchJump(usingCatch)
			c.emit(OpCatch)
			c.emitU32(0)
			c.emitOpU16(OpPutLocal, uint16(usingErrLocal))
			c.emitOpU16(OpGetLocal, uint16(usingStackLocal))
			c.emitOpU16(OpGetLocal, uint16(usingErrLocal))
			c.emit(bodyDisposeSuppressed)
			c.emit(OpThrow)
			c.patchJump(usingEnd)
			c.usingStack = savedUsingStack
		}
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
	if hasSpread(n.Args) {
		// `new C(...args)`: build the argument list array and construct from it.
		c.buildSpreadArray(n.Args) // [ctor, argsArray]
		c.emit(OpNewApply)
		c.emitU16(0)
		return
	}
	for _, arg := range n.Args {
		c.compileExpr(arg)
	}
	c.emit(OpNew)
	c.emitU16(uint16(len(n.Args)))
}

// compileTailCall emits a proper-tail-call form of `return f(args)` (OpTailCall /
// OpTailCallMethod, which reuse the current frame). It returns false — so the
// caller falls back to a normal call+return — for forms that are not eligible:
// spread arguments, optional chains, or super calls.
// tailIsCall reports whether n's tail position holds an ordinary call, following
// the tail positions of an expression: the right operand of && / ||, both arms
// of a ?:, and the last operand of a comma sequence.
func tailIsCall(n *Node) bool {
	if n == nil {
		return false
	}
	switch {
	case n.Kind == NCall, n.Kind == NTaggedTemplate:
		return true
	case n.Kind == NBinary && (n.Op == TokLand || n.Op == TokLor || n.Op == TokNullish):
		return tailIsCall(n.Right)
	case n.Kind == NTernary:
		return tailIsCall(n.Left) || tailIsCall(n.Right)
	case n.Kind == NSequence:
		return tailIsCall(n.Right)
	}
	return false
}

// compileTailReturn compiles `return n` as a proper tail call when n's tail
// position is a call, threading through the short-circuit right operand of
// && / || (`return a && f(x)` tail-calls f). Non-tail branches emit an ordinary
// return. Returns false when n has no tail-position call, so the caller compiles
// an ordinary return instead.
func (c *compiler) compileTailReturn(n *Node) bool {
	if !tailIsCall(n) {
		return false
	}
	switch {
	case n.Kind == NTaggedTemplate:
		c.compileTaggedTemplate(n, true) // frame-reusing TAIL_CALL / TAIL_CALL_METHOD
		return true
	case n.Kind == NCall:
		if c.compileTailCall(n) {
			return true
		}
		// A direct eval is not a tail call, but the same call site IS one when the
		// callee turns out not to be %eval%; mark it so OpEval can reuse the frame.
		if c.isDirectEvalCall(n) && !hasSpread(n.Args) && !containsOptional(n) {
			c.compileDirectEvalAt(n, true)
			c.emit(OpReturn)
			return true
		}
		// A call the tail-call lowering rejects (spread / optional / eval / super):
		// fall back to an ordinary evaluate-and-return.
		c.compileExpr(n)
		c.emit(OpReturn)
		return true
	case n.Kind == NBinary && n.Op == TokLand:
		c.compileExpr(n.Left)
		c.emit(OpDup)
		jmp := c.emitJump(OpJmpFalse)
		c.emit(OpPop)
		c.compileTailReturn(n.Right) // truthy: the tail position
		c.patchJump(jmp)
		c.emit(OpReturn) // falsy: return the short-circuit (left) value
		return true
	case n.Kind == NBinary && n.Op == TokLor:
		c.compileExpr(n.Left)
		c.emit(OpDup)
		jmp := c.emitJump(OpJmpTrue)
		c.emit(OpPop)
		c.compileTailReturn(n.Right)
		c.patchJump(jmp)
		c.emit(OpReturn)
		return true
	case n.Kind == NBinary && n.Op == TokNullish:
		// `a ?? b`: b is in tail position, a is not.
		c.compileExpr(n.Left)
		c.emit(OpDup)
		jmp := c.emitJump(OpJmpNotNullish)
		c.emit(OpPop)
		c.compileTailReturn(n.Right)
		c.patchJump(jmp)
		c.emit(OpReturn)
		return true
	case n.Kind == NTernary:
		// `c ? x : y`: both arms are in tail position.
		c.compileExpr(n.Cond)
		elseJump := c.emitJump(OpJmpFalse)
		c.compileTailBranch(n.Left)
		c.patchJump(elseJump)
		c.compileTailBranch(n.Right)
		return true
	case n.Kind == NSequence:
		// `a, b`: only b is in tail position (a is evaluated and discarded).
		c.compileExpr(n.Left)
		c.emit(OpPop)
		c.compileTailBranch(n.Right)
		return true
	}
	return false
}

// compileTailBranch compiles one tail-position sub-expression, as a tail call
// when it holds one, otherwise as an ordinary evaluate-and-return.
func (c *compiler) compileTailBranch(n *Node) {
	if !c.compileTailReturn(n) {
		c.compileExpr(n)
		c.emit(OpReturn)
	}
}

func (c *compiler) compileTailCall(n *Node) bool {
	if hasSpread(n.Args) || containsOptional(n) {
		return false
	}
	// A direct eval is not an ordinary call: it needs OpEval and the borrowed
	// caller-scope model (compileDirectEval). Never lower it as a tail call, or it
	// degrades to an indirect eval that cannot see the enclosing function's
	// locals, this, super, or private names.
	if c.isDirectEvalCall(n) {
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

	// Direct eval: `eval(src)` with the intrinsic `eval` still bound runs in the
	// caller's scope. Emit OpEval (which verifies the callee at run time). A spread
	// argument list is still a direct eval — `eval(...iter)` spreads the iterator
	// (consuming it fully) and evaluates its first element.
	if c.isDirectEvalCall(n) {
		// The eval code may contain `super`, which an arrow inherits from the
		// method enclosing it. The arrow has no [[HomeObject]] of its own, and the
		// eval frame borrows the running closure's — so the arrow must carry it
		// even though no `super` appears in the arrow's own source.
		if c.fn != nil && c.fn.isArrow {
			c.markInheritedSuper()
		}
		// Same reason inside a class constructor or element: the eval borrows this
		// function's [[HomeObject]], which it only receives when marked.
		for e := c; e != nil; e = e.enclosing {
			if e.fn == nil || e.fn.isArrow {
				continue
			}
			if e.fn.isClassCtor || e.fn.isClassElement {
				e.fn.usesSuper = true
			}
			break
		}
		if spread {
			c.compileDirectEvalSpread(n)
		} else {
			c.compileDirectEval(n)
		}
		return
	}

	// A callee resolved through an object environment (`with (o) { f() }`) is
	// called with that environment's binding object as `this` — the with-object,
	// not undefined. emitWithVarCallee leaves [this, fn], which is what
	// CALL_METHOD wants; a name that resolved lexically instead gets undefined.
	if n.Left != nil && n.Left.Kind == NIdent && c.nameIsWithRouted(n.Left.Str) && !spread {
		c.emitWithVarCallee(n.Left.Str)
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
