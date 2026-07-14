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
}

// Parse tokenizes and parses src into an AST (N_PROGRAM root node).
func Parse(filename, src string) (*Node, error) {
	return parseMode(filename, src, false)
}

func parseMode(filename, src string, strict bool) (*Node, error) {
	p := &parser{lx: newLexer(src, strict), filename: filename}
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
	}
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
	// `yield`/`await` are reserved as binding identifiers inside a generator /
	// async body (regardless of strict mode).
	if p.inGenerator && s == "yield" {
		p.errorf("'yield' cannot be used as a binding identifier in a generator")
		return
	}
	if p.inAsync && s == "await" {
		p.errorf("'await' cannot be used as a binding identifier in an async function")
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

func (p *parser) mkIdentFromTok() *Node {
	n := mkIdent(p.tokIdentStr())
	n.SrcOff = uint32(p.toff())
	n.SrcEnd = uint32(p.toff() + p.tlen())
	return n
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
	n.Str = cookString(p.tokStr())
	return n
}

func nodeSrcEnd(p *parser, node *Node) uint32 {
	if node != nil && node.SrcEnd > node.SrcOff {
		return node.SrcEnd
	}
	return uint32(p.toff() + p.tlen())
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
		if stmt == nil || stmt.Kind == NEmpty {
			continue
		}
		if !canBeExpressionStatement(stmt) {
			inDirective = false
			continue
		}
		if isUseStrict(stmt) {
			p.lx.strict = true
			continue
		}
		inDirective = false
	}
	p.lx.strict = savedStrict
}

func (p *parser) parseBlock(directiveCtx bool) *Node {
	p.expect(TokLBrace)
	block := p.mk(NBlock)
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
		return p.parseObject()
	}
	if isIdentLikeTok(p.tok()) || p.tok() == TokUndef {
		// `undefined` is not a reserved word, so it is a valid BindingIdentifier
		// (it shadows the global `undefined` inside the binding's scope). The lexer
		// tokenizes it as TokUndef; recover the name from the raw source.
		id := p.mkIdentFromTok()
		p.strictCheckBindingIdent(id.Str)
		p.consume()
		return id
	}
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

// isPrivateMemberProp reports whether an NMember's property node names a private
// identifier (goant stores `.#x` as an NIdent whose text keeps the leading '#').
func isPrivateMemberProp(prop *Node) bool {
	return prop != nil && prop.Kind == NIdent && len(prop.Str) > 0 && prop.Str[0] == '#'
}

func (p *parser) parseDotPropertyName() *Node {
	if !isPrivateIdentLikeTok(p.tok()) {
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
			p.consume()
			p.next()
			if !isPrivateIdentLikeTok(p.tok()) {
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

// parseArrowBody parses a concise-body arrow tail. An async arrow establishes
// an await context for its body; a plain arrow inherits the surrounding one
// (an arrow has no Await binding of its own — ArrowFunction[?Await]).
func (p *parser) parseArrowBody(isAsync bool) *Node {
	if isAsync {
		saved := p.inAsync
		p.inAsync = true
		defer func() { p.inAsync = saved }()
	}
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
	case TokIdentifier, TokAs, TokFrom, TokOf, TokUsing, TokWindow:
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
		return n
	case TokVoid:
		p.consume()
		n := p.mk(NVoid)
		n.Right = p.parseUnary()
		return n
	case TokDelete:
		p.consume()
		n := p.mk(NDelete)
		n.Right = p.parseUnary()
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
		p.consume()
		n := p.mk(NYield)
		if p.next() == TokMul {
			p.consume()
			n.Flags = 1
		}
		if t := p.tok(); t != TokSemicolon && t != TokRBrace && t != TokRParen &&
			t != TokRBracket && t != TokEOF && t != TokComma {
			n.Right = p.parseAssign()
		}
		return n
	case TokAwait:
		// Outside an async function, `await` is a plain identifier.
		if !p.inAsync {
			n := p.mkIdentFromTok()
			p.consume()
			return n
		}
		p.consume()
		n := p.mk(NAwait)
		n.Right = p.parseUnary()
		return n
	case TokSuper:
		p.consume()
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
		s.Aux = in[base+segStart : base+i]
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
		if la == TokDot || la == TokLBracket {
			callee = p.parseMemberSuffix(callee, la)
		} else {
			break
		}
		if callee != nil && callee.Kind == NEmpty {
			return callee
		}
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
	p.consume()
	if p.next() != TokLParen {
		return mkIdent("import")
	}
	p.consume()
	n := p.mk(NImport)
	n.Right = p.parseExpr()
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
		if c == '\\' && pos+1 < clen {
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
	p.consume()
	p.next()
	if !isPrivateIdentLikeTok(p.tok()) {
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
		case NArray:
			p.validateArrayPattern(e)
		case NSpread, NRest:
			// A rest element is a BindingRestElement / AssignmentRestElement: it may
			// not carry a default initializer (`[...x = 1]` is an early SyntaxError).
			if e.Right != nil && (e.Right.Kind == NAssign || e.Right.Kind == NAssignPat) {
				p.errorf("`...` rest element may not have a default initializer")
				return
			}
			p.validateArrayPattern(e.Right)
		case NAssign, NAssignPat:
			p.validateArrayPattern(e.Left)
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
	case p.tok() == TokString:
		prop.Left = p.mkStringFromTok()
		p.consume()
	default:
		prop.Left = p.mkIdentFromTok()
		p.consume()
	}
}

func (p *parser) parseObject() *Node {
	p.consume()
	n := p.mk(NObject)
	protoSet := false
	for p.next() != TokRBrace && p.tok() != TokEOF {
		prop := p.mk(NProperty)

		if p.tok() == TokRest {
			p.consume()
			spread := p.mk(NSpread)
			spread.Right = p.parseAssign()
			n.Args = append(n.Args, spread)
			if p.next() == TokComma {
				p.consume()
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
			if la != TokColon && la != TokLParen && la != TokComma && la != TokRBrace {
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
				if protoSet {
					p.errorf("Duplicate __proto__ fields are not allowed in object literals")
					return n
				}
				protoSet = true
			}
			prop.Right = p.parseAssign()
		} else if p.tok() == TokLParen {
			prop.Right = p.parseFunc()
			prop.Right.Flags |= fnMethod
			prop.Right.SrcOff = prop.SrcOff
		} else {
			prop.Right = mkIdent(prop.Left.Str)
			if p.next() == TokAssign {
				p.consume()
				def := p.mk(NAssign)
				def.Op = TokAssign
				def.Left = prop.Right
				def.Right = p.parseAssign()
				prop.Right = def
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
				p.consume()
				p.next()
				if !isPrivateIdentLikeTok(p.tok()) {
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
		if !isValidAssignTarget(n, true) { // ++/-- need a simple assignment target
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
		if p.next() == TokExp {
			p.errorf("Unary operator used immediately before exponentiation expression. Parenthesis must be used to disambiguate operator precedence")
			return p.mk(NEmpty)
		}
		return n
	}
	if la == TokPostInc || la == TokPostDec {
		p.consume()
		target := p.parseUnary()
		if !isValidAssignTarget(target, true) { // ++/-- need a simple assignment target
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
		n.Left = p.parseAssign()
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

func (p *parser) parseAssign() *Node {
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
		// an early SyntaxError.
		if !isValidAssignTarget(left, op != TokAssign) {
			p.errorf("Invalid left-hand side in assignment")
			return p.mk(NEmpty)
		}
		if op == TokAssign && left.Kind == NArray {
			// Destructuring assignment: the array pattern's rest must be last.
			p.validateArrayPattern(left)
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

func (p *parser) parseFunc() *Node {
	fn := p.mk(NFunc)
	// This function's async-ness (set by the caller via pendingAsync). Its body
	// establishes the await context; its parameter list does not (await in an
	// async function's params is a SyntaxError).
	isAsync := p.pendingAsync
	p.pendingAsync = false
	isGenerator := p.pendingGenerator // a method whose `*` the caller already ate
	p.pendingGenerator = false
	savedAsync := p.inAsync
	savedGen := p.inGenerator
	p.inAsync = false     // parameters
	p.inGenerator = false // parameters
	defer func() { p.inAsync = savedAsync; p.inGenerator = savedGen }()
	if p.next() == TokMul {
		p.consume()
		isGenerator = true
	}
	if isGenerator {
		fn.Flags |= fnGenerator
	}
	if isIdentLikeTok(p.next()) {
		fn.Str = p.tokIdentStr()
		// The function name is a BindingIdentifier evaluated in the ENCLOSING
		// yield/await context (inGenerator/inAsync were already reset for this
		// function's own body), so check it against the saved outer context.
		if savedGen && fn.Str == "yield" {
			p.errorf("'yield' cannot be used as a binding identifier in a generator")
		} else if savedAsync && fn.Str == "await" {
			p.errorf("'await' cannot be used as a binding identifier in an async function")
		}
		p.strictCheckBindingIdent(fn.Str)
		p.consume()
	}
	p.expect(TokLParen)
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
	// In strict code, parameter names must be unique and not reserved/eval/
	// arguments (checked here because the enclosing strictness is already known
	// for a preceding directive).
	if p.lx.strict {
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
	if fn.Flags&fnArrow == 0 && referencesArguments(fn.Body) {
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
		p.consume()
	}
	if p.next() == TokIdentifier && p.tlen() == 7 && p.tokStr() == "extends" {
		p.consume()
		cls.Left = p.parseAssign()
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
				savedStrict := p.lx.strict
				p.lx.strict = true
				block := p.parseBlock(false)
				p.lx.strict = savedStrict
				block.Kind = NStaticBlock
				block.Flags = fnStatic | fnClassBody
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
			if la != TokLParen && la != TokAssign && la != TokSemicolon && la != TokRBrace {
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
			method.Left = p.parseAssign()
			p.expect(TokRBracket)
			flags |= fnComputed
		} else if p.tok() == TokHash {
			p.consume()
			p.next()
			if !isPrivateIdentLikeTok(p.tok()) {
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
			method.Right = p.parseAssign()
			if p.next() == TokSemicolon {
				p.consume()
			}
		} else {
			method.Right = p.mk(NUndef)
			if p.tok() == TokSemicolon {
				p.consume()
			}
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
		switch p.tok() {
		case TokLBracket:
			decl.Left = p.parseArray()
			p.validateArrayPattern(decl.Left)
		case TokLBrace:
			decl.Left = p.parseObject()
		case TokErr:
			p.unexpected()
			return v
		default:
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
)

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
		if p.next() == TokSemicolon {
			p.consume()
		}
		decl.Right = spec
		return decl
	}
	sawClause := false
	if isIdentLikeTok(p.next()) {
		sawClause = true
		local := p.tokIdentStr()
		importDeclAddBinding(decl, "default", local, true, importBindDefault)
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
		importDeclAddBinding(decl, "", p.tokIdentStr(), false, importBindNamespace)
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
			importDeclAddBinding(decl, importName, localName, true, 0)
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
	if p.next() == TokSemicolon {
		p.consume()
	}
	return decl
}

func (p *parser) parseExportName() *Node {
	p.next()
	if p.tok() == TokString {
		name := p.mk(NIdent)
		name.Str = cookString(p.tokStr())
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
			if p.next() == TokSemicolon {
				p.consume()
			}
			return decl
		}
		if p.tok() == TokFunc {
			p.consume()
			decl.Left = p.parseFunc()
			if p.next() == TokSemicolon {
				p.consume()
			}
			return decl
		}
		if p.tok() == TokClass {
			classOff := uint32(p.toff())
			p.consume()
			decl.Left = p.parseClass()
			decl.Left.SrcOff = classOff
			if p.next() == TokSemicolon {
				p.consume()
			}
			return decl
		}
		decl.Left = p.parseAssign()
		if p.next() == TokSemicolon {
			p.consume()
		}
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
		}
		if p.next() == TokSemicolon {
			p.consume()
		}
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
		if p.next() == TokSemicolon {
			p.consume()
		}
		return decl
	}
	p.unexpected()
	return decl
}

// ---- statements ----

func (p *parser) parseStmt() *Node {
	p.next()

	if p.tok() == TokUsing {
		p.consume()
		n := p.parseVarDecl(VarUsing, false)
		if p.next() == TokSemicolon {
			p.consume()
		}
		return n
	}
	if p.tok() == TokAwait {
		saved := p.lx.save()
		p.consume()
		p.next()
		if p.tok() == TokUsing {
			p.consume()
			n := p.parseVarDecl(VarAwaitUsing, false)
			if p.next() == TokSemicolon {
				p.consume()
			}
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
		if p.next() == TokSemicolon {
			p.consume()
		}
		return n
	case TokLet:
		p.consume()
		n := p.parseVarDecl(VarLet, false)
		if p.next() == TokSemicolon {
			p.consume()
		}
		return n
	case TokConst:
		p.consume()
		n := p.parseVarDecl(VarConst, false)
		if p.next() == TokSemicolon {
			p.consume()
		}
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
		n.Right = p.parseExpr()
		if p.next() == TokSemicolon {
			p.consume()
		}
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
		if p.next() == TokSemicolon {
			p.consume()
		}
		return p.mk(NDebugger)
	case TokWith:
		return p.parseWith()
	case TokFunc:
		p.consume()
		return p.parseFunc()
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
	n.Left = p.parseStmt()
	if isLexicalDeclStmt(n.Left) {
		p.errorf("Lexical declaration cannot appear in single-statement context")
		return n
	}
	if p.next() == TokElse {
		p.consume()
		n.Right = p.parseStmt()
		if isLexicalDeclStmt(n.Right) {
			p.errorf("Lexical declaration cannot appear in single-statement context")
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
	n.Body = p.parseStmt()
	if isLexicalDeclStmt(n.Body) {
		p.errorf("Lexical declaration cannot appear in single-statement context")
	}
	return n
}

func (p *parser) parseDoWhile() *Node {
	p.consume()
	n := p.mk(NDoWhile)
	n.Body = p.parseStmt()
	if isLexicalDeclStmt(n.Body) {
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
	} else if initNode == nil && (p.tok() == TokVar || p.tok() == TokLet || p.tok() == TokConst) {
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
		n.Right = p.parseExpr()
		p.expect(TokRParen)
		n.Body = p.parseStmt()
		if p.lx.strict && initNode != nil && initNode.Kind == NVar && varDeclHasInitializer(initNode) {
			p.errorf("for-in loop variable declaration may not have an initializer in strict mode")
		}
		if isLexicalDeclStmt(n.Body) {
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
		n.Right = p.parseAssign()
		p.expect(TokRParen)
		n.Body = p.parseStmt()
		if isLexicalDeclStmt(n.Body) {
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
	n.Body = p.parseStmt()
	if isLexicalDeclStmt(n.Body) {
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
	n.Body = p.parseStmt()
	return n
}

func (p *parser) parseExprStmt() *Node {
	if p.tok() == TokIdentifier || isContextualIdentTok(p.tok()) {
		if p.la() == TokColon {
			label := p.mk(NLabel)
			label.Str = p.tokIdentStr()
			p.consume()
			p.next()
			p.consume()
			label.Body = p.parseStmt()
			return label
		}
	}
	expr := p.parseExpr()
	if p.next() == TokSemicolon {
		p.consume()
	}
	return expr
}
