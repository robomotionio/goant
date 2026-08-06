package engine

import "testing"

// ToPrimitive on a TypedArray, which used to kill the process.
//
// Object.prototype.valueOf returns its receiver. ordinaryToPrimitive asked "did
// valueOf give me back an object?" with IsObjectType, and T_TYPEDARRAY is not in
// tObjectMask — so the answer was no, and the VIEW ITSELF was returned as though
// it were a primitive. abstractEquals then recursed on unchanged arguments until
// the Go stack was gone: `fatal error: stack overflow`, which is not a catchable
// RangeError and not stoppable by Interrupt. A host running that flow loses the
// process.
//
// Reachable from `6 == new Int16Array([1,2,3])`. Found by the differential
// fuzzer on its first arm64 chunk, and present with the tier off — an
// interpreter bug the JIT work only happened to surface.
func TestToPrimitiveOnATypedArrayTerminates(t *testing.T) {
	rt := New()
	for _, tc := range []struct{ src, want string }{
		// The crash, and the answer it should have given: valueOf returns the
		// view, so toString runs and the comparison is against "1,2,3".
		{`String(6 == new Int16Array([1,2,3]))`, "false"},
		{`String("1,2,3" == new Int16Array([1,2,3]))`, "true"},
		{`String(6 == new Int16Array([6]))`, "true"},
		{`String(1 + new Int16Array([1]))`, "11"},
		{`String(+new Int16Array([7]))`, "7"},
		{`String(` + "`${new Int16Array([1,2])}`" + `)`, "1,2"},
		{`String(new Int16Array([1,2]) < "2")`, "true"},
		{`String([new Int16Array([1,2])] == "1,2")`, "true"},
		// Every view kind, since each has its own tag path to here.
		{`String(0 == new Float64Array([0]))`, "true"},
		{`String(0 == new Uint8Array([0]))`, "true"},
		{`String("" == new Int8Array([]))`, "true"},
		// A @@toPrimitive that hands back a view must be rejected, not accepted.
		{`(function () {
			var a = new Int16Array([1]);
			a[Symbol.toPrimitive] = function () { return new Int16Array([2]); };
			try { return String(6 == a); } catch (e) { return e.constructor.name; }
		})()`, "TypeError"},
		// And one that hands back a real primitive still works.
		{`(function () {
			var a = new Int16Array([1]);
			a[Symbol.toPrimitive] = function () { return 6; };
			return String(6 == a);
		})()`, "true"},
	} {
		got, err := rt.RunString("p.js", tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
			continue
		}
		if s := string(rt.strBytes(got)); s != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, s, tc.want)
		}
	}
}

// The rest of the family the same predicate broke.
//
// T_TYPEDARRAY is not in tObjectMask, so IsObjectType() answers "no" for a view.
// Anywhere that predicate is asked as "did I get an OBJECT back" rather than
// "which family is this", a view falls through the wrong branch. Three bugs from
// one cause, none of which any suite in this repository caught:
//
//   - ToPrimitive returned the view as if it were a primitive (a process-killing
//     stack overflow; see above)
//   - getPrototypeOf reported null for an object whose prototype IS a view,
//     while the chain itself worked
//   - JSON.stringify dropped views entirely
func TestATypedArrayIsAnObjectWhereItMatters(t *testing.T) {
	rt := New()
	for _, tc := range []struct{ name, src, want string }{
		{"getPrototypeOf reports the view", `
			var ta = new Int16Array([1,2,3]), o = {};
			Object.setPrototypeOf(o, ta);
			String(Object.getPrototypeOf(o) === ta) + "|" + o[0];`, "true|1"},
		{"stringify serialises the indices", `
			JSON.stringify(new Int16Array([1,2,3]));`, `{"0":1,"1":2,"2":3}`},
		{"stringify nested", `
			JSON.stringify({a: new Uint8Array([1,2])});`, `{"a":{"0":1,"1":2}}`},
		{"stringify inside an array", `
			JSON.stringify([new Float64Array([1.5])]);`, `[{"0":1.5}]`},
		{"stringify keeps named properties too", `
			(function () { var t = new Int16Array([1]); t.z = 9; return JSON.stringify(t); })();`,
			`{"0":1,"z":9}`},
		{"stringify honours toJSON", `
			(function () { var t = new Int16Array([1]); t.toJSON = function () { return "TJ"; }; return JSON.stringify(t); })();`,
			`"TJ"`},
		{"an empty view", `JSON.stringify(new Int8Array(0));`, "{}"},
		{"a plain object is unchanged", `JSON.stringify({a: 1, b: [2, 3]});`, `{"a":1,"b":[2,3]}`},
		{"an array is unchanged", `JSON.stringify([1, {a: 2}]);`, `[1,{"a":2}]`},
	} {
		got, err := rt.RunString("t.js", tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if s := string(rt.strBytes(got)); s != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, s, tc.want)
		}
	}
}
