//go:build amd64

package engine

import "testing"

// Reading a local the emitter cannot prove was written.
//
// This used to refuse the whole function, and it is the ordinary shape of
// JavaScript rather than a corner: `var x;` then a conditional assignment, or a
// loop variable declared in one branch. It was measured at 1.0M of richards'
// 5.4M interpreted frame entries and 1.8M of deltablue's 8.7M.
//
// Two things can be in the slot and the difference is observable. A `var` read
// before its assignment holds undefined, which is a value like any other. A
// lexical binding read before its initialiser holds the empty sentinel and must
// throw. The tests below run both, and the third runs what makes the first
// dangerous: the value copied into a slot the tier would otherwise call numeric.

// jitBothWays runs src through a fresh Runtime with the tier off and with it on,
// and requires the two to agree — including on throwing.
func jitBothWays(t *testing.T, name, src string) {
	t.Helper()
	saved := jitEnabled
	defer func() { jitEnabled = saved }()

	jitEnabled = false
	want, errOff := New().RunString(name, src)
	jitEnabled = true
	got, errOn := New().RunString(name, src)

	if (errOff == nil) != (errOn == nil) {
		t.Errorf("%s: interpreted err=%v, compiled err=%v", name, errOff, errOn)
		return
	}
	if errOff != nil {
		if errOff.Error() != errOn.Error() {
			t.Errorf("%s: interpreted %q, compiled %q", name, errOff, errOn)
		}
		return
	}
	if uint64(got) != uint64(want) {
		t.Errorf("%s: compiled %#016x (%v), interpreted %#016x (%v)",
			name, uint64(got), got.Type(), uint64(want), want.Type())
	}
}

func TestJITUnprovenLocalAgreesWithTheInterpreter(t *testing.T) {
	// Each runs its function enough times to tier, then reports something that
	// depends on the unproven slot.
	for _, tc := range []struct{ name, src string }{
		{"var-read-on-one-path", `
			function f(a) { var x; if (a > 0) { x = 1; } return x; }
			var s = 0, u = 0;
			for (var k = 0; k < 40; k++) { var v = f(k - 20); if (v === undefined) u++; else s += v; }
			s * 1000 + u;`},
		{"var-used-in-arithmetic", `
			function f(a) { var x; if (a > 0) { x = 1; } return x + 1; }
			var nan = 0, s = 0;
			for (var k = 0; k < 40; k++) { var v = f(k - 20); if (v !== v) nan++; else s += v; }
			s * 1000 + nan;`},
		// The soundness case: the unproven slot is copied into another local
		// that every other store fills with a Number, so a tier that forgot to
		// propagate would hand an ADDSD the bit pattern of undefined.
		{"copied-into-a-numeric-local", `
			function f(a) { var x; if (a > 0) { x = 2; } var y = x; y = y * 3; return y; }
			var nan = 0, s = 0;
			for (var k = 0; k < 40; k++) { var v = f(k - 20); if (v !== v) nan++; else s += v; }
			s * 1000 + nan;`},
		{"declared-inside-a-loop", `
			function f(n) { var t = 0; for (var i = 0; i < n; i++) { var d = i * 2; t = t + d; } return t + i; }
			var s = 0; for (var k = 0; k < 40; k++) s += f(k % 5); s;`},
		{"never-assigned-at-all", `
			function f(a) { var x; return x; }
			var u = 0; for (var k = 0; k < 40; k++) { if (f(k) === undefined) u++; } u;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// TestJITDeadZoneThrowsLikeTheInterpreter is the half that must not be treated
// as a value. A lexical binding read before its initialiser is a ReferenceError,
// and the message has to match too — a script can catch it and read it.
func TestJITDeadZoneThrowsLikeTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"let-before-its-declaration", `
			function f(a) { if (a > 0) { return x; } let x = 1; return 0; }
			var s = 0; for (var k = 0; k < 40; k++) s += f(k - 20); s;`},
		{"caught-and-reported", `
			function f(a) { try { if (a > 0) { return x; } } catch (e) { return e.message.length; } let x = 1; return 0; }
			var s = 0; for (var k = 0; k < 40; k++) s += f(k - 20); s;`},
		{"const-before-its-declaration", `
			function f(a) { if (a > 0) { return c; } const c = 1; return 0; }
			var s = 0; for (var k = 0; k < 40; k++) s += f(k - 20); s;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// TestJITCompilesAnUnprovenLocal is what stops the tests above from passing
// against a tier that still refuses the whole function — which is how they would
// agree with the interpreter most convincingly of all.
func TestJITCompilesAnUnprovenLocal(t *testing.T) {
	const src = "function f(a){ var x; if (a > 0) { x = 1; } return x; }"
	_, fn := jitFnRT(t, src)
	var why string
	c := jitCompile(fn, &why)
	if c == nil {
		t.Fatalf("refused %q: %s", src, why)
	}
	c.free()
}

// TestJITReadsTheReceiver is the test that was missing when this was a refusal.
//
// Compiled code used to be handed a frame's locals and nothing else, so it
// stepped over the prologue that binds `this` and relied on the read of that
// slot refusing for want of a proof of assignment. Two rules sharing one
// mechanism: relaxing the documented one silently removed the undocumented one,
// and `this.x = 1` in a compiled constructor started reading the undefined the
// frame was filled with. Richards found it; no test did.
//
// The receiver now travels in the context, so this checks the value that comes
// back rather than the refusal — the same property, stated in a way that stays
// true once the feature exists.
func TestJITReadsTheReceiver(t *testing.T) {
	for _, src := range []string{
		"function f(a){ return this.x; }",
		"function f(a){ var t = this; return t.x; }",
		"function f(a){ if (a > 0) { return this.x; } return this.x; }",
	} {
		rt, fn := jitFnRT(t, src)
		var why string
		c := jitCompile(fn, &why)
		if c == nil {
			t.Errorf("refused %q: %s", src, why)
			continue
		}
		recv := rt.newObject(rt.objectProto)
		rt.objPtr(recv).defineOwn("x", tov(99), attrDefault)
		locals := make([]Value, fn.maxLocals)
		locals[0] = tov(1)
		got, e, ok := c.jitRun(rt, fn, nil, locals, recv)
		if !ok || e != nil {
			t.Errorf("%q declined (%v) or threw (%v)", src, !ok, e != nil)
		} else if got != tov(99) {
			t.Errorf("%q read %v from the receiver, want 99", src, got)
		}
		c.free()
	}
}

// TestJITReceiverSurvivesACollection is the root the context field needs.
//
// The receiver reaches compiled code as an integer in the context, and while a
// helper runs it is reachable from nowhere else — the interpreter frame that
// entered compiled code is not something the collector's walk descends into. A
// getter is JavaScript, so a collection can happen at exactly that moment.
func TestJITReceiverSurvivesACollection(t *testing.T) {
	const src = "function f(a){ var t = this.trigger; return this.x; }"
	rt, fn := jitFnRT(t, src)
	c := jitCompile(fn, nil)
	if c == nil {
		t.Fatal("refused")
	}
	defer c.free()

	// Built here and referred to by nothing else the collector can see once it
	// is in the context.
	recv, err := rt.RunString("recv.js", "({ get trigger() { collect(); return 1; }, x: 99 })")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	collector := rt.newNativeFunc("collect", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		rt.collect()
		return mkundef(), nil
	})
	rt.setField(rt.global, "collect", collector)

	locals := make([]Value, fn.maxLocals)
	locals[0] = tov(1)
	got, e, ok := c.jitRun(rt, fn, nil, locals, recv)
	if !ok || e != nil {
		t.Fatalf("declined (%v) or threw (%v)", !ok, e != nil)
	}
	if got != tov(99) {
		t.Errorf("read %v after a collection ran inside the getter, want 99", got)
	}
}

// TestJITThisAgreesWithTheInterpreter is the same thing end to end, so that a
// future template for `this` is checked against behaviour rather than against
// the refusal above.
func TestJITThisAgreesWithTheInterpreter(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"constructor-assigns", `
			function P(v) { this.v = v; this.n = 0; }
			P.prototype.bump = function () { this.n = this.n + 1; return this.n; };
			var s = 0;
			for (var k = 0; k < 60; k++) { var p = new P(k); p.bump(); p.bump(); s += p.v + p.n; }
			s;`},
		{"method-on-a-branch", `
			function P(v) { this.v = v; }
			P.prototype.pick = function (a) { if (a > 0) { this.v = a; } return this.v; };
			var s = 0, p = new P(1);
			for (var k = 0; k < 60; k++) s += p.pick(k - 30);
			s;`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// TestJITDeadZoneLocalIsNotNumeric pins the analysis half directly, because the
// end-to-end tests above would also pass if the emitter simply refused
// everything that reads one.
func TestJITDeadZoneLocalIsNotNumeric(t *testing.T) {
	const src = "function f(a){ var x; if (a > 0) { x = 2; } var y = x; return y * 3; }"
	_, fn := jitFnRT(t, src)
	targets, ok := jitScanTargets(fn, fn.startIP)
	if !ok {
		t.Fatal("could not scan targets")
	}
	blocks, ok := jitAnalyze(fn, fn.startIP, targets)
	if !ok {
		t.Fatal("could not build blocks")
	}
	unproven := jitUnprovenLocals(fn, blocks)
	any := false
	for _, u := range unproven {
		any = any || u
	}
	if !any {
		t.Fatal("no local was found unproven; this function no longer exercises the analysis")
	}
	demand, ok := jitNumberDemand(fn, blocks)
	if !ok {
		t.Fatal("demand analysis failed")
	}
	numeric, ok := jitNumericLocals(fn, blocks, demand)
	if !ok {
		t.Fatal("numeric analysis failed")
	}
	for i, u := range unproven {
		if u && numeric[i] {
			t.Errorf("local %d is read before it is proven written and is still marked numeric", i)
		}
	}
}

// TestJITReadsAnUpvalue covers the last thing a frame carries that compiled code
// could not see.
//
// NavierStokes compiles 0.4% of its frame entries — GET_UPVAL refuses fifteen of
// its functions on its own — so what the tier was worth there was the tiering
// check and nothing else. A captured binding is a pointer to wherever it lives,
// which is a frame's locals slot while that frame is open and a cell of its own
// once it has closed; reading one is the same load either way, and both are
// exercised here.
func TestJITReadsAnUpvalue(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"an-open-upvalue", `
			function outer(n) {
				var base = n * 10;
				function inner(a) { return base + a; }
				var s = 0;
				for (var k = 0; k < 60; k++) s += inner(k);
				return s;
			}
			var t = 0; for (var j = 0; j < 20; j++) t += outer(j); t;`},
		{"a-closed-upvalue", `
			function make(n) { var base = n * 10; return function (a) { return base + a; }; }
			var f = make(7), s = 0;
			for (var k = 0; k < 200; k++) s += f(k);
			s;`},
		{"written-through-after-capture", `
			function make() { var n = 0; return [function () { n = n + 1; }, function () { return n; }]; }
			var p = make(), s = 0;
			for (var k = 0; k < 200; k++) { p[0](); s = p[1](); }
			s;`},
		{"two-closures-over-one-binding", `
			function make() { var n = 5; return [function () { return n * 2; }, function () { return n * 3; }]; }
			var p = make(), s = 0;
			for (var k = 0; k < 200; k++) s = p[0]() + p[1]();
			s;`},
		{"a-const-in-its-dead-zone", `
			function make() {
				var g = function () { return c; };
				var caught = 0;
				for (var k = 0; k < 60; k++) { try { g(); } catch (e) { caught++; } }
				const c = 1;
				return caught;
			}
			make();`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}

// TestJITCompilesAnUpvalueRead is what stops the agreement above from being the
// interpreter agreeing with itself.
func TestJITCompilesAnUpvalueRead(t *testing.T) {
	rt := New()
	saved := jitEnabled
	jitEnabled = true
	defer func() { jitEnabled = saved }()

	v, err := rt.RunString("upval.js", `
		function make(n) { var base = n; return function (a) { return base + a; }; }
		var f = make(1), s = 0;
		for (var k = 0; k < 200; k++) s += f(k);
		f;`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	o := rt.objPtr(v)
	if o == nil || o.clPtr == nil || o.clPtr.fn == nil {
		t.Fatal("the script's last expression was not a closure")
	}
	if o.clPtr.fn.jit.code == nil {
		t.Error("a function whose only unusual opcode is GET_UPVAL was not compiled")
	}
}
