package engine

import "sync/atomic"

// withThreshold lowers the compile threshold for one test and puts it back, so
// a fixture does not have to run a function eight times to have it compiled.
//
// Untagged, unlike the emitter tests that use it: the threshold is the tier's
// and not the backend's, and a test about the compiler wants it too.
func withThreshold(n int32) func() {
	was := atomic.LoadInt32(&jitThreshold)
	atomic.StoreInt32(&jitThreshold, n)
	return func() { atomic.StoreInt32(&jitThreshold, was) }
}
