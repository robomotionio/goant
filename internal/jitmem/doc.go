// Package jitmem owns executable memory for the JIT: page-aligned code blocks
// that are written while writable and then flipped to read-execute, an entry
// trampoline into generated code, and the protocol generated code uses to ask
// the runtime to do something it cannot do itself.
//
// Write-then-flip rather than a mapping that is writable and executable at the
// same time. Apple Silicon refuses the latter without MAP_JIT, hardened runtimes
// and W^X-enforcing kernels dislike it, and nothing here needs it: a block is
// filled once and then executed.
//
// Nothing in this package uses cgo. On Unix that is syscall.Mmap and
// syscall.Mprotect; on Windows it is kernel32 reached through syscall.LazyDLL,
// which is how the standard library itself calls Win32.
//
// See docs/jit-plan.md.
package jitmem
