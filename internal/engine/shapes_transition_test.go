package engine

import (
	"strings"
	"testing"
)

// A shape memoises the last transition taken out of it, because objects are
// overwhelmingly built the same way as the last one. The cache holds one entry,
// so what has to be pinned is that it stays correct when that assumption does
// not hold: when transitions out of the same shape alternate, when the same key
// is added with different attributes, and when a shape is reached by more than
// one route.

// Alternating between two different keys out of the same base shape misses the
// one-entry cache on every step. Each object must still get its own layout.
func TestShapeTransitionsAlternateCorrectly(t *testing.T) {
	rt := New()
	s, err := rt.CompileScript("alt.js", `
		var out = [];
		for (var i = 0; i < 200; i++) {
			var o = (i % 2 === 0) ? {a: i, common: "x"} : {b: i, common: "y"};
			out.push(JSON.stringify(o));
		}
		out.join("|")
	`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.RunScript(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rt.ToString(v)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(got, "|")
	if len(parts) != 200 {
		t.Fatalf("got %d objects, want 200", len(parts))
	}
	for i, p := range parts {
		var want string
		if i%2 == 0 {
			want = `{"a":` + shapeItoa(i) + `,"common":"x"}`
		} else {
			want = `{"b":` + shapeItoa(i) + `,"common":"y"}`
		}
		if p != want {
			t.Fatalf("object %d is %s, want %s — a memoised transition was reused "+
				"for a different key", i, p, want)
		}
	}
}

// The same key added with different attributes is a different transition. A
// cache keyed on the property name alone would hand the second object the
// first one's descriptor.
func TestShapeTransitionsDistinguishAttributes(t *testing.T) {
	rt := New()
	s, err := rt.CompileScript("attrs.js", `
		var plain = {};
		plain.x = 1;

		var hidden = {};
		Object.defineProperty(hidden, "x", {value: 2, enumerable: false,
			writable: false, configurable: false});

		var again = {};
		again.x = 3;

		[Object.keys(plain).length,
		 Object.keys(hidden).length,
		 Object.keys(again).length,
		 JSON.stringify(Object.getOwnPropertyDescriptor(hidden, "x")),
		 JSON.stringify(Object.getOwnPropertyDescriptor(again, "x"))].join("|")
	`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.RunScript(s)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := rt.ToString(v)
	const want = `1|0|1|` +
		`{"value":2,"writable":false,"enumerable":false,"configurable":false}|` +
		`{"value":3,"writable":true,"enumerable":true,"configurable":true}`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

// Deleting a property restructures a shape, which moves the slots after it. A
// later object built along the original path must still find its values in the
// right places.
func TestShapeTransitionsSurviveDelete(t *testing.T) {
	rt := New()
	s, err := rt.CompileScript("del.js", `
		var a = {p: 1, q: 2, r: 3};
		delete a.q;
		var b = {p: 10, q: 20, r: 30};
		[JSON.stringify(a), JSON.stringify(b), a.r, b.q, b.r].join("|")
	`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.RunScript(s)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := rt.ToString(v)
	const want = `{"p":1,"r":3}|{"p":10,"q":20,"r":30}|3|20|30`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

// The parse path is where the memoised transition pays off — every element of
// an array of records repeats the same one. Each element must still come back
// with its own values.
func TestShapeTransitionsAcrossParsedRecords(t *testing.T) {
	rt := New()
	var sb strings.Builder
	sb.WriteString(`{"items":[`)
	for i := 0; i < 500; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"id":` + shapeItoa(i) + `,"name":"n` + shapeItoa(i) + `","ok":true}`)
	}
	sb.WriteString(`]}`)

	v, err := rt.JSONParseBytes([]byte(sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	items, err := rt.GetProp(v, "items")
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 1, 249, 498, 499} {
		elem, err := rt.GetProp(items, shapeItoa(i))
		if err != nil {
			t.Fatal(err)
		}
		id, err := rt.GetProp(elem, "id")
		if err != nil {
			t.Fatal(err)
		}
		n, _ := rt.ToNumber(id)
		if int(n) != i {
			t.Fatalf("element %d has id %v", i, n)
		}
		name, err := rt.GetProp(elem, "name")
		if err != nil {
			t.Fatal(err)
		}
		s, _ := rt.ToString(name)
		if s != "n"+shapeItoa(i) {
			t.Fatalf("element %d has name %q", i, s)
		}
	}
}

func shapeItoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
