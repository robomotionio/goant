//go:build arm64 && !windows

package jitmem

import "unsafe"

// flushICache publishes freshly written code to the instruction fetcher.
//
// Required on arm64, where the data and instruction caches are not coherent:
// the stores Write made may still be sitting in the D-cache, and the core is
// entitled to fetch stale bytes from the same addresses. The sequence is the
// one ARM specifies — clean each D-cache line to the point of unification,
// barrier, invalidate each I-cache line, barrier, then flush the pipeline.
//
// Apple's libc calls this sys_icache_invalidate and glibc calls it
// __clear_cache; both are C, so doing it here keeps the package free of cgo.
func flushICache(mem []byte) {
	if len(mem) == 0 {
		return
	}
	start := uintptr(unsafe.Pointer(&mem[0]))
	flushICacheRange(start, start+uintptr(len(mem)))
}

//go:noescape
func flushICacheRange(start, end uintptr)
