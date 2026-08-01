//go:build windows

package jitmem

// flushICache defers to the kernel on Windows, which is the documented way to
// publish generated code and the only one that stays correct if this ever runs
// on windows/arm64. On x64 the call is close to free.
func flushICache(mem []byte) {
	if len(mem) == 0 {
		return
	}
	flushWindowsICache(mem)
}
