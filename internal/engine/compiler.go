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
	selfName    bool // a named function expression's immutable self-reference
	catchParam  bool // a simple identifier catch parameter (may coexist with a var)
}

type compiler struct {
	rt        *Runtime
	fn        *svFunc
	enclosing *compiler
	locals    []localVar
	upvalues  []upvalDesc
	// capCache memoises mayCapture; see compile_periter.go.
	capCache   map[*Node]bool
	scopeDepth int
	curStack   int
	err        error

	// Index of this function's constant pool, so adding a constant is a lookup
	// rather than a scan — see constant. Strings are keyed by their text because
	// the same text reaches here as more than one handle; everything else by its
	// raw bits.
	strConsts map[string]int
	rawConsts map[Value]int

	// isScript marks the top-level script compilation: top-level `var` and
	// function declarations bind on the global object rather than as locals.
	isScript bool

	// isEval marks eval-body compilation: top-level `var` bindings stay in the
	// eval frame (do not leak to the global object), matching strict eval.
	isEval bool

	// borrowed is the caller's lexical snapshot when compiling a direct eval body:
	// a free name found here (and not shadowed by an eval-local) resolves to the
	// caller binding, captured as an upvalue of the eval function. Nil otherwise.
	borrowed *evalScope

	// evalVarGlobal marks a global-scope direct eval, whose `var`/function
	// declarations bind on the global object rather than as eval-frame locals.
	evalVarGlobal bool

	// evalVarDynamic marks a sloppy function-scope direct eval whose caller has a
	// dynamic variable object (svFunc.dynamicVars): a `var` or hoisted function
	// declaration naming nothing the caller already binds is created there at run
	// time, so its store routes through the with-chain instead of taking an
	// eval-frame local slot that the caller could never see.
	evalVarDynamic bool

	// inParamExpr is set while compiling a function's parameter default/destructuring
	// expressions, whose evaluation scope is the parameter environment. A direct eval
	// there in a non-arrow function may not var-declare `arguments` (the parameter
	// scope already binds it): EvalDeclarationInstantiation throws a SyntaxError.
	inParamExpr bool

	// paramNames holds the current function's formal-parameter names (and, when an
	// arguments object is present, "arguments"). Annex B.3.3 skips the block-level
	// function var-hoisting extension for any name already bound as a parameter, so
	// such a declaration must not overwrite the parameter's slot.
	paramNames map[string]bool

	// isModule marks Module compilation (strict, top-level this undefined).
	isModule bool

	// importBindings maps a locally-bound imported name to the hidden local
	// holding the source module's namespace object plus the export to read from
	// it. Every *reference* compiles to that read, which is what makes an import
	// a live binding rather than a copy.
	importBindings map[string]importBinding

	// completionSlot holds the running script/eval completion value.
	completionSlot int

	// loops is the enclosing-loop stack for break/continue resolution
	// (ant sv_loop_t).
	loops []*loopCtx

	// withDepth > 0 inside a `with` block: unqualified names that would be
	// globals must resolve dynamically against the with-object chain.
	withDepth int

	// inheritedWith is set when compiling a function nested (lexically) inside a
	// `with` block. Unlike withDepth (the with-object is innermost, shadowing even
	// this function's own bindings), an inherited with-object sits *outside* this
	// function's scope: the function's own locals win, but any free name still
	// resolves against the captured with-objects before the enclosing scope. The
	// closure snapshots that scope chain at creation (see capturesWith / OpClosure).
	inheritedWith bool

	// realWith narrows inheritedWith to the case that an actual `with` block
	// encloses this function. The same routing is used for a function containing a
	// direct eval (svFunc.dynamicVars), where `eval` itself is NOT dynamically
	// scoped — only a real with-object could shadow it.
	realWith bool

	// inFieldInit is set while compiling an instance field initializer, whose
	// evaluation context has new.target === undefined even though the code is
	// emitted inline in the constructor (which does have a new.target).
	inFieldInit bool

	// classFields holds a class constructor's instance-field member nodes so the
	// ctor initializes them per-instance (base class: at body entry; derived:
	// right after super()). pendingClassFields/pendingClassDerived hand them from
	// compileClass to the child compiler compileFunc builds for the constructor.
	classFields         []*Node
	classDerived        bool
	pendingClassFields  []*Node
	pendingClassDerived bool

	// fieldKeys maps a computed instance-field member node to the class-scope local
	// slot ("*fkN*") holding its property key, which compileClass evaluates ONCE at
	// class definition (in source order). The constructor captures the slot as an
	// upvalue so emitInstanceFieldInit reads the pre-evaluated key rather than
	// re-evaluating the key expression per instance. pendingFieldKeys hands the map
	// to the constructor's child compiler.
	fieldKeys        map[*Node]int
	pendingFieldKeys map[*Node]int

	// staticSuper marks a static method / static block / static field initializer:
	// its home object is the constructor, so `super.x` reads from the parent
	// constructor (*superctor*) rather than the parent prototype (*superproto*).
	// pendingStaticSuper hands the flag from compileClass to the child compiler.
	staticSuper        bool
	pendingStaticSuper bool

	// classPrivateEnvs is a stack of private-name sets (each including the leading
	// '#') for the class bodies currently being compiled by this compiler — a
	// stack because a nested class expression is compiled on the same compiler as
	// its enclosing class, and both environments stay in scope. A private
	// reference must resolve to a name in some level here or in an enclosing
	// compiler.
	classPrivateEnvs []map[string]string

	// pendingLabel is a label awaiting the loop/statement it prefixes.
	pendingLabel string

	// usingStack is the local slot holding the current block's disposal-record
	// array (for `using` declarations), or -1 when not inside a using block.
	usingStack int

	// tryDepth > 0 inside a try/catch/finally body, where a `return` must run the
	// pending finally rather than tail-call away.
	tryDepth int

	// unwindKinds is the stack of live try/finally scopes at compile time (ant
	// c->unwind_kinds). break/continue crossing a `finally` must route through it
	// (OP_UNWIND_JMP) so the finally runs; crossing a plain try just pops handlers.
	unwindKinds []uint8
}

// unwind-scope kinds tracked while compiling (ant UNW_*).
const (
	unwTryCatch uint8 = iota
	unwTryFinally
	unwFinallyBody
)

func (c *compiler) unwindPush(k uint8) { c.unwindKinds = append(c.unwindKinds, k) }
func (c *compiler) unwindPop()         { c.unwindKinds = c.unwindKinds[:len(c.unwindKinds)-1] }

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
	unwindDepth    int // len(unwindKinds) when the loop was entered
}

// Compile compiles a parsed program to a bytecode function (script mode).
func (rt *Runtime) Compile(prog *Node, filename, source string) (*svFunc, error) {
	return rt.compileProgram(prog, filename, source, false, false)
}

// CompileEval compiles an eval body: `var` bindings stay frame-local.
func (rt *Runtime) CompileEval(prog *Node, filename, source string) (*svFunc, error) {
	return rt.compileProgram(prog, filename, source, true, false)
}

// CompileModule compiles a Module: it is strict, its top-level `this` is
// undefined, and its import/export declarations are handled (imports are not yet
// linked, so a module with static imports is out of scope here).
func (rt *Runtime) CompileModule(prog *Node, filename, source string) (*svFunc, error) {
	return rt.compileProgram(prog, filename, source, false, true)
}

// moduleLocalNames adds the top-level binding names a single module statement
// introduces (var/let/const, function, class, import locals, and the inner
// declaration of an `export <decl>`).
func moduleLocalNames(n *Node, out map[string]bool) {
	if n == nil {
		return
	}
	switch n.Kind {
	case NFunc:
		if n.Str != "" && n.Flags&fnArrow == 0 {
			out[n.Str] = true
		}
	case NClass:
		if n.Str != "" {
			out[n.Str] = true
		}
	case NVar:
		for _, d := range n.Args {
			if d != nil {
				collectBindingNames(d.Left, out)
			}
		}
	case NImportDecl:
		for _, spec := range n.Args {
			if spec.Right != nil {
				out[spec.Right.Str] = true
			}
		}
	case NExport:
		if n.Flags&(exDecl|exDefault) != 0 {
			moduleLocalNames(n.Left, out)
		}
	}
}

// validateModuleLexicalConflicts enforces a Module's top-level declaration
// static semantics: its lexically-declared names (let/const/class, an imported
// binding, AND a top-level function/generator/async declaration — which are
// lexical in a Module, unlike a Script) must be unique and must not also be
// var-declared. An `export <decl>` contributes its inner declaration's names.
func validateModuleLexicalConflicts(stmts []*Node) *SyntaxError {
	lex := map[string]bool{} // let/const/class/function/import locals
	varn := map[string]bool{}
	var walk func(n *Node) *SyntaxError
	addLex := func(name string) *SyntaxError {
		if lex[name] {
			return &SyntaxError{Msg: "duplicate lexical declaration '" + name + "' in module"}
		}
		lex[name] = true
		return nil
	}
	walk = func(n *Node) *SyntaxError {
		if n == nil {
			return nil
		}
		switch n.Kind {
		case NFunc:
			if n.Str != "" && n.Flags&fnArrow == 0 {
				return addLex(n.Str)
			}
		case NClass:
			if n.Str != "" {
				return addLex(n.Str)
			}
		case NVar:
			names := map[string]bool{}
			for _, d := range n.Args {
				if d != nil {
					collectBindingNames(d.Left, names)
				}
			}
			for nm := range names {
				if n.VarKind == VarVar {
					varn[nm] = true
				} else if e := addLex(nm); e != nil {
					return e
				}
			}
		case NImportDecl:
			for _, spec := range n.Args {
				if spec.Right != nil {
					if e := addLex(spec.Right.Str); e != nil {
						return e
					}
				}
			}
		case NExport:
			if n.Flags&(exDecl|exDefault) != 0 {
				return walk(n.Left)
			}
		}
		return nil
	}
	for _, s := range stmts {
		if e := walk(s); e != nil {
			return e
		}
	}
	for nm := range lex {
		if varn[nm] {
			return &SyntaxError{Msg: "'" + nm + "' is declared both lexically and as a var in module"}
		}
	}
	return nil
}

// validateModuleExports enforces a Module's ExportEntries static semantics: the
// exported names must be unique, and a local (no-`from`) named export must refer
// to a top-level binding of the module. A violation is an early SyntaxError.
func validateModuleExports(stmts []*Node) *SyntaxError {
	local := map[string]bool{}
	for _, s := range stmts {
		moduleLocalNames(s, local)
	}
	exported := map[string]bool{}
	add := func(name string) *SyntaxError {
		if exported[name] {
			return &SyntaxError{Msg: "duplicate export name '" + name + "'"}
		}
		exported[name] = true
		return nil
	}
	for _, s := range stmts {
		if s.Kind != NExport {
			continue
		}
		switch {
		case s.Flags&exStar != 0:
			// A bare `export *` forwards names that cannot be known here, but
			// `export * as z from` names z itself and can collide.
			if s.Flags&exNamespace != 0 && len(s.Args) > 0 && s.Args[0].Right != nil {
				if e := add(s.Args[0].Right.Str); e != nil {
					return e
				}
			}
		case s.Flags&exDefault != 0:
			if e := add("default"); e != nil {
				return e
			}
		case s.Flags&exDecl != 0:
			names := map[string]bool{}
			moduleLocalNames(s.Left, names)
			for n := range names {
				if e := add(n); e != nil {
					return e
				}
			}
		case s.Flags&exFrom == 0 && s.Flags&exNamed != 0:
			for _, spec := range s.Args {
				if spec.Right == nil {
					continue
				}
				if e := add(spec.Right.Str); e != nil {
					return e
				}
				if spec.Left != nil && !local[spec.Left.Str] {
					return &SyntaxError{Msg: "export '" + spec.Left.Str + "' is not defined in module"}
				}
			}
		}
	}
	return nil
}

// unwrapModuleStmts lowers a Module's top-level import/export declarations to
// ordinary statements the compiler already handles. Static imports and re-export
// forms (export … from / export *) require linking and are dropped here (a first
// cut targeting modules without static imports); `export <decl>` becomes the
// declaration, and `export default …` becomes its declaration (named func/class)
// or an evaluated expression.
func unwrapModuleStmts(stmts []*Node) []*Node {
	out := make([]*Node, 0, len(stmts))
	// An anonymous `export default function/generator/async function` is a
	// HoistableDeclaration: its binding is initialised before the body runs, so
	// the module's own code can call it above the declaration. It is lowered like
	// any other default export and then moved to the front, which is what
	// hoisting amounts to for a statement whose value is a function literal.
	var hoisted []*Node
	for _, s := range stmts {
		switch s.Kind {
		case NExport:
			switch {
			case s.Flags&exDecl != 0 && s.Left != nil:
				out = append(out, s.Left)
			case s.Flags&exDefault != 0 && s.Left != nil:
				d := s.Left
				if isDefaultDeclaration(d) {
					out = append(out, d) // named default declaration: hoist it
					break
				}
				// An anonymous function/class or an expression is bound to the
				// synthetic name "*default*", which the export table points at.
				// Without this the value was evaluated and thrown away, so the
				// module had no `default` export at all.
				anonFn := d.Kind == NFunc && d.Flags&(fnParen|fnArrow) == 0
				d.Flags |= fnParen
				if anonFn {
					hoisted = append(hoisted, mkDefaultBinding(d))
				} else {
					out = append(out, mkDefaultBinding(d))
				}
			}
			// export { … } / export … from / export * → no local binding to emit.
		case NImportDecl:
			// Static import needs linking; nothing to emit for the unlinked cut.
		default:
			out = append(out, s)
		}
	}
	return append(hoisted, out...)
}

// isDefaultDeclaration reports whether `export default X` declares X by name —
// a function or class DECLARATION. A parenthesised `(class C {})` is an
// expression: it binds nothing of its own, so the export goes through the
// synthetic "*default*" binding instead.
func isDefaultDeclaration(d *Node) bool {
	return (d.Kind == NFunc || d.Kind == NClass) && d.Str != "" &&
		d.Flags&(fnParen|fnInferredName) == 0
}

func (rt *Runtime) compileProgram(prog *Node, filename, source string, isEval, isModule bool) (*svFunc, error) {
	strict := prog.Flags&fnParseStrict != 0 || isModule
	c := &compiler{
		rt:       rt,
		isScript: true,
		isEval:   isEval,
		isModule: isModule,
		// A sloppy indirect eval runs in global scope, so its `var`/function
		// declarations bind on the global object (a strict eval keeps its own
		// variable environment; a direct eval sets this via its own compile path).
		evalVarGlobal: isEval && !isModule && !strict,
		usingStack:    -1,
		// A Module evaluates asynchronously (top-level await), so its body is
		// compiled and driven as an async coroutine.
		fn: &svFunc{name: "", filename: filename, source: source, isStrict: strict, isAsync: isModule},
	}
	if isModule {
		// `import.meta` is one object per Module, shared by every function in it.
		c.fn.metaCell = new(Value)
	}
	// A Module's export/import declarations are validated (ExportEntries static
	// semantics) then lowered to their inner declarations before hoisting so the
	// ordinary declaration machinery applies.
	var moduleStmts []*Node
	if isModule {
		if e := validateModuleLexicalConflicts(prog.Args); e != nil {
			return nil, e
		}
		if e := validateModuleExports(prog.Args); e != nil {
			return nil, e
		}
		moduleStmts = prog.Args // keep the declarations; the export table needs them
		prog.Args = unwrapModuleStmts(prog.Args)
	}
	// Reserve slot 0 for the completion value.
	c.completionSlot = c.addLocal("*completion*", false)
	c.emit(OpUndef)
	c.emitOpU16(OpPutLocal, uint16(c.completionSlot))

	// Bind `this`: the global object for a script, undefined for a module.
	thisSlot := c.addLocal("*this*", false)
	if isModule {
		c.emit(OpUndef)
	} else {
		c.emit(OpThis)
	}
	c.emitOpU16(OpPutLocal, uint16(thisSlot))

	// Global instantiation: pre-create var/function bindings so declarations
	// don't trip the strict "assignment to unresolvable" check, and undeclared
	// assignments in strict mode correctly throw ReferenceError. (Eval bodies
	// keep their vars frame-local, so no global pre-creation.)
	if c.isModule {
		// A Module's top-level var/function bindings are frame-locals (module
		// environment), not global-object properties: pre-declare them as locals
		// (like a function body) so references resolve to them and a `var x` does
		// not create globalThis.x.
		names := map[string]bool{}
		collectVarFuncNames(prog.Args, names)
		for name := range names {
			if c.resolveLocal(name) < 0 {
				c.addLocal(name, false)
			}
		}
		c.emitImportPrologue(moduleStmts)
	} else if !c.isEval {
		// GlobalDeclarationInstantiation: validate every top-level declaration and
		// only then create the global var/function bindings, so a Script that fails
		// leaves the global environment exactly as it found it.
		if e := rt.globalDeclarationInstantiation(prog, strict); e != nil {
			return nil, e
		}
	} else if c.evalVarGlobal {
		// Sloppy indirect eval: pre-create its var/function names on the global as
		// CONFIGURABLE properties (unlike a script's non-configurable vars), so a
		// bare `var x;` binds and eval-created globals are deletable.
		names := map[string]bool{}
		collectVarFuncNames(prog.Args, names)
		// A top-level `class` in eval code is a LEXICAL declaration of the eval's
		// own declarative environment — it binds nothing on the global object and
		// is gone once the eval returns.
		for _, lex := range topLevelLexicalNames(prog.Args) {
			delete(names, lex)
		}
		// EvalDeclarationInstantiation step 5.a.i: a sloppy eval will not create a
		// global var that a global LEXICAL declaration would shadow.
		for name := range names {
			if rt.lookupGlobalLex(name) != nil {
				return nil, &SyntaxError{Msg: "Identifier '" + name + "' has already been declared"}
			}
		}
		g := rt.objPtr(rt.global)
		for name := range names {
			if !g.hasOwn(name) {
				g.defineOwn(name, mkundef(), attrWritable|attrEnumerable|attrConfigurable)
			}
		}
	}

	c.checkBlockDeclConflicts(prog.Args, false)
	c.hoistLexicals(prog.Args)
	if !isEval && !isModule {
		// GlobalDeclarationInstantiation: a Script's top-level lexical bindings
		// belong to the global environment's DECLARATIVE record — not to the global
		// object — and outlive the Script. They keep their frame slots (so this
		// Script's own code and closures are unchanged); recording them here is
		// what publishes them to later Scripts and to eval.
		for i := range c.locals {
			lv := &c.locals[i]
			if lv.dead || !lv.blockScoped || lv.depth != c.scopeDepth || !borrowableName(lv.name) {
				continue
			}
			if c.fn.globalLex == nil {
				c.fn.globalLex = map[string]globalLexDecl{}
			}
			c.fn.globalLex[lv.name] = globalLexDecl{slot: i, isConst: lv.isConst}
		}
	}
	c.hoistFunctions(prog.Args, false)
	// Everything emitted so far is InitializeEnvironment, which for a Module runs
	// at LINK time rather than at evaluation.
	hoistEnd := len(c.fn.code)

	// A `using`/`await using` at the top level of a Module or Script disposes when
	// evaluation finishes — the same scaffolding a function body and a nested
	// block use, and the reason it is emitted below hoistEnd: the disposal stack
	// must be created when the module is EVALUATED, not when it is linked.
	bodyUsing := blockHasUsing(prog.Args)
	dispose, disposeSuppressed := OpUsingDispose, OpUsingDisposeSuppressed
	if blockHasAwaitUsing(prog.Args) {
		// Only a Module can carry `await using` here; a Script has no top-level
		// await, so the parser has already rejected it.
		dispose, disposeSuppressed = OpUsingDisposeAsync, OpUsingDisposeAsyncSuppressed
		c.fn.usesAwait = true
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
	c.compileStmts(prog.Args)
	if c.err != nil {
		return nil, c.err
	}
	if bodyUsing {
		// Normal completion: dispose, leaving the completion value untouched.
		c.emit(OpTryPop)
		c.emitOpU16(OpGetLocal, uint16(usingStackLocal))
		c.emit(dispose)
		c.emit(OpPop)
		usingEnd = c.emitJump(OpJmp)
		// Abrupt completion (throw): dispose-suppressed, then re-throw.
		c.patchJump(usingCatch)
		c.emit(OpCatch)
		c.emitU32(0)
		c.emitOpU16(OpPutLocal, uint16(usingErrLocal))
		c.emitOpU16(OpGetLocal, uint16(usingStackLocal))
		c.emitOpU16(OpGetLocal, uint16(usingErrLocal))
		c.emit(disposeSuppressed)
		c.emit(OpThrow)
		c.patchJump(usingEnd)
		c.usingStack = savedUsingStack
	}
	if isModule {
		// Resolve each export to the top-level slot holding it, while the module
		// scope is still open.
		c.fn.moduleExports = map[string]int{}
		c.fn.moduleIndirect = moduleIndirectExports(moduleStmts)
		for exported, local := range moduleExportEntries(moduleStmts) {
			if slot := c.resolveLocal(local); slot >= 0 {
				c.fn.moduleExports[exported] = slot
				continue
			}
			// The exported name is not a binding of this module's own — it is one
			// this module imported. ParseModule turns `import {a} from "m"; export
			// {a}` (and the `import * as ns` form) into an INDIRECT export entry
			// naming m, so the export denotes m's binding, not a local copy: two
			// modules re-exporting the same import stay unambiguous, and a
			// re-exported namespace object is the very same object.
			for _, mi := range c.fn.moduleImports {
				if mi.local != local {
					continue
				}
				imported := mi.importName
				if imported == "" {
					imported = "*" // `import * as ns`: the whole namespace
				}
				c.fn.moduleIndirect[exported] = indirectExport{specifier: mi.specifier, importName: imported}
				break
			}
		}
		c.fn.moduleStarFrom = moduleStarSpecifiers(moduleStmts)
		// Replace the list emitImportPrologue built (imports only, in its own
		// order) with the full source-ordered one.
		c.fn.moduleRequests = moduleRequestSpecifiers(moduleStmts)
	}
	// Return the completion value.
	if isModule {
		// A Module's body is driven as an async coroutine, so whatever it returns
		// becomes its completion promise's value. A Module has no observable
		// completion value, and resolving with a thenable would cost extra ticks
		// that module-evaluation ordering is sensitive to.
		c.emit(OpUndef)
	} else {
		c.emitOpU16(OpGetLocal, uint16(c.completionSlot))
	}
	c.emit(OpReturn)

	c.fn.maxLocals = len(c.locals)
	if c.fn.maxStack < 8 {
		c.fn.maxStack = 8
	}
	if isModule {
		// Split the prologue off as its own synchronous function over the SAME
		// locals (the module environment): both frames share the record's slice, so
		// a closure hoisted at link time and the body that runs later read and
		// write the very same bindings. Jump targets are absolute, so the body
		// keeps the whole code array and simply starts past the prologue.
		hoist := *c.fn
		hoist.code = append(append([]byte(nil), c.fn.code[:hoistEnd]...), byte(OpUndef), byte(OpReturn))
		hoist.isAsync = false
		hoist.startIP = 0
		c.fn.moduleHoistFn = &hoist
		c.fn.startIP = hoistEnd
	}
	return c.fn, nil
}

func (c *compiler) errorf(format string, args ...any) {
	if c.err == nil {
		c.err = &CompileError{Msg: fmt.Sprintf(format, args...)}
	}
}

// syntaxErrorf records an early (syntactic) error as a SyntaxError rather than a
// CompileError — for constructs the grammar forbids that goant happens to detect
// while compiling (an unresolved break/continue label, `super` outside a method).
// Unlike an unsupported-feature CompileError, these are genuine ECMAScript early
// errors, so tests expect a SyntaxError.
func (c *compiler) syntaxErrorf(format string, args ...any) {
	if c.err == nil {
		c.err = &SyntaxError{Msg: fmt.Sprintf(format, args...)}
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
		// A direct eval body has no enclosing compiler, but its free names may
		// resolve to borrowed caller bindings, which act as its upvalues.
		return c.resolveBorrowed(name)
	}
	if slot := c.enclosing.resolveLocal(name); slot >= 0 {
		c.enclosing.locals[slot].captured = true
		lv := c.enclosing.locals[slot]
		return c.addUpvalue(slot, true, lv.isConst, lv.selfName, name)
	}
	if uv := c.enclosing.resolveUpvalue(name); uv >= 0 {
		u := c.enclosing.upvalues[uv]
		return c.addUpvalue(uv, false, u.isConst, u.selfName, name)
	}
	return -1
}

func (c *compiler) addUpvalue(index int, isLocal, isConst, selfName bool, name string) int {
	for i, u := range c.upvalues {
		if u.index == index && u.isLocal == isLocal {
			return i
		}
	}
	c.upvalues = append(c.upvalues, upvalDesc{index: index, isLocal: isLocal, isConst: isConst, selfName: selfName, name: name})
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

// resolveFunctionVar finds a function-scoped (non-block-scoped) local by name,
// skipping any intervening block-scoped binding (let/const/class or a catch
// parameter) that shadows it. The Annex B.3.3/B.3.4 block-function var update
// targets the function's variable environment, so a `catch (f)` parameter must
// not intercept the write to the outer `var f`.
func (c *compiler) resolveFunctionVar(name string) int {
	for i := len(c.locals) - 1; i >= 0; i-- {
		lv := c.locals[i]
		if lv.dead || lv.blockScoped || lv.name != name {
			continue
		}
		return i
	}
	return -1
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
	case NExport, NImportDecl:
		// import/export are valid only at a Module's top level, where
		// unwrapModuleStmts lowers them before compilation. Reaching here means a
		// nested/misplaced import|export (or one in a Script): an early SyntaxError.
		c.syntaxErrorf("'import' and 'export' may only appear at the top level of a module")
		return
	case NFunc:
		// Function declarations are hoisted (bound before the body runs). A
		// parenthesized function *expression* statement contributes a completion
		// value (used by eval); a bare one is a no-op. An arrow is never a
		// declaration — a bare arrow-expression statement (`(a, a) => {}`) must
		// still be compiled so its early errors (duplicate params, a lexical
		// redeclaration in the body, `super` misuse) surface.
		if n.Flags&(fnParen|fnArrow) != 0 {
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
		// Class declaration: compile and bind to the (lexically scoped) class name.
		c.compileClass(n)
		c.bindClassDecl(n.Str)
		return
	case NVar:
		c.compileVarDecl(n)
	case NBlock:
		if blockHasUsing(n.Args) {
			c.compileBlockWithUsing(n)
			return
		}
		c.scopeDepth++
		c.checkBlockDeclConflicts(n.Args, true)
		c.hoistLexicals(n.Args)
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
		// Proper tail call: `return f(args)` in strict code (outside any try and not
		// in a generator/async body) reuses the current frame instead of recursing.
		if c.fn.isStrict && c.tryDepth == 0 && !c.fn.isGenerator && !c.fn.isAsync &&
			!c.fn.isClassCtor && // a ctor's return value goes through the [[Construct]] result rule
			n.Right != nil && c.compileTailReturn(n.Right) {
			return
		}
		if n.Right != nil {
			c.compileExpr(n.Right)
			// ReturnStatement : return Expression — in an async GENERATOR the value
			// is Awaited (13.10.1 step 3), which costs a tick. A bare `return;`, and
			// falling off the end, do not.
			if c.fn.isAsync && c.fn.isGenerator {
				c.emit(OpAwait)
			}
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
func blockHasAwaitUsing(stmts []*Node) bool {
	for _, s := range stmts {
		d := s
		if s != nil && s.Kind == NExport && s.Left != nil {
			d = s.Left
		}
		if d != nil && d.Kind == NVar && d.VarKind == VarAwaitUsing {
			return true
		}
	}
	return false
}

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
	// A block with a `using` declaration still enforces the same lexical
	// redeclaration early errors as an ordinary block (`{ using f = …; var f; }`).
	c.checkBlockDeclConflicts(n.Args, true)
	c.hoistFunctions(n.Args, true)

	c.emit(OpArray)
	c.emitU16(0)
	stackLocal := c.addLocal("*using*", false)
	c.emitOpU16(OpPutLocal, uint16(stackLocal))
	errLocal := c.addLocal("*usingerr*", false)
	savedUsing := c.usingStack
	c.usingStack = stackLocal

	// A block holding an `await using` disposes in an ASYNC disposal environment:
	// every disposer's result is awaited, so a rejecting async disposer is
	// observed at all.
	disposeOp, disposeSuppressedOp := OpUsingDispose, OpUsingDisposeSuppressed
	if blockHasAwaitUsing(n.Args) {
		disposeOp, disposeSuppressedOp = OpUsingDisposeAsync, OpUsingDisposeAsyncSuppressed
		c.fn.usesAwait = true
	}

	catchHandler := c.emitJump(OpTryPush)
	c.compileStmts(n.Args)
	c.emit(OpTryPop)

	// Normal completion: dispose all resources, discard the completion value.
	c.emitOpU16(OpGetLocal, uint16(stackLocal))
	c.emit(disposeOp)
	c.emit(OpPop)
	endJump := c.emitJump(OpJmp)

	// Abrupt completion (throw): capture the error, dispose-suppressed, re-throw.
	c.patchJump(catchHandler)
	c.emit(OpCatch)
	c.emitU32(0)
	c.emitOpU16(OpPutLocal, uint16(errLocal))
	c.emitOpU16(OpGetLocal, uint16(stackLocal))
	c.emitOpU16(OpGetLocal, uint16(errLocal))
	c.emit(disposeSuppressedOp)
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
			// A `using` binding is immutable, like a const: assigning to it is a
			// TypeError, since the disposal at scope exit must reach the resource the
			// declaration captured.
			slot := c.declareLexical(decl.Left.Str, true)
			// TDZ: the binding is dead until its initializer completes, so a
			// self-reference in the initializer (`using x = x`) is a ReferenceError.
			c.emit(OpEmpty)
			c.emitOpU16(OpPutLocal, uint16(slot))
			c.emitOpU16(OpGetLocal, uint16(c.usingStack)) // entries
			if decl.Right != nil {
				// NamedEvaluation: `using x = () => {}` names the anonymous value "x".
				nameAnonExpr(decl.Right, decl.Left.Str)
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
	// Top-level `var` binds globally; a global-scope direct eval's `var` also binds
	// on the global object; `let`/`const` (and any binding inside a function) are
	// frame locals.
	asGlobal := n.VarKind == VarVar && ((c.isScript && !c.isEval && !c.isModule) || c.evalVarGlobal)
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
		// Sloppy direct eval: a `var` naming an existing caller binding updates that
		// binding in place (var hoisting into the caller's VariableEnvironment)
		// rather than creating an eval-frame local that would shadow it. A strict
		// eval has its own variable environment, so its `var` never leaks.
		if n.VarKind == VarVar && c.borrowed != nil && !c.fn.isStrict {
			uv := -1
			if c.evalVarUpdatesBorrowed(name) {
				uv = c.resolveBorrowed(name)
			}
			if uv >= 0 {
				if decl.Right != nil {
					c.compileExpr(decl.Right)
					c.emitOpU16(OpPutUpval, uint16(uv))
				}
				continue
			}
			// A name the caller does not bind was created on its variable object by
			// EvalDeclarationInstantiation; the initializer writes there.
			if c.evalVarDynamic && c.resolveLocal(name) < 0 {
				if decl.Right != nil {
					c.compileExpr(decl.Right)
					c.emitWithVar(OpWithPutVar, name)
				}
				continue
			}
		}
		if asGlobal {
			// Inside `with(obj)` the Reference is resolved BEFORE the initializer, so
			// an initializer that deletes the with-object's property still writes
			// back through the environment it named.
			if decl.Right != nil && c.withDepth > 0 && c.resolveLocal(name) < 0 {
				c.emitWithVarBase(name)
				c.compileExpr(decl.Right)
				c.emitWithVarRef(OpWithPutVar, name)
				c.emit(OpPop)
				continue
			}
			if decl.Right != nil {
				c.compileExpr(decl.Right)
				// The initializer assignment resolves the name to the nearest binding:
				// a block-scoped local shadowing it (e.g. a catch parameter) takes
				// precedence over the global var binding — Annex B.3.5:
				// `catch (e) { var e = … }` assigns to the catch parameter, leaving the
				// outer/global `e` untouched.
				if slot := c.resolveLocal(name); slot >= 0 {
					c.emitOpU16(OpPutLocal, uint16(slot))
				} else {
					c.emitGlobalPut(name)
				}
			}
			// A bare `var x;` at top level leaves any existing global intact.
			continue
		}
		var slot int
		isLexical := n.VarKind == VarLet || n.VarKind == VarConst
		if isLexical {
			// Reuse the slot pre-declared by hoistLexicals (leaving the binding in
			// TDZ until this store) when present; else declare it now.
			if s := c.lexicalAtCurrentDepth(name); s >= 0 {
				slot = s
			} else {
				slot = c.declareLexical(name, n.VarKind == VarConst)
			}
		} else {
			slot = c.declareVar(name, false)
		}
		// A `var` initializer inside a `with` block assigns through the object
		// environment: if the with-object binds the name the write lands there,
		// otherwise it falls back to this hoisted function-scope slot (emitWithVar
		// encodes the slot as the local fallback). `let`/`const` are the block's own
		// bindings and are never shadowed by the with-object.
		if !isLexical && c.withDepth > 0 {
			if decl.Right == nil {
				continue // bare `var x;` is declaration-only — no assignment
			}
			// The Reference is resolved BEFORE the initializer runs, so an
			// initializer that deletes the with-object's property still writes back
			// through the environment it named (creating the property again).
			c.emitWithVarBase(name)
			c.compileExpr(decl.Right)
			c.emitWithVarRef(OpWithPutVar, name)
			c.emit(OpPop)
			continue
		}
		if decl.Right == nil && !isLexical {
			// A bare `var x;` does not re-initialise an existing binding: the name was
			// already created (as undefined) by FunctionDeclarationInstantiation, and
			// a hoisted function declaration of the same name may have filled it.
			continue
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
	c.resetCompletion()
	c.compileExpr(n.Cond)
	elseJump := c.emitJump(OpJmpFalse)
	c.compileIfBranch(n.Left)
	if n.Right != nil {
		endJump := c.emitJump(OpJmp)
		c.patchJump(elseJump)
		c.compileIfBranch(n.Right)
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
		// `undefined` is not reserved: a binding named `undefined` (e.g. a
		// parameter or `var undefined`) shadows the global, so a reference must
		// resolve to it. With no such binding it is the undefined literal.
		if slot := c.resolveLocal("undefined"); slot >= 0 {
			c.emitOpU16(OpGetLocal, uint16(slot))
		} else if uv := c.resolveUpvalue("undefined"); uv >= 0 {
			c.emitOpU16(OpGetUpval, uint16(uv))
		} else {
			c.emit(OpUndef)
		}
	case NRegexp:
		// A regular expression literal is an early error: validate the pattern at
		// compile time so an invalid one is a (parse-phase) SyntaxError rather than
		// a deferred runtime throw. The literal still creates a fresh RegExp on each
		// evaluation via OpRegexp.
		if _, e := c.rt.newRegExp(n.Str, n.Aux); e != nil {
			msg := "invalid regular expression: /" + n.Str + "/" + n.Aux
			if mv, _ := c.rt.getField(e.Value, "message"); mv.IsString() {
				msg = string(c.rt.strBytes(mv))
			}
			c.syntaxErrorf("%s", msg)
		}
		c.emitConst(c.rt.internString(n.Str))
		c.emitConst(c.rt.internString(n.Aux))
		c.emit(OpRegexp)
	case NImportMeta:
		// import.meta: the module's own meta object (a per-module ordinary object,
		// the same one on every access). Runtime kind 3 of OpSpecialObj.
		c.emit(OpSpecialObj)
		c.emitByte(3)
	case NGlobalThis:
		c.emit(OpGlobal)
	case NNewTarget:
		// Inside an instance field initializer new.target is always undefined,
		// even though the code runs inline in the constructor.
		if c.inFieldInit {
			c.emit(OpUndef)
			break
		}
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
		c.compileTaggedTemplate(n, false)
	case NTypeof:
		// `typeof x` where x is a bare (possibly-undeclared) global must not throw:
		// read it leniently so an absent binding yields "undefined".
		// The lenient path is only for a name with no binding at all. An imported
		// name IS declared, so it must go through the normal read — a binding still
		// in its temporal dead zone has to throw, not report "undefined".
		if r := n.Right; r != nil && r.Kind == NIdent && !c.isImportName(r.Str) &&
			c.resolveLocal(r.Str) < 0 && c.resolveUpvalue(r.Str) < 0 {
			if c.nameIsWithRouted(r.Str) {
				// The name may still be bound by a with-object (or a variable object a
				// direct eval wrote to); only the global fallback is lenient.
				c.emitWithVarLenient(OpWithGetVar, r.Str)
			} else {
				idx := c.constant(c.rt.internString(r.Str))
				c.emit(OpGetGlobalUndef)
				c.emitU32(uint32(idx))
				// The lenient read is not cached — a `typeof` of an undeclared
				// name is not a hot path, and slot 0 belongs to another site.
				c.emitU16(icNoSlot)
			}
		} else {
			c.compileExpr(n.Right)
		}
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
		c.fn.usesAwait = true
		c.emit(OpAwait)
	case NImport:
		// Dynamic import(): evaluate the specifier (and optional options/attributes
		// argument), then OpImport produces the module-request promise.
		c.compileExpr(n.Right)
		if n.Left != nil {
			c.compileExpr(n.Left)
		} else {
			c.emit(OpUndef)
		}
		c.emit(OpImport)
	case NSpread:
		// A spread element (`...x`) is only valid inside an array literal, argument
		// list, or object literal (all handled before reaching here). Anywhere else
		// — e.g. `...x => x`, a bare `...x` expression — it is a SyntaxError.
		c.syntaxErrorf("Unexpected token '...'")
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

// nameIsWithRouted reports whether a name reference must be resolved dynamically
// against the with-object chain. Directly inside a `with` block (withDepth > 0)
// the with-object is innermost, so every name routes. In a function nested inside
// a `with` (inheritedWith), the function's own locals are innermost and win — only
// a free name (not a local of this function) routes against the captured objects.
func (c *compiler) nameIsWithRouted(name string) bool {
	if c.withDepth > 0 {
		return true
	}
	return c.inheritedWith && c.resolveLocal(name) < 0
}

// storeKeepsStaticBinding reports whether a STORE to a with-routed name must
// keep its static semantics anyway: the name is a const or a named function
// expression's immutable self-reference. Routing would write through the
// with-chain's lexical fallback, which cannot express "throw" or "silently
// ignore". It applies only to an INHERITED chain (withDepth == 0), whose objects
// sit outside this function and so can never legitimately shadow the binding;
// directly inside a `with` the object is innermost and does shadow it.
func (c *compiler) storeKeepsStaticBinding(slot, uv int) bool {
	if c.withDepth > 0 {
		return false
	}
	if slot >= 0 {
		return c.locals[slot].isConst || c.locals[slot].selfName
	}
	return uv >= 0 && (c.upvalues[uv].isConst || c.upvalues[uv].selfName)
}

func (c *compiler) compileIdentLoad(n *Node) {
	// A private name is a valid primary only as the left operand of `#x in obj`
	// (handled in compileBinary before the operand reaches here). Reaching this
	// point means `#x` was used in some other expression position (`y in #x`,
	// `(#x)`, `for (#x in …)`), which is a SyntaxError.
	if len(n.Str) > 0 && n.Str[0] == '#' {
		c.syntaxErrorf("Private field '%s' must be used as `%s in obj`", n.Str, n.Str)
		return
	}
	// An imported name is not a local: it reads through the source module's
	// namespace so the binding stays live.
	if b, ok := c.lookupImport(n.Str); ok {
		c.compileImportRead(b)
		return
	}
	// Inside a `with`, every unqualified name is resolved dynamically against the
	// with-object(s) first (emitWithVar carries the lexical fallback), so a local
	// or upvalue of the same name can be shadowed by a with-object property.
	if c.nameIsWithRouted(n.Str) {
		c.emitWithVar(OpWithGetVar, n.Str)
		return
	}
	if slot := c.resolveLocal(n.Str); slot >= 0 {
		c.emitOpU16(OpGetLocal, uint16(slot))
		return
	}
	if uv := c.resolveUpvalue(n.Str); uv >= 0 {
		c.emitOpU16(OpGetUpval, uint16(uv))
		return
	}
	c.emitGlobalGet(n.Str)
}

// compileWith compiles a `with (obj) stmt`: unqualified names inside resolve
// dynamically against obj (then the global) via WITH_GET_VAR/WITH_PUT_VAR.
func (c *compiler) compileWith(n *Node) {
	c.resetCompletion()
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
	c.emitWithVarFlags(op, name, 0)
}

// emitWithVarLenient emits a with-scoped read whose global fallback yields
// undefined instead of throwing (flag 0x40) — the `typeof` of a name that is
// neither a local, an upvalue, nor a property of any with-object.
func (c *compiler) emitWithVarLenient(op Opcode, name string) {
	c.emitWithVarFlags(op, name, 0x40)
}

// emitWithVarBase resolves a with-scoped name to its base ALONE (flags 0xa0:
// reference mode, no value read), for a plain `=` whose Reference is created
// before the right-hand side is evaluated. It must not perform the [[Get]] a
// full reference-mode read would — that getter is not part of an assignment.
func (c *compiler) emitWithVarBase(name string) {
	c.emitWithVarFlags(OpWithGetVar, name, 0x80|0x20)
}

// emitWithVarCallee reads a with-scoped name as a CALLEE, leaving [this, fn]:
// the `this` of a call through an object environment is that environment's
// binding object (WithBaseObject), and undefined when the name resolved
// lexically instead. Flags 0x90: reference mode, with the absent base spelled
// undefined rather than the write-back marker.
func (c *compiler) emitWithVarCallee(name string) {
	c.emitWithVarFlags(OpWithGetVar, name, 0x80|0x10)
}

func (c *compiler) emitWithVarFlags(op Opcode, name string, flags byte) {
	idx := c.constant(c.rt.internString(name))
	// A `with` name access checks the with-object(s) first at run time; when none
	// binds the name it falls back to the ordinary lexical resolution. Encode that
	// fallback in the spare operand bytes: kind 1 = local slot, 2 = upvalue index,
	// 0 = global. Without it, a name that is also a local/upvalue would resolve to
	// that binding statically and the with-object could never shadow it.
	var fbKind byte
	var fbIdx int
	if slot := c.resolveLocal(name); slot >= 0 {
		fbKind, fbIdx = 1, slot
	} else if uv := c.resolveUpvalue(name); uv >= 0 {
		fbKind, fbIdx = 2, uv
	}
	c.emit(op)
	c.emitU32(uint32(idx))
	c.emitU16(uint16(fbIdx))
	c.emitByte(fbKind | flags)
}

// emitWithVarRef emits a with-scoped access in *reference* mode (fallback-kind
// high bit 0x80): OpWithGetVar pushes the resolved base object (or the tEmpty
// marker for the lexical fallback) beneath the value, and OpWithPutVar consumes
// that same base to write back — so a compound assignment reuses one Reference
// across its read and write, as PutValue requires even when a getter deletes the
// binding in between.
func (c *compiler) emitWithVarRef(op Opcode, name string) {
	idx := c.constant(c.rt.internString(name))
	var fbKind byte = 0x80
	var fbIdx int
	if slot := c.resolveLocal(name); slot >= 0 {
		fbKind, fbIdx = 0x80|1, slot
	} else if uv := c.resolveUpvalue(name); uv >= 0 {
		fbKind, fbIdx = 0x80|2, uv
	}
	c.emit(op)
	c.emitU32(uint32(idx))
	c.emitU16(uint16(fbIdx))
	c.emitByte(fbKind)
}

func (c *compiler) compileBinary(n *Node) {
	// Private brand check `#x in obj`: the LHS is a private name, keyed as its
	// string form rather than loaded as a variable.
	if n.Op == TokIn && n.Left != nil && n.Left.Kind == NIdent &&
		len(n.Left.Str) > 0 && n.Left.Str[0] == '#' {
		if !c.privateNameDeclared(n.Left.Str) {
			c.syntaxErrorf("Private field %s must be declared in an enclosing class", quotedName(n.Left.Str))
			return
		}
		c.emitConst(c.rt.internString(c.privateKey(n.Left.Str)))
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
	// `delete x` (unqualified reference): a resolvable environment binding
	// (local/upvalue/parameter, or a script-level var/function which is a
	// non-configurable global property) can't be removed, but a plain
	// global-object property (an implicit global) can. Resolve it: a binding
	// yields false; otherwise delete through the global object, whose
	// configurability decides the result (var/function → false, implicit → true).
	if target != nil && target.Kind == NIdent && !c.nameIsWithRouted(target.Str) {
		name := target.Str
		if c.resolveLocal(name) >= 0 || c.resolveUpvalue(name) >= 0 {
			c.emit(OpFalse)
			return
		}
		c.emit(OpGlobal)
		c.emitConst(c.rt.internString(name))
		c.emit(OpDelete)
		return
	}
	// `undefined` is an IdentifierReference to a (non-configurable) property of the
	// global object, not a literal keyword, so `delete undefined` deletes through
	// the global and yields false. goant parses bare `undefined` as NUndef, so
	// handle it here rather than letting it fall to the `delete <literal>` → true
	// case below. A local `var undefined` shadow (sloppy) resolves to false too.
	if target != nil && target.Kind == NUndef && c.withDepth == 0 {
		if c.resolveLocal("undefined") >= 0 || c.resolveUpvalue("undefined") >= 0 {
			c.emit(OpFalse)
			return
		}
		c.emit(OpGlobal)
		c.emitConst(c.rt.internString("undefined"))
		c.emit(OpDelete)
		return
	}
	// `delete x` inside a `with`: the name may be a with-object property, which is
	// deleted from that object; otherwise it falls back to a global-object delete.
	if target != nil && target.Kind == NIdent && c.nameIsWithRouted(target.Str) {
		idx := c.constant(c.rt.internString(target.Str))
		c.emit(OpWithDelVar)
		c.emitU32(uint32(idx))
		return
	}
	// `delete <expression>` still EVALUATES the operand and then yields true,
	// since the result is not a Reference: `delete foo()` calls foo. Only a
	// literal-shaped operand can be skipped, and evaluating one is harmless
	// anyway, so everything left goes through the same path.
	if target != nil {
		c.compileExpr(target)
		c.emit(OpPop)
	}
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
	if target != nil && target.Kind == NCall {
		// Annex B web-compat (sloppy): a CallExpression update target evaluates the
		// call and then throws a ReferenceError — GetValue on the non-reference
		// throws before the value is coerced to a number.
		c.compileExpr(target)
		c.emit(OpPop)
		c.emitCallTargetRefError()
		return
	}
	if target == nil || target.Kind != NIdent {
		c.errorf("only identifier increment/decrement is supported (slice)")
		return
	}
	// The prefix marker is bit 0; a parenthesized `(++x)` also carries fnParen
	// (bit 9), so test the bit rather than equality (else `(++x)` mis-compiles as
	// a postfix update, yielding the pre-increment value).
	prefix := n.Flags&1 != 0
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

	// A with-routed update resolves its Reference once and reads and writes back
	// through it, as PutValue requires — the getter may delete the binding (or a
	// direct eval may add a nearer one) between the read and the write, and the
	// store still belongs to the base the read used.
	if c.nameIsWithRouted(name) && !c.storeKeepsStaticBinding(slot, uv) {
		c.emitWithVarRef(OpWithGetVar, name) // [base, old]
		if prefix {
			c.emit(incOp)                        // [base, new]
			c.emitWithVarRef(OpWithPutVar, name) // [new]
			return
		}
		c.emit(OpNeg) // ToNumeric, as below
		c.emit(OpNeg)
		c.emit(OpDup)                        // [base, num, num]
		c.emit(incOp)                        // [base, num, new]
		c.emit(OpSwapUnder)                  // [num, base, new]
		c.emitWithVarRef(OpWithPutVar, name) // [num, new]
		c.emit(OpPop)                        // [num]
		return
	}

	load := func() {
		switch {
		case c.nameIsWithRouted(name):
			c.emitWithVar(OpWithGetVar, name)
		case slot >= 0:
			c.emitOpU16(OpGetLocal, uint16(slot))
		case uv >= 0:
			c.emitOpU16(OpGetUpval, uint16(uv))
		default:
			c.emitGlobalGet(name)
		}
	}
	// An update operator writes through the same binding an assignment would, so
	// a const (or a named function expression's self-name) rejects it identically.
	constTarget := (slot >= 0 && (c.locals[slot].isConst || c.locals[slot].selfName)) ||
		(uv >= 0 && (c.upvalues[uv].isConst || c.upvalues[uv].selfName))
	storeKeep := func() { // leaves the stored value on the stack
		switch {
		case constTarget:
			if (slot >= 0 && c.locals[slot].isConst) || (uv >= 0 && c.upvalues[uv].isConst) || c.fn.isStrict {
				c.emitConstAssignError()
			}
		case c.nameIsWithRouted(name):
			c.emit(OpDup)
			c.emitWithVar(OpWithPutVar, name)
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
		case constTarget:
			if (slot >= 0 && c.locals[slot].isConst) || (uv >= 0 && c.upvalues[uv].isConst) || c.fn.isStrict {
				c.emitConstAssignError()
			} else {
				c.emit(OpPop) // sloppy self-name update: silently discarded
			}
		case c.nameIsWithRouted(name):
			c.emitWithVar(OpWithPutVar, name)
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
	// x++: result = ToNumeric(x); x = that ± 1. The update uses ToNumeric (which
	// preserves BigInt), not ToNumber — double negation implements it fp-exactly
	// (-(-x) === x for every float), coercing strings/objects via the first neg's
	// ToNumber while keeping BigInt (OpNeg has a BigInt branch). OpUplus would
	// throw on BigInt. incOp (OpInc/OpDec) then adds the unit, also BigInt-aware.
	load()
	c.emit(OpNeg)
	c.emit(OpNeg)
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

// nameDefaultTarget applies NamedEvaluation to a destructuring default: when the
// binding/assignment target is a plain identifier and the default initializer is
// an anonymous function or class, the function takes the identifier's name
// (`[x = () => {}] = []` ⇒ x.name === "x"). Member targets take no name.
func nameDefaultTarget(target, defExpr *Node) {
	if target != nil && target.Kind == NIdent {
		nameAnonExpr(defExpr, target.Str)
	}
}

// emitConstAssignError emits a throw of `TypeError: Assignment to constant
// variable.` leaving the (already-evaluated) assigned value on the stack.
func (c *compiler) emitConstAssignError() {
	idx := c.constant(c.rt.internString("Assignment to constant variable."))
	c.emit(OpThrowError)
	c.emitU32(uint32(idx))
	c.emitByte(0) // TypeError
}

// emitCallTargetRefError throws the runtime ReferenceError for a sloppy-mode
// CallExpression assignment/update target (Annex B web-compat), after the target
// and any operand have already been evaluated for their side effects.
func (c *compiler) emitCallTargetRefError() {
	c.emit(OpUndef) // nominal expression value (preempted by the throw below)
	idx := c.constant(c.rt.internString("Invalid assignment target"))
	c.emit(OpThrowError)
	c.emitU32(uint32(idx))
	c.emitByte(1) // ReferenceError
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
	if n.Left != nil && n.Left.Kind == NCall {
		// A CallExpression target of a LOGICAL assignment (&&= ||= ??=) is an early
		// SyntaxError: the web-compat tolerance that turns `a() = b` / `a() += b`
		// into a runtime reference error does not extend to logical assignment.
		if _, isLogical := logicalAssignJmp(n.Op); isLogical {
			c.syntaxErrorf("Invalid left-hand side in assignment")
			return
		}
		// Annex B web-compat (sloppy): a CallExpression assignment target evaluates
		// the call (the "reference") and then throws a ReferenceError before the RHS
		// is evaluated or the target coerced — a plain `=` reaches PutValue, and a
		// compound assignment's GetValue on the non-reference throws, both before the
		// right-hand side runs.
		c.compileExpr(n.Left) // evaluate the call for its side effects
		c.emit(OpPop)
		c.emitCallTargetRefError()
		return
	}
	// Bare `undefined` is an IdentifierReference to a non-writable property of the
	// global object, not a literal keyword — goant just parses it as NUndef — so
	// assigning to it goes through the global and is a strict-mode TypeError. A
	// local `var undefined` shadow (sloppy) takes the ordinary path instead.
	if n.Left != nil && n.Left.Kind == NUndef && n.Op == TokAssign &&
		c.withDepth == 0 && c.resolveLocal("undefined") < 0 && c.resolveUpvalue("undefined") < 0 {
		nameAnonExpr(n.Right, "undefined")
		c.compileExpr(n.Right)
		c.emit(OpDup)
		c.emitGlobalPut("undefined")
		return
	}
	// Assignment to a non-reference literal (null/this/true/…) is a no-op that
	// yields the RHS (sloppy mode); strict-mode rejection is a parser concern
	// handled elsewhere.
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
	// NamedEvaluation applies only when the target is an unparenthesized
	// IdentifierReference: `(fn) = function(){}` leaves the function's name "".
	namedEval := n.Left.Flags&fnParen == 0
	nameAnon := func(v *Node) {
		if namedEval {
			nameAnonExpr(v, name)
		}
	}

	loadVar := func() {
		switch {
		case c.nameIsWithRouted(name):
			c.emitWithVar(OpWithGetVar, name)
		case slot >= 0:
			c.emitOpU16(OpGetLocal, uint16(slot))
		case uv >= 0:
			c.emitOpU16(OpGetUpval, uint16(uv))
		default:
			c.emitGlobalGet(name)
		}
	}
	storeVar := func() {
		// Inside a `with`, the store is routed to the with-object(s) first (with a
		// lexical fallback baked into emitWithVar), so a same-named local/upvalue can
		// be shadowed by a with-object property.
		if c.nameIsWithRouted(name) && !c.storeKeepsStaticBinding(slot, uv) {
			c.emit(OpDup)
			c.emitWithVar(OpWithPutVar, name)
			return
		}
		// An imported binding is immutable: assigning to it throws a TypeError,
		// exactly like a const.
		if _, isImport := c.lookupImport(name); isImport {
			c.emitConstAssignError()
			return
		}
		// Assignment to a const binding always throws a TypeError (in strict and
		// sloppy code alike). The value stays on the stack (assignment is an expr).
		if slot >= 0 && c.locals[slot].isConst {
			c.emitConstAssignError()
			return
		}
		// A named function expression's self-reference is immutable: assigning to it
		// throws a TypeError in strict mode and is a silent no-op otherwise (the
		// evaluated value stays on the stack either way). This holds when the
		// reference is captured by a nested function too (a selfName upvalue).
		if (slot >= 0 && c.locals[slot].selfName) || (uv >= 0 && c.upvalues[uv].selfName) {
			if c.fn.isStrict {
				c.emitConstAssignError()
			}
			return
		}
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
		// NamedEvaluation: `x ||= () => {}` names the anonymous RHS "x".
		nameAnon(n.Right)
		c.compileExpr(n.Right)
		storeVar()
		c.patchJump(skip)
		return
	}

	// Evaluate the value to assign, leaving it on the stack.
	if n.Op == TokAssign {
		// A with-routed target resolves its Reference BEFORE the right-hand side is
		// evaluated, and PutValue writes back through that same Reference — so a RHS
		// that deletes the with-object's property (or has a direct eval declare a
		// nearer binding) does not move the store. Only the base is resolved here;
		// a plain assignment performs no [[Get]].
		if c.nameIsWithRouted(name) && !c.storeKeepsStaticBinding(slot, uv) {
			c.emitWithVarBase(name)
			nameAnon(n.Right)
			c.compileExpr(n.Right)
			c.emitWithVarRef(OpWithPutVar, name)
			return
		}
		// In strict code the Reference is RESOLVED before the right-hand side runs
		// and PutValue throws on an unresolvable one afterwards, so
		// `undeclared = (this.undeclared = 5)` is a ReferenceError even though the
		// RHS creates the property: resolve here, throw at the store.
		strictGlobalRef := false
		if c.fn.isStrict && slot < 0 && uv < 0 && c.withDepth == 0 {
			if _, isImport := c.lookupImport(name); !isImport {
				strictGlobalRef = true
				c.emit(OpDeleteVar) // reused: resolve, pushing whether it bound
				c.emitU32(uint32(c.constant(c.rt.internString(name))))
			}
		}
		nameAnon(n.Right)
		c.compileExpr(n.Right)
		if strictGlobalRef {
			// [resolvable, value] -> [value], throwing if the reference was
			// unresolvable when it was taken.
			c.emit(OpPutConst)
			c.emitU32(uint32(c.constant(c.rt.internString(name))))
			return
		}
	} else {
		op, ok := compoundOpcode(n.Op)
		if !ok {
			c.errorf("unsupported compound assignment %v (slice)", n.Op)
			return
		}
		if c.nameIsWithRouted(name) && !c.storeKeepsStaticBinding(slot, uv) {
			// Base-caching compound assignment inside `with`: resolve the reference
			// once (ref mode) so the read and the write target the same base, per
			// PutValue — even when a with-object getter deletes the binding between
			// the read and the write.
			c.emitWithVarRef(OpWithGetVar, name) // [base, oldval]
			c.compileExpr(n.Right)               // [base, oldval, rhs]
			c.emit(op)                           // [base, result]
			c.emitWithVarRef(OpWithPutVar, name) // [result]
			return
		}
		switch {
		case slot >= 0:
			c.emitOpU16(OpGetLocal, uint16(slot))
		case uv >= 0:
			c.emitOpU16(OpGetUpval, uint16(uv))
		default:
			c.emitGlobalGet(name)
		}
		c.compileExpr(n.Right)
		c.emit(op)
	}

	// Inside a `with`, route a plain assignment to the with-object(s) first
	// (emitWithVar carries the lexical fallback), so a with-object property shadows
	// a same-named local/upvalue. The value produced above stays on the stack
	// (assignment is an expression) via the OpDup below. (A compound assignment
	// inside `with` was already handled above via the base-caching ref path.)
	if c.nameIsWithRouted(name) && !c.storeKeepsStaticBinding(slot, uv) {
		c.emit(OpDup)
		c.emitWithVar(OpWithPutVar, name)
		return
	}
	// An imported binding is immutable: assigning to it throws a TypeError.
	if _, isImport := c.lookupImport(name); isImport {
		c.emitConstAssignError()
		return
	}
	// Assignment to a const binding always throws a TypeError — whether it is a
	// local or a const captured from an enclosing scope.
	if slot >= 0 && c.locals[slot].isConst {
		c.emitConstAssignError()
		return
	}
	if uv >= 0 && c.upvalues[uv].isConst {
		c.emitConstAssignError()
		return
	}
	// A named function expression's self-reference is immutable: assigning to it
	// throws a TypeError in strict mode and is a silent no-op otherwise (the value
	// stays on the stack, since assignment is an expression). This also holds when
	// a nested function captures the reference (a selfName upvalue).
	if (slot >= 0 && c.locals[slot].selfName) || (uv >= 0 && c.upvalues[uv].selfName) {
		if c.fn.isStrict {
			c.emitConstAssignError()
		}
		return
	}
	// SET_* keeps the value on the stack (assignment is an expression).
	switch {
	case slot >= 0:
		// TDZ: a plain assignment to a let binding still in its dead zone (declared
		// later in the block) is a ReferenceError. A compound assignment already
		// read the binding above (which throws on the hole); a plain `=` did not, so
		// probe with a checked read that throws on the EMPTY hole and is otherwise a
		// harmless read-and-discard of the old value.
		if n.Op == TokAssign && c.locals[slot].blockScoped {
			c.emitOpU16(OpGetLocal, uint16(slot))
			c.emit(OpPop)
		}
		c.emitOpU16(OpSetLocal, uint16(slot))
	case uv >= 0:
		// A captured let/const may also be assigned in its TDZ (via a closure); a
		// checked read throws on the hole, and a captured var cell is never a hole,
		// so probing every plain upvalue assignment is safe.
		if n.Op == TokAssign {
			c.emitOpU16(OpGetUpval, uint16(uv))
			c.emit(OpPop)
		}
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
