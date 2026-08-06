//go:build amd64 || arm64

package engine

import "testing"

// A method call must be given the right receiver.
//
// FIXED. This was a real, pre-existing bug in the tier — reproducing at
// 6e1a135 — found by the differential fuzzer and reduced from a generated
// program to the two cases below. All thirteen of the fuzzer's open findings
// were this one bug.
//
// `o.method(...)` reaches the runtime with something other than `o` when the
// argument expression contains another METHOD call and the operand stack reaches
// a particular depth. Found by the differential fuzzer; reduced from a generated
// program to the two cases below.
//
// The symptom depends only on what the wrong receiver happens to be:
//
//   - a FUNCTION — `push` tries to write its `length`, functions refuse, and a
//     TypeError surfaces. This is the lucky case: it is loud.
//   - a PLAIN OBJECT — `push` succeeds. It writes "0", "1", ... and "length"
//     onto an object that has nothing to do with the call. Twelve pushes leave
//     seven in the array and five in the bystander, and nothing raises.
//
// The second is the one that matters. It is silent cross-object data corruption
// in the default configuration.
//
// What the reduction established, each by removing one thing:
//
//   - operand DEPTH decides it, and periodically: nesting 3 is clean, 3-plus-one
//     corrupts, 4 raises, 5 is clean again. jitSlot is regs[i%jitStackWindow]
//     with a window of 9, so slot 0 and slot 9 share a register — the outer
//     receiver's and the inner call's.
//   - an inner METHOD call is required. The same depth built from plain calls is
//     clean, and so is an object literal in the same position.
//   - the receiver of the INNER call is what the outer call ends up with, which
//     is why `Object.create(...)` and `Object.keys(...)` trigger it (Object is a
//     function) while `Math.max(...)` does not (Math is not).
//   - the interpreter is correct at every depth.
//
// The cause: GET_FIELD2 copies the receiver up into slot sp and then called the
// runtime with a depth of sp rather than sp+1. jitSlot is regs[i%9], so slot
// sp's register IS slot sp-9's — and sp-9 is exactly where jitCallHelper starts
// spilling registers into the frame. The copy therefore overwrote slot sp-9 in
// memory, which for `o.m(...)` at that depth is the outer call's receiver.
func TestAMethodCallGetsTheRightReceiver(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		// Loud: the stolen receiver is a function, so push raises.
		{"a function receiver is not stolen", `
			var out = [];
			function f0(a, b) { return a; }
			function Holder() {}
			Holder.make = function () { return 1; };
			function body() { out.push(String(f0(1, f0(1, f0(Holder.make(), 1))))); }
			for (var r = 0; r < 20; r++) body();
			out.length;`},
		// Silent: the stolen receiver is a plain object, so push writes into it.
		{"a bystander object is not written to", `
			var out = [];
			function f0(a, b) { return a; }
			var Plain = { make: function () { return 1; } };
			function body() { out.push(String(f0(1, f0(1, f0(Plain.make(), 1))))); }
			for (var r = 0; r < 12; r++) body();
			out.length + "|" + Object.keys(Plain).join(",");`},
	} {
		t.Run(tc.name, func(t *testing.T) { jitBothWays(t, tc.name+".js", tc.src) })
	}
}
