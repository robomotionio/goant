package engine

import "testing"

// Inline caches are invisible when they work and produce wrong *values* when
// they don't, which no timing test would catch.
//
// Every case follows the same shape, and it is load-bearing: a helper function
// holds the access, the helper is called in a loop until its site has cached,
// the object is then mutated, and the helper is called AGAIN. Reading the
// property directly after the mutation would compile to a different bytecode
// site with an empty cache and prove nothing — an earlier version of this test
// made exactly that mistake and passed with the invalidation guard deleted.
func TestPropCacheInvalidation(t *testing.T) {
	// warm is prepended to every case: it drives `read` to a cached state.
	const warm = `function warm(f, o) { for (var i = 0; i < 50; i++) f(o); }`

	cases := []struct{ name, src, want string }{
		{
			name: "monomorphic",
			src: `function read(o) { return o.a + o.b; }
			      var o = {a: 1, b: 2};
			      warm(read, o);
			      String(read(o))`,
			want: "3",
		},
		{
			// delete shifts every later slot down by one, in place when the shape is
			// private. Only the epoch guard catches this: the shape pointer does not
			// change.
			name: "delete shifts slots",
			src: `function read(o) { return o.c; }
			      var o = {a: 1, b: 2, c: 3};
			      warm(read, o);
			      delete o.a;
			      String(read(o))`,
			want: "3",
		},
		{
			name: "data becomes accessor",
			src: `function read(o) { return o.x; }
			      var o = {x: 1};
			      warm(read, o);
			      Object.defineProperty(o, "x", {get: function () { return 99; }});
			      String(read(o))`,
			want: "99",
		},
		{
			name: "freeze blocks cached write",
			src: `function write(o) { o.x = 7; }
			      var o = {x: 1};
			      warm(write, o);
			      Object.freeze(o);
			      o.x = 12345;
			      String(o.x)`,
			want: "7",
		},
		{
			name: "non-writable blocks cached write",
			src: `function write(o) { o.x = 7; }
			      var o = {x: 1};
			      warm(write, o);
			      Object.defineProperty(o, "x", {writable: false});
			      write(o);
			      String(o.x)`,
			want: "7",
		},
		{
			// Two shapes through one site: b.y sits at slot 0, a.y at slot 1.
			name: "polymorphic site",
			src: `function read(o) { return o.y; }
			      var a = {x: 0, y: 11}, b = {y: 22};
			      warm(read, a);
			      String(read(a)) + "," + String(read(b))`,
			want: "11,22",
		},
		{
			// The site caches nothing while the hit is on the prototype; once an own
			// property shadows it, the same site must see the own value.
			name: "own shadows proto",
			src: `function read(o) { return o.v; }
			      function C() {}
			      C.prototype.v = "proto";
			      var o = new C();
			      warm(read, o);
			      var before = read(o);
			      o.v = "own";
			      before + "," + read(o)`,
			want: "proto,own",
		},
		{
			// setPrototypeOf leaves the receiver's shape alone, so the own slot the
			// site cached is still the right answer.
			name: "setPrototypeOf keeps own slot",
			src: `function read(o) { return o.a; }
			      var o = {a: 5};
			      warm(read, o);
			      Object.setPrototypeOf(o, {a: 999});
			      String(read(o))`,
			want: "5",
		},
		{
			// A Proxy reaching a site warmed by an ordinary object must still trap.
			name: "proxy through warmed site",
			src: `function read(o) { return o.v; }
			      warm(read, {v: 1});
			      String(read(new Proxy({v: 1}, {get: function () { return 7; }})))`,
			want: "7",
		},
		{
			// An array reaching a site warmed by {length:3} must report its own
			// exotic length, not the cached slot.
			name: "array through warmed site",
			src: `function read(o) { return o.length; }
			      warm(read, {length: 3});
			      String(read([1, 2, 3, 4]))`,
			want: "4",
		},
		{
			// Mapped arguments alias their formals, so the slot is not the live value.
			name: "mapped arguments alias",
			src: `function read(o) { return o[0]; }
			      function f(a) {
			        warm(read, arguments);
			        a = 100;
			        return read(arguments);
			      }
			      String(f(1))`,
			want: "100",
		},
		{
			// A getter that installs the property on the receiver: the site sees a
			// proto accessor first and an own data slot afterwards.
			name: "getter installs own property",
			src: `function read(o) { return o.lazy; }
			      var proto = {};
			      Object.defineProperty(proto, "lazy", {
			        get: function () { this.lazy = 42; return 42; }, configurable: true});
			      var o = Object.create(proto);
			      warm(read, o);
			      String(read(o))`,
			want: "42",
		},
		{
			// Adding a property after caching appends a slot and must leave the
			// cached lower slot readable.
			name: "append keeps earlier slots",
			src: `function read(o) { return o.a; }
			      var o = {a: 1};
			      warm(read, o);
			      o.b = 2; o.c = 3;
			      String(read(o)) + "," + String(o.c)`,
			want: "1,3",
		},
		{
			// Warming a site directly ON an accessor. The slot behind an accessor
			// holds undefined, so caching it would silently replace every call to
			// the getter with undefined. Distinct from "data becomes accessor",
			// which never warms on an accessor at all.
			name: "accessor is never cached",
			src: `function read(o) { return o.g; }
			      var o = {};
			      Object.defineProperty(o, "g", {get: function () { return 5; }});
			      warm(read, o);
			      String(read(o))`,
			want: "5",
		},
		{
			// The store equivalent: a cached slot write would bypass the setter.
			name: "setter is never cached",
			src: `var log = 0;
			      function write(o) { o.s = 1; }
			      var o = {};
			      Object.defineProperty(o, "s", {set: function (v) { log++; }});
			      for (var i = 0; i < 50; i++) write(o);
			      String(log)`,
			want: "50",
		},
		{
			// An accessor inherited from the prototype, warmed through the same site.
			name: "proto accessor is never cached",
			src: `function read(o) { return o.p; }
			      var proto = {};
			      Object.defineProperty(proto, "p", {get: function () { return "g"; }});
			      var o = Object.create(proto);
			      warm(read, o);
			      read(o)`,
			want: "g",
		},
		{
			// Warming a store site on an already non-writable property. In sloppy
			// mode the store silently fails and must keep failing.
			name: "non-writable stays non-writable",
			src: `function write(o) { o.x = 7; }
			      var o = {};
			      Object.defineProperty(o, "x", {value: 1, writable: false, configurable: true});
			      for (var i = 0; i < 50; i++) write(o);
			      String(o.x)`,
			want: "1",
		},
		{
			// A cached store must land in the slot the reader reads.
			name: "cached write is visible",
			src: `function write(o, v) { o.x = v; }
			      function read(o) { return o.x; }
			      var o = {x: 0, y: 9};
			      for (var i = 0; i < 50; i++) write(o, i);
			      String(read(o)) + "," + String(o.y)`,
			want: "49,9",
		},

		// ---- properties reached through the prototype chain ----
		//
		// These sites cache a holder somewhere up the chain, so the receiver's
		// shape is doing double duty: it identifies the layout AND stands for
		// "this object has no own property of that name". Everything that can
		// falsify the second reading has to be caught.

		{
			// The one the Octane Splay benchmark found. The delete makes o's
			// shape private, and a private shape is appended to IN PLACE — so
			// the shadowing assignment leaves the shape pointer untouched and
			// only the epoch can tell the site that o now answers for itself.
			// splay.js does exactly this: SplayTree.prototype.root_ = null,
			// then this.root_ = new Node(...).
			name: "own shadows proto on a private shape",
			src: `function read(o) { return o.v; }
			      function C() {}
			      C.prototype.v = "proto";
			      var o = new C();
			      o.pad1 = 1; o.pad2 = 2;
			      delete o.pad2;
			      warm(read, o);
			      var before = read(o);
			      o.v = "own";
			      before + "," + read(o)`,
			want: "proto,own",
		},
		{
			// A method three prototypes up: the walk the cache replaces is the
			// whole chain, so the answer has to survive the chain being long.
			name: "method three levels up",
			src: `function call(o) { return o.m(); }
			      function A() {} A.prototype.m = function () { return "A"; };
			      function B() {} B.prototype = Object.create(A.prototype);
			      function C() {} C.prototype = Object.create(B.prototype);
			      var o = new C();
			      warm(call, o);
			      call(o)`,
			want: "A",
		},
		{
			// Installing the name on a NEARER prototype after the site cached a
			// farther one. Nothing about the receiver changes, so only flagging
			// the objects the walk passed through catches it.
			name: "nearer proto shadows cached holder",
			src: `function call(o) { return o.m(); }
			      function A() {} A.prototype.m = function () { return "A"; };
			      function B() {} B.prototype = Object.create(A.prototype);
			      var o = new B();
			      warm(call, o);
			      var before = call(o);
			      B.prototype.m = function () { return "B"; };
			      before + "," + call(o)`,
			want: "A,B",
		},
		{
			// Deleting the property from the holder: the site must fall back to
			// the rest of the chain rather than keep reading a retired slot.
			name: "delete from cached holder",
			src: `function read(o) { return o.v; }
			      var far = {v: "far"};
			      var near = Object.create(far);
			      near.v = "near";
			      var o = Object.create(near);
			      warm(read, o);
			      var before = read(o);
			      delete near.v;
			      before + "," + read(o)`,
			want: "near,far",
		},
		{
			// Re-pointing a link in the middle of the chain.
			name: "setPrototypeOf on a mid-chain object",
			src: `function read(o) { return o.v; }
			      var mid = Object.create({v: "first"});
			      var o = Object.create(mid);
			      warm(read, o);
			      var before = read(o);
			      Object.setPrototypeOf(mid, {v: "second"});
			      before + "," + read(o)`,
			want: "first,second",
		},
		{
			// Two receivers built the same way therefore share a shape, but
			// their prototypes differ — so the shape alone cannot say which
			// holder to read. This is why the entry compares the receiver's
			// [[Prototype]] on every hit.
			name: "same shape different proto",
			src: `function read(o) { return o.v; }
			      var a = Object.create({v: "A"});
			      var b = Object.create({v: "B"});
			      a.x = 1; b.x = 1;
			      warm(read, a);
			      read(a) + "," + read(b) + "," + read(a)`,
			want: "A,B,A",
		},
		{
			// Reassigning a method changes the holder's slot value, not its
			// layout. The cache holds the holder and reads the slot live, so
			// this needs no invalidation at all — and must still be seen.
			name: "reassigned method is seen live",
			src: `function call(o) { return o.m(); }
			      function C() {} C.prototype.m = function () { return 1; };
			      var o = new C();
			      warm(call, o);
			      var before = call(o);
			      C.prototype.m = function () { return 2; };
			      String(before) + "," + String(call(o))`,
			want: "1,2",
		},
		{
			// A property arriving on the holder that was absent when the site
			// cached "not found anywhere".
			name: "proto gains the property after a cached miss",
			src: `function read(o) { return o.late; }
			      var proto = {};
			      var o = Object.create(proto);
			      warm(read, o);
			      var before = read(o);
			      proto.late = "here";
			      String(before) + "," + String(read(o))`,
			want: "undefined,here",
		},
		{
			// Freezing the holder leaves the value readable, but a site that
			// cached a store must stop writing through.
			name: "frozen proto still reads",
			src: `function read(o) { return o.v; }
			      var proto = {v: 3};
			      var o = Object.create(proto);
			      warm(read, o);
			      Object.freeze(proto);
			      String(read(o))`,
			want: "3",
		},
		{
			// A method on Array.prototype reached from an array receiver. The
			// walk passes through an exotic object, whose element storage and
			// length a cached named read must not skip.
			name: "array method through the chain",
			src: `function call(a) { return a.slice(0).length; }
			      warm(call, [1, 2, 3]);
			      String(call([1, 2, 3, 4, 5]))`,
			want: "5",
		},
		{
			// A Proxy in the prototype chain has no shape of its own, so the
			// walk must refuse to cache past it and let the trap run.
			name: "proxy in the chain traps",
			src: `function read(o) { return o.v; }
			      var p = new Proxy({}, {get: function () { return "trap"; }});
			      var o = Object.create(p);
			      warm(read, o);
			      read(o)`,
			want: "trap",
		},
		{
			// A global function is an own property of the global object, read
			// through GET_GLOBAL's cache rather than a field site. Redefining
			// it must be visible.
			name: "global rebound after caching",
			src: `function g() { return 1; }
			      function call() { return g(); }
			      for (var i = 0; i < 50; i++) call();
			      var before = call();
			      g = function () { return 2; };
			      String(before) + "," + String(call())`,
			want: "1,2",
		},
		{
			// A Script-level let shadows a same-named global object property.
			// The declaration is published at frame entry, after any earlier
			// script's sites have already cached the object's slot.
			name: "global lexical shadows a cached property",
			src: `globalThis.shadowed = "prop";
			      function read() { return shadowed; }
			      for (var i = 0; i < 50; i++) read();
			      read()`,
			want: "prop",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := New()
			v, err := rt.RunString(tc.name+".js", warm+"\n"+tc.src)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got, e := rt.toStringValue(v)
			if e != nil {
				t.Fatalf("%s: %v", tc.name, e)
			}
			if s := rt.strGo(got); s != tc.want {
				t.Errorf("%s\n got: %q\nwant: %q", tc.name, s, tc.want)
			}
		})
	}
}
