// Package harness holds the little that the goant measurement tools share.
//
// They shell out to a goant BINARY rather than linking the engine, on purpose:
// one process per test is what keeps a crash, an OOM or a runaway loop from
// taking the whole run with it. That makes the child's environment the only
// channel between a harness and the engine it is measuring, and this package is
// about getting that channel right.
package harness

import "os"

// PinJIT states which tier the child should run instead of letting it choose.
//
// The goant CLI runs the compiled tier by DEFAULT. That is right for someone
// typing `goant script.js` and wrong for every harness here: a conformance
// percentage or a benchmark score that does not say which tier produced it is
// two numbers wearing one label. So absent becomes an explicit off, which is
// what every result in these tools' history was measured with, and the caller
// changes nothing by upgrading.
//
// When GOANT_JIT is set it is already in os.Environ and already means what it
// says, in both directions, so there is nothing to add. Deciding here by
// PRESENCE rather than by value is deliberate: the engine owns the parse of
// that value, and a second opinion about what "0" means is how this repo lost
// weeks to `GOANT_JIT=0` reading as on. See internal/engine/jit_tier.go.
//
// Pass the fully built environment; the returned slice may share its backing
// array, so use the result rather than the argument.
func PinJIT(env []string) []string {
	if _, ok := os.LookupEnv("GOANT_JIT"); ok {
		return env
	}
	return append(env, "GOANT_JIT=0")
}
