package engine

import "testing"

func mustParse(t *testing.T, src string) *Node {
	t.Helper()
	prog, err := Parse("test.js", src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	if prog.Kind != NProgram {
		t.Fatalf("root not program: %v", prog.Kind)
	}
	return prog
}

func TestParseVarDecl(t *testing.T) {
	prog := mustParse(t, "var x = 1, y = 2;")
	if len(prog.Args) != 1 {
		t.Fatalf("want 1 stmt, got %d", len(prog.Args))
	}
	v := prog.Args[0]
	if v.Kind != NVar || v.VarKind != VarVar {
		t.Fatalf("bad var node: %v/%v", v.Kind, v.VarKind)
	}
	if len(v.Args) != 2 {
		t.Fatalf("want 2 declarators, got %d", len(v.Args))
	}
	if v.Args[0].Left.Str != "x" || v.Args[1].Left.Str != "y" {
		t.Fatal("declarator names wrong")
	}
}

func TestParseBinaryPrecedence(t *testing.T) {
	// 1 + 2 * 3 => (1 + (2 * 3))
	prog := mustParse(t, "1 + 2 * 3;")
	e := prog.Args[0]
	if e.Kind != NBinary || e.Op != TokPlus {
		t.Fatalf("top not +: %v %v", e.Kind, e.Op)
	}
	if e.Right.Kind != NBinary || e.Right.Op != TokMul {
		t.Fatalf("right not *: %v", e.Right.Op)
	}
	if e.Left.Num != 1 {
		t.Fatal("left operand")
	}
}

func TestParseExpRightAssoc(t *testing.T) {
	// 2 ** 3 ** 2 => (2 ** (3 ** 2))
	prog := mustParse(t, "2 ** 3 ** 2;")
	e := prog.Args[0]
	if e.Op != TokExp || e.Right.Op != TokExp {
		t.Fatalf("exp not right-assoc: %v/%v", e.Op, e.Right.Op)
	}
}

func TestParseFunctionAndArrow(t *testing.T) {
	prog := mustParse(t, "function f(a, b) { return a + b; } const g = (x) => x * 2;")
	f := prog.Args[0]
	if f.Kind != NFunc || f.Str != "f" || len(f.Args) != 2 {
		t.Fatalf("bad function: %v %q args=%d", f.Kind, f.Str, len(f.Args))
	}
	g := prog.Args[1]
	if g.Kind != NVar {
		t.Fatal("g should be const decl")
	}
	arrow := g.Args[0].Right
	if arrow.Kind != NFunc || arrow.Flags&fnArrow == 0 {
		t.Fatalf("g init not arrow: %v flags=%x", arrow.Kind, arrow.Flags)
	}
}

func TestParseClass(t *testing.T) {
	prog := mustParse(t, "class A extends B { constructor(){} static x = 1; #p = 2; get y(){return 1;} }")
	c := prog.Args[0]
	if c.Kind != NClass || c.Str != "A" {
		t.Fatalf("bad class: %v %q", c.Kind, c.Str)
	}
	if c.Left == nil || c.Left.Str != "B" {
		t.Fatal("extends clause missing")
	}
	if len(c.Args) != 4 {
		t.Fatalf("want 4 members, got %d", len(c.Args))
	}
}

func TestParseObjectAndArray(t *testing.T) {
	prog := mustParse(t, "var o = {a: 1, b, [c]: 3, m(){}, ...rest}; var a = [1, , 3, ...x];")
	o := prog.Args[0].Args[0].Right
	if o.Kind != NObject {
		t.Fatalf("not object: %v", o.Kind)
	}
	arr := prog.Args[1].Args[0].Right
	if arr.Kind != NArray || len(arr.Args) != 4 {
		t.Fatalf("bad array: %v len=%d", arr.Kind, len(arr.Args))
	}
	if arr.Args[1].Kind != NEmpty {
		t.Fatal("array hole not preserved")
	}
}

func TestParseTemplate(t *testing.T) {
	prog := mustParse(t, "var s = `hi ${name} and ${a + b}!`;")
	tpl := prog.Args[0].Args[0].Right
	if tpl.Kind != NTemplate {
		t.Fatalf("not template: %v", tpl.Kind)
	}
	// segments interleave: str expr str expr str => 5 args
	if len(tpl.Args) != 5 {
		t.Fatalf("template args=%d want 5", len(tpl.Args))
	}
	if tpl.Args[0].Str != "hi " {
		t.Fatalf("first segment cooked=%q", tpl.Args[0].Str)
	}
}

func TestParseControlFlow(t *testing.T) {
	srcs := []string{
		"if (x) y(); else z();",
		"while (a) b();",
		"do { x(); } while (y);",
		"for (var i = 0; i < 10; i++) f(i);",
		"for (const k in obj) g(k);",
		"for (let v of arr) h(v);",
		"try { a(); } catch (e) { b(); } finally { c(); }",
		"switch (x) { case 1: a(); break; default: b(); }",
		"label: for (;;) { break label; }",
		"with (obj) { x; }",
	}
	for _, s := range srcs {
		mustParse(t, s)
	}
}

func TestParseModuleSyntax(t *testing.T) {
	prog := mustParse(t, "import x from 'm'; export const y = 1; export default 42;")
	if prog.Flags&fnModuleSyntax == 0 {
		t.Fatal("module syntax flag not set")
	}
	if prog.Args[0].Kind != NImportDecl {
		t.Fatalf("first not import decl: %v", prog.Args[0].Kind)
	}
}

func TestParseUseStrictDirective(t *testing.T) {
	prog := mustParse(t, "'use strict'; var x = 1;")
	if prog.Flags&fnParseStrict == 0 {
		t.Fatal("strict flag not set from directive")
	}
}

func TestParseOptionalChaining(t *testing.T) {
	prog := mustParse(t, "a?.b?.[c]?.(d);")
	e := prog.Args[0]
	// Outermost is a call on an optional chain.
	if e.Kind != NCall {
		t.Fatalf("outer not call: %v", e.Kind)
	}
}

func TestParseErrors(t *testing.T) {
	// ant's parser is deliberately lenient (its `expect` consumes-if-match and
	// never errors); parse errors come only from explicit error paths. These
	// hit those paths.
	bad := []string{
		"`unterminated ${",        // TokErr from lexer (unterminated template)
		"var x = ;",               // unexpected token in primary
		"-2 ** 2;",                // unary before exponentiation
		"'use strict'; with(x){}", // with in strict mode
	}
	for _, s := range bad {
		if _, err := Parse("t.js", s); err == nil {
			t.Errorf("expected parse error for %q", s)
		}
	}
}

func TestParseStrictOctalError(t *testing.T) {
	if _, err := Parse("t.js", "'use strict'; var x = 0777;"); err == nil {
		t.Error("expected strict octal error")
	}
}

func TestParseSourceSpans(t *testing.T) {
	prog := mustParse(t, "function foo() { return 1; }")
	f := prog.Args[0]
	if f.SrcOff != 0 {
		t.Errorf("func src_off=%d want 0", f.SrcOff)
	}
	if f.SrcEnd == 0 || f.SrcEnd <= f.SrcOff {
		t.Errorf("func src_end=%d invalid", f.SrcEnd)
	}
}
