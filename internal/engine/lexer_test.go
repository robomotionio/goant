package engine

import (
	"math"
	"testing"
)

// lex returns the full token stream (kind + text) for src.
func lexAll(src string) []struct {
	tok  Token
	text string
} {
	l := newLexer(src, false)
	var out []struct {
		tok  Token
		text string
	}
	for {
		t := l.next()
		if t == TokEOF || t == TokErr {
			out = append(out, struct {
				tok  Token
				text string
			}{t, ""})
			break
		}
		out = append(out, struct {
			tok  Token
			text string
		}{t, l.tokenText()})
		l.consume()
	}
	return out
}

func TestLexBasics(t *testing.T) {
	toks := lexAll("var x = 1 + 2;")
	want := []struct {
		tok  Token
		text string
	}{
		{TokVar, "var"}, {TokIdentifier, "x"}, {TokAssign, "="},
		{TokNumber, "1"}, {TokPlus, "+"}, {TokNumber, "2"}, {TokSemicolon, ";"},
		{TokEOF, ""},
	}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens, want %d: %+v", len(toks), len(want), toks)
	}
	for i, w := range want {
		if toks[i].tok != w.tok || toks[i].text != w.text {
			t.Errorf("token %d: got {%v %q} want {%v %q}", i, toks[i].tok, toks[i].text, w.tok, w.text)
		}
	}
}

func TestLexNumbers(t *testing.T) {
	cases := []struct {
		src string
		val float64
	}{
		{"0", 0}, {"42", 42}, {"3.14", 3.14}, {"1e3", 1000}, {".5", 0.5},
		{"0xff", 255}, {"0b101", 5}, {"0o17", 15}, {"1_000", 1000},
		{"0xFF_FF", 65535}, {"1.5e-2", 0.015},
	}
	for _, c := range cases {
		l := newLexer(c.src, false)
		tok := l.next()
		if tok != TokNumber {
			t.Errorf("%q: tok=%v want number", c.src, tok)
			continue
		}
		if got := l.numberValue().Number(); got != c.val {
			t.Errorf("%q: value=%v want %v", c.src, got, c.val)
		}
		if l.st.tlen != len(c.src) {
			t.Errorf("%q: tlen=%d want %d", c.src, l.st.tlen, len(c.src))
		}
	}
}

func TestLexBigInt(t *testing.T) {
	l := newLexer("123n", false)
	if l.next() != TokBigInt {
		t.Fatal("expected bigint")
	}
	if l.tokenText() != "123n" {
		t.Fatalf("text=%q", l.tokenText())
	}
}

func TestLexStrictOctal(t *testing.T) {
	l := newLexer("0777", true)
	if l.next() != TokErr {
		t.Fatal("strict-mode legacy octal should be an error")
	}
	l2 := newLexer("0777", false)
	if l2.next() != TokNumber || l2.numberValue().Number() != 511 {
		t.Fatalf("sloppy octal: tok/val wrong (%v)", l2.numberValue().Number())
	}
}

func TestLexStringsAndTemplates(t *testing.T) {
	l := newLexer(`"hi\n" 'a\'b' `+"`t${x}y`", false)
	if l.next() != TokString {
		t.Fatal("expected string 1")
	}
	l.consume()
	if l.next() != TokString {
		t.Fatal("expected string 2")
	}
	l.consume()
	if l.next() != TokTemplate {
		t.Fatal("expected template")
	}
	if l.tokenText() != "`t${x}y`" {
		t.Fatalf("template text=%q", l.tokenText())
	}
}

func TestLexKeywordsAndIdents(t *testing.T) {
	toks := lexAll("function foo async await yield let const")
	kinds := []Token{TokFunc, TokIdentifier, TokAsync, TokAwait, TokYield, TokLet, TokConst, TokEOF}
	for i, k := range kinds {
		if toks[i].tok != k {
			t.Errorf("token %d: got %v want %v", i, toks[i].tok, k)
		}
	}
}

func TestLexNewlineTracking(t *testing.T) {
	l := newLexer("a\nb", false)
	l.next() // a
	if l.st.hadNewline {
		t.Error("first token should not have preceding newline")
	}
	l.consume()
	l.next() // b
	if !l.st.hadNewline {
		t.Error("b should have preceding newline (ASI)")
	}
}

func TestLexComments(t *testing.T) {
	toks := lexAll("a // line\n b /* block */ c")
	kinds := []Token{TokIdentifier, TokIdentifier, TokIdentifier, TokEOF}
	if len(toks) != len(kinds) {
		t.Fatalf("got %d tokens: %+v", len(toks), toks)
	}
	for i, k := range kinds {
		if toks[i].tok != k {
			t.Errorf("token %d: got %v want %v", i, toks[i].tok, k)
		}
	}
}

func TestLexHashbang(t *testing.T) {
	toks := lexAll("#!/usr/bin/env goant\nvar x")
	if toks[0].tok != TokVar {
		t.Fatalf("hashbang not skipped: first tok %v", toks[0].tok)
	}
}

func TestLexOperators(t *testing.T) {
	toks := lexAll("a >>>= b ?? c ?. d => e ** f")
	kinds := []Token{
		TokIdentifier, TokZShrAssign, TokIdentifier, TokNullish, TokIdentifier,
		TokOptionalChain, TokIdentifier, TokArrow, TokIdentifier, TokExp, TokIdentifier, TokEOF,
	}
	for i, k := range kinds {
		if i >= len(toks) || toks[i].tok != k {
			t.Errorf("token %d: got %v want %v", i, toks[i].tok, k)
		}
	}
}

func TestLexNaNGuard(t *testing.T) {
	// Sanity: numeric NaN literal path not applicable, but ensure tov works.
	if !math.IsNaN(tov(math.NaN()).Number()) {
		t.Fatal("tov NaN")
	}
}
