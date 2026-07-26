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
