//go:build !amd64

package engine

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

func (c *jitCode) jitRun(rt *Runtime, fn *svFunc, locals []Value) (Value, *ThrowError, bool) {
	return mkundef(), nil, false
}
