//go:build !amd64 && !arm64

package engine

import "strconv"

// jitHelperNameOf has no enum to consult on a platform with no emitter.
func jitHelperNameOf(id uint64) string { return "helper#" + strconv.FormatUint(id, 10) }

// No emitter for this architecture yet, so the tier is empty and every function
// interprets. jitmem is already portable to arm64; what is missing is the
// instruction encoder, which is mechanical once the amd64 shape has settled.
//
// The interpreter is the fallback tier on every platform, so a target without a
// backend is slow, never broken.
type jitCode struct{}

func jitCompile(fn *svFunc, why *string) *jitCode {
	if why != nil {
		*why = "no-backend"
	}
	return nil
}

func (c *jitCode) jitRun(rt *Runtime, fn *svFunc, cl *closure, fnVal Value, args, locals []Value, this Value) (Value, *ThrowError, bool) {
	return mkundef(), nil, false
}

func (c *jitCode) jitRunOSR(rt *Runtime, fn *svFunc, cl *closure, fnVal Value, args, locals []Value, this Value, header int) (Value, *ThrowError, bool) {
	return mkundef(), nil, false
}

// Nothing was mapped, so there is nothing to unmap. Present so that the
// reclamation in jit_reclaim.go, which is not architecture-specific, builds here
// too rather than needing a build tag of its own.
func (c *jitCode) free() {}

// Every function is missing the same one thing here, and it is not an opcode.
func jitMissingTemplates(fn *svFunc) []string { return nil }
