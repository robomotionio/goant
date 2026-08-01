//go:build amd64 && !windows

package jitmem

// flushICache is a no-op on amd64: the instruction cache snoops writes to the
// data cache, so bytes stored through the writable mapping are visible to the
// fetcher without any maintenance. The mprotect that precedes execution is
// enough of a barrier.
func flushICache(mem []byte) {}
