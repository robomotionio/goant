package engine

import (
	"strings"
	"testing"
)

// These are the source forms that used to end the process rather than the
// script. An embedder runs code it did not write — deskbot runs a customer's
// Function node in the robot's own address space — so "the engine panics" and
// "the product is down" are the same sentence. Every case here must come back
// as a throw.

// runHostile evaluates src on a fresh Runtime and returns the error, failing the
// test only if the process did not survive. A Go panic escaping RunString is the
// bug under test, so it is deliberately NOT recovered: a regression shows up as
// a crashed test binary with the offending stack, which is more use than a
// swallowed message.
func runHostile(t *testing.T, src string) error {
	t.Helper()
	rt := New()
	_, err := rt.RunString("hostile.js", src)
	return err
}

func mustSyntaxError(t *testing.T, src, want string) {
	t.Helper()
	err := runHostile(t, src)
	if err == nil {
		t.Fatalf("%s: parsed and ran; wanted a SyntaxError", src)
	}
	if !strings.Contains(err.Error(), "SyntaxError") || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: %v\nwanted a SyntaxError mentioning %q", src, err, want)
	}
}

// TestAwaitOutsideAnAsyncContextIsRejected covers the first of the two panics.
//
// `for await` and `await using` are [+Await] productions: outside an async
// function or module top level, `await` is an identifier and the head does not
// exist. The parser used to accept them anywhere, and the compiler then emitted
// a suspend into a frame with no coroutine behind it — rt.curGen was nil and the
// send on its channel dereferenced it. Reached from
// staging/sm/AsyncGenerators/for-await-of-error.js, and reachable from any
// script at all, since eval("for await (x of [])") is enough.
func TestAwaitOutsideAnAsyncContextIsRejected(t *testing.T) {
	const forAwait = "'for await' is only valid in async functions"
	const awaitUsing = "'await using' is only valid in async functions"
	const inStatic = "'await' is not allowed in a class static block"

	mustSyntaxError(t, `for await (let x of []) {}`, forAwait)
	mustSyntaxError(t, `function f() { for await (let x of []) {} }`, forAwait)
	mustSyntaxError(t, `function* g() { for await (let x of []) {} }`, forAwait)
	mustSyntaxError(t, `() => { for await (let x of []) {} }`, forAwait)
	mustSyntaxError(t, `for (await using x of [null]) {}`, awaitUsing)
	mustSyntaxError(t, `function f() { for (await using x of [null]) {} }`, awaitUsing)
	mustSyntaxError(t, `function f() { await using x = null; }`, awaitUsing)

	// A class static block reserves `await` without being async, so the head is
	// rejected there too — with the message that says why.
	mustSyntaxError(t, `class C { static { for await (let x of []) {} } }`, inStatic)
	mustSyntaxError(t, `class C { static { await using x = null; } }`, inStatic)
	mustSyntaxError(t, `class C { static { for (await using x of [null]) {} } }`, inStatic)

	// Through eval, which is how a script reaches the parser at run time. The
	// eval body is a Script, so it is [~Await] however async the caller is.
	mustSyntaxError(t, `eval("for await (let x of []) {}")`, forAwait)
	mustSyntaxError(t, `function f() { eval("for await (let x of []) {}") } f()`, forAwait)
	mustSyntaxError(t, `new Function("for await (let x of []) {}")`, forAwait)

	// Direct eval in an async function is the interesting one: the CALLER is
	// async, but eval code is a Script and so [~Await] regardless. An async
	// function turns the throw into a rejected promise, so catch it in the body
	// rather than looking for it on the completion.
	rt := New()
	if _, err := rt.RunString("asyncEval.js", `
		var msg = "";
		async function f() { try { eval("for await (let x of []) {}") } catch (e) { msg = String(e) } }
		f();
	`); err != nil {
		t.Fatalf("direct eval in an async function: %v", err)
	}
	got, err := rt.RunString("asyncEval2.js", `msg`)
	if err != nil {
		t.Fatal(err)
	}
	if m := rt.strGo(got); !strings.Contains(m, "SyntaxError") || !strings.Contains(m, forAwait) {
		t.Fatalf("direct eval in an async function threw %q, wanted a SyntaxError mentioning %q", m, forAwait)
	}

	// The extra `await` the same test262 file checks: `for await await` is a
	// SyntaxError for a duller reason, and must still be one.
	if err := runHostile(t, `eval("async function f() { for await await (let x of []) {} }")`); err == nil ||
		!strings.Contains(err.Error(), "SyntaxError") {
		t.Fatalf("for await await: %v, wanted a SyntaxError", err)
	}
}

// TestAwaitInsideAnAsyncContextStillParses is the other half: the gate must not
// have closed on the legal forms.
func TestAwaitInsideAnAsyncContextStillParses(t *testing.T) {
	rt := New()
	v, err := rt.RunString("ok.js", `
		var log = [];
		async function f() { let s = 0; for await (const x of [1,2,3]) s += x; return s; }
		async function* g() { for await (const x of [4,5]) yield x; }
		async function h() {
			await using r = { [Symbol.asyncDispose]() { log.push("disposed"); } };
			log.push("body");
		}
		const arrow = async () => { for await (const x of [6]) log.push("arrow" + x); };
		(async () => {
			log.push("sum" + await f());
			for await (const v of g()) log.push("gen" + v);
			await h();
			await arrow();
		})();
		log.join(",")
	`)
	if err != nil {
		t.Fatalf("legal for-await forms: %v", err)
	}
	_ = v
	// The completion value is read before the async body runs, so check the log
	// afterwards instead.
	got, err := rt.RunString("ok2.js", `log.join(",")`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "sum6,gen4,gen5,body,disposed,arrow6"; rt.strGo(got) != want {
		t.Fatalf("for-await log = %q want %q", rt.strGo(got), want)
	}
}

// TestAwaitWithNoCoroutineThrowsRatherThanCrashing is the floor under the parser
// fix. requireAwaitContext is supposed to make rt.curGen == nil unreachable at a
// suspend, but "supposed to" is what the previous version of this file would
// have said too. If another path ever emits a suspend into a synchronous frame,
// the embedder gets a catchable throw and not a signal.
func TestAwaitWithNoCoroutineThrowsRatherThanCrashing(t *testing.T) {
	rt := New()
	if rt.curGen != nil {
		t.Fatal("a fresh Runtime is already inside a coroutine")
	}
	resumed, inject := rt.suspend(mknum(1), true)
	if inject == nil || inject.kind != genThrow {
		t.Fatalf("suspend with no coroutine: resumed=%v inject=%v, wanted a throw", resumed, inject)
	}
	if !inject.val.IsObjectType() {
		t.Fatalf("suspend with no coroutine threw %v, wanted an Error object", inject.val)
	}
}

// TestBigIntWrapDoesNotAllocateTheWidthTheScriptNamed covers the second panic.
//
// The `bits` argument of BigInt.asIntN/asUintN goes through ToIndex, so a script
// may name 2^53-1 of them; building 2^bits to take the modulus asked math/big
// for a petabyte and Go answered "makeslice: len out of range", which no recover
// catches. From staging/sm/BigInt/large-bit-length.js.
func TestBigIntWrapDoesNotAllocateTheWidthTheScriptNamed(t *testing.T) {
	rt := New()
	// Every one of these must return in negligible time and memory. The
	// asUintN-of-a-negative cases genuinely need a result that wide, so those
	// are the only ones that may fail — and they must fail as a RangeError.
	v, err := rt.RunString("bits.js", `
		const U = 2**32-1, out = [];
		for (const bits of [U-1, U, U+1, Number.MAX_SAFE_INTEGER]) {
			for (const [name, f] of [
				["asIntN(1n)",  () => BigInt.asIntN(bits, 1n)],
				["asIntN(0n)",  () => BigInt.asIntN(bits, 0n)],
				["asIntN(-1n)", () => BigInt.asIntN(bits, -1n)],
				["asUintN(1n)", () => BigInt.asUintN(bits, 1n)],
				["asUintN(0n)", () => BigInt.asUintN(bits, 0n)],
				["asUintN(-1n)",() => BigInt.asUintN(bits, -1n)],
			]) {
				try { out.push(name + "=" + f()); }
				catch (e) {
					if (!(e instanceof RangeError)) throw e;
					out.push(name + "=RangeError");
				}
			}
		}
		out.join(" ")
	`)
	if err != nil {
		t.Fatalf("large bit lengths: %v", err)
	}
	const want = "asIntN(1n)=1 asIntN(0n)=0 asIntN(-1n)=-1 asUintN(1n)=1 asUintN(0n)=0 asUintN(-1n)=RangeError"
	got := rt.strGo(v)
	for _, chunk := range strings.SplitAfter(got, "asUintN(-1n)=RangeError") {
		if chunk = strings.TrimSpace(chunk); chunk != "" && chunk != want {
			t.Fatalf("large bit lengths gave %q\nwanted each of the four widths to give %q", got, want)
		}
	}
}

// TestBigIntWrapIsUnchangedAtOrdinaryWidths guards the early return that makes
// the fix cheap: it must only skip the modulus when the value is already inside
// the range, never when it changes the answer.
func TestBigIntWrapIsUnchangedAtOrdinaryWidths(t *testing.T) {
	runStr(t, `[
		BigInt.asIntN(64, 2n**63n),   // -9223372036854775808
		BigInt.asIntN(65, 2n**64n),   // -18446744073709551616
		BigInt.asIntN(8, 255n),       // -1
		BigInt.asIntN(8, 256n),       // 0
		BigInt.asIntN(8, 127n),       // 127
		BigInt.asIntN(8, 128n),       // -128
		BigInt.asIntN(8, -128n),      // -128
		BigInt.asIntN(8, -129n),      // 127
		BigInt.asIntN(9, 255n),       // 255  (the width where 255 starts fitting)
		BigInt.asIntN(1, 1n),         // -1
		BigInt.asIntN(1, -1n),        // -1
		BigInt.asIntN(0, 5n),         // 0
		BigInt.asIntN(100, -1n),      // -1
		BigInt.asUintN(8, -1n),       // 255
		BigInt.asUintN(8, 255n),      // 255
		BigInt.asUintN(8, 256n),      // 0
		BigInt.asUintN(9, 256n),      // 256
		BigInt.asUintN(64, -1n),      // 18446744073709551615
		BigInt.asUintN(0, 5n),        // 0
		BigInt.asUintN(1, 3n)         // 1
	].join(",")`,
		"-9223372036854775808,-18446744073709551616,-1,0,127,-128,-128,127,255,-1,-1,0,-1,"+
			"255,255,0,256,18446744073709551615,0,1")
}

// TestAClaimedLengthDoesNotSizeAnAllocation is the same defect as the BigInt
// one, found by probing the rest of the engine for it: `{length: 2**53-1}` is an
// array-like that holds NOTHING, and three builtins sized a buffer from that
// number before reading a single element. Go answered "makeslice: cap out of
// range" and the process was gone.
//
// Each case here throws out of the very first element read, so what is under
// test is the allocation that happens before the loop and not the loop — which
// for a length of 2^53 would legitimately run for a week.
func TestAClaimedLengthDoesNotSizeAnAllocation(t *testing.T) {
	rt := New()
	v, err := rt.RunString("claimed.js", `
		const M = Number.MAX_SAFE_INTEGER, out = [];
		function stops(f) {
			try { f(); return "no throw"; } catch (e) { return e.message; }
		}
		// join: the buffer was sized 4 bytes per CLAIMED element.
		out.push(stops(() => Array.prototype.join.call(
			{length: M, get 0() { throw new Error("read 0"); }}, ",")));
		// sort: capacity for every claimed element, of which none is present.
		out.push(stops(() => Array.prototype.sort.call(new Proxy({}, {
			has() { throw new Error("has 0"); },
			get(t, k) { return k === "length" ? M : undefined; },
		}))));
		// CreateListFromArrayLike: an argument list of 2^53 values.
		out.push(stops(() => Reflect.apply(Math.max, null, {length: M})));
		out.push(stops(() => Math.max.apply(null, {length: 2**31})));
		out.join(" | ")
	`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	const want = "read 0 | has 0 | Array-like is too long to build a list from | Array-like is too long to build a list from"
	if got := rt.strGo(v); got != want {
		t.Fatalf("claimed lengths gave %q\nwant %q", got, want)
	}
}

// TestOrdinaryArgumentListsStillWork — the argument-list cap must sit far above
// anything a program means to do, and the two preallocation hints must not have
// changed a single answer.
func TestOrdinaryArgumentListsStillWork(t *testing.T) {
	runStr(t, `[
		Math.max(...[1,9,3]),
		Math.max.apply(null, [4,2,8]),
		Reflect.apply(Math.max, null, [5,1,7]),
		Math.max.apply(null, Array.from({length: 70000}, (_, i) => i)),
		[3,1,2].sort().join("-"),
		[1,[2,[3]]].flat(2).join(","),
		Array.from({length: 5000}, (_, i) => i).join(",").length,
		[].join(","),
		[,,].join("-")
	].join(" ")`, "9 8 7 69999 1-2-3 1,2,3 23889  -")
}

// TestConsoleLogNamesTheValueNotTheType — inspect's default arm printed the type
// name, so console.log(1n) said "bigint". Not a crash, but it is the same class
// of thing: a host-facing surface that is wrong only for the types nobody put in
// the switch.
func TestConsoleLogNamesTheValueNotTheType(t *testing.T) {
	rt := New()
	for src, want := range map[string]string{
		`1n`:                "1n",
		`-42n`:              "-42n",
		`2n**64n`:           "18446744073709551616n",
		`Symbol("s")`:       "Symbol(s)",
		`Symbol.iterator`:   "Symbol(Symbol.iterator)",
		`Symbol()`:          "Symbol()",
		`Symbol(undefined)`: "Symbol()",
		`Symbol("")`:        "Symbol()",
		`[1n, Symbol("t")]`: "[ 1n, Symbol(t) ]",
		`({a: 1n})`:         "{ a: 1n }",
	} {
		v, err := rt.RunString("inspect.js", src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got := rt.inspect(v, false); got != want {
			t.Errorf("console.log(%s) printed %q want %q", src, got, want)
		}
	}
}

// And it reads the symbol's own description, not the .description accessor, so a
// script cannot change what the host's log says about its values — or, by
// leaving a getter there, be re-entered by the act of printing one.
func TestConsoleLogDoesNotAskTheScriptWhatASymbolIsCalled(t *testing.T) {
	rt := New()
	v, err := rt.RunString("tamper.js", `
		let asked = false;
		Object.defineProperty(Symbol.prototype, "description", {
			get() { asked = true; return "lies"; },
			configurable: true,
		});
		Symbol("truth");
	`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := rt.inspect(v, false); got != "Symbol(truth)" {
		t.Errorf("inspect printed %q; a script redefined description and the host believed it", got)
	}
	asked, err := rt.RunString("asked.js", `asked`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if asked.Bool() {
		t.Error("printing a symbol called back into the script")
	}
}
