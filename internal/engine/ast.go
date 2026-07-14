package engine

// Port of ant src/silver/ast.h — the uniform AST node. A single Node struct
// with a Kind discriminator and generic child slots (Left/Right/Cond/Body/
// Args, Init/Update, Catch/Finally) keeps the recursive-descent parser port
// mechanical, exactly as in ant.

// NodeKind enumerates AST node types (ant sv_node_type_t). Order is preserved.
type NodeKind uint8

const (
	NNumber NodeKind = iota
	NString
	NBigInt
	NBool
	NNull
	NUndef
	NThis
	NGlobalThis
	NTemplate
	NRegexp
	NIdent
	NBinary
	NUnary
	NUpdate
	NAssign
	NTernary
	NCall
	NNew
	NMember
	NOptional
	NArray
	NObject
	NProperty
	NSpread
	NSequence
	NArrow
	NYield
	NAwait
	NTypeof
	NDelete
	NVoid
	NTaggedTemplate
	NBlock
	NVar
	NVarDecl
	NIf
	NWhile
	NDoWhile
	NFor
	NForIn
	NForOf
	NForAwaitOf
	NReturn
	NBreak
	NContinue
	NThrow
	NTry
	NSwitch
	NCase
	NLabel
	NDebugger
	NEmpty
	NWith
	NFunc
	NClass
	NMethod
	NStaticBlock
	NArrayPat
	NObjectPat
	NRest
	NAssignPat
	NNewTarget
	NImport
	NImportDecl
	NImportSpec
	NExport
	NProgram
	nNodeCount
)

// VarKind is a declaration kind (ant sv_var_kind_t).
type VarKind uint8

const (
	VarVar VarKind = iota
	VarLet
	VarConst
	VarUsing
	VarAwaitUsing
	// varAssign marks destructuring-assignment targets (assign to existing
	// bindings / member references rather than declaring).
	varAssign VarKind = 0xFF
)

// Function/property flags (ant ast.h FN_*).
const (
	fnAsync           = 1 << 0
	fnGenerator       = 1 << 1
	fnArrow           = 1 << 2
	fnGetter          = 1 << 3
	fnSetter          = 1 << 4
	fnStatic          = 1 << 5
	fnComputed        = 1 << 6
	fnMethod          = 1 << 7
	fnColon           = 1 << 8
	fnParen           = 1 << 9
	fnUsesArgs        = 1 << 10
	fnInvalidCooked   = 1 << 11
	fnParseStrict     = 1 << 12
	fnTemplateSegment = 1 << 13
	fnUsesNewTarget   = 1 << 14
	fnClassBody       = 1 << 15
	fnModuleSyntax    = 1 << 16
	fnDerivedCtor     = 1 << 17
	fnInferredName    = 1 << 18 // name came from NamedEvaluation (no self-binding)
	fnClassCtor       = 1 << 19 // a class constructor (must be invoked with new)
	fnFuncExpr        = 1 << 20 // a function expression (immutable self-name binding)
	nodeRestComma     = 1 << 21 // an array literal whose rest element is followed by a comma (invalid as a binding/assignment pattern)
	nodeHasCoverInit  = 1 << 22 // an object literal with a CoverInitializedName (`{x = 1}`) — valid only when reinterpreted as a destructuring pattern
)

// Export flags (ant ast.h EX_*).
const (
	exDefault   = 1 << 0
	exDecl      = 1 << 1
	exNamed     = 1 << 2
	exFrom      = 1 << 3
	exStar      = 1 << 4
	exNamespace = 1 << 5
)

// Node is a single AST node (ant struct sv_ast).
type Node struct {
	Kind    NodeKind
	Op      Token
	Flags   uint32
	VarKind VarKind

	Str string // identifier name, cooked string literal, cooked template seg
	Aux string // raw template segment / regexp flags

	Num float64

	Left  *Node
	Right *Node
	Cond  *Node
	Body  *Node
	Args  []*Node

	CatchParam  *Node
	CatchBody   *Node
	FinallyBody *Node

	Init   *Node
	Update *Node

	Line   uint32
	Col    uint32
	SrcOff uint32
	SrcEnd uint32
}

// exprStmtNodes marks node kinds that may begin an expression statement
// (ant sv_ast_can_be_expression_statement).
var exprStmtNodes = func() [nNodeCount]bool {
	var t [nNodeCount]bool
	for _, k := range []NodeKind{
		NNumber, NString, NBigInt, NBool, NNull, NUndef, NThis, NGlobalThis,
		NTemplate, NRegexp, NIdent, NBinary, NUnary, NUpdate, NAssign, NTernary,
		NCall, NNew, NMember, NOptional, NArray, NObject, NProperty, NSpread,
		NSequence, NArrow, NYield, NAwait, NTypeof, NDelete, NVoid,
		NTaggedTemplate, NImport, NNewTarget,
	} {
		t[k] = true
	}
	return t
}()

func canBeExpressionStatement(n *Node) bool {
	if n == nil {
		return false
	}
	if (n.Kind == NFunc || n.Kind == NClass) && n.Flags&fnParen != 0 {
		return true
	}
	if n.Kind == NFunc {
		return !(n.Str != "" && n.Flags&fnArrow == 0)
	}
	if int(n.Kind) >= int(nNodeCount) {
		return false
	}
	return exprStmtNodes[n.Kind]
}

// isUseStrict reports whether stmt is a "use strict" directive
// (ant sv_ast_is_use_strict, matching its cooked-string comparison).
func isUseStrict(n *Node) bool {
	return n != nil && n.Kind == NString && n.Str == "use strict"
}

// bodyHasUseStrict reports whether a function body's directive prologue contains
// a "use strict" directive (a leading run of string-literal statements).
func bodyHasUseStrict(body *Node) bool {
	if body == nil {
		return false
	}
	for _, stmt := range body.Args {
		if stmt == nil || stmt.Kind == NEmpty {
			continue
		}
		if stmt.Kind != NString {
			return false // prologue ended
		}
		if isUseStrict(stmt) {
			return true
		}
	}
	return false
}

// hasNonSimpleParams reports whether any parameter is a rest, default, or
// destructuring binding (i.e. the list is not a simple identifier list). Such a
// list forbids an explicit "use strict" directive in the body.
func hasNonSimpleParams(fn *Node) bool {
	for _, p := range fn.Args {
		if p == nil || p.Kind != NIdent {
			return true
		}
	}
	return false
}

// referencesArguments reports whether the subtree reads `arguments`, not
// crossing into nested non-arrow functions (ant ast_references_arguments).
func referencesArguments(n *Node) bool {
	if n == nil {
		return false
	}
	if n.Kind == NIdent && n.Str == "arguments" {
		return true
	}
	if n.Kind == NFunc && n.Flags&fnArrow == 0 {
		for _, a := range n.Args {
			if referencesArguments(a) {
				return true
			}
		}
		return false
	}
	if referencesArguments(n.Left) || referencesArguments(n.Right) ||
		referencesArguments(n.Cond) || referencesArguments(n.Body) ||
		referencesArguments(n.CatchBody) || referencesArguments(n.FinallyBody) ||
		referencesArguments(n.CatchParam) || referencesArguments(n.Init) ||
		referencesArguments(n.Update) {
		return true
	}
	for _, a := range n.Args {
		if referencesArguments(a) {
			return true
		}
	}
	return false
}

// referencesNewTarget reports whether the subtree reads new.target, not
// crossing into nested non-arrow functions (ant ast_references_new_target).
func referencesNewTarget(n *Node) bool {
	if n == nil {
		return false
	}
	if n.Kind == NNewTarget {
		return true
	}
	if n.Kind == NFunc && n.Flags&fnArrow == 0 {
		return false
	}
	if referencesNewTarget(n.Left) || referencesNewTarget(n.Right) ||
		referencesNewTarget(n.Cond) || referencesNewTarget(n.Body) ||
		referencesNewTarget(n.CatchBody) || referencesNewTarget(n.FinallyBody) ||
		referencesNewTarget(n.CatchParam) || referencesNewTarget(n.Init) ||
		referencesNewTarget(n.Update) {
		return true
	}
	for _, a := range n.Args {
		if referencesNewTarget(a) {
			return true
		}
	}
	return false
}

// programIsStrict reports whether the program's directive prologue contains a
// "use strict" directive (empty statements are transparent, matching ant).
func programIsStrict(program *Node) bool {
	if program == nil {
		return false
	}
	for _, stmt := range program.Args {
		if stmt == nil || stmt.Kind == NEmpty {
			continue
		}
		if stmt.Kind != NString {
			return false
		}
		if stmt.Str == "use strict" {
			return true
		}
	}
	return false
}

// collectVarFuncNames gathers `var` and function-declaration names hoisted to
// the enclosing function/script scope (recursing through blocks and control
// structures, but NOT into nested functions).
// collectBodyVarNames collects the VarDeclaredNames of a statement — the names
// introduced by `var` declarations anywhere within it, not descending into a
// nested function (a separate var scope). Block-level function and class
// declarations are lexical, not var-scoped, so they are excluded; this is used
// to detect a for-head lexical name that a body `var` would shadow.
func collectBodyVarNames(n *Node, out map[string]bool) {
	if n == nil {
		return
	}
	switch n.Kind {
	case NVar:
		if n.VarKind == VarVar {
			for _, d := range n.Args {
				if d != nil {
					collectBindingNames(d.Left, out)
				}
			}
		}
	case NBlock, NCase, NProgram, NSwitch:
		for _, s := range n.Args {
			collectBodyVarNames(s, out)
		}
	case NIf:
		collectBodyVarNames(n.Left, out)
		collectBodyVarNames(n.Right, out)
	case NWhile, NDoWhile, NWith, NLabel:
		collectBodyVarNames(n.Body, out)
	case NFor, NForIn, NForOf:
		collectBodyVarNames(n.Init, out)
		collectBodyVarNames(n.Left, out)
		collectBodyVarNames(n.Body, out)
	case NTry:
		collectBodyVarNames(n.Body, out)
		collectBodyVarNames(n.CatchBody, out)
		collectBodyVarNames(n.FinallyBody, out)
	}
}

func collectVarFuncNames(stmts []*Node, out map[string]bool) {
	for _, n := range stmts {
		collectVarFuncNamesNode(n, out)
	}
}

// collectBindingNames adds every identifier bound by a binding target — a plain
// name or an array/object destructuring pattern (with defaults and rest) — to
// out. Used so `var` names in a pattern are hoisted (pre-declared) like a plain
// `var name`, which strict-mode resolution requires.
func collectBindingNames(pat *Node, out map[string]bool) {
	if pat == nil {
		return
	}
	switch pat.Kind {
	case NIdent:
		out[pat.Str] = true
	case NArray:
		for _, e := range pat.Args {
			collectBindingNames(e, out)
		}
	case NObject:
		for _, prop := range pat.Args {
			if prop == nil {
				continue
			}
			if prop.Kind == NSpread || prop.Kind == NRest {
				collectBindingNames(prop.Right, out)
				continue
			}
			collectBindingNames(prop.Right, out)
		}
	case NAssignPat:
		collectBindingNames(pat.Left, out)
	case NAssign:
		if pat.Op == TokAssign {
			collectBindingNames(pat.Left, out)
		}
	case NRest, NSpread:
		collectBindingNames(pat.Right, out)
	}
}

func collectVarFuncNamesNode(n *Node, out map[string]bool) {
	if n == nil {
		return
	}
	switch n.Kind {
	case NFunc:
		if n.Str != "" && n.Flags&fnArrow == 0 {
			out[n.Str] = true
		}
	case NVar:
		if n.VarKind == VarVar {
			for _, d := range n.Args {
				if d != nil {
					collectBindingNames(d.Left, out)
				}
			}
		}
	case NClass:
		// Class declarations are lexically scoped, but the slice binds them
		// globally; pre-register the name to satisfy strict-mode resolution.
		if n.Str != "" {
			out[n.Str] = true
		}
	case NBlock, NCase, NProgram:
		collectVarFuncNames(n.Args, out)
	case NIf:
		collectVarFuncNamesNode(n.Left, out)
		collectVarFuncNamesNode(n.Right, out)
	case NWhile, NDoWhile, NWith, NLabel:
		collectVarFuncNamesNode(n.Body, out)
	case NFor, NForIn, NForOf:
		collectVarFuncNamesNode(n.Init, out)
		collectVarFuncNamesNode(n.Left, out)
		collectVarFuncNamesNode(n.Body, out)
	case NTry:
		collectVarFuncNamesNode(n.Body, out)
		collectVarFuncNamesNode(n.CatchBody, out)
		collectVarFuncNamesNode(n.FinallyBody, out)
	case NSwitch:
		collectVarFuncNames(n.Args, out)
	}
}

func programHasModuleSyntax(program *Node) bool {
	if program == nil || program.Kind != NProgram {
		return false
	}
	for _, stmt := range program.Args {
		if stmt != nil && (stmt.Kind == NImportDecl || stmt.Kind == NExport) {
			return true
		}
	}
	return false
}
