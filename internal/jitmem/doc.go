// Package jitmem owns executable memory for the JIT: mmap'd RX code pages,
// per-VM JIT stacks with guard pages, entry trampolines (Go asm, NOSPLIT), the
// exit/re-enter helper protocol, and back-edge fuel checks. See PLAN.md Phase 10.
package jitmem
