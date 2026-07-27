package engine

import "testing"

// TestStoreTransitionCacheGuards drives the cases a store-transition entry must
// refuse. Two of them fail without their guard and are what this file is for:
// a receiver that stopped being extensible (extensibility is a flag on the
// object, so the shape still matches), and one whose prototype differs from the
// one the fill concluded against (a shape does not record the prototype).
//
// The rest pass either way, because a second mechanism already covers them —
// a store that resolves to an inherited setter or a non-writable inherited
// property never reaches the fill at all, since the first is not a layout change
// and the second is not a successful store. They are kept as the statement of
// what the cache must never do, not as proof that one line is load-bearing.
func TestStoreTransitionCacheGuards(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		// The same store site, warmed on extensible objects, then handed one
		// that is not. Extensibility is a flag on the object, not on its shape,
		// so the shape still matches and only the hit-path test can refuse.
		{"preventExtensions", `
			function store(o) { o.x = 1; return o; }
			for (var i = 0; i < 50; i++) store({});
			var closed = Object.preventExtensions({});
			store(closed);
			String(Object.getOwnPropertyNames(closed).length) + "," + String(closed.x);
		`, "0,undefined"},

		// A setter appears on a prototype that nothing else ever walked, so the
		// only thing that can have flagged it — and therefore the only thing that
		// retires this entry when it changes — is the fill's own chain walk.
		{"setterOnUnwalkedProto", `
			var base = {};
			var seen = [];
			function store(o) { o.p = 1; return o; }
			for (var i = 0; i < 50; i++) store(Object.create(base));
			Object.defineProperty(base, "p", {
				set: function (v) { seen.push(v); }, get: function () { return "S"; },
				configurable: true
			});
			var o = store(Object.create(base));
			seen.join("|") + "," + String(o.p) + "," + o.hasOwnProperty("p");
		`, "1,S,false"},

		{"protoSetterAppearsLater", `
			var log = [];
			function Base() {}
			function make() { var o = new Base(); o.v = 7; return o; }
			for (var i = 0; i < 50; i++) make();            // warm the site
			Object.defineProperty(Base.prototype, "v", {
				set: function (n) { log.push(n); }, get: function () { return -1; },
				configurable: true
			});
			var after = make();
			log.join("|") + "," + String(after.v) + "," + after.hasOwnProperty("v");
		`, "7,-1,false"},

		{"sameShapeDifferentProto", `
			var withSetter = {};
			var seen = [];
			Object.defineProperty(withSetter, "k", {
				set: function (n) { seen.push(n); }, get: function () { return "S"; },
				configurable: true
			});
			function store(o) { o.k = 1; return o; }
			for (var i = 0; i < 50; i++) store(Object.create(null));  // warm on a bare proto
			var viaSetter = store(Object.create(withSetter));
			seen.join("|") + "," + String(viaSetter.k) + "," + viaSetter.hasOwnProperty("k");
		`, "1,S,false"},

		{"frozenPrototypeProperty", `
			function Base() {}
			Object.defineProperty(Base.prototype, "ro", {value: 5, writable: false});
			function make() { var o = new Base(); o.ro = 9; return o; }
			var results = [];
			for (var i = 0; i < 50; i++) results.push(make().ro);
			String(results[0]) + "," + String(results[49]);
		`, "5,5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := New()
			v, err := rt.RunString("t.js", tc.src)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			got, _ := rt.toStringValue(v)
			if s := rt.strGo(got); s != tc.want {
				t.Errorf("got %q, want %q", s, tc.want)
			}
		})
	}
}
