package engine

import "testing"

// hostNames is what a script can see of the host API. Every one of these is a
// capability the embedder grants, so the default answer for all of them is
// "undefined".
const hostNames = `[
	typeof globalThis.$262,
	typeof globalThis.createRealm,
	typeof globalThis.evalScript,
	typeof globalThis.$262?.detachArrayBuffer,
	typeof globalThis.$262?.createRealm,
	typeof globalThis.$262?.evalScript,
	typeof globalThis.$262?.global
].join(",")`

func hostSurface(t *testing.T, rt *Runtime) string {
	t.Helper()
	v, err := rt.RunString("host.js", hostNames)
	if err != nil {
		t.Fatalf("reading the host surface: %v", err)
	}
	return rt.strGo(v)
}

// TestHostAPIIsOffByDefault is the whole point of the gate. Before it, every
// Runtime this engine built — including the one deskbot hands a customer's
// Function node — carried $262.detachArrayBuffer.
func TestHostAPIIsOffByDefault(t *testing.T) {
	rt := New()
	if got, want := hostSurface(t, rt), "undefined,undefined,undefined,undefined,undefined,undefined,undefined"; got != want {
		t.Fatalf("bare Runtime host surface = %q, want %q", got, want)
	}
	// A realm an embedder makes is a bare one too.
	if got, want := hostSurface(t, rt.NewRealm()), "undefined,undefined,undefined,undefined,undefined,undefined,undefined"; got != want {
		t.Fatalf("NewRealm host surface = %q, want %q", got, want)
	}
}

// TestHostAPIIsCompleteWhenGranted — the conformance runner has to get all of
// it, or the cross-realm and detach tests go back to being unrunnable.
func TestHostAPIIsCompleteWhenGranted(t *testing.T) {
	rt := New()
	rt.EnableHostAPI()
	if got, want := hostSurface(t, rt), "object,function,function,function,function,function,object"; got != want {
		t.Fatalf("granted host surface = %q, want %q", got, want)
	}
	// Twice is harmless — a host that calls it in two places gets one API.
	rt.EnableHostAPI()
	if got, want := hostSurface(t, rt), "object,function,function,function,function,function,object"; got != want {
		t.Fatalf("host surface after a second grant = %q, want %q", got, want)
	}
}

// TestDetachArrayBufferNeedsTheGrant is the capability that named the task.
func TestDetachArrayBufferNeedsTheGrant(t *testing.T) {
	rt := New()
	v, err := rt.RunString("detach.js", `
		const b = new ArrayBuffer(8);
		let reached = false;
		try { $262.detachArrayBuffer(b); reached = true; } catch (e) { }
		reached + "," + b.byteLength
	`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := rt.strGo(v), "false,8"; got != want {
		t.Fatalf("detach without the grant = %q, want %q (the buffer must survive)", got, want)
	}

	granted := New()
	granted.EnableHostAPI()
	v, err = granted.RunString("detach.js", `
		const b = new ArrayBuffer(8);
		$262.detachArrayBuffer(b);
		String(b.byteLength)
	`)
	if err != nil {
		t.Fatalf("run with the grant: %v", err)
	}
	if got := granted.strGo(v); got != "0" {
		t.Fatalf("detach with the grant left byteLength %q, want 0", got)
	}
}

// TestIsHTMLDDACannotBeReachedWithoutTheGrant. Reading this getter switches the
// compiled tier off for the realm and leaves it off, so a customer script that
// touched it used to cost the robot its JIT for the life of the isolate.
func TestIsHTMLDDACannotBeReachedWithoutTheGrant(t *testing.T) {
	rt := New()
	rt.SetJITEnabled(true)
	if _, err := rt.RunString("dda.js", `typeof globalThis.$262?.IsHTMLDDA`); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rt.hasHTMLDDA {
		t.Fatal("a bare Runtime handed out an IsHTMLDDA")
	}
	if !rt.jitEnabled {
		t.Fatal("a bare Runtime lost its compiled tier to a property read")
	}

	granted := New()
	granted.SetJITEnabled(true)
	granted.EnableHostAPI()
	if _, err := granted.RunString("dda.js", `$262.IsHTMLDDA`); err != nil {
		t.Fatalf("run with the grant: %v", err)
	}
	if !granted.hasHTMLDDA {
		t.Fatal("the grant did not produce an IsHTMLDDA")
	}
	if granted.jitEnabled {
		t.Fatal("IsHTMLDDA was handed out with the tier still on")
	}
}

// TestCreateRealmCarriesTheGrantForward. The suite's cross-realm tests reach a
// second realm through $262.createRealm and expect the same API there;
// withholding it would withhold nothing, since the caller already holds it.
func TestCreateRealmCarriesTheGrantForward(t *testing.T) {
	rt := New()
	rt.EnableHostAPI()
	v, err := rt.RunString("realm.js", `
		const other = $262.createRealm();
		[
			typeof other.global.$262,
			typeof other.global.$262.detachArrayBuffer,
			typeof other.createRealm,
			other.global !== globalThis
		].join(",")
	`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := rt.strGo(v), "object,function,function,true"; got != want {
		t.Fatalf("createRealm host surface = %q, want %q", got, want)
	}
}

// TestGrantedHostAPIStillEvaluatesScripts guards the move itself: evalScript's
// defining property is that a Script's top-level var becomes a NON-configurable
// global, which is what separates it from eval.
func TestGrantedHostAPIStillEvaluatesScripts(t *testing.T) {
	rt := New()
	rt.EnableHostAPI()
	v, err := rt.RunString("evalScript.js", `
		evalScript("var fromScript = 7; let lexical = 8;");
		const d = Object.getOwnPropertyDescriptor(globalThis, "fromScript");
		[fromScript, d.configurable, evalScript("lexical")].join(",")
	`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := rt.strGo(v), "7,false,8"; got != want {
		t.Fatalf("evalScript = %q, want %q", got, want)
	}
}
