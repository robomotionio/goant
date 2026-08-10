//go:build !unix

package main

// setAddressSpaceLimit is a no-op where there is no RLIMIT_AS. Windows does not
// overcommit, so an allocation this large fails on its own and takes only the
// engine with it, not the machine.
func setAddressSpaceLimit(bytes uint64) error { return nil }
