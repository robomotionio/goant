package engine

// Token kinds — ported 1:1 from ant include/tokens.h. The exact numeric values
// matter: the parser does range checks (identifier-like tokens live in
// [TokIdentifier, TokIdentLikeEnd); operators start at TokDot=100) and indexes
// precTable by token.
type Token uint8

const (
	TokErr Token = iota
	TokEOF
	TokNumber
	TokString
	TokSemicolon
	TokBigInt
	TokLParen
	TokRParen
	TokLBrace
	TokRBrace
	TokLBracket
	TokRBracket
)

// Identifier-like tokens (keywords + identifiers), based at 50.
const (
	TokIdentifier Token = 50 + iota
	TokAsync
	TokAwait
	TokBreak
	TokCase
	TokCatch
	TokClass
	TokConst
	TokContinue
	TokDefault
	TokDelete
	TokDo
	TokDebugger
	TokElse
	TokExport
	TokFinally
	TokFor
	TokFrom
	TokFunc
	TokIf
	TokImport
	TokIn
	TokInstanceof
	TokLet
	TokNew
	TokOf
	TokReturn
	TokSuper
	TokSwitch
	TokThis
	TokThrow
	TokTry
	TokVar
	TokVoid
	TokWhile
	TokWith
	TokYield
	TokUndef
	TokNull
	TokTrue
	TokFalse
	TokAs
	TokStatic
	TokTypeof
	TokUsing
	TokWindow
	TokGlobalThis
	TokIdentLikeEnd
)

// Operators, based at 100.
const (
	TokDot Token = 100 + iota
	TokCall
	TokBracket
	TokPostInc
	TokPostDec
	TokNot
	TokTilda
	TokUPlus
	TokUMinus
	TokExp
	TokMul
	TokDiv
	TokRem
	TokOptionalChain
	TokRest
	TokPlus
	TokMinus
	TokShl
	TokShr
	TokZShr
	TokLt
	TokLe
	TokGt
	TokGe
	TokEq
	TokNe
	TokSeq
	TokSne
	TokAnd
	TokXor
	TokOr
	TokLand
	TokLor
	TokNullish
	TokColon
	TokQ
	TokAssign
	TokPlusAssign
	TokMinusAssign
	TokMulAssign
	TokDivAssign
	TokRemAssign
	TokShlAssign
	TokShrAssign
	TokZShrAssign
	TokAndAssign
	TokXorAssign
	TokOrAssign
	TokExpAssign
	TokLorAssign
	TokLandAssign
	TokNullishAssign
	TokComma
	TokTemplate
	TokArrow
	TokHash
	TokMax
)

// precTable is the binary-operator precedence table (ant tokens.h prec_table).
// Higher binds tighter; 0 means "not a binary operator".
var precTable = func() [TokMax]uint8 {
	var t [TokMax]uint8
	t[TokLor] = 4
	t[TokLand] = 5
	t[TokNullish] = 5
	t[TokOr] = 6
	t[TokXor] = 7
	t[TokAnd] = 8
	t[TokEq], t[TokNe], t[TokSeq], t[TokSne] = 9, 9, 9, 9
	t[TokLt], t[TokLe], t[TokGt], t[TokGe] = 10, 10, 10, 10
	t[TokInstanceof], t[TokIn] = 10, 10
	t[TokShl], t[TokShr], t[TokZShr] = 11, 11, 11
	t[TokPlus], t[TokMinus] = 12, 12
	t[TokMul], t[TokDiv], t[TokRem] = 13, 13, 13
	t[TokExp] = 14
	return t
}()

// isIdentLike reports whether tok is an identifier or keyword (in the
// [TokIdentifier, TokIdentLikeEnd) band).
func isIdentLike(tok Token) bool {
	return tok >= TokIdentifier && tok < TokIdentLikeEnd
}

// parseKeyword returns the keyword token for buf, or TokIdentifier if none
// (ant sv_parsekeyword). buf is the raw identifier bytes.
func parseKeyword(buf string) Token {
	if len(buf) == 0 {
		return TokIdentifier
	}
	m := func(s string) bool { return buf == s }
	switch buf[0] {
	case 'a':
		switch {
		case m("as"):
			return TokAs
		case m("async"):
			return TokAsync
		case m("await"):
			return TokAwait
		}
	case 'b':
		if m("break") {
			return TokBreak
		}
	case 'c':
		switch {
		case m("case"):
			return TokCase
		case m("catch"):
			return TokCatch
		case m("class"):
			return TokClass
		case m("const"):
			return TokConst
		case m("continue"):
			return TokContinue
		}
	case 'd':
		switch {
		case m("do"):
			return TokDo
		case m("default"):
			return TokDefault
		case m("delete"):
			return TokDelete
		case m("debugger"):
			return TokDebugger
		}
	case 'e':
		switch {
		case m("else"):
			return TokElse
		case m("export"):
			return TokExport
		}
	case 'f':
		switch {
		case m("for"):
			return TokFor
		case m("from"):
			return TokFrom
		case m("false"):
			return TokFalse
		case m("finally"):
			return TokFinally
		case m("function"):
			return TokFunc
		}
	case 'g':
		if m("globalThis") {
			return TokGlobalThis
		}
	case 'i':
		switch {
		case m("if"):
			return TokIf
		case m("in"):
			return TokIn
		case m("import"):
			return TokImport
		case m("instanceof"):
			return TokInstanceof
		}
	case 'l':
		if m("let") {
			return TokLet
		}
	case 'n':
		switch {
		case m("new"):
			return TokNew
		case m("null"):
			return TokNull
		}
	case 'o':
		if m("of") {
			return TokOf
		}
	case 'r':
		if m("return") {
			return TokReturn
		}
	case 's':
		switch {
		case m("super"):
			return TokSuper
		case m("static"):
			return TokStatic
		case m("switch"):
			return TokSwitch
		}
	case 't':
		switch {
		case m("try"):
			return TokTry
		case m("this"):
			return TokThis
		case m("true"):
			return TokTrue
		case m("throw"):
			return TokThrow
		case m("typeof"):
			return TokTypeof
		}
	case 'u':
		switch {
		case m("undefined"):
			return TokUndef
		case m("using"):
			return TokUsing
		}
	case 'v':
		switch {
		case m("var"):
			return TokVar
		case m("void"):
			return TokVoid
		}
	case 'w':
		switch {
		case m("while"):
			return TokWhile
		case m("with"):
			return TokWith
		case m("window"):
			return TokWindow
		}
	case 'y':
		if m("yield") {
			return TokYield
		}
	}
	return TokIdentifier
}

// isEvalOrArgumentsName reports the strict-mode-restricted binding names.
func isEvalOrArgumentsName(s string) bool { return s == "eval" || s == "arguments" }

// isStrictReservedName reports names reserved only in strict mode.
func isStrictReservedName(s string) bool {
	switch s {
	case "interface", "implements", "let", "private", "package", "public",
		"protected", "static", "yield":
		return true
	}
	return false
}
