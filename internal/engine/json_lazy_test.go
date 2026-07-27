package engine

import (
	"fmt"
	"strings"
	"testing"
)

// A lazily parsed document has to be indistinguishable from an eagerly parsed
// one. That is not a property any single test can assert, because the two
// differ in exactly one place — the moment a value gets built — and every
// operation in the language is a candidate for observing it.
//
// So the check is differential rather than expectational: parse the same bytes
// both ways, run the same script against each, and require the same answer.
// A read path that was never taught about unparsed spans shows up here as a
// mismatch instead of as silently wrong data in a customer's message, which is
// the failure mode this whole design is trying not to have.

// lazyProbeDoc exercises the shapes a message can contain: nesting, empty
// containers, duplicate and empty keys, every number form, escapes, keys that
// look like indices, and a key the object model treats specially.
const lazyProbeDoc = `{
  "str": "plain",
  "esc": "q\" b\\ s/ \b\f\n\r\t ué hilo",
  "uni": "héllo 😀",
  "num": 42,
  "neg": -1.5,
  "exp": 1.25e3,
  "big": 1.7976931348623157e308,
  "tiny": 5e-324,
  "zero": 0,
  "negzero": -0,
  "yes": true,
  "no": false,
  "nil": null,
  "emptyObj": {},
  "emptyArr": [],
  "": "empty key",
  "0": "index-like key",
  "12": "another",
  "__proto__": {"polluted": true},
  "toString": "shadow",
  "obj": {"a": {"b": {"c": [1, 2, {"d": "deep"}]}}},
  "arr": [10, 20, 30, 40, 50],
  "mixed": [1, "two", null, true, {"k": "v"}, [1, [2, [3]]], -0],
  "records": [
    {"id": 1, "name": "alpha", "amount": 10.5, "tags": ["x", "y"]},
    {"id": 2, "name": "beta",  "amount": 20.25, "tags": []},
    {"id": 3, "name": "gamma", "amount": 30,    "tags": ["z"]}
  ],
  "dup": 1, "dup": 2,
  "sparse": [1, null, 3]
}`

// lazyProbes are run against `msg` in both parses. Each is an expression; the
// harness reports its type and value, or the throw it produced.
var lazyProbes = []string{
	// --- plain reads, repeated so the second one comes from the inline cache
	`msg.str`, `msg.str + "|" + msg.str`, `msg.num`, `msg.neg`, `msg.exp`,
	`msg.big`, `msg.tiny`, `msg.zero`, `1/msg.negzero`, `msg.yes`, `msg.no`,
	`msg.nil`, `msg.esc`, `msg.uni`, `msg.missing`, `msg[""]`, `msg["0"]`,
	`msg["12"]`, `msg.dup`, `msg.toString`,

	// --- nesting
	`msg.obj.a.b.c[2].d`, `msg.obj.a.b.c.length`, `msg.emptyObj`, `msg.emptyArr`,
	`msg.obj.a.b.c[0] + msg.obj.a.b.c[1]`,

	// --- arrays: indexing, holes, the whole prototype
	`msg.arr[0]`, `msg.arr[4]`, `msg.arr[5]`, `msg.arr[-1]`, `msg.arr.length`,
	`msg.arr.at(-1)`, `msg.arr.join("-")`, `msg.arr.slice(1, 3)`,
	`msg.arr.map(x => x * 2)`, `msg.arr.filter(x => x > 25)`,
	`msg.arr.reduce((a, b) => a + b, 0)`, `msg.arr.indexOf(30)`,
	`msg.arr.includes(40)`, `msg.arr.find(x => x > 15)`,
	`msg.arr.findIndex(x => x > 15)`, `msg.arr.some(x => x > 45)`,
	`msg.arr.every(x => x > 5)`, `msg.arr.concat([60])`, `msg.arr.reverse()`,
	`msg.arr.sort((a, b) => b - a)`, `msg.arr.flat()`, `msg.mixed.flat(2)`,
	`msg.arr.toString()`, `msg.arr.lastIndexOf(20)`, `msg.arr.fill(0, 1, 2)`,
	`msg.arr.copyWithin(0, 3)`, `msg.arr.splice(1, 2)`, `msg.arr.pop()`,
	`msg.arr.shift()`, `msg.arr.entries().next().value`,
	`Array.from(msg.arr)`, `Array.isArray(msg.arr)`, `Array.of(...msg.arr)`,
	`[...msg.arr]`, `[].concat(msg.arr)`, `msg.sparse`,

	// --- iteration
	`(() => { let s = ""; for (const x of msg.arr) s += x + ","; return s; })()`,
	`(() => { let s = ""; for (const k in msg.obj.a.b.c) s += k + ","; return s; })()`,
	`(() => { let s = 0; msg.records.forEach(r => s += r.amount); return s; })()`,
	`(() => { let s = ""; for (const k in msg) s += k + ","; return s; })()`,

	// --- reflection over keys
	`Object.keys(msg)`, `Object.keys(msg.obj.a.b)`, `Object.values(msg.arr)`,
	`Object.entries(msg.records[0])`, `Object.getOwnPropertyNames(msg.arr)`,
	`Object.getOwnPropertyDescriptor(msg, "num")`,
	`Object.getOwnPropertyDescriptor(msg.arr, "1")`,
	`Object.getOwnPropertyDescriptors(msg.records[1])`,
	`Reflect.ownKeys(msg.emptyObj)`, `Reflect.ownKeys(msg.arr)`,
	`Reflect.get(msg, "num")`, `Reflect.has(msg, "str")`,
	`Object.fromEntries(Object.entries(msg.records[2]))`,
	`"str" in msg`, `"nope" in msg`, `0 in msg.arr`, `9 in msg.arr`,
	`msg.hasOwnProperty("num")`, `Object.hasOwn(msg, "arr")`,

	// --- copying, spreading, serialising
	`Object.assign({}, msg.records[0])`, `({...msg.records[0]})`,
	`JSON.stringify(msg.records)`, `JSON.stringify(msg.obj)`,
	`JSON.stringify(msg)`, `JSON.stringify(msg, null, 1)`,
	`JSON.stringify(msg.mixed)`,
	`JSON.parse(JSON.stringify(msg)).obj.a.b.c[2].d`,
	`JSON.stringify(msg, (k, v) => typeof v === "number" ? v + 1 : v)`,
	`new Map(Object.entries(msg.records[0])).get("name")`,
	`Array.from(new Set(msg.arr))`,

	// --- destructuring
	`(() => { const {str, num} = msg; return str + num; })()`,
	`(() => { const {str, ...rest} = msg; return Object.keys(rest).length; })()`,
	`(() => { const [a, b, ...c] = msg.arr; return [a, b, c]; })()`,
	`(() => { const {obj: {a: {b: {c: [first]}}}} = msg; return first; })()`,

	// --- mutation, then read back
	`(() => { msg.str = "changed"; return msg.str; })()`,
	`(() => { msg.fresh = 1; return Object.keys(msg).length; })()`,
	`(() => { delete msg.num; return [msg.num, "num" in msg]; })()`,
	`(() => { delete msg.arr[1]; return [msg.arr.length, msg.arr[1], 1 in msg.arr]; })()`,
	`(() => { msg.arr[1] = 99; return msg.arr; })()`,
	`(() => { msg.arr.push(60); return msg.arr; })()`,
	`(() => { msg.arr.unshift(0); return msg.arr; })()`,
	`(() => { msg.arr.length = 2; return msg.arr; })()`,
	`(() => { msg.records[0].name = "renamed"; return JSON.stringify(msg.records[0]); })()`,
	`(() => { const r = msg.records[1]; delete r.tags; return JSON.stringify(r); })()`,

	// --- integrity, prototypes, proxies
	`(() => { Object.freeze(msg); return [Object.isFrozen(msg), msg.str]; })()`,
	`(() => { Object.seal(msg.obj); return [Object.isSealed(msg.obj), msg.obj.a.b.c[0]]; })()`,
	`(() => { Object.preventExtensions(msg); return msg.str; })()`,
	`Object.getPrototypeOf(msg) === Object.prototype`,
	`Object.getPrototypeOf(msg.arr) === Array.prototype`,
	`new Proxy(msg, {}).str`,
	`(() => { const p = new Proxy(msg, {get: (t, k) => k === "str" ? "trapped" : t[k]}); return [p.str, p.num]; })()`,
	`Reflect.ownKeys(new Proxy(msg.records[0], {})).join(",")`,

	// --- coercion and typeof
	`typeof msg.str`, `typeof msg.num`, `typeof msg.nil`, `typeof msg.obj`,
	`typeof msg.missing`, `msg.num + 1`, `msg.str.length`, "`${msg.num}:${msg.str}`",
	`msg.num > 40`, `msg.nil === null`, `!!msg.emptyObj`, `+msg.num`,
	`String(msg.arr)`, `Number(msg.arr[0])`,

	// --- apply / spread into calls
	`Math.max(...msg.arr)`, `Math.max.apply(null, msg.arr)`,
	`((...a) => a.length)(...msg.arr)`,
	`(function() { return arguments.length; }).apply(null, msg.arr)`,
}

// probeResult runs one expression against one parse of the document and
// returns a printable summary of what happened.
func probeResult(t *testing.T, lazy bool, expr string) string {
	t.Helper()
	rt := New()
	var (
		v   Value
		err error
	)
	if lazy {
		v, err = rt.JSONParseBytesLazy([]byte(lazyProbeDoc))
	} else {
		v, err = rt.JSONParseBytes([]byte(lazyProbeDoc))
	}
	if err != nil {
		return "parse-error|" + err.Error()
	}
	if err := rt.SetProp(rt.Global(), "msg", v); err != nil {
		t.Fatalf("SetProp msg: %v", err)
	}
	// typeof is reported alongside the value so a probe cannot pass by having
	// both sides coerce to the same string from different types.
	src := `(function () {
		try {
			var r = (` + expr + `);
			var s;
			try { s = JSON.stringify(r); } catch (e) { s = "<cycle>"; }
			if (s === undefined) s = String(r);
			return (typeof r) + "|" + s;
		} catch (e) {
			return "throw|" + (e && e.name) + ": " + (e && e.message);
		}
	})()`
	sc, cerr := rt.CompileScript("probe.js", src)
	if cerr != nil {
		t.Fatalf("compile %s: %v", expr, cerr)
	}
	out, rerr := rt.RunScript(sc)
	if rerr != nil {
		return "run-error|" + rerr.Error()
	}
	s, serr := rt.ToString(out)
	if serr != nil {
		t.Fatalf("ToString %s: %v", expr, serr)
	}
	return s
}

func TestLazyParseIsIndistinguishableFromEager(t *testing.T) {
	for _, expr := range lazyProbes {
		expr := expr
		t.Run(strings.NewReplacer("/", "_", " ", "_").Replace(expr), func(t *testing.T) {
			want := probeResult(t, false, expr)
			got := probeResult(t, true, expr)
			if got != want {
				t.Errorf("%s\n  eager: %s\n  lazy:  %s", expr, want, got)
			}
		})
	}
}

// The whole document must round-trip identically, which catches an ordering or
// duplicate-key difference that a per-property probe could miss.
func TestLazyParseRoundTripsWholeDocument(t *testing.T) {
	rt := New()
	eager, err := rt.JSONParseBytes([]byte(lazyProbeDoc))
	if err != nil {
		t.Fatalf("eager: %v", err)
	}
	want, _, err := rt.JSONStringify(eager)
	if err != nil {
		t.Fatalf("stringify eager: %v", err)
	}

	rt2 := New()
	lazy, err := rt2.JSONParseBytesLazy([]byte(lazyProbeDoc))
	if err != nil {
		t.Fatalf("lazy: %v", err)
	}
	got, _, err := rt2.JSONStringify(lazy)
	if err != nil {
		t.Fatalf("stringify lazy: %v", err)
	}
	if got != want {
		t.Errorf("round trip differs\n eager: %s\n lazy:  %s", want, got)
	}
}

// Syntax errors must be reported by the parse, not by whichever read happens
// to reach the damage. That is the reason the lazy parse validates up front,
// and it is the property most easily lost in a later optimisation.
func TestLazyParseRejectsWhatEagerRejects(t *testing.T) {
	bad := []string{
		``, `   `, `{`, `[`, `}`, `]`, `{"a"}`, `{"a":}`, `[1,]`, `{,}`,
		`tru`, `nul`, `01`, `+1`, `.5`, `1.`, `"unterminated`,
		`{"a":1} trailing`, `[1,2] [3]`, `'single'`, `{"a":1,}`,
		`[{"a":[1,2]},]`, `{"a":{"b":}}`, `["ok", nope]`, `{"a" 1}`,
		`{"a":1}}`, `[[[]]`, `"\x41"`, `"\u12"`, `{"a":01}`, `[1 2]`,
		"{\"a\":\"raw\tcontrol\"}",
	}
	for _, src := range bad {
		eagerErr := func() error {
			rt := New()
			_, err := rt.JSONParseBytes([]byte(src))
			return err
		}()
		lazyErr := func() error {
			rt := New()
			_, err := rt.JSONParseBytesLazy([]byte(src))
			return err
		}()
		if (eagerErr == nil) != (lazyErr == nil) {
			t.Errorf("%q: eager err=%v, lazy err=%v", src, eagerErr, lazyErr)
		}
	}
}

// Whatever eager accepts, lazy must accept and read the same way.
func TestLazyParseAcceptsWhatEagerAccepts(t *testing.T) {
	good := []string{
		`null`, `true`, `false`, `0`, `-1.5`, `1e10`, `"plain"`, `""`,
		`"esc \" \\ \n \t é 😀"`, `[]`, `[1,2,3]`, `[[1],[2,[3]]]`,
		`{}`, `{"a":1}`, `{"a":{"b":[1,2,{"c":null}]}}`,
		`  {"leading":"ws"}  `, `{"dup":1,"dup":2}`, `{"":"empty key"}`,
		`[1.7976931348623157e308,5e-324,-0]`, "\r\n\t [1] \r\n\t",
		`[[[[[[[[[[1]]]]]]]]]]`, `{"a":{"a":{"a":{"a":1}}}}`,
	}
	for _, src := range good {
		rt := New()
		eager, err := rt.JSONParseBytes([]byte(src))
		if err != nil {
			t.Fatalf("eager %s: %v", src, err)
		}
		want, _, err := rt.JSONStringify(eager)
		if err != nil {
			t.Fatalf("stringify eager %s: %v", src, err)
		}
		rt2 := New()
		lazy, err := rt2.JSONParseBytesLazy([]byte(src))
		if err != nil {
			t.Fatalf("lazy %s: %v", src, err)
		}
		got, _, err := rt2.JSONStringify(lazy)
		if err != nil {
			t.Fatalf("stringify lazy %s: %v", src, err)
		}
		if got != want {
			t.Errorf("%s: eager %s, lazy %s", src, want, got)
		}
	}
}

// The host byte serializer copies untouched spans straight through. That has
// to hold whatever the script did to the parts it did touch, so the check is
// again differential: whatever comes out must parse to the same document the
// eager path would have produced for the same script.
func TestLazySerializeSplicesUntouchedSpans(t *testing.T) {
	scripts := []string{
		``,                                 // pure pass-through: nothing is ever built
		`msg.num;`,                         // one scalar read
		`msg.records[0].name;`,             // one deep read, the rest untouched
		`msg.str = "changed";`,             // one overwrite beside untouched siblings
		`msg.added = {x: [1, 2]};`,         // a new subtree beside untouched ones
		`delete msg.obj;`,                  // a removal
		`msg.records.push({id: 4});`,       // a spliced array that also grew
		`msg.arr[2] = 99;`,                 // one element replaced, the others spans
		`msg.records[1].amount = 0;`,       // a record built, its siblings not
		`for (const k in msg) { msg[k]; }`, // everything read
	}
	for _, script := range scripts {
		run := func(lazy bool) string {
			rt := New()
			var (
				v   Value
				err error
			)
			if lazy {
				v, err = rt.JSONParseBytesLazy([]byte(lazyProbeDoc))
			} else {
				v, err = rt.JSONParseBytes([]byte(lazyProbeDoc))
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := rt.SetProp(rt.Global(), "msg", v); err != nil {
				t.Fatalf("SetProp: %v", err)
			}
			if script != "" {
				sc, cerr := rt.CompileScript("t.js", script)
				if cerr != nil {
					t.Fatalf("compile %q: %v", script, cerr)
				}
				if _, rerr := rt.RunScript(sc); rerr != nil {
					t.Fatalf("run %q: %v", script, rerr)
				}
			}
			out, ok, serr := rt.JSONStringifyToBytes(v, nil)
			if serr != nil || !ok {
				t.Fatalf("serialize %q: ok=%v err=%v", script, ok, serr)
			}
			// Re-parse and re-serialize canonically. The spliced output keeps the
			// input's formatting, so comparing the bytes directly would test the
			// whitespace of the fixture rather than the content of the message.
			rt2 := New()
			back, perr := rt2.JSONParseBytes(out)
			if perr != nil {
				t.Fatalf("output of %q is not valid JSON: %v\n%s", script, perr, out)
			}
			s, _, err := rt2.JSONStringify(back)
			if err != nil {
				t.Fatalf("re-stringify: %v", err)
			}
			return s
		}
		if got, want := run(true), run(false); got != want {
			t.Errorf("script %q\n  eager: %s\n  lazy:  %s", script, want, got)
		}
	}
}

// Pass-through is the case the splice exists for, so assert the strong form
// directly: the bytes out are the bytes in.
func TestLazySerializePassThroughIsTheInput(t *testing.T) {
	const doc = `{"a":1,"b":[1,2,{"c":"d"}],"e":{"f":null},"g":"h"}`
	rt := New()
	v, err := rt.JSONParseBytesLazy([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, ok, serr := rt.JSONStringifyToBytes(v, nil)
	if serr != nil || !ok {
		t.Fatalf("serialize: ok=%v err=%v", ok, serr)
	}
	if string(out) != doc {
		t.Errorf("pass-through changed the bytes\n in: %s\nout: %s", doc, out)
	}
	if n := rt.LiveObjects(); n == 0 {
		t.Fatal("no live objects at all — the fixture is not measuring anything")
	}
}

// A toJSON on Object.prototype is the one thing that makes splicing wrong: the
// spec calls it for every object, and a value that was never built cannot have
// called anything. The serializer has to notice and stop splicing.
func TestLazySerializeYieldsToPrototypeToJSON(t *testing.T) {
	const doc = `{"a":{"deep":1},"b":2}`
	rt := New()
	v, err := rt.JSONParseBytesLazy([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := rt.SetProp(rt.Global(), "msg", v); err != nil {
		t.Fatalf("SetProp: %v", err)
	}
	sc, cerr := rt.CompileScript("t.js", `Object.prototype.toJSON = function () { return "hijacked"; };`)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	if _, rerr := rt.RunScript(sc); rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	out, _, serr := rt.JSONStringifyToBytes(v, nil)
	if serr != nil {
		t.Fatalf("serialize: %v", serr)
	}
	if string(out) != `"hijacked"` {
		t.Errorf("prototype toJSON was skipped: got %s", out)
	}
}

// The point of the whole exercise: a script that reads nothing should build
// nothing. Live object cells are the honest measure — a lazy parse of a
// document full of records must not allocate one per record until something
// asks for it, and must allocate them once it does.
func TestLazyParseBuildsOnlyWhatIsRead(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"head":1,"records":[`)
	const n = 2000
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"record-%d","amount":%d.5,"note":"padding padding padding"}`, i, i, i)
	}
	b.WriteString(`],"tail":2}`)
	doc := []byte(b.String())

	live := func(parse func(*Runtime) (Value, error), script string) int {
		rt := New()
		rt.SetGCEnabled(false)
		before := rt.LiveObjects()
		v, err := parse(rt)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if err := rt.SetProp(rt.Global(), "msg", v); err != nil {
			t.Fatalf("SetProp: %v", err)
		}
		if script != "" {
			sc, cerr := rt.CompileScript("t.js", script)
			if cerr != nil {
				t.Fatalf("compile: %v", cerr)
			}
			if _, rerr := rt.RunScript(sc); rerr != nil {
				t.Fatalf("run: %v", rerr)
			}
		}
		return rt.LiveObjects() - before
	}

	const traverse = `let s = 0; for (const r of msg.records) s += r.amount;`
	eager := func(rt *Runtime) (Value, error) { return rt.JSONParseBytes(doc) }

	eagerAll := live(eager, "")
	eagerFull := live(eager, traverse)
	lazyIdle := live(func(rt *Runtime) (Value, error) { return rt.JSONParseBytesLazy(doc) }, "")
	lazyHead := live(func(rt *Runtime) (Value, error) { return rt.JSONParseBytesLazy(doc) }, `msg.head;`)
	lazyLen := live(func(rt *Runtime) (Value, error) { return rt.JSONParseBytesLazy(doc) }, `msg.records.length;`)
	lazyAll := live(func(rt *Runtime) (Value, error) { return rt.JSONParseBytesLazy(doc) }, traverse)

	t.Logf("object cells — eager parse %d, eager+traverse %d | lazy idle %d, .head %d, .length %d, +traverse %d",
		eagerAll, eagerFull, lazyIdle, lazyHead, lazyLen, lazyAll)

	// Parsing must build the root and nothing else.
	if lazyIdle > 2 {
		t.Errorf("lazy parse built %d objects for a document it was not asked to read", lazyIdle)
	}
	// Reading a scalar beside the array must not build the array.
	if lazyHead > 2 {
		t.Errorf("reading msg.head built %d objects", lazyHead)
	}
	// Reading the array's length builds the array, but none of its records:
	// this is the case an offset index buys and a per-element materialisation
	// would not.
	if lazyLen > 4 {
		t.Errorf("reading msg.records.length built %d objects, want the array alone", lazyLen)
	}
	// Reading every record has to build every record — laziness must not lose
	// anything, it must only defer it.
	if lazyAll < n {
		t.Errorf("full traversal built %d objects for %d records", lazyAll, n)
	}
	// ...and no more than the eager parse running the same script, which is the
	// claim that laziness costs nothing when the script reads everything.
	if lazyAll > eagerFull {
		t.Errorf("full traversal built %d objects against eager's %d for the same script", lazyAll, eagerFull)
	}
}
