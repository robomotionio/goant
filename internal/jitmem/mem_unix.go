//go:build linux || darwin

package jitmem

import "syscall"

// allocRW maps size bytes of anonymous, private, readable and writable memory.
//
// Not PROT_EXEC as well: on Apple Silicon a mapping that is writable and
// executable at once is refused outright unless it carries MAP_JIT, and a
// process that never asks for one needs no JIT entitlement for the mapping
// itself. Protect adds PROT_EXEC and drops PROT_WRITE once the code is written.
func allocRW(size int) ([]byte, error) {
	return syscall.Mmap(-1, 0, size,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
}

func protectRX(mem []byte) error {
	return syscall.Mprotect(mem, syscall.PROT_READ|syscall.PROT_EXEC)
}

func release(mem []byte) error { return syscall.Munmap(mem) }
