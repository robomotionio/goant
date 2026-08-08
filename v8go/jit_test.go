package v8go

import (
	"testing"

	"github.com/robomotionio/goant/internal/engine"
)

// The tier is a decision a host makes per isolate, so what these pin is the
// decision — who it applies to, which way each control goes, and that turning
// it on does not change an answer.

// processDefault pins the process-wide default for one test and puts it back.
//
// Without this these tests read GOANT_JIT out of whatever environment they were
// run in and assert against it, which is how a test comes to pass under `go
// test` and fail under the tier sweep that exists to find tier bugs.
func processDefault(t *testing.T, on bool) {
	t.Helper()
	was := engine.JITSetEnabled(on)
	t.Cleanup(func() { engine.JITSetEnabled(was) })
}

func TestTheTierIsOffUnlessTheOptionsAskForIt(t *testing.T) {
	processDefault(t, false)

	iso := NewIsolate()
	defer iso.Dispose()
	if iso.JITEnabled() {
		t.Fatal("NewIsolate got the tier without asking for it")
	}

	on := NewIsolateWithOptions(IsolateOptions{JIT: true})
	defer on.Dispose()
	if !on.JITEnabled() {
		t.Fatal("IsolateOptions.JIT did not turn the tier on")
	}

	// And it is per isolate, not per process: the one above being on says
	// nothing about the next one.
	off := NewIsolate()
	defer off.Dispose()
	if off.JITEnabled() {
		t.Fatal("one isolate's tier leaked into another")
	}
}

// The option is additive on purpose — a zero-valued struct field must not
// countermand GOANT_JIT, which a caller set deliberately and a caller filling in
// two heap sizes may not even know about. SetJIT is the control that goes both
// ways.
func TestTheOptionOnlyTurnsTheTierOn(t *testing.T) {
	processDefault(t, true)

	iso := NewIsolateWithOptions(IsolateOptions{MaxOldSpaceBytes: 1 << 20})
	defer iso.Dispose()
	if !iso.JITEnabled() {
		t.Fatal("JIT:false countermanded the process default instead of leaving it alone")
	}

	iso.SetJIT(false)
	if iso.JITEnabled() {
		t.Fatal("SetJIT(false) left the tier on")
	}
	iso.SetJIT(true)
	if !iso.JITEnabled() {
		t.Fatal("SetJIT(true) did not turn the tier back on")
	}
}

// A disposed isolate has no Runtime to ask, and neither call may panic on one:
// releaseV8 and dispose race in the caller's defer chain.
func TestTheTierControlsSurviveDisposal(t *testing.T) {
	iso := NewIsolateWithOptions(IsolateOptions{JIT: true})
	iso.Dispose()
	iso.SetJIT(true)
	if iso.JITEnabled() {
		t.Fatal("a disposed isolate reported a tier")
	}
	var nilIso *Isolate
	nilIso.SetJIT(true)
	if nilIso.JITEnabled() {
		t.Fatal("a nil isolate reported a tier")
	}
}

// The tier changes how fast a script runs, not what it computes. This is the
// same script run both ways, hot enough to have been compiled.
func TestTheTierDoesNotChangeTheAnswer(t *testing.T) {
	const script = `
		function work(n) {
			let s = 0, o = {a: 0, b: ""};
			for (let i = 0; i < n; i++) {
				o.a = i * 3 + 1;
				o.b = "x" + (i % 7);
				s += o.a + o.b.length + (i % 2 ? -i : i);
			}
			return s + "|" + [1,2,3].map(x => x * n).join(",");
		}
		work(5000);
	`
	run := func(jit bool) string {
		iso := NewIsolateWithOptions(IsolateOptions{JIT: jit})
		defer iso.Dispose()
		ctx := NewContext(iso, nil)
		defer ctx.Close()
		v, err := ctx.RunScript(script, "t.js")
		if err != nil {
			t.Fatalf("jit=%v: %v", jit, err)
		}
		return v.String()
	}
	interpreted, compiled := run(false), run(true)
	if interpreted != compiled {
		t.Fatalf("the tier changed the answer:\n interpreted %s\n compiled    %s",
			interpreted, compiled)
	}
}
