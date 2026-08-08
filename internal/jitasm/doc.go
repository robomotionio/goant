// Package jitasm encodes machine instructions for the JIT.
//
// It is an encoder, not a compiler: callers choose the registers and the
// instructions, and this package turns them into bytes. That division is what
// makes a baseline JIT small — a template compiler picks a fixed register
// assignment per opcode and never needs the register allocator that a package
// with opinions would have to contain.
//
// The instruction set is deliberately narrow. swarm.c, the JIT this port is
// modelled on, emits about 25 distinct operations for the whole of JavaScript:
// moves, integer arithmetic for NaN-box tag manipulation, double arithmetic for
// the arithmetic itself, compares, conditional branches and calls. Anything
// outside that goes to a helper, which measures at 7.6ns — cheap enough for a
// slow path and far too expensive for a fast one.
package jitasm
