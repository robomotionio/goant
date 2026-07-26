// Package goant is the public embedding API for the pure-Go port of the ant
// JavaScript engine. See PLAN.md / TODO.md for the porting roadmap.
package goant

import "github.com/robomotionio/goant/internal/engine"

// Runtime is an embeddable JavaScript isolate.
type Runtime struct {
	e *engine.Runtime
}

// New creates a fresh Runtime.
func New() *Runtime {
	return &Runtime{e: engine.New()}
}

// RunString evaluates src (attributed to filename) and returns the completion
// value. Not fully wired until the interpreter lands (Phase 3).
func (rt *Runtime) RunString(filename, src string) error {
	_, err := rt.e.RunString(filename, src)
	return err
}
