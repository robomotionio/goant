package v8go

import (
	"errors"

	"github.com/robomotionio/goant/internal/engine"
)

// JSError is a JavaScript exception that escaped to the host. Callers type-
// assert on it — `err.(*JSError)` — so the shape of these fields matters as
// much as the message.
type JSError struct {
	Message    string
	Location   string
	StackTrace string
}

func (e *JSError) Error() string {
	if e == nil {
		return ""
	}
	if e.Location != "" {
		return e.Message + " at " + e.Location
	}
	return e.Message
}

// ErrTerminated reports that execution was stopped by TerminateExecution. It
// is deliberately not a *JSError: a termination is not something the script
// did, and a caller that type-asserts to *JSError to read a message should get
// a failed assertion here rather than an invented one.
var ErrTerminated = engine.ErrTerminated

// asJSError converts an engine error into the binding's error shape. A JS
// exception becomes a *JSError carrying the thrown value's message and stack;
// anything else (a parse failure, a termination) is passed through unchanged so
// errors.Is still works on it.
func asJSError(r *engine.Runtime, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, engine.ErrTerminated) {
		return err
	}
	v, ok := engine.ExceptionValue(err)
	if !ok {
		// A parse or compile failure: real, but not a thrown value. Report it as
		// a JSError so the caller's single error path still reads a message.
		return &JSError{Message: err.Error()}
	}

	out := &JSError{Message: err.Error()}
	if r.IsObject(v) {
		if m, e := r.GetProp(v, "message"); e == nil && !r.IsUndefined(m) {
			if s, e := r.ToString(m); e == nil {
				out.Message = s
			}
		}
		if name, e := r.GetProp(v, "name"); e == nil && !r.IsUndefined(name) {
			if s, e := r.ToString(name); e == nil && s != "" {
				out.Message = s + ": " + out.Message
			}
		}
		if st, e := r.GetProp(v, "stack"); e == nil && !r.IsUndefined(st) {
			if s, e := r.ToString(st); e == nil {
				out.StackTrace = s
			}
		}
	} else if s, e := r.ToString(v); e == nil {
		// A script may throw any value, not only an Error.
		out.Message = s
	}
	return out
}
