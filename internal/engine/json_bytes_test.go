package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// The byte entry points must agree with the builtins they shortcut. If they
// ever diverge, a host and a script would disagree about the same payload,
// which is the worst kind of bug to chase — so the tests compare against
// JSON.parse/JSON.stringify rather than against hand-written expectations.

func TestJSONParseBytesMatchesBuiltin(t *testing.T) {
	cases := []string{
		`null`, `true`, `false`, `0`, `-1.5`, `1e10`, `"plain"`, `""`,
		`"esc \" \\ \n \t é 😀"`,
		`[]`, `[1,2,3]`, `[[1],[2,[3]]]`,
		`{}`, `{"a":1}`, `{"a":{"b":[1,2,{"c":null}]}}`,
		`{"unicode":"héllo 😀","empty":{},"arr":[]}`,
		`  {"leading":"ws"}  `,
		`{"dup":1,"dup":2}`,
		`{"":"empty key"}`,
		`[1.7976931348623157e308,5e-324,-0]`,
	}
	for _, src := range cases {
		rt := New()

		want, err := rt.JSONParse(src)
		if err != nil {
			t.Fatalf("builtin JSON.parse(%s): %v", src, err)
		}
		wantJSON, _, err := rt.JSONStringify(want)
		if err != nil {
			t.Fatalf("re-stringify(%s): %v", src, err)
		}

		got, err := rt.JSONParseBytes([]byte(src))
		if err != nil {
			t.Fatalf("JSONParseBytes(%s): %v", src, err)
		}
		gotJSON, _, err := rt.JSONStringify(got)
		if err != nil {
			t.Fatalf("stringify parsed(%s): %v", src, err)
		}
		if gotJSON != wantJSON {
			t.Fatalf("%s: JSONParseBytes gave %s, JSON.parse gave %s", src, gotJSON, wantJSON)
		}
	}
}

func TestJSONParseBytesRejectsBadInput(t *testing.T) {
	bad := []string{
		``, `   `, `{`, `[`, `}`, `]`, `{"a"}`, `{"a":}`, `[1,]`, `{,}`,
		`tru`, `nul`, `01`, `+1`, `.5`, `1.`, `"unterminated`,
		`{"a":1} trailing`, `[1,2] [3]`, `'single'`,
	}
	for _, src := range bad {
		rt := New()
		if _, err := rt.JSONParseBytes([]byte(src)); err == nil {
			t.Fatalf("JSONParseBytes(%q) accepted invalid JSON", src)
		}
	}
}

func TestJSONStringifyToBytesMatchesBuiltin(t *testing.T) {
	srcs := []string{
		`null`, `true`, `0`, `-0`, `1e21`, `"s"`, `"quote \" and \\ and \n"`,
		`[]`, `[1,null,"x"]`, `{}`, `{"a":1,"b":[1,2],"c":{"d":null}}`,
		`{"é":"héllo 😀"}`,
	}
	for _, src := range srcs {
		rt := New()
		v, err := rt.JSONParseBytes([]byte(src))
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		want, wok, err := rt.JSONStringify(v)
		if err != nil {
			t.Fatalf("builtin stringify %s: %v", src, err)
		}
		got, gok, err := rt.JSONStringifyToBytes(v, nil)
		if err != nil {
			t.Fatalf("JSONStringifyToBytes %s: %v", src, err)
		}
		if gok != wok {
			t.Fatalf("%s: ok mismatch %v vs %v", src, gok, wok)
		}
		if string(got) != want {
			t.Fatalf("%s: got %s, want %s", src, got, want)
		}
	}
}

// A value that serializes to nothing must report ok=false and leave the buffer
// alone. Writing "undefined", or an empty string, would both be wrong: the
// caller has to be able to tell "no value" from "the empty string".
func TestJSONStringifyToBytesNoValue(t *testing.T) {
	rt := New()
	for _, src := range []string{`undefined`, `(function(){})`, `Symbol()`} {
		v, err := rt.RunString("t.js", src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		dst := []byte("PREFIX")
		out, ok, err := rt.JSONStringifyToBytes(v, dst)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if ok {
			t.Fatalf("%s: reported a value", src)
		}
		if string(out) != "PREFIX" {
			t.Fatalf("%s: buffer was modified: %q", src, out)
		}
	}
}

// Appending must extend the caller's buffer rather than replace it, so a host
// can reuse one buffer across a whole batch.
func TestJSONStringifyToBytesAppends(t *testing.T) {
	rt := New()
	v, err := rt.JSONParseBytes([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	buf := []byte("head:")
	buf, ok, err := rt.JSONStringifyToBytes(v, buf)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if string(buf) != `head:{"a":1}` {
		t.Fatalf("got %q", buf)
	}
}

// The multi-output path: several results written back to back, with offsets to
// slice them apart. This is what replaces stringifying an array of already-
// stringified results.
func TestJSONStringifyEachToBytes(t *testing.T) {
	rt := New()
	var vals []Value
	for _, s := range []string{`{"a":1}`, `[1,2]`, `"x"`} {
		v, err := rt.JSONParseBytes([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		vals = append(vals, v)
	}
	// An undefined in the middle must contribute an empty span, not shift the
	// others.
	undef, _ := rt.RunString("u.js", `undefined`)
	vals = append(vals[:2], append([]Value{undef}, vals[2:]...)...)

	buf, ends, err := rt.JSONStringifyEachToBytes(vals, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`{"a":1}`, `[1,2]`, ``, `"x"`}
	if len(ends) != len(want) {
		t.Fatalf("got %d spans, want %d", len(ends), len(want))
	}
	start := 0
	for i, w := range want {
		got := string(buf[start:ends[i]])
		if got != w {
			t.Fatalf("span %d = %q, want %q", i, got, w)
		}
		start = ends[i]
	}
}

// Parsing takes a view over the caller's bytes, so strings in the result must
// be copies — otherwise a caller reusing its buffer would silently corrupt
// values the script is still holding.
func TestJSONParseBytesDoesNotAliasStrings(t *testing.T) {
	rt := New()
	buf := []byte(`{"k":"original"}`)
	v, err := rt.JSONParseBytes(buf)
	if err != nil {
		t.Fatal(err)
	}
	// Scribble over the caller's buffer.
	for i := range buf {
		buf[i] = 'X'
	}
	got, _, err := rt.JSONStringify(v)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"k":"original"}` {
		t.Fatalf("parsed value changed when the input buffer was overwritten: %s", got)
	}
}

// Round-trip against Go's own encoder on a realistic message, as an independent
// check that neither side is quietly reinterpreting anything.
func TestJSONBytesRoundTripAgainstEncodingJSON(t *testing.T) {
	type row struct {
		ID     string   `json:"id"`
		Amount float64  `json:"amount"`
		Tags   []string `json:"tags"`
	}
	in := map[string]any{
		"records": []row{
			{ID: "a", Amount: 1.5, Tags: []string{"x", "y"}},
			{ID: `quote"and\slash`, Amount: -2, Tags: nil},
			{ID: "héllo 😀", Amount: 0, Tags: []string{}},
		},
	}
	src, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}

	rt := New()
	v, err := rt.JSONParseBytes(src)
	if err != nil {
		t.Fatalf("JSONParseBytes: %v", err)
	}
	out, ok, err := rt.JSONStringifyToBytes(v, nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}

	var a, b any
	if err := json.Unmarshal(src, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatalf("our output is not valid JSON: %v\n%s", err, out)
	}
	as, _ := json.Marshal(a)
	bs, _ := json.Marshal(b)
	if !strings.EqualFold(string(as), string(bs)) {
		t.Fatalf("round trip changed the document:\n in: %s\nout: %s", as, bs)
	}
}
