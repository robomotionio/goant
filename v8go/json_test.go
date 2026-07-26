package v8go_test

import (
	"encoding/json"
	"strings"
	"testing"

	v8go "github.com/robomotionio/goant/v8go"
)

// The byte entry points exist to avoid copying the payload, which means the
// engine reads memory the host still owns. These tests pin the two things that
// makes true: that nothing keeps a view of the caller's buffer, and that the
// output spans are correct and independent.

func TestParseJSONBytes_RoundTrip(t *testing.T) {
	iso := v8go.NewIsolate()
	defer iso.Dispose()
	ctx := v8go.NewContext(iso)
	defer ctx.Close()

	const src = `{"a":1,"b":"two","c":[1,2,3],"d":{"e":null},"f":true}`
	v, err := ctx.ParseJSONBytes([]byte(src))
	if err != nil {
		t.Fatalf("ParseJSONBytes: %v", err)
	}
	if err := ctx.Global().Set("x", v); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := ctx.RunScript(`JSON.stringify(x)`, "t.js")
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got.String() != src {
		t.Errorf("round trip\n got %s\nwant %s", got.String(), src)
	}
}

// TestParseJSONBytes_DoesNotRetainCallerBuffer is the contract that makes the
// whole thing safe. The parser reads the caller's bytes in place, so if any
// string or key were a view over them rather than a copy, overwriting the
// buffer afterwards would silently corrupt values already handed to the script.
func TestParseJSONBytes_DoesNotRetainCallerBuffer(t *testing.T) {
	iso := v8go.NewIsolate()
	defer iso.Dispose()
	ctx := v8go.NewContext(iso)
	defer ctx.Close()

	buf := []byte(`{"key":"value","nested":{"deep":"payload"}}`)
	v, err := ctx.ParseJSONBytes(buf)
	if err != nil {
		t.Fatalf("ParseJSONBytes: %v", err)
	}
	if err := ctx.Global().Set("x", v); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Scribble over every byte the caller lent the engine.
	for i := range buf {
		buf[i] = 'Z'
	}

	got, err := ctx.RunScript(`JSON.stringify(x)`, "t.js")
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	const want = `{"key":"value","nested":{"deep":"payload"}}`
	if got.String() != want {
		t.Errorf("value changed when the caller reused its buffer\n got %s\nwant %s", got.String(), want)
	}
}

func TestParseJSONBytes_Rejects(t *testing.T) {
	iso := v8go.NewIsolate()
	defer iso.Dispose()
	ctx := v8go.NewContext(iso)
	defer ctx.Close()

	for _, src := range []string{``, `{`, `{"a":}`, `{"a":1} trailing`, `nope`} {
		if _, err := ctx.ParseJSONBytes([]byte(src)); err == nil {
			t.Errorf("ParseJSONBytes(%q) = nil error, want a parse error", src)
		}
	}
}

func TestJSONElementsToBytes(t *testing.T) {
	iso := v8go.NewIsolate()
	defer iso.Dispose()
	ctx := v8go.NewContext(iso)
	defer ctx.Close()

	arr, err := ctx.RunScript(`[{"a":1}, undefined, "text", 42, null]`, "t.js")
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	buf, ends, err := arr.JSONElementsToBytes(nil, -1)
	if err != nil {
		t.Fatalf("JSONElementsToBytes: %v", err)
	}
	got := spans(buf, ends)
	want := []string{`{"a":1}`, ``, `"text"`, `42`, `null`}
	if len(got) != len(want) {
		t.Fatalf("got %d spans %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("span %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A value that serializes to nothing must leave an empty span rather than
// shifting the ones after it — that is how the caller tells "no output here"
// from an output that happens to be short.
func TestJSONElementsToBytes_EmptySpanKeepsPositions(t *testing.T) {
	iso := v8go.NewIsolate()
	defer iso.Dispose()
	ctx := v8go.NewContext(iso)
	defer ctx.Close()

	arr, err := ctx.RunScript(`[undefined, undefined, {"third":true}]`, "t.js")
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	buf, ends, err := arr.JSONElementsToBytes(nil, -1)
	if err != nil {
		t.Fatalf("JSONElementsToBytes: %v", err)
	}
	got := spans(buf, ends)
	if len(got) != 3 {
		t.Fatalf("got %d spans, want 3", len(got))
	}
	if got[0] != "" || got[1] != "" {
		t.Errorf("leading undefined should be empty, got %q and %q", got[0], got[1])
	}
	if got[2] != `{"third":true}` {
		t.Errorf("span 2 = %q, want the third element", got[2])
	}
}

func TestJSONElementsToBytes_LimitAndAppend(t *testing.T) {
	iso := v8go.NewIsolate()
	defer iso.Dispose()
	ctx := v8go.NewContext(iso)
	defer ctx.Close()

	arr, err := ctx.RunScript(`[1, 2, 3, 4]`, "t.js")
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	// A limit stops the work, not just the reporting.
	buf, ends, err := arr.JSONElementsToBytes([]byte("PRE"), 2)
	if err != nil {
		t.Fatalf("JSONElementsToBytes: %v", err)
	}
	if len(ends) != 2 {
		t.Fatalf("got %d spans, want 2", len(ends))
	}
	if string(buf) != "PRE12" {
		t.Errorf("buf = %q, want PRE12 (appended to what it was given)", buf)
	}

	if _, _, err := arr.JSONElementsToBytes(nil, 0); err != nil {
		t.Errorf("limit 0: %v", err)
	}
}

func TestJSONElementsToBytes_NotAnArray(t *testing.T) {
	iso := v8go.NewIsolate()
	defer iso.Dispose()
	ctx := v8go.NewContext(iso)
	defer ctx.Close()

	v, err := ctx.RunScript(`({"a":1})`, "t.js")
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if _, _, err := v.JSONElementsToBytes(nil, -1); err == nil {
		t.Error("want an error for a non-array value")
	}
}

// TestJSONElementsToBytes_MatchesEncodingJSON checks the escaping the host now
// does itself, since the script no longer does it. Anything that differs from
// encoding/json here would reach a customer as a corrupted payload.
func TestJSONElementsToBytes_MatchesEncodingJSON(t *testing.T) {
	iso := v8go.NewIsolate()
	defer iso.Dispose()
	ctx := v8go.NewContext(iso)
	defer ctx.Close()

	cases := []string{
		`plain`,
		// Each of these carries exactly one kind of trouble. A case that mixes
		// them cannot tell which check caught it, and would still pass with one
		// of them broken.
		`he said "hi"`,
		`back\slash`,
		"bare\nnewline",
		"bare\ttab",
		`quote " backslash \ slash /`,
		"control \x00\x01\x1f bell \x07",
		"tab\there\nnewline\rcarriage",
		`unicode é ü ß 中文 🎉`,
		strings.Repeat("long ", 500),
		`mixed "é\n中` + "\x1b",
	}
	for _, s := range cases {
		if err := ctx.Global().Set("s", s); err != nil {
			t.Fatalf("Set: %v", err)
		}
		arr, err := ctx.RunScript(`[s]`, "t.js")
		if err != nil {
			t.Fatalf("RunScript: %v", err)
		}
		buf, ends, err := arr.JSONElementsToBytes(nil, -1)
		if err != nil {
			t.Fatalf("JSONElementsToBytes: %v", err)
		}
		got := spans(buf, ends)[0]

		// encoding/json escapes <, > and & as \u00xx by default; the JSON
		// grammar does not require it and JSON.stringify does not do it, so
		// compare by what the bytes decode to rather than byte for byte.
		var back string
		if err := json.Unmarshal([]byte(got), &back); err != nil {
			t.Errorf("%q serialized to %q, which is not valid JSON: %v", s, got, err)
			continue
		}
		if back != s {
			t.Errorf("round trip changed the string\n in  %q\n out %q\n json %s", s, back, got)
		}
	}
}

// spans slices a serialized buffer apart at the offsets each element ended at.
func spans(buf []byte, ends []int) []string {
	out := make([]string, len(ends))
	start := 0
	for i, end := range ends {
		out[i] = string(buf[start:end])
		start = end
	}
	return out
}
