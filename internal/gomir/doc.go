// Package gomir is a pure-Go port of MIR (themackabu/mir fork @ cb71e1ee),
// the JIT IR and code generator underneath ant's swarm.c. It provides the IR
// core + builder + text I/O (G1), an interpreter (G2), and mir-gen native
// backends for amd64/arm64 (G3/G4). See PLAN.md Phase 9.
package gomir
