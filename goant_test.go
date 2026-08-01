package goant_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robomotionio/goant"
)

func mustRun(t *testing.T, rt *goant.Runtime, src string) goant.Value {
	t.Helper()
	v, err := rt.RunString(src)
	if err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	return v
}

func TestRunStringReturnsCompletionValue(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	if got := mustRun(t, rt, `[1,2,3].map(n => n * 2).join("-")`).String(); got != "2-4-6" {
		t.Fatalf("got %q", got)
	}
	if got := mustRun(t, rt, `40 + 2`).Int(); got != 42 {
		t.Fatalf("got %d", got)
	}
	if !mustRun(t, rt, `1 < 2`).Bool() {
		t.Fatal("expected true")
	}
}

func TestKindsAndPredicates(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	cases := []struct {
		src  string
		kind goant.Kind
	}{
		{`undefined`, goant.KindUndefined},
		{`null`, goant.KindNull},
		{`true`, goant.KindBoolean},
		{`1.5`, goant.KindNumber},
		{`"x"`, goant.KindString},
		{`Symbol("s")`, goant.KindSymbol},
		{`10n`, goant.KindBigInt},
		{`({})`, goant.KindObject},
		{`[]`, goant.KindObject},
		{`(function(){})`, goant.KindObject},
	}
	for _, c := range cases {
		if got := mustRun(t, rt, c.src).Kind(); got != c.kind {
			t.Errorf("%s: kind %v, want %v", c.src, got, c.kind)
		}
	}

	if !mustRun(t, rt, `[]`).IsArray() {
		t.Error("[] should be an array")
	}
	if !mustRun(t, rt, `(function(){})`).IsFunction() {
		t.Error("function should be callable")
	}
	if !mustRun(t, rt, `new Date()`).IsDate() {
		t.Error("Date should be a date")
	}
	if !mustRun(t, rt, `new TypeError("x")`).IsError() {
		t.Error("TypeError should be an error")
	}
	if !mustRun(t, rt, `Promise.resolve(1)`).IsPromise() {
		t.Error("promise should be a promise")
	}
	if !mustRun(t, rt, `new Uint8Array(2)`).IsTypedArray() {
		t.Error("Uint8Array should be a typed array")
	}
	if mustRun(t, rt, `({})`).IsArray() {
		t.Error("{} should not be an array")
	}
}

func TestZeroValueIsUndefinedAndSafe(t *testing.T) {
	var v goant.Value
	if !v.IsUndefined() {
		t.Error("zero Value should be undefined")
	}
	if v.String() != "" || v.Int() != 0 || v.Bool() {
		t.Error("zero Value should read as empty")
	}
	if v.Object() != nil || v.Function() != nil || v.Promise() != nil {
		t.Error("zero Value has no object view")
	}
	if v.Export() != nil {
		t.Error("zero Value exports as nil")
	}
}

func TestObjectPropertyAccess(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	obj := mustRun(t, rt, `({a: 1, b: "two"})`).Object()
	if obj == nil {
		t.Fatal("expected an object")
	}

	a, err := obj.Get("a")
	if err != nil || a.Int() != 1 {
		t.Fatalf("a = %v, %v", a.Int(), err)
	}
	missing, err := obj.Get("nope")
	if err != nil || !missing.IsUndefined() {
		t.Fatalf("missing key should read undefined, got %v %v", missing, err)
	}

	if err := obj.Set("c", []int{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	keys, err := obj.Keys()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(keys, ","); got != "a,b,c" {
		t.Fatalf("keys = %q", got)
	}

	has, err := obj.Has("b")
	if err != nil || !has {
		t.Fatalf("has b = %v, %v", has, err)
	}
	gone, err := obj.Delete("b")
	if err != nil || !gone {
		t.Fatalf("delete b = %v, %v", gone, err)
	}
	if has, _ := obj.Has("b"); has {
		t.Fatal("b should be gone")
	}
}

func TestArrayIndexAccess(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	arr := mustRun(t, rt, `["a","b","c"]`).Object()
	n, err := arr.Len()
	if err != nil || n != 3 {
		t.Fatalf("len = %d, %v", n, err)
	}
	v, err := arr.At(1)
	if err != nil || v.String() != "b" {
		t.Fatalf("at(1) = %q, %v", v.String(), err)
	}
	if err := arr.SetAt(3, "d"); err != nil {
		t.Fatal(err)
	}
	if n, _ := arr.Len(); n != 4 {
		t.Fatalf("len after append = %d", n)
	}

	vals, err := arr.Values()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range vals {
		got = append(got, v.String())
	}
	if strings.Join(got, "") != "abcd" {
		t.Fatalf("values = %v", got)
	}
}

func TestObjectMethodCall(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	obj := mustRun(t, rt, `({n: 10, add(x) { return this.n + x }})`).Object()
	v, err := obj.Call("add", 5)
	if err != nil {
		t.Fatal(err)
	}
	if v.Int() != 15 {
		t.Fatalf("add(5) = %d, want 15 (this must be bound)", v.Int())
	}
}

func TestGlobalsRoundTrip(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	if err := rt.Set("answer", 42); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `answer`).Int(); got != 42 {
		t.Fatalf("got %d", got)
	}
	v, err := rt.Get("answer")
	if err != nil || v.Int() != 42 {
		t.Fatalf("Get = %v, %v", v.Int(), err)
	}
	if err := rt.SetAll(map[string]any{"a": 1, "b": "two"}); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `a + b`).String(); got != "1two" {
		t.Fatalf("got %q", got)
	}
}

// --- conversions ------------------------------------------------------------

func TestToValueScalars(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	cases := []struct {
		in   any
		want string
	}{
		{nil, "null"},
		{true, "true"},
		{"hi", "hi"},
		{int8(-3), "-3"},
		{uint64(7), "7"},
		{3.5, "3.5"},
		{big.NewInt(1 << 62), "4611686018427387904"},
	}
	for _, c := range cases {
		v, err := rt.ToValue(c.in)
		if err != nil {
			t.Fatalf("%v: %v", c.in, err)
		}
		if got := v.String(); got != c.want {
			t.Errorf("%#v -> %q, want %q", c.in, got, c.want)
		}
	}

	if _, err := rt.ToValue(make(chan int)); err == nil {
		t.Error("a channel has no JavaScript form and should be an error")
	}
}

func TestToValueContainers(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	if err := rt.Set("m", map[string]any{"a": 1, "b": []string{"x", "y"}}); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `m.a + "," + m.b.join("")`).String(); got != "1,xy" {
		t.Fatalf("got %q", got)
	}
	if got := mustRun(t, rt, `Array.isArray(m.b)`).Bool(); !got {
		t.Fatal("a Go slice should become a real array")
	}

	if err := rt.Set("nums", []int{3, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `nums.sort().join("")`).String(); got != "123" {
		t.Fatalf("sorted = %q", got)
	}
}

type addr struct {
	City string `json:"city"`
}

type person struct {
	addr
	Name    string `json:"name"`
	Age     int    `json:"age"`
	Secret  string `json:"-"`
	skipped string
}

func (p person) Greet(greeting string) string { return greeting + ", " + p.Name }

func TestToValueStruct(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	p := person{addr: addr{City: "izmir"}, Name: "ada", Age: 36, Secret: "x", skipped: "y"}
	if err := rt.Set("p", p); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `p.name + "/" + p.age + "/" + p.city`).String(); got != "ada/36/izmir" {
		t.Fatalf("got %q", got)
	}
	if got := mustRun(t, rt, `"Secret" in p`).Bool(); got {
		t.Fatal(`json:"-" should be omitted`)
	}
	if got := mustRun(t, rt, `p.greet("hi")`).String(); got != "hi, ada" {
		t.Fatalf("method call = %q", got)
	}
}

func TestToValueRespectsFieldNamer(t *testing.T) {
	rt := goant.New(goant.WithFieldNamer(goant.GoFieldNamer))
	defer rt.Close()

	if err := rt.Set("p", person{Name: "ada"}); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `p.Name`).String(); got != "ada" {
		t.Fatalf("got %q", got)
	}
}

func TestToValueCycle(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	type node struct {
		Name string `json:"name"`
		Next *node  `json:"next"`
	}
	a := &node{Name: "a"}
	a.Next = a

	if err := rt.Set("a", a); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `a.next.next.next === a`).Bool(); !got {
		t.Fatal("a cycle should converge on the same object")
	}
}

func TestToValueTimeAndBytes(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if err := rt.Set("t", when); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `t instanceof Date && t.getTime()`).Int(); got != when.UnixMilli() {
		t.Fatalf("time = %d, want %d", got, when.UnixMilli())
	}

	if err := rt.Set("b", []byte{1, 2, 250}); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `b instanceof Uint8Array && b[2]`).Int(); got != 250 {
		t.Fatalf("bytes = %d", got)
	}
}

func TestBytesAreNotCopied(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	buf := []byte{0, 0, 0}
	if err := rt.Set("b", buf); err != nil {
		t.Fatal(err)
	}
	mustRun(t, rt, `b[1] = 9`)
	if buf[1] != 9 {
		t.Fatalf("the script's write should land in the caller's slice, got %v", buf)
	}
}

func TestExport(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	got := mustRun(t, rt, `({n: 1, s: "x", b: true, a: [1, "two"], o: {k: null}})`).Export()
	want := map[string]any{
		"n": float64(1),
		"s": "x",
		"b": true,
		"a": []any{float64(1), "two"},
		"o": map[string]any{"k": nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("export = %#v\nwant %#v", got, want)
	}
}

func TestExportCycle(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	got := mustRun(t, rt, `(() => { const o = {n: 1}; o.self = o; return o })()`).Export()
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("export = %T", got)
	}
	self, ok := m["self"].(map[string]any)
	if !ok {
		t.Fatalf("self = %T", m["self"])
	}
	if fmt.Sprintf("%p", self) != fmt.Sprintf("%p", m) {
		t.Fatal("a cycle should export as the same Go map")
	}
}

func TestExportToStruct(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	var p person
	p.Age = 99 // a property the script omits must be left alone
	v := mustRun(t, rt, `({name: "ada", city: "izmir"})`)
	if err := v.ExportTo(&p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "ada" || p.City != "izmir" || p.Age != 99 {
		t.Fatalf("exported %+v", p)
	}
}

func TestExportToScalarsAndContainers(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	var s string
	if err := mustRun(t, rt, `"x"`).ExportTo(&s); err != nil || s != "x" {
		t.Fatalf("string: %q %v", s, err)
	}
	var n int
	if err := mustRun(t, rt, `7.9`).ExportTo(&n); err != nil || n != 7 {
		t.Fatalf("int: %d %v", n, err)
	}
	var f float64
	if err := mustRun(t, rt, `"2.5"`).ExportTo(&f); err != nil || f != 2.5 {
		t.Fatalf("float: %v %v", f, err)
	}
	var xs []string
	if err := mustRun(t, rt, `["a","b"]`).ExportTo(&xs); err != nil || strings.Join(xs, "") != "ab" {
		t.Fatalf("slice: %v %v", xs, err)
	}
	var m map[string]int
	if err := mustRun(t, rt, `({a: 1, b: 2})`).ExportTo(&m); err != nil || m["a"] != 1 || m["b"] != 2 {
		t.Fatalf("map: %v %v", m, err)
	}
	var b []byte
	if err := mustRun(t, rt, `new Uint8Array([1,2,3])`).ExportTo(&b); err != nil || len(b) != 3 || b[2] != 3 {
		t.Fatalf("bytes: %v %v", b, err)
	}
	var when time.Time
	if err := mustRun(t, rt, `new Date(1000)`).ExportTo(&when); err != nil || when.UnixMilli() != 1000 {
		t.Fatalf("time: %v %v", when, err)
	}
	var ptr *int
	if err := mustRun(t, rt, `5`).ExportTo(&ptr); err != nil || ptr == nil || *ptr != 5 {
		t.Fatalf("pointer: %v %v", ptr, err)
	}
	var nilptr *int = new(int)
	if err := mustRun(t, rt, `null`).ExportTo(&nilptr); err != nil || nilptr != nil {
		t.Fatalf("null into pointer: %v %v", nilptr, err)
	}
	var overflow int8
	if err := mustRun(t, rt, `1000`).ExportTo(&overflow); err == nil {
		t.Fatal("an overflowing number should be reported, not truncated")
	}
}

func TestExportToFunc(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	mustRun(t, rt, `function sum(a, b) { return a + b }`)
	v, err := rt.Get("sum")
	if err != nil {
		t.Fatal(err)
	}
	var sum func(int, int) int
	if err := v.ExportTo(&sum); err != nil {
		t.Fatal(err)
	}
	if got := sum(40, 2); got != 42 {
		t.Fatalf("sum(40,2) = %d", got)
	}

	mustRun(t, rt, `function boom() { throw new RangeError("nope") }`)
	bv, _ := rt.Get("boom")
	var boom func() error
	if err := bv.ExportTo(&boom); err != nil {
		t.Fatal(err)
	}
	err = boom()
	if err == nil {
		t.Fatal("a throw should reach the error return")
	}
	var jsErr *goant.Error
	if !errors.As(err, &jsErr) || jsErr.Name != "RangeError" {
		t.Fatalf("error = %#v", err)
	}
}

// --- host functions ---------------------------------------------------------

func TestHostFunctionReflected(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	if err := rt.Set("greet", func(name string) string { return "hello " + name }); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `greet("ada")`).String(); got != "hello ada" {
		t.Fatalf("got %q", got)
	}

	if err := rt.Set("div", func(a, b float64) (float64, error) {
		if b == 0 {
			return 0, errors.New("divide by zero")
		}
		return a / b, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `div(9, 3)`).Float(); got != 3 {
		t.Fatalf("got %v", got)
	}
	if got := mustRun(t, rt, `(() => { try { div(1, 0) } catch (e) { return e.message } })()`).String(); got != "divide by zero" {
		t.Fatalf("thrown message = %q", got)
	}
}

func TestHostFunctionMissingAndExtraArguments(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	rt.Set("f", func(a, b int) int { return a*100 + b })
	if got := mustRun(t, rt, `f(1)`).Int(); got != 100 {
		t.Fatalf("a missing argument should be the zero value, got %d", got)
	}
	if got := mustRun(t, rt, `f(1, 2, 3)`).Int(); got != 102 {
		t.Fatalf("extra arguments should be ignored, got %d", got)
	}
}

func TestHostFunctionVariadic(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	rt.Set("join", func(sep string, parts ...string) string { return strings.Join(parts, sep) })
	if got := mustRun(t, rt, `join("-", "a", "b", "c")`).String(); got != "a-b-c" {
		t.Fatalf("got %q", got)
	}
	if got := mustRun(t, rt, `join("-")`).String(); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestHostFunctionStructArgument(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	rt.Set("describe", func(p person) string { return fmt.Sprintf("%s/%d", p.Name, p.Age) })
	if got := mustRun(t, rt, `describe({name: "ada", age: 36})`).String(); got != "ada/36" {
		t.Fatalf("got %q", got)
	}
}

func TestHostFunctionRawForm(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	rt.Set("count", goant.Func(func(c *goant.Call) (any, error) {
		return c.Len(), nil
	}))
	if got := mustRun(t, rt, `count(1, 2, 3)`).Int(); got != 3 {
		t.Fatalf("got %d", got)
	}

	rt.Set("fail", goant.Func(func(c *goant.Call) (any, error) {
		return nil, errors.New("host said no")
	}))
	if _, err := rt.RunString(`fail()`); err == nil {
		t.Fatal("expected the host error to surface")
	} else if !strings.Contains(err.Error(), "host said no") {
		t.Fatalf("error = %v", err)
	}

	rt.Set("this0", goant.Func(func(c *goant.Call) (any, error) {
		v, err := c.This.Object().Get("n")
		if err != nil {
			return nil, err
		}
		return v.Int(), nil
	}))
	if got := mustRun(t, rt, `({n: 4, m: this0}).m()`).Int(); got != 4 {
		t.Fatalf("this = %d", got)
	}
}

func TestHostFunctionReturningStruct(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	rt.Set("make", func(name string) person { return person{Name: name, Age: 1} })
	if got := mustRun(t, rt, `make("x").name`).String(); got != "x" {
		t.Fatalf("got %q", got)
	}
}

// --- errors -----------------------------------------------------------------

func TestThrownErrorCarriesItsValue(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	_, err := rt.RunString(`throw Object.assign(new TypeError("bad"), {code: 7})`)
	var jsErr *goant.Error
	if !errors.As(err, &jsErr) {
		t.Fatalf("error = %#v", err)
	}
	if jsErr.Name != "TypeError" || jsErr.Message != "bad" {
		t.Fatalf("name/message = %q/%q", jsErr.Name, jsErr.Message)
	}
	if jsErr.Error() != "TypeError: bad" {
		t.Fatalf("Error() = %q", jsErr.Error())
	}
	code, gerr := jsErr.Value().Object().Get("code")
	if gerr != nil || code.Int() != 7 {
		t.Fatalf("the thrown value should still be readable: %v %v", code.Int(), gerr)
	}
}

func TestThrownNonError(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	_, err := rt.RunString(`throw "just a string"`)
	var jsErr *goant.Error
	if !errors.As(err, &jsErr) {
		t.Fatalf("error = %#v", err)
	}
	if jsErr.Message != "just a string" || jsErr.Name != "" {
		t.Fatalf("message/name = %q/%q", jsErr.Message, jsErr.Name)
	}
}

func TestSyntaxError(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	_, err := rt.Compile("bad.js", `function (`)
	var se *goant.SyntaxError
	if !errors.As(err, &se) {
		t.Fatalf("error = %#v", err)
	}
	if se.File != "bad.js" {
		t.Fatalf("file = %q", se.File)
	}
}

func TestInterrupt(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	time.AfterFunc(50*time.Millisecond, rt.Interrupt)
	_, err := rt.RunString(`for (;;) {}`)
	if !errors.Is(err, goant.ErrInterrupted) {
		t.Fatalf("error = %#v", err)
	}
	if !rt.Interrupted() {
		t.Fatal("the interruption should persist until cleared")
	}
	rt.ClearInterrupt()
	if got := mustRun(t, rt, `1 + 1`).Int(); got != 2 {
		t.Fatalf("the runtime should be usable again, got %d", got)
	}
}

func TestWithContextDeadline(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	stop := rt.WithContext(ctx)
	defer stop()

	_, err := rt.RunString(`for (;;) {}`)
	if !errors.Is(err, goant.ErrInterrupted) {
		t.Fatalf("error = %#v", err)
	}
}

func TestMemoryLimit(t *testing.T) {
	rt := goant.New(goant.WithMemoryLimit(4 << 20))
	defer rt.Close()

	_, err := rt.RunString(`const a = []; for (;;) { a.push({x: 1, y: 2, z: 3}) }`)
	if !errors.Is(err, goant.ErrMemoryLimit) {
		t.Fatalf("error = %#v", err)
	}
}

func TestUseAfterClose(t *testing.T) {
	rt := goant.New()
	rt.Close()
	if _, err := rt.RunString(`1`); !errors.Is(err, goant.ErrClosed) {
		t.Fatalf("error = %#v", err)
	}
	rt.Close() // idempotent
}

func TestValueFromAnotherRuntimeIsRefused(t *testing.T) {
	a, b := goant.New(), goant.New()
	defer a.Close()
	defer b.Close()

	v := mustRun(t, a, `({})`)
	if err := b.Set("x", v); err == nil {
		t.Fatal("a value from another Runtime must be refused, not reinterpreted")
	}
}

// --- promises and jobs ------------------------------------------------------

func TestAwait(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	v := mustRun(t, rt, `(async () => { const n = await Promise.resolve(20); return n * 2 })()`)
	res, err := rt.Await(v)
	if err != nil {
		t.Fatal(err)
	}
	if res.Int() != 40 {
		t.Fatalf("got %d", res.Int())
	}

	// A non-promise passes straight through.
	plain, err := rt.Await(mustRun(t, rt, `5`))
	if err != nil || plain.Int() != 5 {
		t.Fatalf("plain = %v, %v", plain.Int(), err)
	}
}

func TestAwaitRejection(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	v := mustRun(t, rt, `(async () => { throw new Error("nope") })()`)
	_, err := rt.Await(v)
	var jsErr *goant.Error
	if !errors.As(err, &jsErr) || jsErr.Message != "nope" {
		t.Fatalf("error = %#v", err)
	}
}

func TestPromiseState(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	p := mustRun(t, rt, `Promise.resolve(3)`).Promise()
	if p == nil {
		t.Fatal("expected a promise")
	}
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if p.State() != goant.Fulfilled {
		t.Fatalf("state = %v", p.State())
	}
	if p.Result().Int() != 3 {
		t.Fatalf("result = %d", p.Result().Int())
	}
}

// --- JSON -------------------------------------------------------------------

func TestJSONRoundTrip(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	in := []byte(`{"a":1,"b":["x","y"],"c":{"d":true}}`)
	v, err := rt.ParseJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("m", v); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `m.b[1] + m.c.d`).String(); got != "ytrue" {
		t.Fatalf("got %q", got)
	}

	out, ok, err := v.AppendJSON(nil)
	if err != nil || !ok {
		t.Fatalf("append = %v %v", ok, err)
	}
	if string(out) != string(in) {
		t.Fatalf("round trip = %s", out)
	}
}

func TestAppendJSONNoValue(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	dst := []byte("keep")
	out, ok, err := mustRun(t, rt, `undefined`).AppendJSON(dst)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("undefined has no JSON form")
	}
	if string(out) != "keep" {
		t.Fatalf("nothing should have been appended, got %q", out)
	}
}

func TestAppendJSONEach(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	arr := mustRun(t, rt, `[{a:1}, undefined, "x"]`).Object()
	buf, ends, err := arr.AppendJSONEach(nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ends) != 3 {
		t.Fatalf("ends = %v", ends)
	}
	var got []string
	start := 0
	for _, end := range ends {
		got = append(got, string(buf[start:end:end]))
		start = end
	}
	want := []string{`{"a":1}`, ``, `"x"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payloads = %q, want %q", got, want)
	}
}

func TestParseJSONLazyOnlyBuildsWhatIsRead(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	in := []byte(`{"kept":{"deep":[1,2,3]},"read":7}`)
	v, err := rt.ParseJSONLazy(in)
	if err != nil {
		t.Fatal(err)
	}
	rt.Set("m", v)
	if got := mustRun(t, rt, `m.read`).Int(); got != 7 {
		t.Fatalf("got %d", got)
	}
	out, _, err := v.AppendJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(in) {
		t.Fatalf("untouched document should round trip: %s", out)
	}
}

func TestValueMarshalJSON(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	b, err := mustRun(t, rt, `({a: [1, 2]})`).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"a":[1,2]}` {
		t.Fatalf("got %s", b)
	}
	b, _ = mustRun(t, rt, `undefined`).MarshalJSON()
	if string(b) != "null" {
		t.Fatalf("undefined marshals as null, got %s", b)
	}
}

// --- compile once, run many -------------------------------------------------

func TestCompileOnceRunMany(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	p, err := rt.Compile("main.js", `input * 2`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		rt.Set("input", i)
		v, err := rt.RunProgram(p)
		if err != nil {
			t.Fatal(err)
		}
		if v.Int() != int64(i*2) {
			t.Fatalf("run %d = %d", i, v.Int())
		}
	}
	if p.Name() != "main.js" {
		t.Fatalf("name = %q", p.Name())
	}
}

func TestConstruct(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	// A top-level class declaration is a lexical binding, not a property of
	// globalThis — so reach it the way a script would have to.
	mustRun(t, rt, `globalThis.Point = class { constructor(x, y) { this.x = x; this.y = y } }`)
	ctor, err := rt.Get("Point")
	if err != nil {
		t.Fatal(err)
	}
	obj, err := ctor.Function().Construct(3, 4)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := obj.Get("x")
	y, _ := obj.Get("y")
	if x.Int() != 3 || y.Int() != 4 {
		t.Fatalf("point = %d,%d", x.Int(), y.Int())
	}
	if mustRun(t, rt, `1`).Function() != nil {
		t.Fatal("a number has no Function view")
	}
	if _, err := mustRun(t, rt, `({})`).Function().Construct(); err == nil {
		t.Fatal("constructing a non-function should fail, not panic")
	}
}

func TestRunModule(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	if _, err := rt.RunModule("m.js", `globalThis.fromModule = 1 + 1`); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `fromModule`).Int(); got != 2 {
		t.Fatalf("got %d", got)
	}
}

func TestEqualsIsStrict(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	mustRun(t, rt, `var o = {}`)
	a, _ := rt.Get("o")
	b, _ := rt.Get("o")
	if !a.Equals(b) {
		t.Fatal("the same object should be strictly equal to itself")
	}
	if a.Equals(mustRun(t, rt, `({})`)) {
		t.Fatal("distinct objects are not equal")
	}
	if mustRun(t, rt, `1`).Equals(mustRun(t, rt, `"1"`)) {
		t.Fatal("=== does not coerce")
	}
}

func TestExportTypedArrays(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	// A byte-sized view exports as bytes, because that is unambiguously what
	// it holds.
	got := mustRun(t, rt, `new Uint8Array([1, 2, 3])`).Export()
	b, ok := got.([]byte)
	if !ok || len(b) != 3 || b[2] != 3 {
		t.Fatalf("Uint8Array exported as %T %v", got, got)
	}

	// A wider one exports as its numbers, not as the bytes underneath them.
	got = mustRun(t, rt, `new Float64Array([1.5, 2.5])`).Export()
	xs, ok := got.([]any)
	if !ok || len(xs) != 2 || xs[0] != 1.5 || xs[1] != 2.5 {
		t.Fatalf("Float64Array exported as %T %v", got, got)
	}
}

func TestExportToStructThroughEmbeddedPointer(t *testing.T) {
	type Inner struct {
		City string `json:"city"`
	}
	type Outer struct {
		*Inner
		Name string `json:"name"`
	}

	rt := goant.New()
	defer rt.Close()

	var o Outer
	if err := mustRun(t, rt, `({name: "ada", city: "izmir"})`).ExportTo(&o); err != nil {
		t.Fatal(err)
	}
	if o.Name != "ada" || o.Inner == nil || o.City != "izmir" {
		t.Fatalf("exported %+v", o)
	}

	if err := rt.Set("o", o); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `o.name + "/" + o.city`).String(); got != "ada/izmir" {
		t.Fatalf("round trip = %q", got)
	}
}

func TestEmbeddedFieldWithAJSONNameIsNotPromoted(t *testing.T) {
	type Inner struct {
		City string `json:"city"`
	}
	type Outer struct {
		Inner `json:"addr"`
		Name  string `json:"name"`
	}

	rt := goant.New()
	defer rt.Close()

	if err := rt.Set("o", Outer{Inner: Inner{City: "izmir"}, Name: "ada"}); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, rt, `o.addr.city + "/" + (o.city === undefined)`).String(); got != "izmir/true" {
		t.Fatalf("got %q", got)
	}
}

func TestSymbolExportsAsAValue(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	got := mustRun(t, rt, `Symbol("s")`).Export()
	v, ok := got.(goant.Value)
	if !ok || !v.IsSymbol() {
		t.Fatalf("symbol exported as %T", got)
	}
}

func TestNewBytesSurvivesCollection(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	if err := rt.Set("b", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	// Allocate enough to make a collection worth doing, then force one and
	// check the array the host handed over is still intact — its backing
	// buffer is reachable only through the view.
	mustRun(t, rt, `for (let i = 0; i < 5000; i++) ({junk: "x" + i})`)
	rt.Collect()

	if got := mustRun(t, rt, `String.fromCharCode(...b)`).String(); got != "hello" {
		t.Fatalf("after a collection the bytes read as %q", got)
	}
	if got := mustRun(t, rt, `b.buffer.byteLength`).Int(); got != 5 {
		t.Fatalf("the backing buffer was lost: byteLength = %d", got)
	}
}
