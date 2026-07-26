package engine

// Port of ant src/silver/ast.c — the recursive-descent parser. Behavioral
// parity with ant is the goal; ant's parser is deliberately lenient (its
// `expect` consumes-if-match without erroring), so we mirror that.
//
// The full ES2025 grammar is parsed from day one (PLAN.md): later milestones
// gate semantics/builtins, never the grammar.

import "fmt"

// SyntaxError is a parse-time error carrying a source offset.
type SyntaxError struct {
	Msg      string
	Offset   int
	Filename string
}

func (e *SyntaxError) Error() string {
	if e.Filename != "" {
		return fmt.Sprintf("%s:%d: SyntaxError: %s", e.Filename, e.Offset, e.Msg)
	}
	return fmt.Sprintf("SyntaxError: %s (offset %d)", e.Msg, e.Offset)
}

type parser struct {
	lx       *lexer
	noIn     bool
	err      error
	filename string
	// inAsync is true inside an async function body (so `await` is the operator,
	// not an identifier). pendingAsync is set by a caller right before parseFunc
	// to mark the function it is about to parse as async.
	inAsync      bool
	pendingAsync bool
	// inGenerator is true inside a generator function body (so `yield` is the
	// operator, not an identifier). Like inAsync it is cleared for the parameter
	// list and any nested non-generator function.
	inGenerator bool
	// pendingGenerator marks the function parseFunc is about to parse as a
	// generator when the caller (an object/class method) already consumed the `*`.
	pendingGenerator bool
	// funcDepth > 0 inside any function/arrow body, so a `return` outside every
	// function (top-level script or eval code) is a SyntaxError.
	funcDepth int
	// newTargetOK is true inside a non-arrow function/method body (where
	// new.target is meaningful); an arrow inherits the enclosing value, so
	// `new.target` at the top level or in a top-level arrow is a SyntaxError.
	newTargetOK bool
	// inStaticBlock reserves `await` as an identifier in a class static block's
	// direct scope (a static block is not async, but await is still forbidden
	// there). Unlike inAsync it is CLEARED when entering a nested function or
	// arrow, whose own async-ness then governs await.
	inStaticBlock bool
	// singleStmt is a one-shot flag set by parseSubStmt before parsing the body
	// of a loop/if (a single-Statement context, where a Declaration is not
	// allowed). parseStmt captures and clears it immediately; it governs the
	// sloppy-mode disambiguation of a leading `let` (an identifier there, never a
	// LexicalDeclaration).
	singleStmt bool
	// pendingFuncExpr is a one-shot flag a caller sets immediately before
	// parseFunc to mark that it is parsing a FunctionExpression (not a
	// declaration). A FunctionExpression's name is [~Yield, ~Await], so `yield`/
	// `await` are valid there even inside an enclosing generator/async function.
	pendingFuncExpr bool
	// usingAllowed reports whether a `using` / `await using` declaration is a legal
	// statement at the current position. It is true inside a Block, function body,
	// or class static block, and false at the top level of a Script and directly
	// within a CaseClause/DefaultClause StatementList (a Module top level allows it).
	usingAllowed bool
}

// Parse tokenizes and parses src into an AST (N_PROGRAM root node).
func Parse(filename, src string) (*Node, error) {
	return parseMode(filename, src, false, false)
}

func parseMode(filename, src string, strict, module bool) (*Node, error) {
	p := &parser{lx: newLexer(src, strict), filename: filename}
	p.lx.module = module
	// A Module's top level is an async context: `await` is the await operator
	// (top-level await), reset to identifier inside any nested non-async function.
	p.inAsync = module
	// A `using` declaration is illegal directly at the top level of a Script (but
	// legal at a Module top level, and in any Block/function body below).
	p.usingAllowed = module
	program := p.mk(NProgram)
	p.parseStmtList(&program.Args, false, true)
	if p.err != nil {
		return nil, p.err
	}
	// The lexer's strict flag is restored to its entry value when the top-level
	// statement list returns (nested scopes must not leak strictness), so the
	// program's own strictness is (re)derived from its directive prologue.
	if strict || programIsStrict(program) {
		program.Flags |= fnParseStrict
	}
	if programHasModuleSyntax(program) {
		program.Flags |= fnModuleSyntax
	}
	return program, nil
}

// ---- lexer plumbing (ant P/NEXT/CONSUME/LA macros) ----

func (p *parser) next() Token      { return p.lx.next() }
func (p *parser) tok() Token       { return p.lx.st.tok }
func (p *parser) consume()         { p.lx.consume() }
func (p *parser) la() Token        { return p.lx.lookahead() }
func (p *parser) toff() int        { return p.lx.st.toff }
func (p *parser) tlen() int        { return p.lx.st.tlen }
func (p *parser) hadNewline() bool { return p.lx.st.hadNewline }

// escKeyword reports whether the current token is an escaped reserved word
// (lexed as TokErr): invalid as a keyword/reference but a valid IdentifierName.
func (p *parser) escKeyword() bool { return p.lx.st.tok == TokErr && p.lx.st.escKeyword }
func (p *parser) tval() Value      { return p.lx.st.tval }
func (p *parser) code() string     { return p.lx.code }

// tokStr returns the raw current-token text.
func (p *parser) tokStr() string { return p.lx.tokenText() }

// tokIdentStr returns the current identifier token, decoding \u escapes.
func (p *parser) tokIdentStr() string {
	raw := p.tokStr()
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' {
			return decodeIdentEscapes(raw)
		}
	}
	return raw
}

func (p *parser) eat(t Token) bool {
	p.next()
	if p.tok() == t {
		p.consume()
		return true
	}
	return false
}

// expect consumes t if present (lenient, matching ant's expect).
func (p *parser) expect(t Token) {
	p.next()
	if p.tok() == t {
		p.consume()
		return
	}
	// A required token is missing: a genuine SyntaxError (e.g. `while 1 …` without
	// parentheses). Only reported once (errorf keeps the first error).
	p.errorf("Unexpected token")
}

func (p *parser) mkPlain(kind NodeKind) *Node { return &Node{Kind: kind} }

func (p *parser) mk(kind NodeKind) *Node {
	return &Node{Kind: kind, SrcOff: uint32(p.toff())}
}

func (p *parser) errorf(format string, args ...any) {
	if p.err == nil {
		p.err = &SyntaxError{Msg: fmt.Sprintf(format, args...), Offset: p.toff(), Filename: p.filename}
	}
}

func (p *parser) unexpected() {
	if p.toff() < len(p.code()) && p.tlen() > 0 {
		p.errorf("Unexpected token '%s'", p.tokStr())
	} else {
		p.errorf("Unexpected token 'EOF'")
	}
}

func (p *parser) hasErr() bool { return p.err != nil }

// ---- token classification helpers (ant is_*_tok) ----

func isContextualIdentTok(t Token) bool {
	return t == TokAs || t == TokFrom || t == TokOf || t == TokAsync || t == TokUsing
}

func isIdentLikeTok(t Token) bool {
	return t == TokIdentifier || t == TokDefault || isContextualIdentTok(t)
}

func isPrivateIdentLikeTok(t Token) bool {
	return t >= TokIdentifier && t < TokIdentLikeEnd
}

func (p *parser) isLeadingDotNumberTok() bool {
	return p.tok() == TokNumber && p.tlen() > 0 && p.toff() < len(p.code()) && p.code()[p.toff()] == '.'
}

func (p *parser) strictForbiddenBinding(s string) bool {
	return isEvalOrArgumentsName(s) || isStrictReservedName(s)
}

func (p *parser) isStrictRestrictedAssignTarget(n *Node) bool {
	return p.lx.strict && n != nil && n.Kind == NIdent && isEvalOrArgumentsName(n.Str)
}

func (p *parser) strictCheckBindingIdent(s string) {
	if s == "" {
		return
	}
	// `extends` and `enum` are always-reserved words with no dedicated token (they
	// lex as identifiers, so isReservedWordTok misses them). Reject them as binding
	// identifiers in every context; the decoded name also rejects `extend\u{73}`.
	if s == "extends" || s == "enum" {
		p.errorf("Unexpected reserved word")
		return
	}
	// `yield`/`await` are reserved as binding identifiers inside a generator /
	// async body (regardless of strict mode).
	if p.inGenerator && s == "yield" {
		p.errorf("'yield' cannot be used as a binding identifier in a generator")
		return
	}
	if (p.inAsync || p.inStaticBlock) && s == "await" {
		p.errorf("'await' cannot be used as a binding identifier here")
		return
	}
	if p.lx.strict && p.strictForbiddenBinding(s) {
		p.errorf("Invalid binding identifier '%s' in strict mode", s)
	}
}

func isLexicalDeclStmt(n *Node) bool {
	return n != nil && n.Kind == NVar &&
		(n.VarKind == VarLet || n.VarKind == VarConst ||
			n.VarKind == VarUsing || n.VarKind == VarAwaitUsing)
}

// isDeclNotStatement reports whether n is a Declaration that may not stand where
// only a Statement (optionally an Annex B FunctionDeclaration) is allowed: a
// lexical/class/async/generator/async-generator declaration always, and a plain
// FunctionDeclaration in strict mode (Annex B permits it in sloppy). This is the
// LabelledItem rule; if/else and iteration bodies additionally forbid a labelled
// FunctionDeclaration (see isForbiddenIfBody / isForbiddenLoopBody).
func isDeclNotStatement(n *Node, strict bool) bool {
	if n == nil {
		return false
	}
	if isLexicalDeclStmt(n) {
		return true
	}
	switch n.Kind {
	case NFunc:
		if n.Flags&(fnArrow|fnFuncExpr) != 0 || n.Str == "" {
			return false // a function expression statement
		}
		if n.Flags&(fnAsync|fnGenerator) != 0 {
			return true
		}
		return strict
	case NClass:
		return n.Str != ""
	}
	return false
}

// isForbiddenIfBody reports whether a statement may not be the body of an
// `if`/`else`: any Declaration that isn't an Annex B sloppy FunctionDeclaration,
// or a labelled FunctionDeclaration (which is fine at StatementList level but not
// as a single-statement body).
func isForbiddenIfBody(n *Node, strict bool) bool {
	return isDeclNotStatement(n, strict) || isLabelledFunction(n)
}

// isLabelledFunction reports whether n is a (possibly multiply) labelled
// FunctionDeclaration — `L: function f(){}`, `L1: L2: function f(){}`. Such a
// statement is permitted at StatementList level (Annex B.3.1) but is an early
// error as the sole body of an `if`/`else` or an iteration statement, in both
// strict and sloppy mode.
func isLabelledFunction(n *Node) bool {
	labelled := false
	for n != nil && n.Kind == NLabel {
		labelled = true
		n = n.Body
	}
	return labelled && n != nil && n.Kind == NFunc &&
		n.Flags&(fnArrow|fnFuncExpr) == 0 && n.Str != ""
}

// isForbiddenLoopBody reports whether a statement may not be the body of a
// for/for-in/for-of/while/do-while loop: a lexical declaration, a
// function/generator/async-function/class declaration (the loop body is a
// Statement, which excludes all Declarations — no Annex B exception here), or a
// labelled function declaration (IsLabelledFunction).
func isForbiddenLoopBody(n *Node) bool {
	if n == nil {
		return false
	}
	if isLexicalDeclStmt(n) || isLabelledFunction(n) {
		return true
	}
	switch n.Kind {
	case NFunc:
		return n.Flags&(fnArrow|fnFuncExpr) == 0 && n.Str != ""
	case NClass:
		return n.Str != ""
	}
	return false
}

func varDeclHasInitializer(n *Node) bool {
	if n == nil || n.Kind != NVar {
		return false
	}
	for _, d := range n.Args {
		if d != nil && d.Kind == NVarDecl && d.Right != nil {
			return true
		}
	}
	return false
}

func isAssignOp(t Token) bool { return t >= TokAssign && t <= TokNullishAssign }

// ---- node constructors (ant mk_*) ----

func mkNum(val float64) *Node { return &Node{Kind: NNumber, Num: val} }
func mkIdent(s string) *Node  { return &Node{Kind: NIdent, Str: s} }

// escapedIdentName decodes an escaped-keyword token that is nonetheless a valid
// Identifier. A ReservedWord is a literal sequence of characters, so a word
// spelled with a Unicode escape is not that keyword: for a CONTEXTUAL one — let,
// yield, async, await, static, of, … — what remains is an ordinary
// IdentifierName. A true ReservedWord (`\u0069f`) has nothing it could be and
// stays an error, as does a name the current mode reserves.
func (p *parser) escapedIdentName() (string, bool) {
	if !p.escKeyword() {
		return "", false
	}
	name := p.tokIdentStr()
	if isReservedWordTok(parseKeyword(name)) {
		return "", false
	}
	if p.lx.strict && isStrictReservedName(name) {
		return "", false
	}
	if name == "yield" && p.inGenerator {
		return "", false
	}
	if name == "await" && (p.lx.module || p.inAsync) {
		return "", false
	}
	return name, true
}

// mkEscapedIdent builds an identifier node from an escaped-keyword token and
// consumes it.
func (p *parser) mkEscapedIdent(name string) *Node {
	n := mkIdent(name)
	n.SrcOff = uint32(p.toff())
	n.SrcEnd = uint32(p.toff() + p.tlen())
	p.consume()
	return n
}

func (p *parser) mkIdentFromTok() *Node {
	n := mkIdent(p.tokIdentStr())
	n.SrcOff = uint32(p.toff())
	n.SrcEnd = uint32(p.toff() + p.tlen())
	return n
}

// privateNameAdjacent consumes the current `#` token and peeks the following
// identifier, reporting whether it is immediately adjacent. A PrivateIdentifier
// is a single token: no whitespace or line terminator may separate `#` from the
// name (`# x` is an early SyntaxError).
func (p *parser) privateNameAdjacent() bool {
	hashEnd := p.toff() + p.tlen()
	p.consume()
	p.next()
	return p.toff() == hashEnd
}

func (p *parser) mkPrivateIdentFromTok() *Node {
	off := p.toff()
	start := off
	if off > 0 {
		start = off - 1
	}
	// The private name's identity is its StringValue: decode any \u escapes in the
	// identifier part so `#a` and `#a` name the same private field.
	n := &Node{Kind: NIdent, Str: "#" + p.tokIdentStr(), SrcOff: uint32(start), SrcEnd: uint32(off + p.tlen())}
	return n
}

func (p *parser) mkStringFromTok() *Node {
	n := p.mk(NString)
	raw := p.tokStr()
	n.Str = cookString(raw)
	// A directive's Use Strict form must match the RAW source `use strict`; any
	// escape sequence or line continuation (both start with a backslash) makes the
	// raw text differ from the cooked value, so it is not a Use Strict Directive.
	// A LegacyOctalEscapeSequence / NonOctalDecimalEscapeSequence (`\1`–`\9`, or
	// `\0` immediately followed by a digit) additionally makes the literal illegal
	// in strict code — recorded for the directive-prologue early error.
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' || i+1 >= len(raw) {
			continue
		}
		n.Flags |= fnStrHadEscape
		if c := raw[i+1]; c >= '1' && c <= '9' {
			n.Flags |= fnStrLegacyOctal
		} else if c == '0' && i+2 < len(raw) && raw[i+2] >= '0' && raw[i+2] <= '9' {
			n.Flags |= fnStrLegacyOctal
		}
		i++ // skip the escaped character so `\\1` is not misread as `\1`
	}
	return n
}

func nodeSrcEnd(p *parser, node *Node) uint32 {
	if node != nil && node.SrcEnd > node.SrcOff {
		return node.SrcEnd
	}
	// When the current token is still unconsumed it was only peeked (the body's
	// last token was the previous one, e.g. the `)` after `x=>x+1`); otherwise
	// the current token IS the body's last token (e.g. `}` of a block body).
	if !p.lx.st.consumed {
		return uint32(p.lx.st.prevEnd)
	}
	return uint32(p.lx.st.toff + p.lx.st.tlen)
}

// lookaheadCrossesLineTerminator reports whether a newline precedes the next
// token (ant lookahead_crosses_line_terminator).
func (p *parser) lookaheadCrossesLineTerminator() bool {
	saved := p.lx.save()
	p.lx.st.consumed = true
	p.next()
	nl := p.hadNewline()
	p.lx.restore(saved)
	return nl
}

// ---- statement list & block ----

func (p *parser) parseStmtList(out *[]*Node, stopAtRBrace, directiveCtx bool) {
	savedStrict := p.lx.strict
	inDirective := directiveCtx
	for {
		p.next()
		if p.tok() == TokEOF {
			break
		}
		if stopAtRBrace && p.tok() == TokRBrace {
			break
		}
		if p.tok() == TokErr {
			p.unexpected()
			break
		}
		stmt := p.parseStmt()
		if stmt != nil {
			*out = append(*out, stmt)
		}
		if p.hasErr() {
			break
		}
		if !inDirective {
			continue
		}
		if stmt == nil {
			continue
		}
		// The Directive Prologue is the leading run of string-literal
		// ExpressionStatements. A non-string statement — including an EmptyStatement
		// (`;`) — ends it. A non-"use strict" string directive (e.g. `"a";`) stays in
		// the prologue, so a `"use strict"` that follows it is still recognized.
		if stmt.Kind != NString || !canBeExpressionStatement(stmt) {
			inDirective = false
			continue
		}
		if isUseStrict(stmt) {
			p.lx.strict = true
			// A "use strict" directive makes the whole function strict, including any
			// earlier prologue string. A prologue string before it that used a legacy
			// octal / non-octal decimal escape is now an illegal strict string literal.
			for _, prior := range *out {
				if prior.Kind == NString && prior.Flags&fnStrLegacyOctal != 0 {
					p.errorf("Octal escape sequences are not allowed in strict mode")
					break
				}
			}
			if p.hasErr() {
				break
			}
		}
	}
	p.lx.strict = savedStrict
}

func (p *parser) parseBlock(directiveCtx bool) *Node {
	p.expect(TokLBrace)
	block := p.mk(NBlock)
	// A Block (and a function body / class static block, which parse through here)
	// is a legal position for a `using` declaration regardless of the outer context.
	savedUsing := p.usingAllowed
	p.usingAllowed = true
	defer func() { p.usingAllowed = savedUsing }()
	p.parseStmtList(&block.Args, true, directiveCtx)
	if p.hasErr() {
		return block
	}
	p.expect(TokRBrace)
	return block
}

// ---- binding patterns ----

func (p *parser) parseBindingPattern() *Node {
	p.next()
	if p.tok() == TokLBracket {
		arr := p.parseArray()
		p.validateArrayPattern(arr)
		return arr
	}
	if p.tok() == TokLBrace {
		obj := p.parseObject()
		p.validateObjectPattern(obj)
		return obj
	}
	// `yield`/`await` are contextual: valid BindingIdentifiers except where the
	// current context reserves them (a generator body for yield, an async body for
	// await, or strict mode) — strictCheckBindingIdent enforces exactly that.
	if p.tok() == TokYield || p.tok() == TokAwait {
		id := mkIdent(p.tokStr())
		id.SrcOff = uint32(p.toff())
		p.strictCheckBindingIdent(id.Str)
		p.consume()
		return id
	}
	if isIdentLikeTok(p.tok()) || p.tok() == TokUndef {
		// `undefined` is not a reserved word, so it is a valid BindingIdentifier
		// (it shadows the global `undefined` inside the binding's scope). The lexer
		// tokenizes it as TokUndef; recover the name from the raw source.
		if p.tokIsEscapedReservedWord() {
			p.errorf("Keyword must not contain escaped characters")
			return p.mk(NEmpty)
		}
		id := p.mkIdentFromTok()
		p.strictCheckBindingIdent(id.Str)
		p.consume()
		return id
	}
	if isReservedWordTok(p.tok()) {
		p.errorf("Unexpected reserved word")
		return p.mk(NEmpty)
	}
	// A BindingElement must be an identifier or an array/object binding pattern;
	// anything else (a literal, `)`, an operator — e.g. `catch ("22")`) is an
	// early SyntaxError rather than a silently-empty binding.
	p.errorf("Unexpected token; expected a binding identifier or pattern")
	p.consume()
	return p.mk(NEmpty)
}

func pushArrowParamsFromExpr(fn, expr *Node) {
	if fn == nil || expr == nil {
		return
	}
	if expr.Kind == NSequence {
		pushArrowParamsFromExpr(fn, expr.Left)
		pushArrowParamsFromExpr(fn, expr.Right)
		return
	}
	if expr.Kind == NAssign && expr.Op == TokAssign {
		def := &Node{Kind: NAssignPat, Left: expr.Left, Right: expr.Right, SrcOff: expr.SrcOff}
		fn.Args = append(fn.Args, def)
		return
	}
	if expr.Kind == NSpread {
		rest := &Node{Kind: NRest, Right: expr.Right, SrcOff: expr.SrcOff}
		fn.Args = append(fn.Args, rest)
		return
	}
	if expr.Kind == NUndef {
		// `(undefined) => …` — `undefined` names the parameter (it is not reserved),
		// so recover it as an identifier binding rather than the undefined literal.
		fn.Args = append(fn.Args, &Node{Kind: NIdent, Str: "undefined", SrcOff: expr.SrcOff, SrcEnd: expr.SrcEnd})
		return
	}
	fn.Args = append(fn.Args, expr)
}

// ---- member/call suffixes ----

// nodeContainsArguments reports whether an `arguments` identifier reference
// appears in an expression outside of any nested non-arrow function (which
// would bind its own `arguments`). Used for the class-field-initializer early
// error (a field initializer may not contain `arguments`).
func nodeContainsArguments(n *Node) bool {
	if n == nil {
		return false
	}
	if n.Kind == NIdent && n.Str == "arguments" {
		return true
	}
	// A non-arrow function establishes its own `arguments` binding.
	if n.Kind == NFunc && n.Flags&fnArrow == 0 {
		return false
	}
	for _, c := range []*Node{n.Left, n.Right, n.Cond, n.Body, n.Init, n.Update, n.CatchParam, n.CatchBody, n.FinallyBody} {
		if nodeContainsArguments(c) {
			return true
		}
	}
	for _, c := range n.Args {
		if nodeContainsArguments(c) {
			return true
		}
	}
	return false
}

// nodeHasDirectEval reports whether a subtree contains a syntactic `eval(…)`
// call in the SAME variable environment. Every function form — including an
// arrow and a class field initializer — establishes its own variable
// environment, so the walk stops at one: a direct eval nested there declares its
// vars in that function's environment, not this one's.
//
// The test is deliberately syntactic and over-approximate (a local binding named
// `eval` makes the call non-direct); a false positive only costs one object
// allocation per call to a function that never uses it.
func nodeHasDirectEval(n *Node) bool {
	if n == nil {
		return false
	}
	if n.Kind == NFunc || n.Kind == NClass {
		return false
	}
	if n.Kind == NCall && n.Left != nil && n.Left.Kind == NIdent && n.Left.Str == "eval" {
		return true
	}
	for _, c := range []*Node{n.Left, n.Right, n.Cond, n.Body, n.Init, n.Update, n.CatchParam, n.CatchBody, n.FinallyBody} {
		if nodeHasDirectEval(c) {
			return true
		}
	}
	for _, c := range n.Args {
		if nodeHasDirectEval(c) {
			return true
		}
	}
	return false
}

// classFieldASI terminates a field definition: it consumes a following `;`, or
// verifies that automatic semicolon insertion applies (a line terminator
// precedes the next token, or it is `}` / end of input). A field followed on the
// same line by another member without a separator is an early SyntaxError.
func (p *parser) classFieldASI() bool {
	if p.next() == TokSemicolon {
		p.consume()
		return true
	}
	if p.hadNewline() || p.tok() == TokRBrace || p.tok() == TokEOF {
		return true
	}
	p.errorf("Unexpected token in class body; a field definition must be followed by ';'")
	return false
}

// isClassFieldMember reports whether a class member node is a field (not a
// method, generator, or accessor): its value is an initializer expression
// rather than a concise-method function.
func isClassFieldMember(m *Node) bool {
	if m == nil || m.Kind != NMethod {
		return false
	}
	if m.Flags&(fnGetter|fnSetter) != 0 {
		return false
	}
	if m.Right != nil && m.Right.Kind == NFunc && m.Right.Flags&fnMethod != 0 {
		return false
	}
	return true
}

// isPrivateMemberProp reports whether an NMember's property node names a private
// identifier (goant stores `.#x` as an NIdent whose text keeps the leading '#').
func isPrivateMemberProp(prop *Node) bool {
	return prop != nil && prop.Kind == NIdent && len(prop.Str) > 0 && prop.Str[0] == '#'
}

func (p *parser) parseDotPropertyName() *Node {
	// A property name is an IdentifierName: any identifier-like or keyword token,
	// including an escaped reserved word (`o.if` → property "if"), which the
	// lexer surfaces as TokErr flagged escKeyword.
	if !isPrivateIdentLikeTok(p.tok()) && !p.escKeyword() {
		p.unexpected()
		return nil
	}
	name := p.mkIdentFromTok()
	p.consume()
	return name
}

func (p *parser) parseMemberSuffix(left *Node, la Token) *Node {
	if la == TokDot {
		p.consume()
		p.next()
		mem := p.mk(NMember)
		mem.Left = left
		if p.tok() == TokHash {
			// SuperProperty is `super.IdentifierName` / `super[Expression]` only: a
			// private name after `super.` (`super.#x`) is an early SyntaxError.
			if left != nil && left.Kind == NIdent && left.Str == "super" {
				p.errorf("a private member cannot be accessed via 'super'")
				return p.mk(NEmpty)
			}
			if !p.privateNameAdjacent() || !isPrivateIdentLikeTok(p.tok()) {
				p.errorf("private field name expected")
				return p.mk(NEmpty)
			}
			mem.Right = p.mkPrivateIdentFromTok()
			p.consume()
		} else {
			mem.Right = p.parseDotPropertyName()
			if mem.Right == nil {
				return p.mk(NEmpty)
			}
		}
		return mem
	}
	if la == TokLBracket {
		p.consume()
		mem := p.mk(NMember)
		mem.Left = left
		mem.Right = p.parseExpr()
		mem.Flags = 1
		p.expect(TokRBracket)
		return mem
	}
	return nil
}

// checkArrowEarlyErrors enforces the arrow-specific early errors that are not
// caught while compiling the body: an arrow with a non-simple parameter list
// (rest / default / destructuring) may not carry an explicit "use strict"
// directive (ES2016 §14.1.2 / §14.2.1), just like an ordinary function.
func (p *parser) checkArrowEarlyErrors(fn *Node) {
	if bodyHasUseStrict(fn.Body) && hasNonSimpleParams(fn) {
		p.errorf("Illegal 'use strict' directive in function with non-simple parameter list")
	}
	// An async arrow's parameters are [+Await]: `await` may not appear there,
	// whether as a binding name or a reference (`async (await) => {}`,
	// `async (x = await) => {}`).
	if fn.Flags&fnAsync != 0 && paramsReferenceName(fn.Args, "await") {
		p.errorf("'await' is not allowed in the parameters of an async arrow function")
	}
	// An arrow inherits the enclosing generator/async context for its parameter
	// list, so a yield/await EXPRESSION there is an early error even when the
	// arrow is nested in the body of a generator/async function:
	// `function* g(){ (x = yield) => {} }`, `async f(){ (x = await 1) => {} }`.
	awaitCtx := fn.Flags&fnAsync != 0 || p.inAsync
	yieldCtx := p.inGenerator
	if (awaitCtx || yieldCtx) && paramsContainYieldAwaitExpr(fn.Args, yieldCtx, awaitCtx) {
		p.errorf("a yield or await expression is not allowed in arrow-function parameters")
	}
	// In strict code (enclosing, or a "use strict" body directive) an arrow's
	// simple parameter may not be `eval`/`arguments` or a reserved word.
	if p.lx.strict || bodyHasUseStrict(fn.Body) {
		p.checkStrictParams(fn)
	}
	// Arrow parameters come from a parenthesized cover grammar, so the rest and
	// destructuring-pattern early errors are validated here rather than in parseFunc.
	for i, param := range fn.Args {
		switch param.Kind {
		case NRest:
			if i != len(fn.Args)-1 {
				p.errorf("Rest parameter must be the last formal parameter")
			}
			if param.Right != nil && (param.Right.Kind == NAssign || param.Right.Kind == NAssignPat) {
				p.errorf("A rest parameter may not have a default initializer")
			}
			p.validatePatternTarget(param.Right)
		case NArray:
			p.validateArrayPattern(param)
		case NObject:
			p.validateObjectPattern(param)
		case NAssignPat:
			p.validatePatternTarget(param.Left)
		}
	}
}

// parseArrowBody parses a concise-body arrow tail. An async arrow establishes
// an await context for its body; a plain arrow inherits the surrounding one
// (an arrow has no Await binding of its own — ArrowFunction[?Await]).
func (p *parser) parseArrowBody(isAsync bool) *Node {
	if isAsync {
		saved := p.inAsync
		p.inAsync = true
		defer func() { p.inAsync = saved }()
	}
	// A nested arrow escapes an enclosing class static block's await restriction;
	// its own async-ness (above) governs await instead.
	savedSB := p.inStaticBlock
	p.inStaticBlock = false
	defer func() { p.inStaticBlock = savedSB }()
	p.funcDepth++ // return is legal inside an arrow body
	defer func() { p.funcDepth-- }()
	if p.next() == TokLBrace {
		return p.parseBlock(true)
	}
	return p.parseAssign()
}

// ---- primary expressions ----

func (p *parser) tryParseAsyncArrow() *Node {
	la := p.la()
	asyncOff := uint32(p.toff())

	if la == TokLParen {
		saved := p.lx.save()
		p.next()
		p.consume()
		if p.next() == TokRParen {
			p.consume()
			if p.la() == TokArrow {
				p.next()
				p.consume()
				fn := p.mk(NFunc)
				fn.Flags = fnArrow | fnAsync
				fn.Body = p.parseArrowBody(true)
				fn.SrcOff = asyncOff
				fn.SrcEnd = nodeSrcEnd(p, fn.Body)
				return fn
			}
		}
		p.lx.restore(saved)
		p.next()
		p.consume()
		expr := p.parseParenExpr()
		p.expect(TokRParen)
		if p.la() == TokArrow {
			p.next()
			p.consume()
			fn := p.mk(NFunc)
			fn.Flags = fnArrow | fnAsync
			pushArrowParamsFromExpr(fn, expr)
			fn.Body = p.parseArrowBody(true)
			p.checkArrowEarlyErrors(fn)
			fn.SrcOff = asyncOff
			fn.SrcEnd = nodeSrcEnd(p, fn.Body)
			return fn
		}
		p.lx.restore(saved)
		return nil
	}

	if la == TokIdentifier {
		saved := p.lx.save()
		p.next()
		p.consume()
		id := p.mkIdentFromTok()
		if p.la() == TokArrow {
			p.next()
			p.consume()
			fn := p.mk(NFunc)
			fn.Flags = fnArrow | fnAsync
			fn.Args = append(fn.Args, id)
			fn.Body = p.parseArrowBody(true)
			fn.SrcOff = asyncOff
			fn.SrcEnd = nodeSrcEnd(p, fn.Body)
			return fn
		}
		p.lx.restore(saved)
		return nil
	}
	return nil
}

func (p *parser) parsePrimary() *Node {
	p.next()
	switch p.tok() {
	case TokNumber:
		p.consume()
		return mkNum(tod(p.tval()))
	case TokString:
		n := p.mkStringFromTok()
		p.consume()
		return n
	case TokBigInt:
		n := p.mk(NBigInt)
		n.Str = p.tokStr()
		p.consume()
		return n
	case TokTrue:
		p.consume()
		return &Node{Kind: NBool, Num: 1, SrcOff: uint32(p.toff())}
	case TokFalse:
		p.consume()
		return &Node{Kind: NBool, Num: 0, SrcOff: uint32(p.toff())}
	case TokNull:
		n := p.mk(NNull)
		p.consume()
		return n
	case TokUndef:
		n := p.mk(NUndef)
		p.consume()
		return n
	case TokThis:
		n := p.mk(NThis)
		p.consume()
		return n
	case TokGlobalThis:
		n := p.mk(NGlobalThis)
		p.consume()
		return n
	case TokErr:
		// An escaped CONTEXTUAL keyword is an ordinary identifier reference
		// (`l\u0065t`, `\u0061sync`, `aw\u0061it` outside a module).
		if name, ok := p.escapedIdentName(); ok {
			return p.mkEscapedIdent(name)
		}
		p.unexpected()
		return p.mk(NEmpty)
	case TokIdentifier, TokAs, TokFrom, TokOf, TokUsing, TokWindow:
		if p.tokIsEscapedReservedWord() {
			p.errorf("Keyword must not contain escaped characters")
			return p.mk(NEmpty)
		}
		n := p.mkIdentFromTok()
		// A strict future-reserved word (implements/interface/package/private/
		// protected/public — let/static/yield have their own tokens) may not be an
		// IdentifierReference in strict mode.
		if p.lx.strict && isStrictReservedName(n.Str) {
			p.errorf("'%s' is reserved in strict mode", n.Str)
			return p.mk(NEmpty)
		}
		p.consume()
		return n
	case TokLet, TokStatic:
		// `let` / `static` are contextual: valid identifier references in sloppy
		// mode only (reserved in strict mode).
		if p.lx.strict {
			p.unexpected()
			return p.mk(NEmpty)
		}
		n := p.mkIdentFromTok()
		p.consume()
		return n
	case TokLParen:
		return p.parseParen()
	case TokLBracket:
		return p.parseArray()
	case TokLBrace:
		return p.parseObject()
	case TokFunc:
		p.consume()
		p.pendingFuncExpr = true
		fn := p.parseFunc()
		fn.Flags |= fnFuncExpr
		return fn
	case TokClass:
		classOff := uint32(p.toff())
		p.consume()
		cls := p.parseClass()
		cls.SrcOff = classOff
		return cls
	case TokAsync:
		return p.parseAsyncPrimary()
	case TokTemplate:
		return p.parseTemplate()
	case TokNew:
		return p.parseNew()
	case TokTypeof:
		p.consume()
		n := p.mk(NTypeof)
		n.Right = p.parseUnary()
		if p.rejectExpAfterUnary() {
			return p.mk(NEmpty)
		}
		return n
	case TokVoid:
		p.consume()
		n := p.mk(NVoid)
		n.Right = p.parseUnary()
		if p.rejectExpAfterUnary() {
			return p.mk(NEmpty)
		}
		return n
	case TokDelete:
		p.consume()
		n := p.mk(NDelete)
		n.Right = p.parseUnary()
		if p.rejectExpAfterUnary() {
			return p.mk(NEmpty)
		}
		if p.lx.strict && n.Right != nil && n.Right.Kind == NIdent {
			p.errorf("cannot delete bindings in strict mode")
			return p.mk(NEmpty)
		}
		// A `delete` operand (possibly parenthesized) that is a member access whose
		// property is a private identifier is an early SyntaxError.
		if op := n.Right; op != nil && op.Kind == NMember && isPrivateMemberProp(op.Right) {
			p.errorf("Private fields can not be deleted")
			return p.mk(NEmpty)
		}
		return n
	case TokYield:
		// `yield` is the yield operator only inside a generator body. Outside one
		// it is a plain IdentifierReference in sloppy mode; in strict mode it is a
		// reserved word (mirrors `await` outside async).
		if !p.inGenerator {
			if p.lx.strict {
				p.errorf("'yield' is not allowed as an identifier in strict mode")
				p.consume()
				return p.mk(NEmpty)
			}
			n := p.mkIdentFromTok()
			p.consume()
			return n
		}
		// In a generator a YieldExpression is at the AssignmentExpression level
		// (handled by parseAssign); reaching it here means it appeared as a
		// unary/binary operand or other sub-expression position — `void yield`,
		// `1 + yield`, `yield 3 + yield 4` — which is an early SyntaxError.
		p.errorf("A yield expression is only allowed at the top of an assignment expression")
		return p.mk(NEmpty)
	case TokAwait:
		// Outside an async function, `await` is a plain identifier — except in a
		// class static block's direct scope, where it is reserved (but is not the
		// await operator, since a static block is not async).
		if !p.inAsync {
			if p.inStaticBlock {
				p.errorf("'await' is not allowed in a class static block")
				return p.mk(NEmpty)
			}
			n := p.mkIdentFromTok()
			p.consume()
			return n
		}
		p.consume()
		n := p.mk(NAwait)
		n.Right = p.parseUnary()
		// `await x` is a UnaryExpression, so it may not be the base of an
		// un-parenthesized `**` (`await x ** y` needs `(await x) ** y`).
		if p.rejectExpAfterUnary() {
			return p.mk(NEmpty)
		}
		return n
	case TokSuper:
		p.consume()
		// `super` is only a valid expression as super.property, super[expr], or a
		// super(...) call; a bare `super` is an early SyntaxError. (Whether super is
		// allowed in the current context is checked later, in the compiler.)
		if la := p.next(); la != TokDot && la != TokLBracket && la != TokLParen {
			p.errorf("'super' keyword unexpected here")
			return p.mk(NEmpty)
		}
		return mkIdent("super")
	case TokRest:
		p.consume()
		n := p.mk(NSpread)
		n.Right = p.parseAssign()
		return n
	case TokImport:
		return p.parseImportExpr()
	case TokDiv, TokDivAssign:
		return p.parseRegex()
	case TokHash:
		return p.parsePrivateName()
	}
	p.unexpected()
	return p.mk(NEmpty)
}

func (p *parser) parseParen() *Node {
	parenOff := uint32(p.toff())
	p.consume()
	if p.next() == TokRParen {
		p.consume()
		// `()` is not an expression: an empty parenthesized form is only ever an
		// arrow function's parameter list, so anything but `=>` after it is a
		// SyntaxError. (This is what rejects `export default function(){}()`, whose
		// trailing `()` has to start a new statement.)
		if p.next() != TokArrow {
			p.errorf("Unexpected token %q; `()` is only valid as an arrow parameter list", p.tokStr())
			return p.mk(NEmpty)
		}
		n := p.mk(NUndef)
		n.Flags |= fnParen
		n.SrcOff = parenOff
		return n
	}
	outerNoIn := p.noIn
	p.noIn = false
	expr := p.parseParenExpr()
	p.noIn = outerNoIn
	p.expect(TokRParen)
	expr.Flags |= fnParen
	if expr.Kind != NFunc && expr.Kind != NClass {
		expr.SrcOff = parenOff
	}
	return expr
}

func (p *parser) parseAsyncPrimary() *Node {
	asyncOff := uint32(p.toff())
	p.consume()
	hasLineTerm := p.lookaheadCrossesLineTerminator()
	if !hasLineTerm && p.la() == TokFunc {
		p.next()
		p.consume()
		p.pendingAsync = true
		p.pendingFuncExpr = true
		fn := p.parseFunc()
		fn.Flags |= fnAsync | fnFuncExpr
		fn.SrcOff = asyncOff
		return fn
	}
	if hasLineTerm {
		return p.mkIdentFromTok()
	}
	if arrow := p.tryParseAsyncArrow(); arrow != nil {
		return arrow
	}
	return p.mkIdentFromTok()
}

func (p *parser) parseTemplate() *Node {
	p.consume()
	n := p.mk(NTemplate)
	in := p.code()
	base := p.toff()
	tplLen := p.tlen()
	i := 1
	for {
		segStart := i
		for i < tplLen-1 {
			if in[base+i] == '\\' && i+1 < tplLen-1 {
				i += 2
				continue
			}
			if in[base+i] == '$' && i+1 < tplLen-1 && in[base+i+1] == '{' {
				break
			}
			i++
		}
		s := p.mk(NString)
		s.Flags |= fnTemplateSegment
		s.Aux = normalizeCRLF(in[base+segStart : base+i])
		cooked, valid := cookTemplateSegment(in, base+segStart, base+i)
		if valid {
			s.Str = cooked
		} else {
			s.Flags |= fnInvalidCooked
		}
		n.Args = append(n.Args, s)
		if i >= tplLen-1 || in[base+i] != '$' {
			break
		}
		i += 2
		exprStart := i
		exprMaxLen := 0
		if tplLen > 0 && exprStart < tplLen-1 {
			exprMaxLen = tplLen - 1 - exprStart
		}
		sub := in[base+exprStart : base+exprStart+exprMaxLen]
		cp := p.lx.pushSource(sub)
		expr := p.parseExpr()
		n.Args = append(n.Args, expr)
		if p.next() != TokRBrace {
			p.errorf("Unterminated template expression")
			p.lx.popSource(cp)
			return n
		}
		p.consume()
		consumedExpr := p.lx.st.pos
		p.lx.popSource(cp)
		if consumedExpr == 0 {
			p.errorf("Unterminated template expression")
			return n
		}
		i = exprStart + consumedExpr
	}
	return n
}

func (p *parser) parseNew() *Node {
	p.consume()
	if p.next() == TokDot {
		p.consume()
		if p.next() == TokIdentifier && p.tlen() == 6 && p.tokStr() == "target" {
			p.consume()
			if !p.newTargetOK {
				p.errorf("new.target expression is not allowed here")
				return p.mk(NEmpty)
			}
			return p.mk(NNewTarget)
		}
		p.unexpected()
		return p.mk(NEmpty)
	}
	n := p.mk(NNew)
	callee := p.parsePrimary()
	for {
		la := p.next()
		switch la {
		case TokDot, TokLBracket:
			callee = p.parseMemberSuffix(callee, la)
		case TokTemplate:
			// MemberExpression : MemberExpression TemplateLiteral — a tagged template
			// is part of the callee, so `new tag`x`` constructs what tag returns.
			tagged := p.mk(NTaggedTemplate)
			tagged.Left = callee
			tagged.Right = p.parsePrimary()
			callee = tagged
		default:
			la = 0
		}
		if la == 0 {
			break
		}
		if callee != nil && callee.Kind == NEmpty {
			return callee
		}
	}
	// `import(...)` is a CallExpression, so neither it nor a member access rooted
	// at it (`new import(x).prop`) is a valid MemberExpression operand for `new`.
	// A parenthesized `new (import(x))` IS valid, though — the parens make it a
	// PrimaryExpression — so it parses and throws a TypeError only at runtime.
	for b := callee; b != nil && b.Kind == NMember; b = b.Left {
		if b.Left != nil && b.Left.Kind == NImport && b.Left.Flags&fnParen == 0 {
			callee = b.Left
			break
		}
	}
	if callee != nil && callee.Kind == NImport && callee.Flags&fnParen == 0 {
		p.errorf("Invalid new expression: import() is not a valid constructor")
		return p.mk(NEmpty)
	}
	n.Left = callee
	if p.next() == TokLParen {
		p.consume()
		for p.next() != TokRParen && p.tok() != TokEOF {
			if p.tok() == TokRest {
				p.consume()
				spread := p.mk(NSpread)
				spread.Right = p.parseAssign()
				n.Args = append(n.Args, spread)
			} else {
				n.Args = append(n.Args, p.parseAssign())
			}
			if p.next() == TokComma {
				p.consume()
			} else {
				break
			}
		}
		p.expect(TokRParen)
	}
	return n
}

func (p *parser) parseImportExpr() *Node {
	p.consume() // 'import'
	switch p.next() {
	case TokDot:
		p.consume() // '.'
		// `import.meta` is valid only in a Module goal.
		if p.next() == TokIdentifier && p.tlen() == 4 && p.tokStr() == "meta" {
			p.consume()
			if !p.lx.module {
				p.errorf("'import.meta' may only appear in a module")
				return p.mk(NEmpty)
			}
			return p.mk(NImportMeta)
		}
		// import.defer(...) / import.source(...) — the deferred-module and
		// module-source ImportCall proposals; both parse as a dynamic import.
		if p.next() == TokIdentifier && ((p.tlen() == 5 && p.tokStr() == "defer") || (p.tlen() == 6 && p.tokStr() == "source")) {
			p.consume() // 'defer' | 'source'
			if p.next() != TokLParen {
				p.errorf("import.defer / import.source must be called with a specifier")
				return p.mk(NEmpty)
			}
			return p.parseImportCallArgs()
		}
		p.errorf("'import.meta' may only appear in a module")
		return p.mk(NEmpty)
	case TokLParen:
		return p.parseImportCallArgs()
	default:
		p.errorf("'import' is only valid in a call import(...) here")
		return p.mk(NEmpty)
	}
}

// parseImportCallArgs parses `( AssignmentExpression [, AssignmentExpression] )`
// with the current token positioned at the opening '(' (shared by import(),
// import.defer() and import.source()). ImportCall arguments are
// AssignmentExpressions — not a comma sequence, and no spread.
func (p *parser) parseImportCallArgs() *Node {
	p.consume() // '('
	n := p.mk(NImport)
	if p.next() == TokRParen {
		p.errorf("import() requires a specifier argument")
		return p.mk(NEmpty)
	}
	if p.next() == TokRest {
		p.errorf("`...` spread is not allowed in import()")
		return p.mk(NEmpty)
	}
	n.Right = p.parseAssign()
	if p.next() == TokComma {
		p.consume()
		if p.next() != TokRParen {
			if p.next() == TokRest {
				p.errorf("`...` spread is not allowed in import()")
				return p.mk(NEmpty)
			}
			n.Left = p.parseAssign()
			if p.next() == TokComma {
				p.consume() // trailing comma
			}
		}
	}
	p.expect(TokRParen)
	return n
}

func (p *parser) parseRegex() *Node {
	p.consume()
	code := p.code()
	clen := len(code)
	patternStart := p.lx.st.pos
	if p.tok() == TokDivAssign {
		patternStart = p.toff() + 1
		p.lx.st.pos = patternStart
	}
	pos := p.lx.st.pos
	inClass := false
	for pos < clen {
		c := code[pos]
		// A regular-expression literal may not contain a LineTerminator (LF, CR,
		// LS, PS), even inside a class or after a backslash — it is a SyntaxError.
		if c == '\n' || c == '\r' || isLSorPS(code, pos, clen) {
			p.errorf("regular expression literal may not contain a line terminator")
			return p.mk(NEmpty)
		}
		if c == '\\' && pos+1 < clen {
			if n := code[pos+1]; n == '\n' || n == '\r' || isLSorPS(code, pos+1, clen) {
				p.errorf("regular expression literal may not contain a line terminator")
				return p.mk(NEmpty)
			}
			pos += 2
			continue
		}
		if c == '[' {
			inClass = true
		} else if c == ']' {
			inClass = false
		} else if c == '/' && !inClass {
			break
		}
		pos++
	}
	patternEnd := pos
	if pos < clen {
		pos++
	}
	flagsStart := pos
	for pos < clen {
		c := code[pos]
		if c == 'd' || c == 'g' || c == 'i' || c == 'm' || c == 's' || c == 'u' || c == 'v' || c == 'y' {
			pos++
		} else {
			break
		}
	}
	flagsEnd := pos
	n := p.mk(NRegexp)
	n.Str = code[patternStart:patternEnd]
	n.Aux = code[flagsStart:flagsEnd]
	p.lx.st.pos = pos
	p.lx.st.consumed = true
	return n
}

func (p *parser) parsePrivateName() *Node {
	if !p.privateNameAdjacent() || !isPrivateIdentLikeTok(p.tok()) {
		p.errorf("private field name expected")
		return p.mk(NEmpty)
	}
	n := p.mkPrivateIdentFromTok()
	p.consume()
	return n
}

// ---- array & object literals ----

func (p *parser) parseArray() *Node {
	p.consume()
	n := p.mk(NArray)
	// `in` is always permitted inside array-literal brackets, even when the
	// enclosing context suppresses it (a for-in/for-of head): the [~In] guard
	// only governs the head's top level, not a nested element or default.
	outerNoIn := p.noIn
	p.noIn = false
	defer func() { p.noIn = outerNoIn }()
	for p.next() != TokRBracket && p.tok() != TokEOF {
		if p.tok() == TokComma {
			p.consume()
			n.Args = append(n.Args, p.mk(NEmpty))
			continue
		}
		if p.tok() == TokRest {
			p.consume()
			spread := p.mk(NSpread)
			spread.Right = p.parseAssign()
			n.Args = append(n.Args, spread)
			if p.next() == TokComma {
				// A rest element followed by a comma is valid spread in an array
				// literal but invalid in a binding/assignment pattern.
				n.Flags |= nodeRestComma
			}
		} else {
			n.Args = append(n.Args, p.parseAssign())
		}
		if p.next() == TokComma {
			p.consume()
		} else {
			break
		}
	}
	p.expect(TokRBracket)
	return n
}

// isReservedWordTok reports whether a token is a reserved word that may never be
// used as a binding identifier or identifier reference. The contextual keywords
// (async/await/from/let/of/yield/as/static/using/undefined/…) are excluded — they
// are handled by strictCheckBindingIdent or their own context checks.
func isReservedWordTok(t Token) bool {
	switch t {
	case TokBreak, TokCase, TokCatch, TokClass, TokConst, TokContinue,
		TokDefault, TokDelete, TokDo, TokDebugger, TokElse, TokExport,
		TokFinally, TokFor, TokFunc, TokIf, TokImport, TokIn, TokInstanceof,
		TokNew, TokReturn, TokSuper, TokSwitch, TokThis, TokThrow, TokTry,
		TokVar, TokVoid, TokWhile, TokWith, TokTypeof, TokNull, TokTrue, TokFalse:
		return true
	}
	return false
}

// tokIsEscapedReservedWord reports whether the current TokIdentifier is a
// reserved word written with a Unicode escape (e.g. `break` → "break").
// Such a token is a valid property name but not a valid identifier reference or
// binding identifier.
func (p *parser) tokIsEscapedReservedWord() bool {
	if p.tok() != TokIdentifier {
		return false
	}
	raw := p.tokStr()
	hasEsc := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' {
			hasEsc = true
			break
		}
	}
	if !hasEsc {
		return false
	}
	return isReservedWordTok(parseKeyword(p.tokIdentStr()))
}

// validatePatternTarget dispatches to the array/object pattern validator for a
// nested destructuring target (a no-op for a plain identifier/member target).
func (p *parser) validatePatternTarget(n *Node) {
	switch {
	case n == nil:
	case n.Kind == NArray:
		p.validateArrayPattern(n)
	case n.Kind == NObject:
		p.validateObjectPattern(n)
	case n.Kind == NIdent:
		// A leaf AssignmentTarget inside a destructuring assignment pattern must be
		// a valid IdentifierReference: in strict mode it may not be `eval`/
		// `arguments` nor a strict future-reserved word (`let`, `static`,
		// `implements`, … — escaped or not, since n.Str is the decoded name).
		if p.lx.strict && p.strictForbiddenBinding(n.Str) {
			p.errorf("Invalid assignment target: '%s' is reserved in strict mode", n.Str)
		}
	}
}

// validateObjectPattern raises a SyntaxError when an object pattern misuses its
// rest element (an AssignmentRestProperty / BindingRestProperty must be the last
// element, may not be followed by a comma, and may not carry a default). It
// recurses into nested patterns.
func (p *parser) validateObjectPattern(n *Node) {
	if n == nil || n.Kind != NObject {
		return
	}
	if n.Flags&nodeRestComma != 0 {
		p.errorf("Rest element must be last element")
		return
	}
	for i, prop := range n.Args {
		if prop == nil {
			continue
		}
		if prop.Kind == NSpread || prop.Kind == NRest {
			if i != len(n.Args)-1 {
				p.errorf("Rest element must be last element")
				return
			}
			if prop.Right != nil && (prop.Right.Kind == NAssign || prop.Right.Kind == NAssignPat) {
				p.errorf("`...` rest element may not have a default initializer")
				return
			}
			continue
		}
		target := prop.Right
		if target != nil && (target.Kind == NAssign || target.Kind == NAssignPat) {
			target = target.Left
		}
		p.validatePatternTarget(target)
	}
}

// validateArrayPattern raises a SyntaxError when an array pattern misuses its
// rest element: a rest must be the last element and may not be followed by a
// comma (BindingRestElement / AssignmentRestElement). It recurses into nested
// array/object patterns.
func (p *parser) validateArrayPattern(n *Node) {
	if n == nil || n.Kind != NArray {
		return
	}
	if n.Flags&nodeRestComma != 0 {
		p.errorf("Rest element must be last element")
		return
	}
	for i, e := range n.Args {
		if e == nil {
			continue
		}
		if (e.Kind == NSpread || e.Kind == NRest) && i != len(n.Args)-1 {
			p.errorf("Rest element must be last element")
			return
		}
		switch e.Kind {
		case NArray, NObject, NIdent:
			p.validatePatternTarget(e)
		case NSpread, NRest:
			// A rest element is a BindingRestElement / AssignmentRestElement: it may
			// not carry a default initializer (`[...x = 1]` is an early SyntaxError).
			if e.Right != nil && (e.Right.Kind == NAssign || e.Right.Kind == NAssignPat) {
				p.errorf("`...` rest element may not have a default initializer")
				return
			}
			p.validatePatternTarget(e.Right)
		case NAssign, NAssignPat:
			p.validatePatternTarget(e.Left)
		}
	}
}

func (p *parser) validateAccessorParams(fn *Node, flags uint32) bool {
	if flags&(fnGetter|fnSetter) == 0 || fn == nil {
		return true
	}
	if flags&fnGetter != 0 && len(fn.Args) != 0 {
		p.errorf("Getter must not have parameters")
		return false
	}
	if flags&fnSetter != 0 && (len(fn.Args) != 1 ||
		(len(fn.Args) == 1 && fn.Args[0] != nil && fn.Args[0].Kind == NRest)) {
		p.errorf("Setter must have exactly one non-rest parameter")
		return false
	}
	return true
}

// parseObjectKey parses a computed/number/string/ident property key into prop.
func (p *parser) parseObjectKey(prop *Node) {
	switch {
	case p.tok() == TokLBracket:
		p.consume()
		prop.Left = p.parseAssign()
		p.expect(TokRBracket)
		prop.Flags |= fnComputed
	case p.tok() == TokNumber:
		p.consume()
		prop.Left = mkNum(tod(p.tval()))
	case p.tok() == TokBigInt:
		// A BigInt literal key (`{1n: v}`, `1n(){}`) names the property by its
		// BigInt::toString — the decimal digits — handled in propKeyName.
		n := p.mk(NBigInt)
		n.Str = p.tokStr()
		prop.Left = n
		p.consume()
	case p.tok() == TokString:
		prop.Left = p.mkStringFromTok()
		p.consume()
	case p.tok() == TokHash:
		// A private name is only valid as a class element key, never in an object
		// literal (`{ get #x(){} }`, `{ *#x(){} }`).
		p.errorf("Private names are not allowed in object literals")
	default:
		prop.Left = p.mkIdentFromTok()
		p.consume()
	}
}

func (p *parser) parseObject() *Node {
	p.consume()
	n := p.mk(NObject)
	protoSet := false
	// As with array literals, `in` is unrestricted inside object-literal braces
	// regardless of an enclosing for-head [~In] context.
	outerNoIn := p.noIn
	p.noIn = false
	defer func() { p.noIn = outerNoIn }()
	for p.next() != TokRBrace && p.tok() != TokEOF {
		prop := p.mk(NProperty)

		if p.tok() == TokRest {
			p.consume()
			spread := p.mk(NSpread)
			spread.Right = p.parseAssign()
			n.Args = append(n.Args, spread)
			if p.next() == TokComma {
				p.consume()
				n.Flags |= nodeRestComma // a rest followed by a comma is invalid in a pattern
			}
			continue
		}

		if p.tok() == TokLBracket {
			p.consume()
			prop.Left = p.parseAssign()
			p.expect(TokRBracket)
			prop.Flags |= fnComputed
		} else if p.tok() == TokMul {
			prop.Flags |= fnGenerator
			p.consume()
			p.next()
			p.parseObjectKey(prop)
			p.pendingGenerator = true // body gets a yield context
			prop.Right = p.parseFunc()
			prop.Right.Flags |= fnGenerator | fnMethod
			prop.Right.SrcOff = prop.SrcOff
			n.Args = append(n.Args, prop)
			if p.next() == TokComma {
				p.consume()
			}
			continue
		} else if p.tlen() == 3 && (p.tokStr() == "get" || p.tokStr() == "set") {
			gs := p.tokStr()[0]
			la := p.la()
			if la != TokColon && la != TokLParen && la != TokComma && la != TokRBrace {
				p.consume()
				if gs == 'g' {
					prop.Flags |= fnGetter
				} else {
					prop.Flags |= fnSetter
				}
				p.next()
				p.parseObjectKey(prop)
				prop.Right = p.parseFunc()
				if !p.validateAccessorParams(prop.Right, prop.Flags) {
					return n
				}
				prop.Right.Flags |= fnMethod
				prop.Right.SrcOff = prop.SrcOff
				n.Args = append(n.Args, prop)
				if p.next() == TokComma {
					p.consume()
				}
				continue
			}
			prop.Left = p.mkIdentFromTok()
			p.consume()
		} else if p.tok() == TokAsync {
			la := p.la()
			// `async [no LineTerminator here] MethodDefinition`: a line terminator
			// after `async` makes it an ordinary property name, not the async
			// modifier (`{ async \n foo(){} }` is then a missing-comma SyntaxError).
			if la != TokColon && la != TokLParen && la != TokComma && la != TokRBrace &&
				!p.lookaheadCrossesLineTerminator() {
				p.consume()
				prop.Flags |= fnAsync
				p.next()
				if p.tok() == TokMul {
					prop.Flags |= fnGenerator
					p.consume()
					p.next()
				}
				p.parseObjectKey(prop)
				p.pendingAsync = true
				p.pendingGenerator = prop.Flags&fnGenerator != 0
				prop.Right = p.parseFunc()
				prop.Right.Flags |= fnAsync | fnMethod
				if prop.Flags&fnGenerator != 0 {
					prop.Right.Flags |= fnGenerator
				}
				prop.Right.SrcOff = prop.SrcOff
				n.Args = append(n.Args, prop)
				if p.next() == TokComma {
					p.consume()
				}
				continue
			}
			prop.Left = p.mkIdentFromTok()
			p.consume()
		} else if p.tok() == TokNumber {
			p.consume()
			prop.Left = mkNum(tod(p.tval()))
		} else if p.tok() == TokBigInt {
			// A BigInt literal key (`{1n: v}`) names the property by its
			// BigInt::toString (decimal digits); propKeyName performs the conversion.
			bi := p.mk(NBigInt)
			bi.Str = p.tokStr()
			prop.Left = bi
			p.consume()
		} else if p.tok() == TokString {
			prop.Left = p.mkStringFromTok()
			p.consume()
		} else {
			prop.Left = p.mkIdentFromTok()
			p.consume()
		}

		if p.next() == TokColon {
			p.consume()
			prop.Flags |= fnColon
			// A plain data property named __proto__ (via identifier or string key,
			// but NOT a computed key) is the prototype setter; two of them error.
			if prop.Flags&fnComputed == 0 && prop.Left != nil &&
				(prop.Left.Kind == NIdent || prop.Left.Kind == NString) &&
				prop.Left.Str == "__proto__" {
				// Two `__proto__:` data properties are an error in an object literal,
				// but the same syntax is a valid destructuring pattern (they are
				// ordinary AssignmentProperties there). Defer the error: flag the node
				// and let compileObject reject it only if it stays a literal.
				if protoSet {
					n.Flags |= nodeDupProto
				}
				protoSet = true
			}
			prop.Right = p.parseAssign()
		} else if p.tok() == TokLParen {
			prop.Right = p.parseFunc()
			prop.Right.Flags |= fnMethod
			prop.Right.SrcOff = prop.SrcOff
		} else {
			// Shorthand `{ id }` / `{ id = default }`: the key must be a plain
			// IdentifierReference — a numeric/string literal or a computed key needs
			// an explicit `: value` and is not a valid shorthand.
			if prop.Flags&fnComputed != 0 || prop.Left == nil || prop.Left.Kind != NIdent {
				p.errorf("Unexpected token; a shorthand property must be an identifier")
				return n
			}
			// id is an IdentifierReference, so it may not be a reserved word (even one
			// written with escapes, since prop.Left.Str is the decoded StringValue).
			// `enum` and `extends` are reserved but have no dedicated token — they lex
			// as identifiers — so they are checked by name.
			if prop.Left.Kind == NIdent && (isReservedWordTok(parseKeyword(prop.Left.Str)) ||
				prop.Left.Str == "enum" || prop.Left.Str == "extends") {
				p.errorf("Unexpected reserved word")
				return n
			}
			// `yield`/`await` may not be a shorthand IdentifierReference inside a
			// generator / async body (where they are always keywords). A class
			// static block reserves `await` too (`{ static { var {await} = {} } }`).
			if prop.Left.Kind == NIdent && p.inGenerator && prop.Left.Str == "yield" {
				p.errorf("'yield' cannot be used as an identifier here")
				return n
			}
			if prop.Left.Kind == NIdent && (p.inAsync || p.inStaticBlock) && prop.Left.Str == "await" {
				p.errorf("'await' cannot be used as an identifier here")
				return n
			}
			// A strict future-reserved word (`let`, `static`, `package`, …) is not a
			// valid IdentifierReference in strict mode. (`eval`/`arguments` ARE valid
			// references — only assignment to them is restricted — so they are fine
			// as a shorthand value.)
			if prop.Left.Kind == NIdent && p.lx.strict && isStrictReservedName(prop.Left.Str) {
				p.errorf("Unexpected strict-mode reserved word")
				return n
			}
			prop.Right = mkIdent(prop.Left.Str)
			if p.next() == TokAssign {
				p.consume()
				def := p.mk(NAssign)
				def.Op = TokAssign
				def.Left = prop.Right
				def.Right = p.parseAssign()
				prop.Right = def
				// `{ id = expr }` is a CoverInitializedName: valid only when this
				// object is later reinterpreted as a destructuring pattern. Compiling
				// it as a plain object literal is an early SyntaxError.
				n.Flags |= nodeHasCoverInit
			}
		}
		n.Args = append(n.Args, prop)
		if p.next() == TokComma {
			p.consume()
		} else {
			break
		}
	}
	p.expect(TokRBrace)
	return n
}

// ---- call / postfix / unary / binary / ternary / assign ----

func (p *parser) parseCall() *Node {
	n := p.parsePrimary()
	// A TemplateLiteral may not appear in the tail of an OptionalChain
	// (`a?.b`x``): parenthesizing (`(a?.b)`x``) starts a fresh parseCall, so the
	// flag correctly resets there.
	sawOptional := false
	for {
		la := p.next()
		switch la {
		case TokLParen:
			p.consume()
			call := p.mk(NCall)
			call.Left = n
			for p.next() != TokRParen && p.tok() != TokEOF {
				if p.tok() == TokRest {
					p.consume()
					spread := p.mk(NSpread)
					spread.Right = p.parseAssign()
					call.Args = append(call.Args, spread)
				} else {
					call.Args = append(call.Args, p.parseAssign())
				}
				if p.next() == TokComma {
					p.consume()
				} else {
					break
				}
			}
			p.expect(TokRParen)
			n = call
		case TokDot, TokLBracket:
			n = p.parseMemberSuffix(n, la)
			if n != nil && n.Kind == NEmpty {
				return n
			}
		case TokOptionalChain:
			sawOptional = true
			p.consume()
			opt := p.mk(NOptional)
			opt.Left = n
			if p.next() == TokLBracket {
				p.consume()
				opt.Right = p.parseExpr()
				opt.Flags = 1
				p.expect(TokRBracket)
			} else if p.tok() == TokLParen {
				call := p.mk(NCall)
				call.Left = opt
				p.consume()
				for p.next() != TokRParen && p.tok() != TokEOF {
					call.Args = append(call.Args, p.parseAssign())
					if p.next() == TokComma {
						p.consume()
					} else {
						break
					}
				}
				p.expect(TokRParen)
				n = call
				continue
			} else if p.tok() == TokHash {
				if !p.privateNameAdjacent() || !isPrivateIdentLikeTok(p.tok()) {
					p.errorf("private field name expected")
					return p.mk(NEmpty)
				}
				opt.Right = p.mkPrivateIdentFromTok()
				p.consume()
			} else {
				opt.Right = p.parseDotPropertyName()
				if opt.Right == nil {
					return p.mk(NEmpty)
				}
			}
			n = opt
		case TokTemplate:
			if sawOptional {
				p.errorf("Tagged template literals may not be used in an optional chain")
				return p.mk(NEmpty)
			}
			tagged := p.mk(NTaggedTemplate)
			tagged.Left = n
			tagged.Right = p.parsePrimary()
			n = tagged
		default:
			return n
		}
	}
}

func (p *parser) parsePostfix() *Node {
	n := p.parseCall()
	la := p.next()
	if p.isLeadingDotNumberTok() && !p.hadNewline() {
		p.unexpected()
		return p.mk(NEmpty)
	}
	if (la == TokPostInc || la == TokPostDec) && !p.hadNewline() {
		if !isValidAssignTarget(n, true) && !(n.Kind == NCall && !p.lx.strict) { // ++/-- need a simple assignment target
			p.errorf("Invalid left-hand side expression in postfix operation")
			return p.mk(NEmpty)
		}
		if p.isStrictRestrictedAssignTarget(n) {
			p.errorf("cannot modify eval or arguments in strict mode")
			return p.mk(NEmpty)
		}
		p.consume()
		u := p.mk(NUpdate)
		u.Op = la
		u.Right = n
		return u
	}
	return n
}

// rejectExpAfterUnary reports (and records an error) when a UnaryExpression is
// immediately followed by `**`: the base of an ExponentiationExpression may not
// be an un-parenthesized unary expression (`-x ** y`, `typeof x ** y`).
func (p *parser) rejectExpAfterUnary() bool {
	if p.next() == TokExp {
		p.errorf("Unary operator used immediately before exponentiation expression. Parenthesis must be used to disambiguate operator precedence")
		return true
	}
	return false
}

func (p *parser) parseUnary() *Node {
	la := p.next()
	if la == TokNot || la == TokTilda || la == TokUPlus || la == TokUMinus ||
		la == TokPlus || la == TokMinus {
		p.consume()
		n := p.mk(NUnary)
		switch la {
		case TokPlus:
			n.Op = TokUPlus
		case TokMinus:
			n.Op = TokUMinus
		default:
			n.Op = la
		}
		n.Right = p.parseUnary()
		if p.rejectExpAfterUnary() {
			return p.mk(NEmpty)
		}
		return n
	}
	if la == TokPostInc || la == TokPostDec {
		p.consume()
		target := p.parseUnary()
		if !isValidAssignTarget(target, true) && !(target.Kind == NCall && !p.lx.strict) { // ++/-- need a simple assignment target
			p.errorf("Invalid left-hand side expression in prefix operation")
			return p.mk(NEmpty)
		}
		if p.isStrictRestrictedAssignTarget(target) {
			p.errorf("cannot modify eval or arguments in strict mode")
			return p.mk(NEmpty)
		}
		n := p.mk(NUpdate)
		n.Op = la
		n.Right = target
		n.Flags = 1
		return n
	}
	if la == TokThrow {
		p.consume()
		n := p.mk(NThrow)
		n.Right = p.parseAssign()
		return n
	}
	return p.parsePostfix()
}

// isBareLogicalOp reports whether n is an un-parenthesized binary expression
// whose operator is one of ops — used to forbid mixing `??` with `&&`/`||`.
func isBareLogicalOp(n *Node, ops ...Token) bool {
	if n == nil || n.Kind != NBinary || n.Flags&fnParen != 0 {
		return false
	}
	for _, op := range ops {
		if n.Op == op {
			return true
		}
	}
	return false
}

func (p *parser) parseBinary(minPrec int) *Node {
	left := p.parseUnary()
	for {
		op := p.next()
		if op >= TokMax {
			break
		}
		if op == TokIn && p.noIn {
			break
		}
		prec := int(precTable[op])
		if prec == 0 || prec < minPrec {
			break
		}
		p.consume()
		nextPrec := prec + 1
		if op == TokExp {
			nextPrec = prec
		}
		right := p.parseBinary(nextPrec)
		bin := p.mk(NBinary)
		bin.Op = op
		bin.Left = left
		bin.Right = right
		// `??` may not be combined with `&&` or `||` at the same level without
		// parentheses (`a ?? b && c`, `a || b ?? c` are early SyntaxErrors).
		mixed := false
		if op == TokNullish {
			mixed = isBareLogicalOp(bin.Left, TokLand, TokLor) || isBareLogicalOp(bin.Right, TokLand, TokLor)
		} else if op == TokLand || op == TokLor {
			mixed = isBareLogicalOp(bin.Left, TokNullish) || isBareLogicalOp(bin.Right, TokNullish)
		}
		if mixed {
			p.errorf("'??' cannot be mixed with '&&' or '||'; wrap an operand in parentheses")
			return p.mk(NEmpty)
		}
		left = bin
	}
	return left
}

func (p *parser) parseTernary() *Node {
	cond := p.parseBinary(1)
	if p.next() == TokQ {
		p.consume()
		n := p.mk(NTernary)
		n.Cond = cond
		// The consequent is [+In] whatever the enclosing context; only the
		// alternate inherits the [~In] of a for-head.
		outerNoIn := p.noIn
		p.noIn = false
		n.Left = p.parseAssign()
		p.noIn = outerNoIn
		p.expect(TokColon)
		n.Right = p.parseAssign()
		return n
	}
	return cond
}

// isValidAssignTarget reports whether n may be the target of an assignment: an
// identifier or member reference, or — for a plain `=` (compound == false) — an
// array/object destructuring pattern. Everything else (a literal, call,
// `this`, binary/update expression, …) is an invalid AssignmentTarget.
func isValidAssignTarget(n *Node, compound bool) bool {
	if n == nil {
		return false
	}
	switch n.Kind {
	case NIdent, NMember:
		return true
	case NUndef, NGlobalThis:
		// `undefined` / `globalThis` are IdentifierReferences (goant models them as
		// their own node kinds); assigning is a sloppy no-op / strict TypeError, not
		// a SyntaxError — unlike the literals null/true/false or `this`.
		return true
	case NArray, NObject, NArrayPat, NObjectPat:
		return !compound
	}
	return false
}

// parseYieldExpr parses a YieldExpression in a generator body. It sits at the
// AssignmentExpression level, so its operand is itself an AssignmentExpression
// (`yield a = b`, `yield yield x`), and `yield` may not be a sub-expression
// operand — parsePrimary rejects a `yield` reached below this level.
func (p *parser) parseYieldExpr() *Node {
	p.consume() // yield
	n := p.mk(NYield)
	if p.next() == TokMul {
		if p.hadNewline() {
			// yield [no LineTerminator here] * — a newline before `*` is invalid.
			p.errorf("No line terminator is allowed before '*' in a yield expression")
			return p.mk(NEmpty)
		}
		p.consume()
		n.Flags = 1
	}
	// yield [no LineTerminator here] AssignmentExpression: without `*`, a newline
	// terminates the expression (bare `yield`); `yield*` always takes an operand.
	if n.Flags&1 == 0 && p.hadNewline() {
		return n
	}
	if t := p.tok(); t != TokSemicolon && t != TokRBrace && t != TokRParen &&
		t != TokRBracket && t != TokEOF && t != TokComma && t != TokColon {
		n.Right = p.parseAssign()
	}
	return n
}

func (p *parser) parseAssign() *Node {
	if p.inGenerator && p.next() == TokYield {
		return p.parseYieldExpr()
	}
	left := p.parseTernary()
	op := p.next()
	if op == TokArrow {
		if p.hadNewline() {
			p.errorf("Line terminator not allowed before arrow")
			return p.mk(NEmpty)
		}
		p.consume()
		fn := p.mk(NFunc)
		fn.Flags = fnArrow
		fn.SrcOff = left.SrcOff
		if left.Kind == NIdent {
			// A single unparenthesized arrow parameter is a BindingIdentifier, so it
			// may not be a reserved word (`enum => 1`) or a strict/context-reserved
			// name — the parenthesized form is validated separately.
			p.strictCheckBindingIdent(left.Str)
			fn.Args = append(fn.Args, left)
		} else if left.Kind == NUndef && left.Flags&fnParen == 0 {
			// `undefined => …` — a single unparenthesized `undefined` parameter.
			fn.Args = append(fn.Args, &Node{Kind: NIdent, Str: "undefined", SrcOff: left.SrcOff, SrcEnd: left.SrcEnd})
		} else if left.Flags&fnParen != 0 {
			pushArrowParamsFromExpr(fn, left)
		} else {
			p.errorf("Malformed arrow function parameter list")
			return p.mk(NEmpty)
		}
		fn.Body = p.parseArrowBody(false)
		p.checkArrowEarlyErrors(fn)
		fn.SrcEnd = nodeSrcEnd(p, fn.Body)
		return fn
	}
	if isAssignOp(op) {
		if left.Kind == NNewTarget {
			p.errorf("Invalid left-hand side in assignment")
			return p.mk(NEmpty)
		}
		if left.Flags&fnParen != 0 && op == TokAssign &&
			(left.Kind == NObject || left.Kind == NObjectPat ||
				left.Kind == NArray || left.Kind == NArrayPat) {
			p.errorf("Invalid destructuring assignment target")
			return p.mk(NEmpty)
		}
		if p.isStrictRestrictedAssignTarget(left) {
			p.errorf("cannot modify eval or arguments in strict mode")
			return p.mk(NEmpty)
		}
		// The target must be a valid AssignmentTarget: an identifier or member
		// reference, or (for plain `=`) an array/object destructuring pattern.
		// Anything else (a literal, call, `this`, binary/update expression, …) is
		// an early SyntaxError — except a CallExpression in sloppy code, which the
		// Annex B web-compat semantics defer to a runtime ReferenceError.
		if !isValidAssignTarget(left, op != TokAssign) && !(left.Kind == NCall && !p.lx.strict) {
			p.errorf("Invalid left-hand side in assignment")
			return p.mk(NEmpty)
		}
		if op == TokAssign && (left.Kind == NArray || left.Kind == NObject) {
			// Destructuring assignment: validate the array/object pattern.
			p.validatePatternTarget(left)
		}
		p.consume()
		n := p.mk(NAssign)
		n.Op = op
		n.Left = left
		n.Right = p.parseAssign()
		return n
	}
	return left
}

func (p *parser) parseExpr() *Node {
	left := p.parseAssign()
	for p.next() == TokComma {
		p.consume()
		n := p.mk(NSequence)
		n.Left = left
		n.Right = p.parseAssign()
		left = n
	}
	return left
}

func (p *parser) parseParenExpr() *Node {
	left := p.parseAssign()
	for p.next() == TokComma {
		p.consume()
		if p.next() == TokRParen && p.la() == TokArrow {
			// A trailing comma is permitted in an arrow parameter list, but not
			// after a rest element: `(...a,) => {}` / `(a, ...b,) => {}` are early
			// SyntaxErrors (BindingRestElement may not be followed by a comma).
			last := left
			for last != nil && last.Kind == NSequence {
				last = last.Right
			}
			if last != nil && last.Kind == NSpread {
				p.errorf("Rest parameter may not be followed by a trailing comma")
			}
			break
		}
		n := p.mk(NSequence)
		n.Left = left
		n.Right = p.parseAssign()
		left = n
	}
	return left
}

// ---- functions & classes ----

// paramsContainYieldAwait reports whether any of a function's parameter nodes
// contains a YieldExpression (when yield) or an AwaitExpression (when await) —
// an early error for a generator's / async function's FormalParameters. It does
// not descend into a nested non-arrow function, which establishes its own
// yield/await context; an arrow inherits the enclosing one, so it is searched.
func paramsContainYieldAwait(params []*Node, yield, await bool) bool {
	var walk func(n *Node) bool
	walk = func(n *Node) bool {
		if n == nil {
			return false
		}
		switch n.Kind {
		case NYield:
			if yield {
				return true
			}
		case NAwait:
			if await {
				return true
			}
		case NFunc:
			if n.Flags&fnArrow == 0 {
				return false
			}
		}
		if walk(n.Left) || walk(n.Right) || walk(n.Body) ||
			walk(n.Init) || walk(n.Cond) || walk(n.Update) {
			return true
		}
		for _, a := range n.Args {
			if walk(a) {
				return true
			}
		}
		return false
	}
	for _, p := range params {
		if walk(p) {
			return true
		}
	}
	return false
}

// checkStrictParams enforces the strict-mode FormalParameters early errors on a
// simple (identifier) parameter list: names must be unique and none may be
// `eval`, `arguments`, or a strict future-reserved word. Non-simple lists are
// handled separately (a strict directive with non-simple params is itself an
// error), so only NIdent parameters are inspected here.
func (p *parser) checkStrictParams(fn *Node) {
	seen := map[string]bool{}
	for _, param := range fn.Args {
		if param.Kind == NIdent {
			if seen[param.Str] {
				p.errorf("Duplicate parameter name not allowed in this context")
			}
			seen[param.Str] = true
			if isEvalOrArgumentsName(param.Str) || isStrictReservedName(param.Str) {
				p.errorf("Unexpected strict-mode reserved parameter name '%s'", param.Str)
			}
		}
	}
}

func (p *parser) parseFunc() *Node {
	fn := p.mk(NFunc)
	// This function's async-ness (set by the caller via pendingAsync). Its body
	// establishes the await context; its parameter list does not (await in an
	// async function's params is a SyntaxError).
	isAsync := p.pendingAsync
	p.pendingAsync = false
	isGenerator := p.pendingGenerator // a method whose `*` the caller already ate
	p.pendingGenerator = false
	isExpr := p.pendingFuncExpr
	p.pendingFuncExpr = false
	savedAsync := p.inAsync
	savedGen := p.inGenerator
	savedSB := p.inStaticBlock
	p.inAsync = false      // parameters
	p.inGenerator = false  // parameters
	p.inStaticBlock = false // a nested function has its own await context
	defer func() { p.inAsync = savedAsync; p.inGenerator = savedGen; p.inStaticBlock = savedSB }()
	if p.next() == TokMul {
		p.consume()
		isGenerator = true
	}
	if isGenerator {
		fn.Flags |= fnGenerator
	}
	// The optional BindingIdentifier accepts the contextual keywords
	// yield/await/let/static as names too; their validity in the current context
	// is enforced just below (generator/async) and by strictCheckBindingIdent.
	nameTok := p.next()
	if isIdentLikeTok(nameTok) || nameTok == TokYield || nameTok == TokAwait ||
		nameTok == TokLet || nameTok == TokStatic {
		fn.Str = p.tokIdentStr()
		// A FunctionDeclaration's name is a BindingIdentifier in the ENCLOSING
		// yield/await context; a FunctionExpression's name is [~Yield, ~Await],
		// constrained only by the function's OWN generator/async-ness. So
		// `function* g(){ (function yield(){}); }` is valid but
		// `function* yield(){}` (a generator expression) is not.
		yieldGen, awaitAsync := savedGen, savedAsync
		if isExpr {
			yieldGen, awaitAsync = isGenerator, isAsync
		} else if savedSB {
			// A FunctionDeclaration directly in a class static block: `await` is
			// reserved there (`static { function await() {} }` is a SyntaxError).
			awaitAsync = true
		}
		if yieldGen && fn.Str == "yield" {
			p.errorf("'yield' cannot be used as a binding identifier in a generator")
		} else if awaitAsync && fn.Str == "await" {
			p.errorf("'await' cannot be used as a binding identifier in an async function")
		}
		p.strictCheckBindingIdent(fn.Str)
		p.consume()
	}
	p.expect(TokLParen)
	// A generator's FormalParameters are [+Yield] and an async function's are
	// [+Await]: `yield`/`await` are forbidden as a parameter binding there (and a
	// yield/await expression in a default is a separate early error, below). The
	// name above was intentionally checked in the *enclosing* context.
	p.inGenerator = isGenerator
	p.inAsync = isAsync
	for p.next() != TokRParen && p.tok() != TokEOF {
		if p.tok() == TokRest {
			p.consume()
			rest := p.mk(NRest)
			rest.Right = p.parseBindingPattern()
			fn.Args = append(fn.Args, rest)
			break
		}
		param := p.parseBindingPattern()
		if p.next() == TokAssign {
			p.consume()
			def := p.mk(NAssignPat)
			def.Left = param
			def.Right = p.parseAssign()
			param = def
		}
		fn.Args = append(fn.Args, param)
		if p.next() == TokComma {
			p.consume()
			if p.next() == TokRParen {
				break
			}
		} else {
			break
		}
	}
	p.expect(TokRParen)
	// A YieldExpression may not appear in a generator's FormalParameters, nor an
	// AwaitExpression in an async function's (`function*(x = yield)`,
	// `async function(x = await p)`).
	if paramsContainYieldAwait(fn.Args, isGenerator, isAsync) {
		p.errorf("yield/await is not allowed in this function's parameters")
	}
	// In strict code, parameter names must be unique and not reserved/eval/
	// arguments (checked here because the enclosing strictness is already known
	// for a preceding directive). If the enclosing code is sloppy, the function's
	// own "use strict" body directive can still make it strict — re-checked below,
	// once the body has been parsed.
	if p.lx.strict {
		p.checkStrictParams(fn)
	}
	p.inAsync = isAsync         // the body establishes the await context
	p.inGenerator = isGenerator // and the yield context
	p.funcDepth++               // return is legal inside the body
	savedNT := p.newTargetOK
	p.newTargetOK = true // a non-arrow function body has a meaningful new.target
	fn.Body = p.parseBlock(true)
	p.newTargetOK = savedNT
	p.funcDepth--
	fn.SrcEnd = uint32(p.toff() + p.tlen())
	// A function with a non-simple parameter list (rest / default / destructuring)
	// may not carry an explicit "use strict" directive (ES2016 §14.1.2).
	if bodyHasUseStrict(fn.Body) && hasNonSimpleParams(fn) {
		p.errorf("Illegal 'use strict' directive in function with non-simple parameter list")
	}
	// A sloppy-enclosed function whose body opens with "use strict" is itself
	// strict, so its (simple) parameters — and its own name — must satisfy the
	// strict restrictions (BindingIdentifier may not be eval/arguments or a
	// strict future-reserved word). The name was parsed in the enclosing
	// (sloppy) context, so re-check it here.
	if !p.lx.strict && bodyHasUseStrict(fn.Body) {
		p.checkStrictParams(fn)
		if fn.Str != "" && (isEvalOrArgumentsName(fn.Str) || isStrictReservedName(fn.Str)) {
			p.errorf("'%s' may not be used as a function name in strict mode", fn.Str)
		}
	}
	if fn.Flags&fnArrow == 0 && (referencesArguments(fn.Body) || paramsReferenceArguments(fn.Args)) {
		// `arguments` is available in parameter default expressions too (they are
		// evaluated after the arguments object is instantiated), so a reference
		// there — `function*(x = arguments[2]){}` — must mark the function.
		fn.Flags |= fnUsesArgs
	}
	if fn.Flags&fnArrow == 0 && referencesNewTarget(fn.Body) {
		fn.Flags |= fnUsesNewTarget
	}
	return fn
}

func (p *parser) parseClass() *Node {
	cls := p.mk(NClass)
	if isIdentLikeTok(p.next()) && !(p.tlen() == 7 && p.tokStr() == "extends") {
		cls.Str = p.tokIdentStr()
		// A class definition is strict code, so its name is a strict
		// BindingIdentifier: not eval/arguments or a strict future-reserved word
		// (`class eval {}`, `class package {}` are SyntaxErrors). `await` is handled
		// separately below since it is contextual, not strict-reserved.
		if isEvalOrArgumentsName(cls.Str) || isStrictReservedName(cls.Str) {
			p.errorf("'%s' may not be used as a class name", cls.Str)
		}
		p.consume()
	} else if p.next() == TokAwait && !p.inAsync && !p.inStaticBlock {
		// A class body is strict, but `await` is not a strict-reserved word, so it
		// is a valid class name outside an async (or module / class-static-block)
		// context, all of which reserve await.
		cls.Str = "await"
		p.consume()
	} else if p.next() == TokErr && !p.inStaticBlock {
		// A contextual keyword written with an escape is a plain identifier, so it
		// can name a class (`class aw\u0061it {}` outside a module).
		if name, ok := p.escapedIdentName(); ok && !isEvalOrArgumentsName(name) && !isStrictReservedName(name) {
			cls.Str = name
			p.consume()
		}
	}
	if p.next() == TokIdentifier && p.tlen() == 7 && p.tokStr() == "extends" {
		p.consume()
		// All parts of a class definition are strict-mode code, including the
		// ClassHeritage, so a nested function there is strict too (`class C extends
		// (function(){ with ({}) {} }) {}` is a SyntaxError).
		savedStrict := p.lx.strict
		p.lx.strict = true
		cls.Left = p.parseAssign()
		p.lx.strict = savedStrict
		// ClassHeritage is `extends LeftHandSideExpression`: an unparenthesized
		// arrow (`class extends () => {}`) or a bare assignment is not one.
		if h := cls.Left; h != nil && h.Flags&fnParen == 0 &&
			((h.Kind == NFunc && h.Flags&fnArrow != 0) || h.Kind == NAssign) {
			p.errorf("Invalid class heritage: expected a left-hand-side expression")
		}
	}
	p.expect(TokLBrace)
	sawCtor := false
	for p.next() != TokRBrace && p.tok() != TokEOF {
		if p.tok() == TokSemicolon {
			p.consume()
			continue
		}
		var flags uint32
		if p.tok() == TokStatic && p.la() != TokLParen && p.la() != TokAssign &&
			p.la() != TokSemicolon && p.la() != TokRBrace {
			flags |= fnStatic
			p.consume()
			p.next()
			if p.tok() == TokLBrace {
				// A class static block is strict, has a meaningful new.target
				// (undefined), and reserves `await`; `yield`/`return` are already
				// disallowed (strict reserves yield; the block is not a function body
				// so a top-level return is illegal). `arguments` and an await
				// expression are early errors, checked once the block is parsed.
				savedStrict, savedNT := p.lx.strict, p.newTargetOK
				savedAsync, savedGen := p.inAsync, p.inGenerator
				savedSB, savedFD := p.inStaticBlock, p.funcDepth
				p.lx.strict = true
				p.newTargetOK = true
				p.inAsync = false
				p.inGenerator = false
				p.inStaticBlock = true
				p.funcDepth = 0 // a static block is not a function body: `return` is illegal
				block := p.parseBlock(false)
				p.lx.strict, p.newTargetOK = savedStrict, savedNT
				p.inAsync, p.inGenerator = savedAsync, savedGen
				p.inStaticBlock, p.funcDepth = savedSB, savedFD
				block.Kind = NStaticBlock
				block.Flags = fnStatic | fnClassBody
				if nodeContainsArguments(block) {
					p.errorf("'arguments' is not allowed in a class static block")
				}
				cls.Args = append(cls.Args, block)
				continue
			}
		}

		method := p.mk(NMethod)
		methodSrcOff := uint32(p.toff())

		if p.tok() == TokAsync && p.la() != TokLParen {
			flags |= fnAsync
			p.consume()
			p.next()
		}
		if p.tok() == TokMul {
			flags |= fnGenerator
			p.consume()
			p.next()
		}
		if p.tlen() == 3 && (p.tokStr() == "get" || p.tokStr() == "set") {
			la := p.la()
			// `get`/`set` names an accessor only when a PropertyName follows. `(`, `=`,
			// `;`, `}` make it a plain field/method name; `*` does too (an accessor is
			// never a generator), leaving the `*` to start the next member.
			if la != TokLParen && la != TokAssign && la != TokSemicolon && la != TokRBrace && la != TokMul {
				if p.tokStr()[0] == 'g' {
					flags |= fnGetter
				} else {
					flags |= fnSetter
				}
				p.consume()
				p.next()
			}
		}

		if p.tok() == TokLBracket {
			p.consume()
			// A ComputedPropertyName is [+In]: `class { get ['x' in o]() {} }` is
			// legal even in a for-head, where the enclosing context suppresses `in`.
			outerNoIn := p.noIn
			p.noIn = false
			method.Left = p.parseAssign()
			p.noIn = outerNoIn
			p.expect(TokRBracket)
			flags |= fnComputed
		} else if p.tok() == TokHash {
			if !p.privateNameAdjacent() || !isPrivateIdentLikeTok(p.tok()) {
				p.errorf("private field name expected")
				return p.mk(NEmpty)
			}
			method.Left = p.mkPrivateIdentFromTok()
			p.consume()
		} else if p.tok() == TokString {
			method.Left = p.mkStringFromTok()
			p.consume()
		} else if p.tok() == TokNumber {
			method.Left = mkNum(tod(p.tval()))
			p.consume()
		} else if p.tok() == TokBigInt {
			// A BigInt literal class-element key (`class C { 1n(){} }`) names the
			// member by its BigInt::toString (decimal); propKeyName converts it.
			bi := p.mk(NBigInt)
			bi.Str = p.tokStr()
			method.Left = bi
			p.consume()
		} else {
			method.Left = p.mkIdentFromTok()
			p.consume()
		}
		method.Flags = flags

		if flags&fnStatic == 0 && flags&fnGenerator != 0 && method.Left != nil &&
			method.Left.Kind == NIdent && method.Left.Str == "constructor" {
			p.errorf("Class constructor may not be a generator")
			return cls
		}
		// A non-static, non-computed member named "constructor" may not be an
		// accessor or async method (a constructor can't be a special method — the
		// generator case is rejected above), and a class may declare at most one.
		if flags&(fnStatic|fnComputed) == 0 &&
			method.Left != nil && method.Left.Kind == NIdent &&
			method.Left.Str == "constructor" {
			if flags&(fnGetter|fnSetter|fnAsync) != 0 {
				p.errorf("Class constructor may not be an accessor or async method")
				return cls
			}
			if p.next() == TokLParen {
				if sawCtor {
					p.errorf("A class may only have one constructor")
					return cls
				}
				sawCtor = true
			}
		}

		if p.next() == TokLParen {
			savedStrict := p.lx.strict
			p.lx.strict = true
			p.pendingAsync = flags&fnAsync != 0         // async method body gets an await context
			p.pendingGenerator = flags&fnGenerator != 0 // generator method body gets a yield context
			method.Right = p.parseFunc()
			p.lx.strict = savedStrict
			if !p.validateAccessorParams(method.Right, method.Flags) {
				return cls
			}
			method.Right.Flags |= (flags & (fnAsync | fnGenerator)) | fnMethod | fnClassBody
			method.Right.SrcOff = methodSrcOff
		} else if p.tok() == TokAssign {
			p.consume()
			// A field initializer is evaluated in a function-like context where
			// `new.target` is meaningful (it evaluates to undefined).
			savedNT := p.newTargetOK
			p.newTargetOK = true
			method.Right = p.parseAssign()
			p.newTargetOK = savedNT
			if !p.classFieldASI() {
				return cls
			}
		} else {
			method.Right = p.mk(NUndef)
			if !p.classFieldASI() {
				return cls
			}
		}
		// Class member name early errors (non-computed, non-private names):
		//   - a field may not be named "constructor"; a static field may not be
		//     named "prototype";
		//   - a static method may not be named "prototype".
		if method.Flags&fnComputed == 0 {
			if name, ok := propKeyName(method.Left); ok {
				field := isClassFieldMember(method)
				static := method.Flags&fnStatic != 0
				if name == "#constructor" {
					p.errorf("Class private name #constructor is reserved")
					return cls
				}
				if field && name == "constructor" {
					p.errorf("Classes may not have a field named 'constructor'")
					return cls
				}
				if static && name == "prototype" {
					p.errorf("Classes may not have a static property named 'prototype'")
					return cls
				}
			}
		}
		// A class field initializer may not reference `arguments`.
		if isClassFieldMember(method) && nodeContainsArguments(method.Right) {
			p.errorf("'arguments' is not allowed in a class field initializer")
			return cls
		}
		cls.Args = append(cls.Args, method)
	}
	p.expect(TokRBrace)
	// A class body's private names must be unique, with one exception: a single
	// getter/setter pair sharing a name (both static or both non-static).
	if !p.validatePrivateNames(cls) {
		return cls
	}
	cls.SrcEnd = uint32(p.toff() + p.tlen())
	return cls
}

// validatePrivateNames enforces the "no duplicate PrivateBoundIdentifiers"
// early error over a class body, allowing one get/set accessor pair per name.
func (p *parser) validatePrivateNames(cls *Node) bool {
	type privEntry struct {
		get, set, other int
		getSt, setSt    bool
	}
	priv := map[string]*privEntry{}
	for _, m := range cls.Args {
		if m == nil || m.Kind != NMethod || !isPrivateMemberProp(m.Left) {
			continue
		}
		e := priv[m.Left.Str]
		if e == nil {
			e = &privEntry{}
			priv[m.Left.Str] = e
		}
		static := m.Flags&fnStatic != 0
		switch {
		case m.Flags&fnGetter != 0:
			e.get++
			e.getSt = static
		case m.Flags&fnSetter != 0:
			e.set++
			e.setSt = static
		default:
			e.other++
		}
	}
	for name, e := range priv {
		total := e.get + e.set + e.other
		pair := e.other == 0 && e.get == 1 && e.set == 1 && e.getSt == e.setSt
		if total > 1 && !pair {
			p.errorf("Duplicate private name " + name)
			return false
		}
	}
	return true
}

// ---- var declarations ----

func (p *parser) parseVarDecl(kind VarKind, allowUninitConst bool) *Node {
	v := p.mk(NVar)
	v.VarKind = kind
	for {
		p.next()
		decl := p.mk(NVarDecl)
		usingKind := kind == VarUsing || kind == VarAwaitUsing
		switch p.tok() {
		case TokLBracket:
			// `using`/`await using` bind only BindingIdentifiers — an array or object
			// binding pattern is a SyntaxError, even after a valid identifier binding
			// (`using x = null, [] = null`).
			if usingKind {
				p.errorf("'using' declarations may not have a binding pattern")
				return v
			}
			decl.Left = p.parseArray()
			p.validateArrayPattern(decl.Left)
		case TokLBrace:
			if usingKind {
				p.errorf("'using' declarations may not have a binding pattern")
				return v
			}
			decl.Left = p.parseObject()
			p.validateObjectPattern(decl.Left)
		case TokErr:
			// An escaped contextual keyword is an ordinary BindingIdentifier.
			if name, ok := p.escapedIdentName(); ok {
				decl.Left = p.mkEscapedIdent(name)
				break
			}
			p.unexpected()
			return v
		default:
			if isReservedWordTok(p.tok()) {
				p.errorf("Unexpected reserved word")
				return v
			}
			if p.tokIsEscapedReservedWord() {
				p.errorf("Keyword must not contain escaped characters")
				return v
			}
			// A BindingIdentifier is an identifier: `var 1 = 1` or `var "s" = 1` is a
			// SyntaxError, not a binding whose name happens to read as a literal.
			if p.tok() < TokIdentifier || p.tok() >= TokIdentLikeEnd {
				p.unexpected()
				return v
			}
			decl.Left = p.mkIdentFromTok()
			p.strictCheckBindingIdent(decl.Left.Str)
			p.consume()
		}
		if p.next() == TokAssign {
			p.consume()
			decl.Right = p.parseAssign()
		} else if (kind == VarConst || kind == VarUsing || kind == VarAwaitUsing) && !allowUninitConst {
			p.errorf("Missing initializer in const declaration")
		}
		v.Args = append(v.Args, decl)
		if p.next() == TokComma {
			p.consume()
			continue
		}
		break
	}
	return v
}

// ---- import / export ----

func (p *parser) skipImportStmt() *Node {
	for p.next() != TokSemicolon && p.tok() != TokEOF {
		p.consume()
	}
	if p.tok() == TokSemicolon {
		p.consume()
	}
	return p.mk(NEmpty)
}

const (
	importBindDefault   = 1 << 0
	importBindNamespace = 1 << 1
	// exportNameFromString marks a ModuleExportName written as a StringLiteral
	// rather than an IdentifierName.
	exportNameFromString = 1 << 2
)

// hasLoneSurrogate reports whether a cooked (WTF-8) string contains an unpaired
// surrogate. cookString combines a valid pair into its astral code point, so any
// remaining rune in D800..DFFF is by definition lone.
func hasLoneSurrogate(s string) bool {
	for _, r := range wtf8ToRunes([]byte(s)) {
		if r >= 0xD800 && r <= 0xDFFF {
			return true
		}
	}
	return false
}

func (p *parser) importDeclAddBinding(decl *Node, importName, localName string, hasImport bool, flags uint32) {
	// An imported binding is a BindingIdentifier, and module code is always
	// strict — so `import { eval }` / `import { x as arguments }` are early errors.
	p.strictCheckBindingIdent(localName)
	importDeclAddBinding(decl, importName, localName, hasImport, flags)
}

func importDeclAddBinding(decl *Node, importName, localName string, hasImport bool, flags uint32) {
	spec := &Node{Kind: NImportSpec, Flags: flags}
	if hasImport {
		spec.Left = mkIdent(importName)
	}
	spec.Right = mkIdent(localName)
	decl.Args = append(decl.Args, spec)
}

func (p *parser) parseImportStmt() *Node {
	decl := p.mk(NImportDecl)
	if p.next() == TokString {
		spec := p.mkStringFromTok()
		p.consume()
		decl.Str = p.parseWithClause() // the "type" attribute, if any
		p.semicolon()
		decl.Right = spec
		return decl
	}
	sawClause := false
	if isIdentLikeTok(p.next()) {
		sawClause = true
		local := p.tokIdentStr()
		p.importDeclAddBinding(decl, "default", local, true, importBindDefault)
		p.consume()
		if p.next() == TokComma {
			p.consume()
		} else {
			goto parseFrom
		}
	}
	if p.next() == TokMul {
		sawClause = true
		p.consume()
		p.expect(TokAs)
		p.next()
		if !isIdentLikeTok(p.tok()) {
			return p.skipImportStmt()
		}
		p.importDeclAddBinding(decl, "", p.tokIdentStr(), false, importBindNamespace)
		p.consume()
	} else if p.next() == TokLBrace {
		sawClause = true
		p.consume()
		for p.next() != TokRBrace && p.tok() != TokEOF {
			var importName string
			if p.tok() == TokString {
				importName = cookString(p.tokStr())
				p.consume()
			} else if !(p.tok() >= TokIdentifier && p.tok() < TokIdentLikeEnd) {
				p.consume()
				continue
			} else {
				importName = p.tokIdentStr()
				p.consume()
			}
			localName := importName
			if p.next() == TokAs {
				p.consume()
				p.next()
				if !isIdentLikeTok(p.tok()) {
					return p.skipImportStmt()
				}
				localName = p.tokIdentStr()
				p.consume()
			}
			p.importDeclAddBinding(decl, importName, localName, true, 0)
			if p.next() == TokComma {
				p.consume()
				if p.next() == TokRBrace {
					break
				}
			}
		}
		p.expect(TokRBrace)
	}

parseFrom:
	if !sawClause {
		return p.skipImportStmt()
	}
	p.expect(TokFrom)
	if p.next() != TokString {
		return p.skipImportStmt()
	}
	spec := p.mkStringFromTok()
	p.consume()
	decl.Right = spec
	decl.Str = p.parseWithClause() // the "type" attribute, if any
	p.semicolon()
	return decl
}

func (p *parser) parseExportName() *Node {
	p.next()
	if p.tok() == TokString {
		name := p.mk(NIdent)
		name.Str = cookString(p.tokStr())
		// A ModuleExportName spelled as a string must be well-formed UTF-16: a
		// lone surrogate has no valid name to bind or resolve.
		if hasLoneSurrogate(name.Str) {
			p.errorf("Module export name must not contain a lone surrogate")
		}
		name.Flags |= exportNameFromString
		p.consume()
		return name
	}
	if !(p.tok() >= TokIdentifier && p.tok() < TokIdentLikeEnd) {
		p.unexpected()
		return nil
	}
	name := p.mkIdentFromTok()
	p.consume()
	return name
}

// exportDefaultDeclEnd terminates `export default <declaration>`. A
// HoistableDeclaration or ClassDeclaration needs no semicolon, and whatever
// follows is simply the next statement — so `export default function(){} if (x)
// {}` is fine, while `export default function(){}()` fails when `()` is parsed
// as one.
func (p *parser) exportDefaultDeclEnd() {
	if p.next() == TokSemicolon {
		p.consume()
	}
}

func (p *parser) parseExportStmt() *Node {
	decl := p.mk(NExport)
	p.next()

	if p.tok() == TokDefault {
		p.consume()
		decl.Flags |= exDefault
		if p.next() == TokAsync && p.la() == TokFunc && !p.lookaheadCrossesLineTerminator() {
			asyncOff := uint32(p.toff())
			p.consume()
			p.next()
			p.consume()
			p.pendingAsync = true
			decl.Left = p.parseFunc()
			decl.Left.Flags |= fnAsync
			decl.Left.SrcOff = asyncOff
			p.exportDefaultDeclEnd()
			return decl
		}
		if p.tok() == TokFunc {
			p.consume()
			decl.Left = p.parseFunc()
			p.exportDefaultDeclEnd()
			return decl
		}
		if p.tok() == TokClass {
			classOff := uint32(p.toff())
			p.consume()
			decl.Left = p.parseClass()
			decl.Left.SrcOff = classOff
			p.exportDefaultDeclEnd()
			return decl
		}
		decl.Left = p.parseAssign()
		p.semicolon()
		return decl
	}

	if p.tok() == TokAsync && p.la() == TokFunc && !p.lookaheadCrossesLineTerminator() {
		decl.Flags |= exDecl
		asyncOff := uint32(p.toff())
		p.consume()
		p.next()
		p.consume()
		p.pendingAsync = true
		decl.Left = p.parseFunc()
		decl.Left.Flags |= fnAsync
		decl.Left.SrcOff = asyncOff
		if decl.Left.Str == "" {
			p.errorf("exported function declarations require a name")
		}
		return decl
	}
	if p.tok() == TokFunc {
		decl.Flags |= exDecl
		p.consume()
		decl.Left = p.parseFunc()
		if decl.Left.Str == "" {
			p.errorf("exported function declarations require a name")
		}
		return decl
	}
	if p.tok() == TokClass {
		decl.Flags |= exDecl
		classOff := uint32(p.toff())
		p.consume()
		decl.Left = p.parseClass()
		decl.Left.SrcOff = classOff
		if decl.Left.Str == "" {
			p.errorf("exported class declarations require a name")
		}
		return decl
	}
	if p.tok() == TokVar || p.tok() == TokLet || p.tok() == TokConst {
		decl.Flags |= exDecl
		kind := VarVar
		if p.tok() == TokLet {
			kind = VarLet
		} else if p.tok() == TokConst {
			kind = VarConst
		}
		p.consume()
		decl.Left = p.parseVarDecl(kind, false)
		if p.next() == TokSemicolon {
			p.consume()
		}
		return decl
	}
	if p.tok() == TokLBrace {
		decl.Flags |= exNamed
		p.consume()
		for p.next() != TokRBrace && p.tok() != TokEOF {
			localName := p.parseExportName()
			if localName == nil {
				return decl
			}
			exportName := localName
			if p.next() == TokAs {
				p.consume()
				exportName = p.parseExportName()
				if exportName == nil {
					return decl
				}
			}
			spec := p.mk(NImportSpec)
			spec.Left = localName
			spec.Right = exportName
			decl.Args = append(decl.Args, spec)
			if p.next() == TokComma {
				p.consume()
				if p.next() == TokRBrace {
					break
				}
			} else {
				break
			}
		}
		p.expect(TokRBrace)
		if p.next() == TokFrom {
			p.consume()
			if p.next() != TokString {
				p.unexpected()
				return decl
			}
			decl.Right = p.mkStringFromTok()
			decl.Flags |= exFrom
			p.consume()
			decl.Str = p.parseWithClause()
		} else {
			// Without `from` the local name denotes a binding in this module, so it
			// must be an IdentifierName; a string is only a name to look up in the
			// module being re-exported from.
			for _, spec := range decl.Args {
				if spec != nil && spec.Left != nil && spec.Left.Flags&exportNameFromString != 0 {
					p.errorf("A string may only name an export when re-exporting with `from`")
					break
				}
			}
		}
		p.semicolon()
		return decl
	}
	if p.tok() == TokMul {
		decl.Flags |= exStar
		p.consume()
		if p.next() == TokAs {
			decl.Flags |= exNamespace
			p.consume()
			name := p.parseExportName()
			if name == nil {
				return decl
			}
			spec := p.mk(NImportSpec)
			spec.Right = name
			decl.Args = append(decl.Args, spec)
		}
		p.expect(TokFrom)
		if p.next() != TokString {
			p.unexpected()
			return decl
		}
		decl.Right = p.mkStringFromTok()
		decl.Flags |= exFrom
		p.consume()
		decl.Str = p.parseWithClause()
		p.semicolon()
		return decl
	}
	p.unexpected()
	return decl
}

// ---- statements ----

// usingBeginsDeclaration reports whether a leading `using` token begins a
// UsingDeclaration: it must be directly followed (no line terminator) by a
// BindingIdentifier. `using [`, `using {`, `using` + newline, `using =`, and
// `using.x` make `using` a plain identifier (an ExpressionStatement), so
// `using [x] = y` is a member assignment and `using [] = null` a subscript error.
func (p *parser) usingBeginsDeclaration() bool {
	if p.lookaheadCrossesLineTerminator() {
		return false
	}
	la := p.la()
	return la == TokIdentifier || isContextualIdentTok(la) ||
		la == TokYield || la == TokAwait || la == TokLet || la == TokStatic
}

// parseSubStmt parses the body of a loop or if branch: a single-Statement
// context in which a Declaration is not a valid production. The one-shot flag is
// consumed at the top of parseStmt.
func (p *parser) parseSubStmt() *Node {
	p.singleStmt = true
	return p.parseStmt()
}

func (p *parser) parseStmt() *Node {
	singleStmt := p.singleStmt
	p.singleStmt = false
	p.next()

	if p.tok() == TokUsing && p.usingBeginsDeclaration() {
		if !p.usingAllowed {
			p.errorf("'using' declarations are not allowed in this position")
			return p.mk(NEmpty)
		}
		p.consume()
		n := p.parseVarDecl(VarUsing, false)
		p.semicolon()
		return n
	}
	if p.tok() == TokAwait {
		saved := p.lx.save()
		p.consume()
		p.next()
		if p.tok() == TokUsing && p.usingBeginsDeclaration() {
			// `await using` is a declaration only when `using` is immediately (no
			// LineTerminator) followed by a BindingIdentifier — not `[` (so
			// `await using[0]` is indexing) and not across a newline (so
			// `await using\nlet = 1` is `await using;` then `let = 1`).
			if !p.usingAllowed {
				p.errorf("'await using' declarations are not allowed in this position")
				return p.mk(NEmpty)
			}
			p.consume()
			n := p.parseVarDecl(VarAwaitUsing, false)
			p.semicolon()
			return n
		}
		p.lx.restore(saved)
	}

	switch p.tok() {
	case TokSemicolon:
		p.consume()
		return p.mk(NEmpty)
	case TokLBrace:
		return p.parseBlock(false)
	case TokVar:
		p.consume()
		n := p.parseVarDecl(VarVar, false)
		p.semicolon()
		return n
	case TokLet:
		// In a single-statement context (a loop or if body) a Declaration is not a
		// valid production, so a leading `let` is only the start of a
		// LexicalDeclaration when it unambiguously begins one — `let [` (a
		// restricted form, even across a line terminator) or, on the SAME line,
		// `let {` / `let <BindingIdentifier>`. Those route to the declaration path
		// and become an early error (isForbiddenLoopBody / isForbiddenIfBody). Any
		// other follow token, or an intervening line terminator, makes `let` an
		// identifier ExpressionStatement (sloppy mode): `for (…) let\nx = 1` is
		// `let;` then `x = 1`, whereas `for (…) let x = 1` is a SyntaxError.
		if singleStmt {
			la := p.la()
			declStart := la == TokLBracket
			if !declStart && !p.lookaheadCrossesLineTerminator() {
				declStart = la == TokLBrace || la == TokIdentifier ||
					isContextualIdentTok(la) || la == TokLet ||
					la == TokYield || la == TokAwait || la == TokStatic
			}
			if !declStart {
				break // `let` is an identifier: fall to the expression statement
			}
		} else if !p.lx.strict {
			// At the statement level in sloppy mode, `let` begins a LexicalDeclaration
			// only when followed by a BindingIdentifier, `[`, or `{`; otherwise it is
			// an identifier ExpressionStatement (`let = 5`, `let;`, `let.foo`, `let++`).
			// (A line terminator is irrelevant here — `let` is not a restricted
			// production — so `let\nx = 1` is still `let x = 1`. In strict mode `let`
			// is reserved, so a non-declaration follow routes to parseVarDecl's error.)
			la := p.la()
			if la != TokLBracket && la != TokLBrace && la != TokIdentifier &&
				!isContextualIdentTok(la) && la != TokLet && la != TokYield &&
				la != TokAwait && la != TokStatic {
				break // `let` is an identifier: fall to the expression statement
			}
		}
		p.consume()
		n := p.parseVarDecl(VarLet, false)
		p.semicolon()
		return n
	case TokConst:
		p.consume()
		n := p.parseVarDecl(VarConst, false)
		p.semicolon()
		return n
	case TokIf:
		return p.parseIf()
	case TokWhile:
		return p.parseWhile()
	case TokDo:
		return p.parseDoWhile()
	case TokFor:
		return p.parseFor()
	case TokReturn:
		return p.parseReturn()
	case TokThrow:
		p.consume()
		n := p.mk(NThrow)
		// `throw [no LineTerminator here] Expression ;` — a line terminator (or an
		// immediate `;`) after `throw` leaves it without an operand: an early
		// SyntaxError (ASI never supplies the missing Expression).
		if p.next() == TokSemicolon || p.hadNewline() {
			p.errorf("Illegal newline after throw")
			return n
		}
		n.Right = p.parseExpr()
		p.semicolon()
		return n
	case TokBreak:
		return p.parseBreakContinue(NBreak)
	case TokContinue:
		return p.parseBreakContinue(NContinue)
	case TokTry:
		return p.parseTry()
	case TokSwitch:
		return p.parseSwitch()
	case TokDebugger:
		p.consume()
		p.semicolon()
		return p.mk(NDebugger)
	case TokWith:
		return p.parseWith()
	case TokFunc:
		p.consume()
		fn := p.parseFunc()
		// A FunctionDeclaration requires a name (only a FunctionExpression may be
		// anonymous), so `function () {}` at statement position is a SyntaxError.
		if fn != nil && fn.Kind == NFunc && fn.Str == "" {
			p.errorf("Function statements require a function name")
		}
		return fn
	case TokClass:
		classOff := uint32(p.toff())
		p.consume()
		cls := p.parseClass()
		cls.SrcOff = classOff
		return cls
	case TokAsync:
		la := p.la()
		asyncOff := uint32(p.toff())
		if la == TokFunc && !p.lookaheadCrossesLineTerminator() {
			p.consume()
			p.next()
			p.consume()
			p.pendingAsync = true
			fn := p.parseFunc()
			fn.Flags |= fnAsync
			fn.SrcOff = asyncOff
			// An async FunctionDeclaration also requires a name.
			if fn.Kind == NFunc && fn.Str == "" {
				p.errorf("Function statements require a function name")
			}
			return fn
		}
		return p.parseExprStmt()
	case TokImport:
		la := p.la()
		if la == TokLParen || la == TokDot {
			return p.parseExprStmt()
		}
		p.consume()
		return p.parseImportStmt()
	case TokExport:
		p.consume()
		return p.parseExportStmt()
	}
	return p.parseExprStmt()
}

func (p *parser) parseIf() *Node {
	p.consume()
	n := p.mk(NIf)
	p.expect(TokLParen)
	n.Cond = p.parseExpr()
	p.expect(TokRParen)
	n.Left = p.parseSubStmt()
	if isForbiddenIfBody(n.Left, p.lx.strict) {
		p.errorf("Declaration cannot appear in a single-statement context")
		return n
	}
	if p.next() == TokElse {
		p.consume()
		n.Right = p.parseSubStmt()
		if isForbiddenIfBody(n.Right, p.lx.strict) {
			p.errorf("Declaration cannot appear in a single-statement context")
			return n
		}
	}
	return n
}

func (p *parser) parseWhile() *Node {
	p.consume()
	n := p.mk(NWhile)
	p.expect(TokLParen)
	n.Cond = p.parseExpr()
	p.expect(TokRParen)
	n.Body = p.parseSubStmt()
	if isForbiddenLoopBody(n.Body) {
		p.errorf("Lexical declaration cannot appear in single-statement context")
	}
	return n
}

func (p *parser) parseDoWhile() *Node {
	p.consume()
	n := p.mk(NDoWhile)
	n.Body = p.parseSubStmt()
	if isForbiddenLoopBody(n.Body) {
		p.errorf("Lexical declaration cannot appear in single-statement context")
	}
	p.expect(TokWhile)
	p.expect(TokLParen)
	n.Cond = p.parseExpr()
	p.expect(TokRParen)
	if p.next() == TokSemicolon {
		p.consume()
	}
	return n
}

func (p *parser) parseFor() *Node {
	p.consume()
	isForAwait := false
	if p.next() == TokAwait {
		p.consume()
		isForAwait = true
	}
	p.expect(TokLParen)
	var initNode *Node

	p.next()
	if p.tok() == TokAwait {
		saved := p.lx.save()
		p.consume()
		p.next()
		if p.tok() == TokUsing {
			p.consume()
			p.noIn = true
			initNode = p.parseVarDecl(VarAwaitUsing, true)
			p.noIn = false
		} else {
			p.lx.restore(saved)
		}
	}

	if initNode == nil && p.tok() == TokUsing {
		p.consume()
		p.noIn = true
		initNode = p.parseVarDecl(VarUsing, true)
		p.noIn = false
	} else if initNode == nil && (p.tok() == TokVar || p.tok() == TokConst ||
		(p.tok() == TokLet && p.la() != TokIn)) {
		// `for (let in …)` treats `let` as an identifier reference (sloppy mode), not
		// a lexical declaration — the lookahead after `let` is `in`.
		kind := VarVar
		if p.tok() == TokLet {
			kind = VarLet
		} else if p.tok() == TokConst {
			kind = VarConst
		}
		p.consume()
		p.noIn = true
		initNode = p.parseVarDecl(kind, true)
		p.noIn = false
	} else if initNode == nil && p.tok() != TokSemicolon {
		p.noIn = true
		initNode = p.parseExpr()
		p.noIn = false
	}

	la := p.next()
	if la == TokIn {
		p.consume()
		n := p.mk(NForIn)
		n.Left = initNode
		// A `using` / `await using` declaration is valid only as a for-of head, not
		// for-in (`for (using x in obj)` is a SyntaxError).
		if initNode != nil && initNode.Kind == NVar &&
			(initNode.VarKind == VarUsing || initNode.VarKind == VarAwaitUsing) {
			p.errorf("'using' declaration is not allowed in a for-in loop")
		}
		p.validatePatternTarget(initNode)
		n.Right = p.parseExpr()
		p.expect(TokRParen)
		n.Body = p.parseSubStmt()
		if initNode != nil && initNode.Kind == NVar && varDeclHasInitializer(initNode) {
			// A for-in head declaration may not carry an initializer, except the
			// sloppy-mode legacy form `var <Identifier> = init` (Annex B.3.6).
			simpleVar := initNode.VarKind == VarVar && len(initNode.Args) == 1 &&
				initNode.Args[0] != nil && initNode.Args[0].Left != nil &&
				initNode.Args[0].Left.Kind == NIdent
			if p.lx.strict || !simpleVar {
				p.errorf("for-in loop variable declaration may not have an initializer")
			}
		}
		if isForbiddenLoopBody(n.Body) {
			p.errorf("Lexical declaration cannot appear in single-statement context")
		}
		return n
	}
	if la == TokOf || (la == TokIdentifier && p.tlen() == 2 && p.tokStr() == "of") {
		p.consume()
		kind := NForOf
		if isForAwait {
			kind = NForAwaitOf
		}
		n := p.mk(kind)
		n.Left = initNode
		// `for ( [lookahead ∉ { async of }] LeftHandSideExpression of ...)`: a
		// bare (unparenthesized) `async` immediately before `of` is a Syntax Error
		// in a plain for-of, disambiguating it from an async arrow head.
		if !isForAwait && initNode != nil && initNode.Kind == NIdent &&
			initNode.Str == "async" && initNode.Flags&fnParen == 0 {
			p.errorf("'async' may not be the left-hand side of a for-of statement")
		}
		p.validatePatternTarget(initNode)
		// A for-of head declaration may never carry an initializer.
		if initNode != nil && initNode.Kind == NVar && varDeclHasInitializer(initNode) {
			p.errorf("a for-of loop variable declaration may not have an initializer")
		}
		n.Right = p.parseAssign()
		p.expect(TokRParen)
		n.Body = p.parseSubStmt()
		if isForbiddenLoopBody(n.Body) {
			p.errorf("Lexical declaration cannot appear in single-statement context")
		}
		return n
	}

	if initNode != nil && initNode.Kind == NVar &&
		(initNode.VarKind == VarConst || initNode.VarKind == VarUsing || initNode.VarKind == VarAwaitUsing) {
		for _, decl := range initNode.Args {
			if decl != nil && decl.Kind == NVarDecl && decl.Right == nil {
				p.errorf("Missing initializer in const declaration")
				return p.mk(NEmpty)
			}
		}
	}

	n := p.mk(NFor)
	n.Init = initNode
	p.expect(TokSemicolon)
	if p.next() != TokSemicolon {
		n.Cond = p.parseExpr()
	}
	p.expect(TokSemicolon)
	if p.next() != TokRParen {
		n.Update = p.parseExpr()
	}
	p.expect(TokRParen)
	n.Body = p.parseSubStmt()
	if isForbiddenLoopBody(n.Body) {
		p.errorf("Lexical declaration cannot appear in single-statement context")
	}
	return n
}

func (p *parser) parseReturn() *Node {
	if p.funcDepth == 0 {
		p.errorf("Illegal return statement") // return outside a function
	}
	p.consume()
	n := p.mk(NReturn)
	if p.next() != TokSemicolon && p.tok() != TokRBrace && p.tok() != TokEOF && !p.hadNewline() {
		n.Right = p.parseExpr()
	}
	if p.next() == TokSemicolon {
		p.consume()
	}
	return n
}

func (p *parser) parseBreakContinue(kind NodeKind) *Node {
	p.consume()
	n := p.mk(kind)
	if (p.next() == TokIdentifier || isContextualIdentTok(p.tok())) && !p.hadNewline() {
		n.Str = p.tokIdentStr()
		p.consume()
	}
	if p.next() == TokSemicolon {
		p.consume()
	}
	return n
}

func (p *parser) parseTry() *Node {
	p.consume()
	n := p.mk(NTry)
	n.Body = p.parseBlock(false)
	if p.next() == TokCatch {
		p.consume()
		if p.next() == TokLParen {
			p.consume()
			n.CatchParam = p.parseBindingPattern()
			if n.CatchParam.Kind == NIdent {
				p.strictCheckBindingIdent(n.CatchParam.Str)
			}
			p.expect(TokRParen)
		}
		n.CatchBody = p.parseBlock(false)
	}
	if p.next() == TokFinally {
		p.consume()
		n.FinallyBody = p.parseBlock(false)
	}
	return n
}

func (p *parser) parseSwitch() *Node {
	p.consume()
	n := p.mk(NSwitch)
	p.expect(TokLParen)
	n.Cond = p.parseExpr()
	p.expect(TokRParen)
	if p.next() != TokLBrace { // the switch body must be a CaseBlock
		p.errorf("Missing '{' before switch body")
		return p.mk(NEmpty)
	}
	p.consume()
	// A `using` declaration directly within a CaseClause/DefaultClause StatementList
	// is a Syntax Error (it must be wrapped in a Block); a nested block restores the
	// permission via parseBlock.
	savedUsing := p.usingAllowed
	p.usingAllowed = false
	defer func() { p.usingAllowed = savedUsing }()
	sawDefault := false
	for p.next() != TokRBrace && p.tok() != TokEOF {
		c := p.mk(NCase)
		if p.tok() == TokCase {
			p.consume()
			c.Left = p.parseExpr()
		} else if p.tok() == TokDefault {
			if sawDefault {
				p.errorf("More than one default clause in switch statement")
				return p.mk(NEmpty)
			}
			sawDefault = true
			p.consume()
		} else {
			// Every statement in a switch must belong to a case/default clause.
			p.errorf("Unexpected token in switch: a clause must begin with 'case' or 'default'")
			return p.mk(NEmpty)
		}
		p.expect(TokColon)
		for p.next() != TokCase && p.tok() != TokDefault && p.tok() != TokRBrace && p.tok() != TokEOF {
			c.Args = append(c.Args, p.parseStmt())
			if p.hasErr() {
				break
			}
		}
		n.Args = append(n.Args, c)
		if p.hasErr() {
			break
		}
	}
	p.expect(TokRBrace)
	return n
}

func (p *parser) parseWith() *Node {
	if p.lx.strict {
		p.errorf("with statement not allowed in strict mode")
		return p.mk(NEmpty)
	}
	p.consume()
	n := p.mk(NWith)
	p.expect(TokLParen)
	n.Left = p.parseExpr()
	p.expect(TokRParen)
	n.Body = p.parseSubStmt()
	// A `with` body is a single Statement: a Declaration (let/const/class or any
	// FunctionDeclaration, including a labelled one) is not allowed there.
	if isForbiddenLoopBody(n.Body) {
		p.errorf("Declaration cannot appear in a single-statement context")
	}
	return n
}

func (p *parser) parseExprStmt() *Node {
	// `yield`/`await` are valid label identifiers where they are not reserved (a
	// generator/strict context for yield, an async context for await).
	escName, escIdent := "", false
	if p.tok() == TokErr && p.la() == TokColon {
		// An escaped contextual keyword is a plain identifier, so it can label a
		// statement (`yi\u0065ld: …` in sloppy code).
		escName, escIdent = p.escapedIdentName()
	}
	labelTok := escIdent || p.tok() == TokIdentifier || isContextualIdentTok(p.tok()) ||
		(p.tok() == TokYield && !p.inGenerator && !p.lx.strict) ||
		(p.tok() == TokAwait && !p.inAsync && !p.inStaticBlock)
	if labelTok {
		if p.la() == TokColon {
			label := p.mk(NLabel)
			label.Str = p.tokIdentStr()
			if escIdent {
				label.Str = escName
			}
			p.consume()
			p.next()
			p.consume()
			// A LabelledItem is a single-Statement context, so a leading `let`
			// there is an identifier (like a loop/if body): `l: let\nx` is `let;`.
			label.Body = p.parseSubStmt()
			// A LabelledItem is a Statement or (Annex B, sloppy only) a plain
			// FunctionDeclaration — never a lexical/class/async/generator declaration.
			// A nested labelled statement (including a labelled function) is a
			// Statement, so isLabelledFunction is deliberately not consulted here.
			if isDeclNotStatement(label.Body, p.lx.strict) {
				p.errorf("Declaration cannot appear in a single-statement context")
				return label
			}
			return label
		}
	}
	expr := p.parseExpr()
	p.semicolon()
	return expr
}

// semicolon consumes a statement-terminating semicolon, applying automatic
// semicolon insertion: an explicit `;` is consumed; otherwise a `}` / EOF, or a
// line terminator before the next token, ends the statement without one. Any
// other token (a second token on the same line, e.g. `a b`) is a SyntaxError.
func (p *parser) semicolon() {
	if p.next() == TokSemicolon {
		p.consume()
		return
	}
	if p.tok() == TokRBrace || p.tok() == TokEOF || p.hadNewline() {
		return
	}
	p.errorf("Unexpected token %q; expected a semicolon or line terminator", p.tokStr())
}

// parseWithClause parses the import-attributes clause that may follow a module
// specifier — `with { type: 'json', 'other-key': 'v', }` — enforcing its grammar
// and its one early error, a duplicate key. It returns the value of the "type"
// attribute, the only one the spec gives meaning to, or "". A line terminator
// before `with` is allowed, and in a Module `with` can never start a statement
// (it is strict code), so the keyword here is unambiguously this clause.
func (p *parser) parseWithClause() string {
	typ := ""
	if p.next() != TokWith {
		return typ
	}
	p.consume()
	if p.next() != TokLBrace {
		p.unexpected()
		return typ
	}
	p.consume()
	seen := map[string]bool{}
	for p.next() != TokRBrace && p.tok() != TokEOF {
		var key string
		switch {
		case p.tok() == TokString:
			key = cookString(p.tokStr())
		case p.tok() >= TokIdentifier && p.tok() < TokIdentLikeEnd:
			// Any IdentifierName, reserved words included (`with {if: ''}`).
			key = p.tokIdentStr()
		default:
			p.unexpected()
			return typ
		}
		if seen[key] {
			p.errorf("Duplicate import attribute key '%s'", key)
			return typ
		}
		seen[key] = true
		p.consume()
		if p.next() != TokColon {
			p.unexpected()
			return typ
		}
		p.consume()
		if p.next() != TokString { // the value is always a StringLiteral
			p.unexpected()
			return typ
		}
		if key == "type" {
			typ = cookString(p.tokStr())
		}
		p.consume()
		if p.next() != TokComma {
			break
		}
		p.consume() // a trailing comma before `}` is allowed
	}
	p.expect(TokRBrace)
	return typ
}
