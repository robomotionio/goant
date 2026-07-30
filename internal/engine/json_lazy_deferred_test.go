package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// An array envelope that says how long it is can be answered about without
// being opened. `msg.rows.length` is the case that matters: the count is in the
// message already, and fetching a blob to rediscover it was reading the book to
// count its pages — a file read, a zstd decompress and a scan of the result,
// per read, which inside a loop is per element.
//
// These pin both halves: that the blob is not fetched until an element is
// genuinely needed, and — the half that could go silently wrong — that an array
// is only written back out as its envelope while it really does still stand for
// the whole of it.

// deferredFixture builds a message whose one large field is an array envelope
// declaring its length, and the blob that envelope stands for.
func deferredFixture(rows int) (msg, blob []byte) {
	var b strings.Builder
	b.WriteString(`[`)
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"amount":%d}`, i, i*2)
	}
	b.WriteString(`]`)
	msg = []byte(fmt.Sprintf(
		`{"head":1,"rows":{"__ref":"xxh3:abc","__magic":20260301,"__type":"array","__len":%d},"tail":2}`,
		rows))
	return msg, []byte(b.String())
}

// runDeferred evaluates script against the fixture and reports how many times
// the blob was fetched.
func runDeferred(t *testing.T, msg, blob []byte, script string) (rt *Runtime, out Value, calls int) {
	t.Helper()
	rt = New()
	rt.SetBlobResolver(func(ref string) ([]byte, error) {
		calls++
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
	sc, cerr := rt.CompileScript("t.js", script)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	out, rerr := rt.RunScript(sc)
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	return rt, out, calls
}

func TestDeferredArrayAnswersLengthWithoutFetching(t *testing.T) {
	msg, blob := deferredFixture(500)

	for _, tc := range []struct {
		name      string
		script    string
		want      float64
		wantCalls int
	}{
		{"length alone", `msg.rows.length`, 500, 0},
		{"length twice", `msg.rows.length + msg.rows.length`, 1000, 0},
		{"length of an untouched array beside a read sibling", `msg.head + msg.rows.length`, 501, 0},
		{"Array.isArray does not open it", `Array.isArray(msg.rows) ? msg.rows.length : -1`, 500, 0},
		{"an element opens it", `msg.rows[3].amount`, 6, 1},
		{"the last element opens it", `msg.rows[499].id`, 499, 1},
		{"opened once, however many reads", `msg.rows[0].id + msg.rows[9].id + msg.rows.length`, 509, 1},
		{"iteration opens it", `msg.rows.reduce((a, r) => a + r.amount, 0)`, 249500, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out, calls := runDeferred(t, msg, blob, tc.script)
			if got := out.Number(); got != tc.want {
				t.Errorf("%s = %v, want %v", tc.script, got, tc.want)
			}
			if calls != tc.wantCalls {
				t.Errorf("blob fetched %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// The whole point of not fetching is undone if the array is then written out as
// data. An untouched one has to go back as the reference it came in as.
func TestDeferredArrayUntouchedRoundTripsAsItsEnvelope(t *testing.T) {
	msg, blob := deferredFixture(300)

	for _, tc := range []struct {
		name         string
		script       string
		wantEnvelope bool
	}{
		{"never mentioned", `msg`, true},
		{"length read", `msg.rows.length; msg`, true},
		{"element read", `msg.rows[0].id; msg`, false},
		{"summed", `msg.rows.reduce((a, r) => a + r.amount, 0); msg`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, out, _ := runDeferred(t, msg, blob, tc.script)
			b, ok, err := rt.JSONStringifyToBytes(out, nil)
			if err != nil || !ok {
				t.Fatalf("stringify: ok=%v err=%v", ok, err)
			}
			gotEnvelope := strings.Contains(string(b), "__magic")
			if gotEnvelope != tc.wantEnvelope {
				t.Errorf("output carries the envelope = %v, want %v\n%s",
					gotEnvelope, tc.wantEnvelope, truncate(string(b)))
			}
			// Whatever form it took, the data has to survive.
			if !gotEnvelope && !strings.Contains(string(b), `"id":299`) {
				t.Errorf("expanded output lost the last row:\n%s", truncate(string(b)))
			}
		})
	}
}

// The check that an array still stands for its whole envelope is made by
// walking it, not by clearing a flag on write. This is that decision under
// test: every one of these mutates the array without reading an element, and
// every one of them must stop the envelope being written back — otherwise the
// message would carry a reference where the script left data.
//
// Both halves are asserted. "Not written back as its envelope" on its own says
// only that the output is data; it does not say the output is the data the
// script produced, and a mutation that the fetch then discarded satisfies it
// just as well. That is not hypothetical — `pushed` passed this test for as
// long as the push was being dropped, because 200 rows without the pushed one
// is still not an envelope. want is what the mutation should be visible as.
func TestDeferredArrayMutatedIsWrittenAsData(t *testing.T) {
	msg, blob := deferredFixture(200)

	for _, tc := range []struct {
		name   string
		script string
		want   string // empty when the mutation is not observable in JSON
	}{
		{"element assigned", `msg.rows[0] = {id: -1, amount: -1}; msg`, `"rows":[{"id":-1,"amount":-1},{"id":1,`},
		{"pushed", `msg.rows.push({id: -1, amount: -1}); msg`, `{"id":199,"amount":398},{"id":-1,"amount":-1}]`},
		{"popped", `msg.rows.pop(); msg`, `{"id":198,"amount":396}]`},
		{"truncated", `msg.rows.length = 5; msg`, `{"id":4,"amount":8}]`},
		{"lengthened", `msg.rows.length = 400; msg`, `{"id":199,"amount":398},null,`},
		{"element deleted", `delete msg.rows[0]; msg`, `"rows":[null,{"id":1,`},
		// A named property is not part of an array's JSON, so there is nothing to
		// look for in the output; the envelope check is the whole assertion.
		{"property defined on the array", `Object.defineProperty(msg.rows, "tag", {value: 1, enumerable: true}); msg`, ""},
		{"named property set", `msg.rows.tag = 1; msg`, ""},
		{"reversed", `msg.rows.reverse(); msg`, `"rows":[{"id":199,"amount":398},`},
		{"sorted", `msg.rows.sort((a, b) => b.id - a.id); msg`, `"rows":[{"id":199,"amount":398},`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, out, _ := runDeferred(t, msg, blob, tc.script)
			b, ok, err := rt.JSONStringifyToBytes(out, nil)
			if err != nil || !ok {
				t.Fatalf("stringify: ok=%v err=%v", ok, err)
			}
			if strings.Contains(string(b), "__magic") {
				t.Fatalf("a mutated array was written back as its envelope, losing the "+
					"mutation — the untouched check let a write through:\n%s", truncate(string(b)))
			}
			if tc.want != "" && !strings.Contains(string(b), tc.want) {
				t.Errorf("the output is data, but not the script's data: %q is missing, "+
					"so the mutation was made and then dropped\n%s", tc.want, truncate(string(b)))
			}
		})
	}
}

// A parked array can be written to without ever being opened — push appends at
// a length the envelope already knows, and neither a truncation nor an
// assignment past the end needs to look at an element either. So the fetch,
// when something finally does read one, lands on an array the script has
// already changed, and has to merge under those changes rather than over them.
//
// This is the failure that took the longest to find in the field: five pushes
// followed by one element write left the array back at its original length with
// the pushes gone, and the message went on to the next node that way. Nothing
// errored; the rows were simply fewer than the script had made them.
func TestDeferredArrayKeepsWritesMadeBeforeItWasOpened(t *testing.T) {
	const pushFive = `for (var i = 0; i < 5; i++) { r.push({id: 900000 + i, amount: 7}); }`

	for _, tc := range []struct {
		name      string
		script    string
		want      string
		wantCalls int
	}{
		{
			// The field report, reduced: push, then touch.
			"push, then write an element",
			`var r = msg.rows; var n0 = r.length; ` + pushFive + ` r[0].amount = 999999;
			 n0 + "/" + r.length + "/" + r[0].amount + "/" + r[504].id`,
			"500/505/999999/900004", 1,
		},
		{
			"push, then read an element",
			`var r = msg.rows; var n0 = r.length; ` + pushFive + ` var got = r[0].amount;
			 n0 + "/" + r.length + "/" + got + "/" + r[504].id`,
			"500/505/0/900004", 1,
		},
		{
			// Reading first is the order that always worked; it must still work.
			"read an element, then push",
			`var r = msg.rows; var got = r[0].amount; var n0 = r.length; ` + pushFive + `
			 n0 + "/" + r.length + "/" + got + "/" + r[504].id`,
			"500/505/0/900004", 1,
		},
		{
			// Never reads an element at all: the fetch comes from serializing.
			"push, then serialize",
			`var r = msg.rows; ` + pushFive + `
			 var j = JSON.stringify(r);
			 r.length + "/" + (j.indexOf("900004") >= 0 ? "kept" : "lost")`,
			"505/kept", 1,
		},
		{
			"assign past the end, then read",
			`var r = msg.rows; r[600] = {id: 42}; var got = r[0].amount;
			 r.length + "/" + r[600].id + "/" + (r[550] === undefined ? "hole" : "filled")`,
			"601/42/hole", 1,
		},
		{
			"truncate, then read",
			`var r = msg.rows; r.length = 10; var got = r[0].amount;
			 r.length + "/" + got + "/" + (r[10] === undefined ? "gone" : "still there")`,
			"10/0/gone", 1,
		},
		{
			"push, then read nothing",
			`var r = msg.rows; ` + pushFive + ` String(r.length)`,
			"505", 0, // the length still comes from the envelope plus the pushes
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, blob := deferredFixture(500)
			rt, out, calls := runDeferred(t, msg, blob, tc.script)
			got, serr := rt.ToString(out)
			if serr != nil {
				t.Fatalf("ToString: %v", serr)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if calls != tc.wantCalls {
				t.Errorf("fetched the blob %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// A wrong __len and a script that writes are the two hard cases meeting: the
// writes were made at indices chosen against a length that was not real, and
// the blob then arrives disagreeing about how many elements there are.
//
// There is no reading of that which is right in every sense — a push made at
// the length the envelope claimed went to an index chosen from a lie, and no
// later correction moves it somewhere better. So the two things that must hold
// are narrower: nothing the script wrote is dropped because the envelope was
// wrong, and the blob's own rows are still there to be read.
func TestDeferredArrayKeepsWritesWhenTheEnvelopeLiedAboutTheLength(t *testing.T) {
	_, blob := deferredFixture(50) // the truth: 50 rows

	for _, tc := range []struct {
		name  string
		claim string
		want  string // length / id of the last pushed row / id of blob row 0
	}{
		// Pushed at 500-504, so the length has to reach past the 50 real rows to
		// cover them; 50-499 are holes.
		{"claimed more than there are", "500", "505/900004/0"},
		// Pushed at 3-7, over real rows the script was told did not exist. The
		// pushes are what survives there, because they are what it wrote.
		{"claimed fewer than there are", "3", "55/900004/0"},
		{"claimed exactly right", "50", "55/900004/0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := []byte(fmt.Sprintf(
				`{"rows":{"__ref":"xxh3:abc","__magic":20260301,"__type":"array","__len":%s}}`,
				tc.claim))
			rt, out, _ := runDeferred(t, msg, blob, `
				var r = msg.rows;
				for (var i = 0; i < 5; i++) { r.push({id: 900000 + i}); }
				var row0 = r[0].id;   // opens the blob, under the pushes
				var last = -1;
				for (var j = 0; j < r.length; j++) { if (r[j] && r[j].id === 900004) { last = j; } }
				r.length + "/" + (last >= 0 ? r[last].id : "LOST") + "/" + row0`)
			got, serr := rt.ToString(out)
			if serr != nil {
				t.Fatalf("ToString: %v", serr)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// __len rides in the message rather than in the content-addressed blob, so it
// is the one part of an envelope that is not self-verifying. A wrong one must
// degrade to a wrong length, never to a panic or to invented data: the blob is
// authoritative the moment it arrives.
func TestDeferredArrayTrustsLengthUntilTheBlobDisagrees(t *testing.T) {
	_, blob := deferredFixture(50)

	for _, tc := range []struct {
		name   string
		claim  string
		script string
		want   float64
	}{
		{"claims more than there are", "5000", `msg.rows.length`, 5000},
		{"corrected once opened", "5000", `msg.rows[0].id; msg.rows.length`, 50},
		{"past the end reads undefined", "5000", `msg.rows[4000] === undefined ? 1 : 0`, 1},
		{"claims fewer than there are", "3", `msg.rows.length`, 3},
		{"corrected upwards once opened", "3", `msg.rows[0].id; msg.rows.length`, 50},
		{"not a count at all", `"nope"`, `msg.rows.length`, 50},
		{"zero falls back to opening it", "0", `msg.rows.length`, 50},
		{"negative falls back to opening it", "-4", `msg.rows.length`, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := []byte(fmt.Sprintf(
				`{"rows":{"__ref":"xxh3:abc","__magic":20260301,"__type":"array","__len":%s}}`,
				tc.claim))
			_, out, _ := runDeferred(t, m, blob, tc.script)
			if got := out.Number(); got != tc.want {
				t.Errorf("%s = %v, want %v", tc.script, got, tc.want)
			}
		})
	}
}

// __len only means an element count when __type says "array". A string envelope
// spells its character count the same way, and believing that would stand up an
// array of the wrong length that nothing would ever correct.
func TestDeferredArrayIgnoresLengthOnANonArrayEnvelope(t *testing.T) {
	blob := []byte(`"hello there"`)
	msg := []byte(`{"s":{"__ref":"xxh3:abc","__magic":20260301,"__type":"string","__len":11}}`)
	_, out, calls := runDeferred(t, msg, blob, `msg.s.length`)
	if got := out.Number(); got != 11 {
		t.Errorf("msg.s.length = %v, want 11", got)
	}
	if calls != 1 {
		t.Errorf("a string envelope was fetched %d times, want 1 — its __len is "+
			"characters, not elements, so it cannot be answered from the envelope", calls)
	}
}

// A blob that cannot be fetched has to stop the script rather than leave the
// elements reading as undefined, which would turn a missing blob into a wrong
// answer. Reading the length alone never needs the blob, so it still succeeds.
func TestDeferredArrayReportsAFetchFailureOnFirstElement(t *testing.T) {
	msg, _ := deferredFixture(100)
	boom := errors.New("blob is gone")

	newRT := func() *Runtime {
		rt := New()
		rt.SetBlobResolver(func(string) ([]byte, error) { return nil, boom })
		v, err := rt.JSONParseBytesLazy(msg)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if err := rt.SetProp(rt.Global(), "msg", v); err != nil {
			t.Fatalf("SetProp: %v", err)
		}
		return rt
	}

	rt := newRT()
	sc, _ := rt.CompileScript("t.js", `msg.rows.length`)
	out, err := rt.RunScript(sc)
	if err != nil {
		t.Errorf("reading the length needed the blob: %v", err)
	} else if out.Number() != 100 {
		t.Errorf("length = %v, want 100", out.Number())
	}

	rt = newRT()
	sc, _ = rt.CompileScript("t.js", `msg.rows[0].id`)
	if _, err := rt.RunScript(sc); err == nil {
		t.Error("reading an element of a blob that is gone was accepted silently")
	} else if !errors.Is(err, boom) {
		t.Errorf("failure reported as %v, want it to carry %v", err, boom)
	}
}

// Deferring must not change any answer. Same document, same expression, parsed
// both ways — the lazy one with the array behind an envelope, the eager one
// with it inline — and the two have to agree.
func TestDeferredArrayMatchesTheEagerParse(t *testing.T) {
	msg, blob := deferredFixture(120)
	inline := []byte(fmt.Sprintf(`{"head":1,"rows":%s,"tail":2}`, blob))

	for _, script := range []string{
		`msg.rows.length`,
		`msg.rows[0].id`,
		`msg.rows[119].amount`,
		`msg.rows.reduce((a, r) => a + r.amount, 0)`,
		`Object.keys(msg).join(",").length`,
		`JSON.stringify(msg.rows[7])`,
		`msg.rows.filter(r => r.id % 17 === 0).length`,
		`msg.rows.map(r => r.id).slice(-3).join("-")`,
		`(msg.rows.length, msg.rows[2].id)`,
		`"rows" in msg ? msg.rows.length : -1`,
		`typeof msg.rows`,
		`Array.isArray(msg.rows) ? "yes" : "no"`,
		`msg.rows.slice(0, 2).map(r => r.amount).join(",")`,
		`(() => { let n = 0; for (const r of msg.rows) n += r.id; return n; })()`,
	} {
		t.Run(script, func(t *testing.T) {
			// A Value is a handle into the Runtime that produced it, so it has
			// to be read back through that same Runtime.
			lazyRT, lazyOut, _ := runDeferred(t, msg, blob, script)
			lazyStr, _ := lazyRT.ToString(lazyOut)

			rt := New()
			v, err := rt.JSONParseBytes(inline)
			if err != nil {
				t.Fatalf("eager parse: %v", err)
			}
			if err := rt.SetProp(rt.Global(), "msg", v); err != nil {
				t.Fatalf("SetProp: %v", err)
			}
			sc, cerr := rt.CompileScript("t.js", script)
			if cerr != nil {
				t.Fatalf("compile: %v", cerr)
			}
			eagerOut, rerr := rt.RunScript(sc)
			if rerr != nil {
				t.Fatalf("eager run: %v", rerr)
			}
			eagerStr, _ := rt.ToString(eagerOut)

			if lazyStr != eagerStr {
				t.Errorf("%s\n  deferred: %s\n  eager:    %s", script, lazyStr, eagerStr)
			}
		})
	}
}

func truncate(s string) string {
	if len(s) <= 300 {
		return s
	}
	return s[:300] + "..."
}
