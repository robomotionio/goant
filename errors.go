package goant

import (
	"errors"
	"fmt"

	"github.com/robomotionio/goant/internal/engine"
)

// Error is a JavaScript exception that reached the host.
//
// It carries the thrown value as well as its text, because a script may throw
// anything — an Error, a string, an object with fields the host is meant to
// read — and flattening that to a message throws away what the host was told.
type Error struct {
	// Name is the error's constructor name: "TypeError", "RangeError", and so
	// on. It is empty when what was thrown is not an Error.
	Name string

	// Message is the error's message, or the thrown value's string form.
	Message string

	// Stack is the JavaScript stack trace, when the thrown value has one.
	Stack string

	val Value
}

// Error renders the exception the way JavaScript would print it.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Name != "" {
		return e.Name + ": " + e.Message
	}
	return e.Message
}

// Value returns the thrown value itself, for a host that needs to inspect it
// rather than read its message. It is valid as long as its Runtime is.
func (e *Error) Value() Value {
	if e == nil {
		return Value{}
	}
	return e.val
}

// SyntaxError is a source that would not parse or compile. It is reported
// before anything runs, so nothing in the script has happened yet.
type SyntaxError struct {
	Message string

	// File and Offset locate the problem: the source name it was compiled
	// under, and a byte offset into that source.
	File   string
	Offset int
}

func (e *SyntaxError) Error() string {
	if e == nil {
		return ""
	}
	if e.File != "" {
		return fmt.Sprintf("%s:%d: SyntaxError: %s", e.File, e.Offset, e.Message)
	}
	return "SyntaxError: " + e.Message
}

// ErrInterrupted reports a script stopped from outside — by Interrupt, or by a
// context passed to WithContext reaching its deadline.
//
// It is deliberately not an *Error: an interruption is not something the script
// did, and a caller that type-asserts to read a message should get a failed
// assertion rather than an invented one.
var ErrInterrupted = errors.New("goant: execution interrupted")

// ErrMemoryLimit reports a script stopped for retaining more than the
// Runtime's memory limit (see WithMemoryLimit). It is a distinct error from
// ErrInterrupted because it is a distinct problem with a distinct fix: the
// message is too large or the script holds too much, not that it took too long.
var ErrMemoryLimit = errors.New("goant: memory limit exceeded")

// ExitError reports a script that called the host exit hook. Code is the status
// it asked for.
type ExitError struct{ Code int }

func (e *ExitError) Error() string {
	return fmt.Sprintf("goant: script exited with status %d", e.Code)
}

// wrap converts an engine error into this package's error shape.
//
// The distinctions it draws are the ones a caller acts on: a thrown value it
// can inspect, a source it could not compile, a script it stopped, and a script
// that outgrew its budget. Everything else passes through unchanged so
// errors.Is still works on it.
func (rt *Runtime) wrap(err error) error {
	if err == nil {
		return nil
	}

	// A memory-limit stop arrives as a termination, because that is how the
	// engine stops a running script. Ask the Runtime which it was before
	// reporting: the two are the same mechanism and different problems.
	if errors.Is(err, engine.ErrTerminated) {
		if e, oerr := rt.engineOf(); oerr == nil && e.HeapLimitExceeded() {
			return ErrMemoryLimit
		}
		return ErrInterrupted
	}

	var se *engine.SyntaxError
	if errors.As(err, &se) {
		return &SyntaxError{Message: se.Msg, File: se.Filename, Offset: se.Offset}
	}
	var ce *engine.CompileError
	if errors.As(err, &ce) {
		return &SyntaxError{Message: ce.Msg}
	}
	var ge *engine.GlobalDeclError
	if errors.As(err, &ge) {
		return &SyntaxError{Message: ge.Msg}
	}
	var xe *engine.ExitError
	if errors.As(err, &xe) {
		return &ExitError{Code: xe.Code}
	}

	v, ok := engine.ExceptionValue(err)
	if !ok {
		return err
	}
	return rt.thrown(v)
}

// thrown builds an *Error from a thrown JavaScript value.
func (rt *Runtime) thrown(v engine.Value) error {
	e, oerr := rt.engineOf()
	if oerr != nil {
		return oerr
	}
	out := &Error{val: rt.val(v)}

	if !e.IsObject(v) {
		// A script may throw anything: a string, a number, an object literal.
		// Report what it threw rather than inventing an Error around it.
		if s, err := e.ToString(v); err == nil {
			out.Message = s
		}
		return out
	}
	if m, err := e.GetProp(v, "message"); err == nil && !e.IsUndefined(m) {
		if s, err := e.ToString(m); err == nil {
			out.Message = s
		}
	}
	if n, err := e.GetProp(v, "name"); err == nil && !e.IsUndefined(n) {
		if s, err := e.ToString(n); err == nil {
			out.Name = s
		}
	}
	if st, err := e.GetProp(v, "stack"); err == nil && !e.IsUndefined(st) {
		if s, err := e.ToString(st); err == nil {
			out.Stack = s
		}
	}
	if out.Message == "" && out.Name == "" {
		if s, err := e.ToString(v); err == nil {
			out.Message = s
		}
	}
	return out
}
