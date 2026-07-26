package v8go

import (
	"errors"

	"github.com/robomotionio/goant/internal/engine"
)

// CompilerCachedData is a compilation cache blob. Under V8 this let a fresh
// isolate skip the parse phase. goant has no serialised bytecode format, so
// CreateCodeCache returns nil and a supplied cache is reported Rejected — the
// caller's existing fallback path (recompile from source) then runs, which is
// exactly the behaviour a version-drifted V8 cache would have produced.
type CompilerCachedData struct {
	Bytes    []byte
	Rejected bool
}

// CompileOptions configures compilation.
type CompileOptions struct {
	CachedData *CompilerCachedData
}

// UnboundScript is a compiled script not yet tied to a context, so one compile
// can serve many contexts. That is the property the pooled-isolate pattern
// depends on, and it holds here: compilation resolves nothing about globals,
// which are looked up per run against the running realm.
type UnboundScript struct {
	iso    *Isolate
	script *engine.Script
	origin string
}

// CompileUnboundScript parses and compiles src.
func (i *Isolate) CompileUnboundScript(src, origin string, opts CompileOptions) (*UnboundScript, error) {
	r, err := i.runtime()
	if err != nil {
		return nil, err
	}
	// Reject any supplied cache up front: we cannot honour it, and silently
	// accepting one would let a caller believe parsing was skipped.
	if opts.CachedData != nil {
		opts.CachedData.Rejected = true
	}
	s, cerr := r.CompileScript(origin, src)
	if cerr != nil {
		return nil, asJSError(r, cerr)
	}
	return &UnboundScript{iso: i, script: s, origin: origin}, nil
}

// CreateCodeCache returns nil: there is no serialised bytecode format to
// produce. Callers already handle a nil or rejected cache, because V8 can
// refuse to produce one too.
func (u *UnboundScript) CreateCodeCache() *CompilerCachedData { return nil }

// Run executes the script in ctx and returns its completion value.
//
// The script was compiled against the isolate's root realm but runs against
// ctx's globals, which is what makes compile-once/run-many work: nothing about
// a global binding is resolved at compile time.
func (u *UnboundScript) Run(ctx *Context) (*Value, error) {
	if u == nil || ctx == nil {
		return nil, errors.New("v8go: nil script or context")
	}
	v, err := ctx.r.RunScript(u.script)
	if err != nil {
		return nil, asJSError(ctx.r, err)
	}
	return wrap(ctx.r, v), nil
}
