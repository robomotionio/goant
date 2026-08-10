//go:build unix

package main

import "syscall"

// setAddressSpaceLimit caps this process's address space, in bytes.
//
// This is a HARD cap from the kernel, and it is deliberately not the engine's
// SetMemoryLimit. That one is a budget charged on engine cells and out-of-line
// payload, and it does not see a built-in that grows a plain Go slice — which
// is the family in TODO #38, and is what a single test262 file used to drive to
// 30.6 GB. Measured: with GOANT_HEAP_LIMIT=1024 the suite still peaked at
// 30.6 GB, unchanged, because the budget never bound.
//
// Under this limit the allocation fails instead, the Go runtime stops the
// process, and the harness records one failed test. Without it, Linux
// overcommits, the pages get touched, and the OOM killer chooses a victim —
// which on a CI runner is as likely to be the agent as the engine, and reports
// itself as "the runner has received a shutdown signal" with nothing about
// memory anywhere in the log. Windows never showed this because it does not
// overcommit: the request simply fails.
func setAddressSpaceLimit(bytes uint64) error {
	lim := syscall.Rlimit{Cur: bytes, Max: bytes}
	return syscall.Setrlimit(syscall.RLIMIT_AS, &lim)
}
