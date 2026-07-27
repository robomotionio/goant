package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// An envelope stands in for data the host keeps outside the message. Resolving
// it on first read rather than before the parse is the whole point: a field the
// script never mentions costs nothing, however large the blob behind it.
func blobFixture(t *testing.T, rows int) (msg []byte, blob []byte, calls *int) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`[`)
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"amount":%d}`, i, i*2)
	}
	b.WriteString(`]`)
	blob = []byte(b.String())
	msg = []byte(`{"head":1,"rows":{"__ref":"xxh3:abc","__magic":20260301,"__type":"array"},"tail":2}`)
	n := 0
	return msg, blob, &n
}

func TestLazyBlobFetchedOnlyWhenRead(t *testing.T) {
	msg, blob, calls := blobFixture(t, 500)

	for _, tc := range []struct {
		name   string
		script string
		want   int
	}{
		{"never mentioned", `msg.head;`, 0},
		{"pass-through", `msg;`, 0},
		{"read once", `msg.rows.length;`, 1},
		{"read twice", `msg.rows.length + msg.rows[0].id;`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*calls = 0
			rt := New()
			rt.SetBlobResolver(func(ref string) ([]byte, error) {
				*calls++
				if ref != "xxh3:abc" {
					t.Errorf("resolver asked for %q", ref)
				}
				return blob, nil
			})
			v, err := rt.JSONParseBytesLazy(msg)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := rt.SetProp(rt.Global(), "msg", v); err != nil {
				t.Fatalf("SetProp: %v", err)
			}
			sc, cerr := rt.CompileScript("t.js", tc.script)
			if cerr != nil {
				t.Fatalf("compile: %v", cerr)
			}
			if _, rerr := rt.RunScript(sc); rerr != nil {
				t.Fatalf("run: %v", rerr)
			}
			if *calls != tc.want {
				t.Errorf("resolver called %d times, want %d", *calls, tc.want)
			}
		})
	}
}

// The resolved blob has to read as the data it stands for, not as the envelope.
func TestLazyBlobReadsAsItsContents(t *testing.T) {
	msg, blob, _ := blobFixture(t, 500)
	rt := New()
	rt.SetBlobResolver(func(string) ([]byte, error) { return blob, nil })
	v, err := rt.JSONParseBytesLazy(msg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rt.SetProp(rt.Global(), "msg", v)
	sc, _ := rt.CompileScript("t.js", `
		let sum = 0;
		for (const r of msg.rows) sum += r.amount;
		[Array.isArray(msg.rows), msg.rows.length, sum, msg.rows[499].id, msg.head].join(",");
	`)
	out, rerr := rt.RunScript(sc)
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	got, _ := rt.ToString(out)
	if want := "true,500,249500,499,1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A blob that cannot be produced stops the script and is reported. Handing the
// envelope through instead would surface as a type error somewhere unrelated,
// with nothing naming the missing blob.
func TestLazyBlobResolveFailureIsReported(t *testing.T) {
	msg, _, _ := blobFixture(t, 10)
	boom := errors.New("blob xxh3:abc is missing")
	rt := New()
	rt.SetBlobResolver(func(string) ([]byte, error) { return nil, boom })
	v, err := rt.JSONParseBytesLazy(msg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rt.SetProp(rt.Global(), "msg", v)

	// Not reading it must still succeed — the failure only exists if asked for.
	sc, _ := rt.CompileScript("t.js", `msg.head;`)
	if _, rerr := rt.RunScript(sc); rerr != nil {
		t.Fatalf("a script that never touched the blob was stopped: %v", rerr)
	}
	if rt.BlobResolveFailed() {
		t.Fatal("reported a resolve failure for a blob nobody asked for")
	}

	sc2, _ := rt.CompileScript("t.js", `msg.rows.length;`)
	if _, rerr := rt.RunScript(sc2); rerr == nil {
		t.Fatal("reading an unresolvable blob succeeded")
	}
	if !rt.BlobResolveFailed() {
		t.Error("stopped, but not reported as a blob failure")
	}
	if !errors.Is(rt.BlobResolveError(), boom) {
		t.Errorf("BlobResolveError = %v, want %v", rt.BlobResolveError(), boom)
	}
}

// Without a resolver an envelope is ordinary data, which is what a host with no
// blob store should see.
func TestLazyEnvelopeIsPlainDataWithoutAResolver(t *testing.T) {
	msg, _, _ := blobFixture(t, 10)
	rt := New()
	v, err := rt.JSONParseBytesLazy(msg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rt.SetProp(rt.Global(), "msg", v)
	sc, _ := rt.CompileScript("t.js", `msg.rows.__ref;`)
	out, rerr := rt.RunScript(sc)
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	if got, _ := rt.ToString(out); got != "xxh3:abc" {
		t.Errorf("got %q, want the raw envelope field", got)
	}
}
