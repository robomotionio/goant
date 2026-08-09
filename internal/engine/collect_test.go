package engine

import "testing"

// These drive the collector explicitly rather than waiting for a threshold, so
// they pin its two halves: that a trace reaches everything a script can still
// see, and that a sweep gives back everything it cannot.

// TestCollectReclaimsGarbage is the whole point of the file: a script that
// allocates and drops must end with a heap near what it is still holding, not
// near what it allocated.
func TestCollectReclaimsGarbage(t *testing.T) {
	rt := New()
	if _, err := rt.RunString("t.js", `
		var keep = [];
		for (var i = 0; i < 20000; i++) {
			var dead = {a: i, b: "s" + i, c: [i, i + 1]};
			if (i % 1000 === 0) keep.push(dead);
		}
	`); err != nil {
		t.Fatalf("run: %v", err)
	}
	before := rt.LiveObjects()
	rt.Collect()
	after := rt.LiveObjects()

	if after >= before {
		t.Errorf("collection freed nothing: %d live before, %d after", before, after)
	}
	// 20000 iterations allocate at least 40000 objects; twenty survive in keep.
	// Anything close to `before` means the sweep is not reaching the garbage.
	if after > before/2 {
		t.Errorf("collection freed less than half: %d -> %d", before, after)
	}
}

// TestCollectKeepsReachable is the other half, and the one that matters: what
// the script can still name has to be intact and usable afterwards.
func TestCollectKeepsReachable(t *testing.T) {
	rt := New()
	if _, err := rt.RunString("t.js", `
		var obj = {n: 1, s: "hello", nested: {deep: [1, 2, 3]}};
		var fn = function (x) { return x + obj.n; };
		var m = new Map([["k", obj]]);
		var sym = Symbol("tag");
		obj[sym] = "bysymbol";
		var proto = {greet: function () { return "hi " + this.s; }};
		Object.setPrototypeOf(obj, proto);
		var arr = [];
		for (var i = 0; i < 5000; i++) arr.push({i: i});
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	rt.Collect()

	v, err := rt.RunString("t2.js", `
		[obj.n, obj.s, obj.nested.deep[2], fn(41), m.get("k").s,
		 obj[sym], obj.greet(), arr.length, arr[4999].i].join(",")
	`)
	if err != nil {
		t.Fatalf("after collect: %v", err)
	}
	got, e := rt.toStringValue(v)
	if e != nil {
		t.Fatalf("%v", e)
	}
	const want = "1,hello,3,42,hello,bysymbol,hi hello,5000,4999"
	if s := rt.strGo(got); s != want {
		t.Errorf("reachable state changed across a collection\n got: %q\nwant: %q", s, want)
	}
}

// TestCollectKeepsComputedStrings covers strings the program built rather than
// wrote, which are the ones with no other reference.
//
// A string Value carries (handle << 2) | tag, not a bare handle, so a trace that
// marks v.handle() marks a cell that does not exist and every computed string is
// swept. Literals and property names hide it: they are interned, and the intern
// table is rooted by handle. The symptom is a live string turning into an
// unrelated one allocated later.
func TestCollectKeepsComputedStrings(t *testing.T) {
	rt := New()
	if _, err := rt.RunString("t.js", `
		var kept = [];
		for (var i = 0; i < 2000; i++) kept.push("computed-" + i + "-" + (i * 7));
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	rt.Collect()
	// Allocate over whatever the sweep released, so a freed cell now holds
	// something else and a stale handle reads it back.
	if _, err := rt.RunString("t2.js", `for (var i = 0; i < 4000; i++) ("filler-" + i);`); err != nil {
		t.Fatalf("refill: %v", err)
	}

	v, err := rt.RunString("t3.js", `
		var bad = 0;
		for (var i = 0; i < kept.length; i++) {
			if (kept[i] !== "computed-" + i + "-" + (i * 7)) bad++;
		}
		bad + "/" + kept.length;
	`)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, _ := rt.toStringValue(v)
	if s := rt.strGo(got); s != "0/2000" {
		t.Errorf("computed strings did not survive a collection: %s corrupted", s)
	}
}

// TestCollectKeepsNativeClosureState covers the state a built-in written as a Go
// closure holds on an object's behalf — an iterator helper's source iterator,
// here. A func value is opaque, so that state is reachable only through
// holdCaptures; without it the helper resumes against freed cells.
func TestCollectKeepsNativeClosureState(t *testing.T) {
	rt := New()
	if _, err := rt.RunString("t.js", `
		// The source iterators exist only inside the helpers' Go closures: the
		// arrays are literals here and nothing else names the iterators.
		var helpers = [
			[1, 2, 3, 4, 5].values().map(function (x) { return x * 2; }),
			[1, 2, 3, 4, 5].values().filter(function (x) { return x % 2 === 1; }),
			Iterator.zip([[1, 2, 3].values(), [4, 5, 6].values()]),
			Iterator.concat([7, 8].values(), [9].values())
		];
		helpers.forEach(function (h) { h.next(); });
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	rt.Collect()
	if _, err := rt.RunString("t2.js", `for (var i = 0; i < 5000; i++) ({junk: i});`); err != nil {
		t.Fatalf("refill: %v", err)
	}

	v, err := rt.RunString("t3.js", `
		helpers.map(function (h) { return JSON.stringify(h.next().value); }).join("|")
	`)
	if err != nil {
		t.Fatalf("resuming a helper after a collection: %v", err)
	}
	got, _ := rt.toStringValue(v)
	const want = `4|3|[2,5]|8`
	if s := rt.strGo(got); s != want {
		t.Errorf("iterator helper state lost across a collection\n got: %s\nwant: %s", s, want)
	}
}

// TestCollectKeepsSuspendedAsyncFrame covers an async function parked at an
// await. It has no generator object, so between suspension and resumption its
// whole frame is referenced only by the Go closure driving it.
func TestCollectKeepsSuspendedAsyncFrame(t *testing.T) {
	rt := New()
	if _, err := rt.RunString("t.js", `
		var log = [];
		var release;
		var gate = new Promise(function (r) { release = r; });
		(async function () {
			// Computed, so it is not interned: an interned string is rooted by
			// the intern table and would survive whether the frame did or not.
			var local = "held-" + "across-" + "the-await";
			var n = await gate;
			log.push(local + ":" + n);
		})();
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	rt.Collect()
	// Churn through nested frames as well as the heap: a stale entry left in the
	// interpreter's published frames would otherwise keep the suspended
	// coroutine's values marked by accident.
	if _, err := rt.RunString("t2.js", `
		var filler = [];
		function deep(n) {
			var s = "junk-" + n + "-" + (n * 3);
			filler.push(s);
			return n > 0 ? deep(n - 1) : s;
		}
		for (var i = 0; i < 800; i++) deep(24);
	`); err != nil {
		t.Fatalf("refill: %v", err)
	}

	// Releasing queues the resumption; the reader runs after the queue drains.
	if _, err := rt.RunString("t3.js", `release(41);`); err != nil {
		t.Fatalf("resuming after a collection: %v", err)
	}
	v, err := rt.RunString("t4.js", `log.join(",")`)
	if err != nil {
		t.Fatalf("reading the result: %v", err)
	}
	got, _ := rt.toStringValue(v)
	if s := rt.strGo(got); s != "held-across-the-await:41" {
		t.Errorf("suspended async frame lost across a collection: %q", s)
	}
}

// TestCollectKeepsUnreachedConstants covers a running function's constant pool.
// A script or an eval body is not reachable from any object — nothing called it
// — so unless the frame publishes the code it is running, its constants are
// unmarked. Most are interned strings and survive anyway; a tagged template's
// frozen strings array does not, which is what this uses.
//
// It also exercises the automatic trigger rather than an explicit Collect: the
// cycle happens at a loop back edge in the middle of this very script.
func TestCollectKeepsUnreachedConstants(t *testing.T) {
	rt := New()
	v, err := rt.RunString("t.js", `
		function tag(strings, sub) { return strings[0] + sub + strings.raw[1]; }
		var churn = [];
		for (var i = 0; i < 80000; i++) churn.push({n: i});
		churn = null;
		for (var j = 0; j < 80000; j++) ({m: j});
		// The strings array behind this was built at compile time and has sat in
		// the constant pool, unreferenced, across both loops.
		tag`+"`head-${42}-tail`"+`;
	`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := rt.toStringValue(v)
	if s := rt.strGo(got); s != "head-42-tail" {
		t.Errorf("constants lost across a collection: %q", s)
	}
}

// TestCollectAcrossRealms covers realms sharing one set of pools. A handle names
// the same cell in both, so a collection driven from either has to trace both —
// otherwise the one that collects frees the other's heap.
func TestCollectAcrossRealms(t *testing.T) {
	rt := New()
	other := rt.NewRealm()
	if _, err := other.RunString("other.js", `
		var kept = [];
		for (var i = 0; i < 1000; i++) kept.push({tag: "sibling-" + i});
	`); err != nil {
		t.Fatalf("sibling setup: %v", err)
	}
	// Collect from the first realm, which knows nothing about the second.
	rt.Collect()
	if _, err := rt.RunString("t.js", `for (var i = 0; i < 5000; i++) ({junk: "j" + i});`); err != nil {
		t.Fatalf("refill: %v", err)
	}

	v, err := other.RunString("other2.js", `
		var bad = 0;
		for (var i = 0; i < kept.length; i++) if (kept[i].tag !== "sibling-" + i) bad++;
		bad + "/" + kept.length;
	`)
	if err != nil {
		t.Fatalf("sibling after collect: %v", err)
	}
	got, _ := other.toStringValue(v)
	if s := other.strGo(got); s != "0/1000" {
		t.Errorf("a sibling realm's heap did not survive: %s corrupted", s)
	}
}

// TestCollectDropsDeadIteratorState covers the object-keyed side tables. They
// hold state on behalf of an object and are keyed by it, so treating them as
// ordinary roots would make every iterator ever created immortal — which for a
// loop over an array is most of the heap.
func TestCollectDropsDeadIteratorState(t *testing.T) {
	rt := New()
	if _, err := rt.RunString("t.js", `
		var a = [1, 2, 3];
		var sum = 0;
		for (var r = 0; r < 500; r++) { for (var v of a) sum += v; }
	`); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := len(rt.agent.arrIterStates); n < 100 {
		t.Fatalf("expected the side table to have accumulated entries, got %d", n)
	}
	rt.Collect()
	if n := len(rt.agent.arrIterStates); n > 8 {
		t.Errorf("dead iterator state survived: %d entries left", n)
	}
}

// TestCollectIsRepeatable checks the second cycle behaves like the first: mark
// bits are reused between cycles, and a stale bit would keep garbage alive (or,
// worse, a cleared one would free something live).
func TestCollectIsRepeatable(t *testing.T) {
	rt := New()
	run := func(src string) {
		t.Helper()
		if _, err := rt.RunString("t.js", src); err != nil {
			t.Fatalf("run %q: %v", src, err)
		}
	}
	run(`var live = {tag: "one"}; for (var i = 0; i < 5000; i++) ({junk: i});`)
	rt.Collect()
	first := rt.LiveObjects()
	run(`for (var i = 0; i < 5000; i++) ({junk: i}); live.tag;`)
	rt.Collect()
	second := rt.LiveObjects()

	if second > first+64 {
		t.Errorf("heap grew across equivalent cycles: %d then %d", first, second)
	}
	v, err := rt.RunString("t2.js", `live.tag`)
	if err != nil {
		t.Fatalf("after two collections: %v", err)
	}
	got, _ := rt.toStringValue(v)
	if s := rt.strGo(got); s != "one" {
		t.Errorf("live object lost across two collections: %q", s)
	}
	if rt.GCCycles() < 2 {
		t.Errorf("GCCycles = %d, want at least the two forced cycles", rt.GCCycles())
	}
}

// TestCollectNotInsideNative guards the rule the whole scheme rests on: a
// built-in holds values in Go locals that nothing publishes, so a collection
// underneath one would free them.
func TestCollectNotInsideNative(t *testing.T) {
	rt := New()
	rt.nativeDepth = 1
	before := rt.GCCycles()
	rt.Collect()
	rt.maybeCollect()
	if rt.GCCycles() != before {
		t.Error("collected while a native was on the stack")
	}
	rt.nativeDepth = 0
	rt.Collect()
	if rt.GCCycles() != before+1 {
		t.Error("did not collect once the native returned")
	}
}
