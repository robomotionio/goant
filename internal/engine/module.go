package engine

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ES module records and the host loader.
//
// A module's top-level bindings are frame locals (see compileProgram), and the
// frame's `locals` slice is allocated once at a fixed size — so keeping a
// reference to it after evaluation gives every importer a *live* view of those
// bindings, which is exactly what ES module semantics require. A module record
// therefore holds that slice plus a name->slot export table; a namespace object
// and an `import` binding both read straight through it.

type moduleStatus int

// The spec's [[Status]] values, minus the linking ones goant resolves eagerly.
// "evaluated with an [[EvaluationError]]" is split out as modErrored because a
// failed module is remembered and rethrown to every later importer.
const (
	modNew moduleStatus = iota // spec: linked
	modEvaluating
	modEvaluatingAsync
	modEvaluated
	modErrored
)

// settled reports whether evaluation of this module has finished, successfully
// or not — the spec's "evaluating-async or evaluated" test, which also covers
// modErrored since that is an evaluated module carrying an error.
func (s moduleStatus) settled() bool {
	return s == modEvaluatingAsync || s == modEvaluated || s == modErrored
}

type moduleRecord struct {
	path   string // resolved absolute path; the registry key
	fn     *svFunc
	locals []Value // the evaluated frame's locals, kept live for importers
	// exports maps an export name to the local slot holding it. Slots are
	// resolved at compile time from the module's top-level bindings.
	exports   map[string]int
	indirect  map[string]indirectExport // `export … from` re-exports
	starFrom  []string                  // specifiers of `export * from` re-exports
	namespace Value
	hasNS     bool // namespace built; the zero Value decodes as 0, not undefined
	// deferredNS is the namespace an `import defer` hands back -- a different
	// object from the one above, since the two are observably not equal.
	deferredNS    Value
	hasDeferredNS bool
	hoisted       bool // InitializeEnvironment has run; m.locals is the environment
	status        moduleStatus
	evalErr       *ThrowError
	// evalPromise/evalCap are [[TopLevelCapability]]: present only on a module
	// Evaluate() was called on directly (an entry point or a dynamic import), and
	// settled when that module's whole subgraph has finished.
	evalPromise Value
	evalCap     *object

	// Tarjan bookkeeping for InnerModuleEvaluation. Modules that are mutually
	// reachable form one strongly connected component and must be treated as a
	// unit: the component's root carries everyone's completion, so a cycle
	// containing a top-level await makes the WHOLE cycle async.
	dfsIndex         int
	dfsAncestorIndex int
	cycleRoot        *moduleRecord
	// asyncEvaluation marks a module that cannot finish synchronously — it has
	// top-level await, or it depends on something that does. asyncEvalOrder is
	// the agent-wide sequence number it acquired, which orders the modules that
	// become runnable together.
	asyncEvaluation bool
	asyncEvalOrder  uint64
	// asyncParents are the modules waiting on this one; pendingAsyncDeps counts
	// how many async dependencies this module is still waiting for.
	asyncParents     []*moduleRecord
	pendingAsyncDeps int
}

// root returns the module that owns this one's evaluation — itself unless it
// belongs to a cycle, in which case it is the cycle's root.
func (m *moduleRecord) root() *moduleRecord {
	if m.cycleRoot != nil {
		return m.cycleRoot
	}
	return m
}

// exportValue reads an export's current value. A lexical binding that has not
// been initialised yet holds the empty Value — its temporal dead zone — which
// callers surface as a ReferenceError rather than undefined.
func (m *moduleRecord) exportValue(name string) (Value, bool) {
	slot, ok := m.exports[name]
	if !ok {
		return mkundef(), false
	}
	if slot < 0 || slot >= len(m.locals) {
		// The module's frame has not started yet (it is a dependency in a cycle, or
		// evaluation has not reached it): every binding is still uninitialised.
		return tEmpty, true
	}
	return m.locals[slot], true
}

// exportNames lists every export a module provides, including the ones reached
// through `export * from`, sorted as the namespace object's key order requires.
func (rt *Runtime) exportNames(m *moduleRecord, seen map[string]bool) []string {
	if seen == nil {
		seen = map[string]bool{}
	}
	if seen[m.path] {
		return nil
	}
	seen[m.path] = true
	names := make([]string, 0, len(m.exports)+len(m.indirect))
	for n := range m.exports {
		names = append(names, n)
	}
	for n := range m.indirect {
		names = append(names, n)
	}
	for _, spec := range m.starFrom {
		dep, ok := rt.modules[rt.resolveSpecifier(spec, m.path)]
		if !ok {
			continue
		}
		// `export *` does not re-export `default`.
		for _, n := range rt.exportNames(dep, seen) {
			if n != "default" {
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	return names
}

// resolveExport finds the module that actually owns an export name, following
// `export * from` chains. Two star-exports providing the same name from
// different modules make it ambiguous, which is a link error rather than an
// arbitrary winner.
func (rt *Runtime) resolveExport(m *moduleRecord, name string, seen map[string]bool) (r exportTarget, ambiguous bool) {
	if seen == nil {
		seen = map[string]bool{}
	}
	// Keyed on (module, export name), not module alone: mutually re-exporting
	// modules legally form long alternating chains (A.a -> B.a -> A.b -> B.c …),
	// and a module-only guard would abandon the chain at the first revisit.
	key := m.path + "\x00" + name
	if seen[key] {
		return exportTarget{}, false // a genuine cycle contributes nothing
	}
	seen[key] = true
	if _, ok := m.exports[name]; ok {
		return exportTarget{owner: m, localName: name}, false
	}
	// A re-export resolves through the module it names; `export * as ns from`
	// denotes that module's whole namespace object rather than one binding.
	if ind, ok := m.indirect[name]; ok {
		dep, ok := rt.modules[rt.resolveSpecifier(ind.specifier, m.path)]
		if !ok {
			return exportTarget{}, false
		}
		if ind.importName == "*" {
			return exportTarget{namespaceOf: dep, deferred: ind.deferred}, false
		}
		return rt.resolveExport(dep, ind.importName, seen)
	}
	if name == "default" {
		return exportTarget{}, false // `export *` never forwards default
	}
	for _, spec := range m.starFrom {
		dep, ok := rt.modules[rt.resolveSpecifier(spec, m.path)]
		if !ok {
			continue
		}
		found, amb := rt.resolveExport(dep, name, seen)
		if amb {
			return exportTarget{}, true
		}
		if !found.found() {
			continue
		}
		if r.found() && r != found {
			return exportTarget{}, true
		}
		r = found
	}
	return r, false
}

// exportTarget is what an export name ultimately denotes: a binding in some
// module, or another module's namespace object (`export * as ns from`).
type exportTarget struct {
	owner       *moduleRecord
	localName   string
	namespaceOf *moduleRecord
	deferred    bool // namespaceOf is a DEFERRED namespace
}

func (t exportTarget) found() bool { return t.owner != nil || t.namespaceOf != nil }

// moduleKeySep separates a specifier from its import "type" attribute in the
// single string the compiler emits and the module registry keys on. The same
// file imported with `type: "json"` and without it are DIFFERENT modules — one
// is parsed as source, the other as JSON — so the type belongs in the key.
const moduleKeySep = "\x00"

func joinModuleKey(spec, typ string) string {
	if typ == "" {
		return spec
	}
	return spec + moduleKeySep + typ
}

func splitModuleKey(key string) (spec, typ string) {
	if i := strings.Index(key, moduleKeySep); i >= 0 {
		return key[:i], key[i+len(moduleKeySep):]
	}
	return key, ""
}

// ModuleResolverFunc is how a host answers "where does this specifier live".
//
// It is given the specifier exactly as written and the path of the module doing
// the importing (empty at an entry point), and returns the source and a path.
// The path is the registry key: two importers that resolve to the same path get
// the same module instance, which is what makes a shared dependency shared.
// Returning empty source means "read that path from disk", so a resolver can map
// bare specifiers onto real files without also taking over reading them.
type ModuleResolverFunc func(specifier, referrer string) (source, path string, err error)

// SetModuleResolver installs the host's module resolution. Without one the
// engine resolves a specifier as a path relative to the importing module, which
// covers relative imports and nothing else — a bare `import "@scope/pkg"` has no
// meaning the engine can supply, because the meaning is the host's package
// layout, an embedded bundle, or an import map.
func (rt *Runtime) SetModuleResolver(fn ModuleResolverFunc) { rt.moduleResolver = fn }

// hostResolve asks the resolver where a specifier lives and records the answer.
//
// The import type rides along untouched: `with { type: "json" }` names a
// different module from the same file imported as source, and that distinction
// belongs to the engine rather than to the host.
func (rt *Runtime) hostResolve(spec, referrer string) *ThrowError {
	if rt.moduleResolver == nil {
		return nil
	}
	cacheKey := referrer + moduleKeySep + moduleKeySep + spec
	if rt.moduleKeys != nil {
		if _, ok := rt.moduleKeys[cacheKey]; ok {
			return nil
		}
	}
	bare, typ := splitModuleKey(spec)
	ref, _ := splitModuleKey(referrer)
	src, path, err := rt.moduleResolver(bare, ref)
	if err != nil {
		return rt.typeError("cannot resolve module '" + bare + "': " + err.Error())
	}
	if path == "" {
		return rt.typeError("cannot resolve module '" + bare + "': resolver returned no path")
	}
	key := joinModuleKey(path, typ)
	if rt.moduleKeys == nil {
		rt.moduleKeys = map[string]string{}
	}
	rt.moduleKeys[cacheKey] = key
	if src != "" {
		if rt.moduleSources == nil {
			rt.moduleSources = map[string]string{}
		}
		// Only for a module not yet loaded: a resolver returning source for a path
		// already in the registry is describing a module that is already running,
		// and replacing its text now would change nothing but the next reload.
		if _, loaded := rt.modules[key]; !loaded {
			rt.moduleSources[key] = src
		}
	}
	return nil
}

// resolveSpecifier turns an import specifier into an absolute path, relative to
// the importing module's directory (or the process cwd at the entry point). The
// import type rides along, so the result is the registry key.
func (rt *Runtime) resolveSpecifier(key, referrer string) string {
	// What the host resolver said, if it was asked. Every later lookup of the
	// same import — linking, evaluation, the deferred-namespace walk — has to
	// land on the same record, so the answer is remembered rather than recomputed.
	if rt.moduleKeys != nil {
		if p, ok := rt.moduleKeys[referrer+moduleKeySep+moduleKeySep+key]; ok {
			return p
		}
	}
	spec, typ := splitModuleKey(key)
	referrer, _ = splitModuleKey(referrer)
	if filepath.IsAbs(spec) {
		return joinModuleKey(filepath.Clean(spec), typ)
	}
	// A specifier is relative to the importing *module*. The entry point may be a
	// script (the harness concatenates those into a scratch file), whose path says
	// nothing about where its imports live, so anything not in the registry falls
	// back to the configured base directory.
	base := rt.moduleDir
	if referrer != "" {
		if _, isModule := rt.modules[referrer]; isModule {
			base = filepath.Dir(referrer)
		}
	}
	return joinModuleKey(filepath.Clean(filepath.Join(base, spec)), typ)
}

// moduleURL renders a module's registry key as the URL import.meta.url reports.
//
// A key is normally an absolute path, which becomes a file: URL. It is not
// always: a host resolver may key a module on something that is not a file at
// all — an entry in an embedded bundle, a generated shim — and such a key
// usually carries its own scheme. One that already looks like a URL is passed
// through, because inventing a file: path for a module that has no file would
// only mislead the code reading it.
func moduleURL(path string) string {
	path, _ = splitModuleKey(path)
	if path == "" {
		return ""
	}
	if i := strings.Index(path, ":"); i > 1 && !strings.ContainsAny(path[:i], `/\`) {
		// A scheme, unless it is a Windows drive letter — those are one character,
		// which is why the index has to be past the first.
		return path
	}
	u := filepath.ToSlash(path)
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return "file://" + u
}

// loadModule runs the three module phases in order: instantiate (parse and
// compile the whole graph), link (resolve every import), then evaluate. Linking
// is separate because an unresolvable or ambiguous import is a SyntaxError that
// must be raised *before* any module body runs.
func (rt *Runtime) loadModule(spec, referrer string) (*moduleRecord, *ThrowError) {
	m, se, e := rt.instantiateModule(spec, referrer)
	if e != nil {
		return nil, e
	}
	if se == nil {
		se = rt.linkModule(m, map[string]bool{})
	}
	if se != nil {
		// Reaching here through import(): the rejection value is a SyntaxError.
		return nil, rt.syntaxError(se.Msg)
	}
	if e := rt.hoistModuleGraph(m, map[string]bool{}); e != nil {
		return nil, e
	}
	if e := rt.evaluateModule(m); e != nil {
		return nil, e
	}
	return m, nil
}

// instantiateModule parses and compiles a module and everything it requests,
// registering each record before recursing so a cycle terminates.
//
// A dependency that will not parse is a malformed module graph, not a thrown
// exception: no code has run, so it is returned as a *SyntaxError (an early
// error) exactly like an unresolvable import. A specifier the host cannot find
// is different — that is a load failure, and its TypeError is what a dynamic
// import must reject with.
func (rt *Runtime) instantiateModule(spec, referrer string) (*moduleRecord, *SyntaxError, *ThrowError) {
	if e := rt.hostResolve(spec, referrer); e != nil {
		return nil, nil, e
	}
	path := rt.resolveSpecifier(spec, referrer)
	if rt.modules == nil {
		rt.modules = map[string]*moduleRecord{}
	}
	if m, ok := rt.modules[path]; ok {
		if m.status == modErrored {
			return nil, nil, m.evalErr
		}
		return m, nil, nil
	}
	file, typ := splitModuleKey(path)
	var src []byte
	if text, ok := rt.moduleSources[path]; ok {
		// Source the host supplied for this specifier — a module that is not on
		// disk at all.
		src = []byte(text)
	} else {
		var err error
		src, err = os.ReadFile(file)
		if err != nil {
			bare, _ := splitModuleKey(spec)
			return nil, nil, rt.typeError("cannot resolve module '" + bare + "'")
		}
	}
	if typ != "" {
		m, se, e := rt.syntheticModule(path, typ, string(src))
		if se != nil || e != nil {
			return nil, se, e
		}
		rt.modules[path] = m
		return m, nil, nil
	}
	prog, perr := parseMode(file, string(src), true, true)
	if perr != nil {
		return nil, &SyntaxError{Msg: perr.Error()}, nil
	}
	fn, cerr := rt.CompileModule(prog, path, string(src))
	if cerr != nil {
		return nil, &SyntaxError{Msg: cerr.Error()}, nil
	}
	m := newModuleRecord(path, fn)
	rt.modules[path] = m
	for _, req := range m.requestedSpecifiers() {
		if _, se, e := rt.instantiateModule(req, path); se != nil || e != nil {
			return nil, se, e
		}
	}
	return m, nil, nil
}

// linkModule checks that every import in the graph names an export that exists
// and is unambiguous.
func (rt *Runtime) linkModule(m *moduleRecord, seen map[string]bool) *SyntaxError {
	if seen[m.path] {
		return nil
	}
	seen[m.path] = true
	// Every requested module is linked, including one imported purely for its
	// side effects (`import "m"`), which binds nothing and so appears in no
	// moduleImports entry.
	if m.fn == nil {
		return nil // a synthetic module has no imports to link
	}
	for _, req := range m.fn.moduleRequests {
		dep, ok := rt.modules[rt.resolveSpecifier(req.key, m.path)]
		if !ok {
			return &SyntaxError{Msg: "cannot resolve module '" + req.key + "'"}
		}
		if e := rt.linkModule(dep, seen); e != nil {
			return e
		}
	}
	for _, imp := range m.fn.moduleImports {
		dep, ok := rt.modules[rt.resolveSpecifier(imp.specifier, m.path)]
		if !ok {
			return &SyntaxError{Msg: "cannot resolve module '" + imp.specifier + "'"}
		}
		if imp.importName != "" { // a namespace import needs no named export
			target, ambiguous := rt.resolveExport(dep, imp.importName, nil)
			if ambiguous {
				return &SyntaxError{Msg: "ambiguous export '" + imp.importName + "' in '" + imp.specifier + "'"}
			}
			if !target.found() {
				return &SyntaxError{Msg: "module '" + imp.specifier + "' has no export named '" + imp.importName + "'"}
			}
		}
		if e := rt.linkModule(dep, seen); e != nil {
			return e
		}
	}
	for _, ind := range m.indirect {
		dep, ok := rt.modules[rt.resolveSpecifier(ind.specifier, m.path)]
		if !ok {
			return &SyntaxError{Msg: "cannot resolve module '" + ind.specifier + "'"}
		}
		if ind.importName != "*" {
			target, ambiguous := rt.resolveExport(dep, ind.importName, nil)
			if ambiguous {
				return &SyntaxError{Msg: "ambiguous export '" + ind.importName + "' in '" + ind.specifier + "'"}
			}
			if !target.found() {
				return &SyntaxError{Msg: "module '" + ind.specifier + "' has no export named '" + ind.importName + "'"}
			}
		}
		if e := rt.linkModule(dep, seen); e != nil {
			return e
		}
	}
	for _, spec := range m.starFrom {
		if dep, ok := rt.modules[rt.resolveSpecifier(spec, m.path)]; ok {
			if e := rt.linkModule(dep, seen); e != nil {
				return e
			}
		}
	}
	return nil
}

// newModuleRecord builds the record for a compiled module.
func newModuleRecord(path string, fn *svFunc) *moduleRecord {
	m := &moduleRecord{
		path: path, fn: fn,
		exports:  fn.moduleExports,
		indirect: fn.moduleIndirect,
		starFrom: fn.moduleStarFrom,
	}
	if m.exports == nil {
		m.exports = map[string]int{}
	}
	return m
}

// requestedSpecifiers lists every module this one names, from both its imports
// and its `export * from` re-exports.
func (m *moduleRecord) requestedSpecifiers() []string {
	if m.fn == nil {
		return nil // a synthetic module (JSON) requests nothing
	}
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, req := range m.fn.moduleRequests {
		add(req.key)
	}
	for _, imp := range m.fn.moduleImports {
		add(imp.specifier)
	}
	for _, ind := range m.indirect {
		add(ind.specifier)
	}
	for _, s := range m.starFrom {
		add(s)
	}
	return out
}

// requestedWith lists [[RequestedModules]] with the phase each was asked for,
// in source order and WITHOUT deduplication. The duplicates are the point: the
// same module may be named twice in two phases, and it is the eager naming that
// decides where it evaluates.
func (m *moduleRecord) requestedWith() []moduleRequest {
	if m.fn == nil {
		return nil // a synthetic module (JSON, text) requests nothing
	}
	out := append([]moduleRequest(nil), m.fn.moduleRequests...)
	named := map[string]bool{}
	for _, r := range out {
		named[r.key] = true
	}
	// Belt and braces: every import binding and re-export is already a request,
	// but anything that somehow is not is an eager one.
	add := func(s string) {
		if !named[s] {
			named[s] = true
			out = append(out, moduleRequest{key: s})
		}
	}
	for _, imp := range m.fn.moduleImports {
		add(imp.specifier)
	}
	for _, ind := range m.indirect {
		add(ind.specifier)
	}
	for _, s := range m.starFrom {
		add(s)
	}
	return out
}

// gatherAsyncDeps is GatherAsynchronousTransitiveDependencies: the modules under
// a deferred one that cannot be left until later. A top-level await has to be
// started while there is still a turn of the event loop to finish it in, and the
// touch that triggers a deferred module is an ordinary property read with no way
// to wait -- so an async module below a deferred one runs anyway, eagerly, and
// only the synchronous ones wait.
//
// A module with a top-level await ends the walk and is itself the answer:
// evaluating it will reach everything below it.
func (rt *Runtime) gatherAsyncDeps(m *moduleRecord, visited map[*moduleRecord]bool) []*moduleRecord {
	if m == nil || visited[m] || m.fn == nil || m.status.settled() {
		return nil
	}
	visited[m] = true
	if m.fn.usesAwait {
		return []*moduleRecord{m}
	}
	var out []*moduleRecord
	for _, req := range m.requestedWith() {
		dep, ok := rt.modules[rt.resolveSpecifier(req.key, m.path)]
		if !ok {
			continue
		}
		out = append(out, rt.gatherAsyncDeps(dep, visited)...)
	}
	return out
}

// evaluationList is what InnerModuleEvaluation actually walks: the modules this
// one evaluates, each named once, in source order. An eager request contributes
// the module it names; a deferred one contributes not that module but the
// asynchronous dependencies underneath it.
func (rt *Runtime) evaluationList(m *moduleRecord) []*moduleRecord {
	var out []*moduleRecord
	seen := map[*moduleRecord]bool{}
	add := func(d *moduleRecord) {
		if d != nil && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, req := range m.requestedWith() {
		dep, ok := rt.modules[rt.resolveSpecifier(req.key, m.path)]
		if !ok {
			continue
		}
		if req.deferred {
			for _, a := range rt.gatherAsyncDeps(dep, map[*moduleRecord]bool{}) {
				add(a)
			}
			continue
		}
		add(dep)
	}
	return out
}

// hoistModuleGraph runs InitializeEnvironment for every module in the graph,
// which is the second half of linking: each module's environment is created and
// its imports, temporal dead zones and hoisted function declarations are
// installed BEFORE any body runs. That is what lets a cyclic importer whose body
// runs first call a function the module it is importing from has not reached
// yet — the ordinary `export function` mutual-recursion pattern.
func (rt *Runtime) hoistModuleGraph(m *moduleRecord, seen map[string]bool) *ThrowError {
	if seen[m.path] {
		return nil
	}
	seen[m.path] = true
	for _, req := range m.requestedSpecifiers() {
		if dep, ok := rt.modules[rt.resolveSpecifier(req, m.path)]; ok {
			if e := rt.hoistModuleGraph(dep, seen); e != nil {
				return e
			}
		}
	}
	if m.hoisted || m.fn == nil || m.fn.moduleHoistFn == nil {
		return nil
	}
	m.hoisted = true
	// runFrame publishes the frame's locals slice to the pending record: those
	// slots are the module environment, and the body frame adopts the same slice.
	rt.pendingModule = m
	_, e := rt.runFrame(m.fn.moduleHoistFn, nil, mkundef(), mkundef(), nil)
	rt.pendingModule = nil
	return e
}

// evaluateModule runs a module graph for a host that needs a synchronous answer
// (the entry point, ShadowRealm.importValue): it starts evaluation and drives
// the loop until the graph settles. A module that suspends on a top-level await
// nothing ever resolves simply runs the host out of work, which is what a real
// host observes too.
func (rt *Runtime) evaluateModule(m *moduleRecord) *ThrowError {
	p := rt.moduleEvaluate(m)
	rt.runEventLoop()
	if po := rt.objPtr(p); po != nil && po.promise() != nil && po.promise().state == 2 {
		if m.evalErr == nil {
			m.evalErr = &ThrowError{Value: po.promise().value, rt: rt}
		}
		return m.evalErr
	}
	return nil
}

// moduleEvaluate is Evaluate() (16.2.1.5.3): it returns a promise for this
// module's completion WITHOUT draining the loop, so the graph advances on the
// microtask queue and a suspended module does not block its siblings.
func (rt *Runtime) moduleEvaluate(m *moduleRecord) Value {
	// A module that has already finished reports through its cycle's root — the
	// record that owns the whole component's completion.
	if m.status.settled() {
		m = m.root()
	}
	if m.evalCap != nil {
		return m.evalPromise
	}
	m.evalPromise, m.evalCap = rt.makePromise()
	stack, _, err := rt.innerModuleEvaluation(m, nil, 0)
	if err != nil {
		// Everything still on the stack is in the failed component, or an ancestor
		// of it, and records the same [[EvaluationError]].
		for _, x := range stack {
			x.status, x.evalErr = modErrored, err
		}
		m.status, m.evalErr = modErrored, err
		rt.rejectPromise(m.evalCap, err.Value)
	} else if !m.asyncEvaluation {
		rt.resolvePromise(m.evalPromise, m.evalCap, mkundef())
	}
	return m.evalPromise
}

// innerModuleEvaluation is InnerModuleEvaluation: one Tarjan walk that both
// evaluates in depth-first post-order and groups the graph into strongly
// connected components. Modules that can reach each other must be treated as a
// unit — the component's root carries everyone's completion — which is what
// makes a cycle containing a top-level await suspend as a whole rather than
// letting one member of it appear to have finished.
//
// `index` is the DFS counter; `stack` holds the modules of the component being
// built, and is returned so an error can name everything left unfinished.
func (rt *Runtime) innerModuleEvaluation(m *moduleRecord, stack []*moduleRecord, index int) ([]*moduleRecord, int, *ThrowError) {
	switch {
	case m.status == modErrored:
		return stack, index, m.evalErr
	case m.status.settled(), m.status == modEvaluating:
		// Already done, in flight, or a back edge into the component being built.
		return stack, index, nil
	}
	if m.fn == nil {
		m.status = modEvaluated // a synthetic module (JSON) has no body to run
		return stack, index, nil
	}
	m.status = modEvaluating
	m.dfsIndex, m.dfsAncestorIndex = index, index
	m.pendingAsyncDeps = 0
	index++
	stack = append(stack, m)

	for _, dep := range rt.evaluationList(m) {
		var err *ThrowError
		if stack, index, err = rt.innerModuleEvaluation(dep, stack, index); err != nil {
			return stack, index, err
		}
		if dep.status == modEvaluating {
			// A back edge: dep is still on the stack, so m belongs to its component.
			if dep.dfsAncestorIndex < m.dfsAncestorIndex {
				m.dfsAncestorIndex = dep.dfsAncestorIndex
			}
		} else {
			dep = dep.root()
			if dep.status == modErrored {
				return stack, index, dep.evalErr
			}
		}
		if dep.asyncEvaluation {
			m.pendingAsyncDeps++
			dep.asyncParents = append(dep.asyncParents, m)
		}
	}

	switch {
	case m.pendingAsyncDeps > 0 || m.fn.usesAwait:
		// [[HasTLA]] is STATIC: `if (false) await x` still makes the module an
		// async-evaluating one, and the order it acquires here is what sequences
		// the modules that later become runnable together.
		rt.asyncEvalOrder++
		m.asyncEvaluation, m.asyncEvalOrder = true, rt.asyncEvalOrder
		if m.pendingAsyncDeps == 0 {
			rt.executeAsyncModule(m)
		}
	default:
		if err := rt.executeModuleBody(m); err != nil {
			return stack, index, err
		}
	}

	// m roots its component (nothing below it reached higher): pop the component
	// and publish it. A member is only "evaluated" if it is not waiting on a
	// top-level await, its own or one inherited from the component.
	if m.dfsAncestorIndex == m.dfsIndex {
		for {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if last.asyncEvaluation {
				last.status = modEvaluatingAsync
			} else {
				last.status = modEvaluated
			}
			last.cycleRoot = m
			if last == m {
				break
			}
		}
	}
	return stack, index, nil
}

// executeModuleBody runs a module that has no top-level await. Its body cannot
// suspend, so the coroutine either completes or throws before genDrive returns.
func (rt *Runtime) executeModuleBody(m *moduleRecord) *ThrowError {
	// runFrame publishes the frame's locals slice to the pending record as the
	// frame starts — before any nested import can run and claim the slot.
	rt.pendingModule = m
	res := rt.genDrive(rt.newGenState(m.fn, nil, mkundef(), mkundef(), nil), genNext, mkundef())
	rt.pendingModule = nil
	return res.err
}

// executeAsyncModule is ExecuteAsyncModule: the body runs as a coroutine and the
// promise for its completion decides whether the module fulfils or rejects.
func (rt *Runtime) executeAsyncModule(m *moduleRecord) {
	rt.pendingModule = m
	body := rt.runAsync(m.fn, nil, mkundef(), mkundef(), nil)
	rt.pendingModule = nil
	bo := rt.objPtr(body)
	if bo == nil || bo.promise() == nil {
		rt.asyncModuleExecutionFulfilled(m)
		return
	}
	rt.promiseThen(
		rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			rt.asyncModuleExecutionFulfilled(m)
			return mkundef(), nil
		}),
		rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			rt.asyncModuleExecutionRejected(m, arg(a, 0))
			return mkundef(), nil
		}),
		bo)
}

// gatherAvailableAncestors collects the modules that m finishing has unblocked.
// Each async parent loses one pending dependency; one that reaches zero joins
// the list, and if it has no top-level await of its own the walk continues
// through it, because it will run to completion within the same job.
func (rt *Runtime) gatherAvailableAncestors(m *moduleRecord, execList *[]*moduleRecord) {
	for _, p := range m.asyncParents {
		if p.root().status == modErrored || containsModule(*execList, p) {
			continue
		}
		p.pendingAsyncDeps--
		if p.pendingAsyncDeps == 0 {
			*execList = append(*execList, p)
			if !p.fn.usesAwait {
				rt.gatherAvailableAncestors(p, execList)
			}
		}
	}
}

func containsModule(list []*moduleRecord, m *moduleRecord) bool {
	for _, x := range list {
		if x == m {
			return true
		}
	}
	return false
}

// asyncModuleExecutionFulfilled settles a finished async module and then runs
// every ancestor its completion unblocked, oldest [[AsyncEvaluationOrder]]
// first — the order in which those ancestors started waiting. That ordering is
// observable: it is why a leaf's importers settle leaf-to-root.
func (rt *Runtime) asyncModuleExecutionFulfilled(m *moduleRecord) {
	if m.status == modEvaluated || m.status == modErrored {
		return // already settled, by a rejection travelling the other way
	}
	m.asyncEvaluation = false
	m.status = modEvaluated
	if m.evalCap != nil {
		rt.resolvePromise(m.evalPromise, m.evalCap, mkundef())
	}
	var execList []*moduleRecord
	rt.gatherAvailableAncestors(m, &execList)
	sort.SliceStable(execList, func(i, j int) bool {
		return execList[i].asyncEvalOrder < execList[j].asyncEvalOrder
	})
	for _, p := range execList {
		switch {
		case p.status == modEvaluated || p.status == modErrored:
			// settled underneath us while this list was being worked through
		case p.fn.usesAwait:
			rt.executeAsyncModule(p)
		default:
			if err := rt.executeModuleBody(p); err != nil {
				rt.asyncModuleExecutionRejected(p, err.Value)
				continue
			}
			p.asyncEvaluation = false
			p.status = modEvaluated
			if p.evalCap != nil {
				rt.resolvePromise(p.evalPromise, p.evalCap, mkundef())
			}
		}
	}
}

// asyncModuleExecutionRejected fails a module and, transitively, everything
// waiting on it.
func (rt *Runtime) asyncModuleExecutionRejected(m *moduleRecord, err Value) {
	if m.status == modEvaluated || m.status == modErrored {
		return
	}
	m.evalErr = &ThrowError{Value: err, rt: rt}
	m.status = modErrored
	m.asyncEvaluation = false
	// This module's own waiters are told BEFORE its ancestors are failed, so a
	// rejection surfaces leaf-to-root: whoever imported the module that actually
	// threw learns of it first.
	if m.evalCap != nil {
		rt.rejectPromise(m.evalCap, err)
	}
	for _, p := range m.asyncParents {
		rt.asyncModuleExecutionRejected(p, err)
	}
}

// moduleNamespace returns the module's namespace exotic object, creating it on
// first use. Each export is an accessor so reads see the current binding value;
// the object is sealed against new properties and tagged "Module".
func (rt *Runtime) moduleNamespace(m *moduleRecord) Value {
	if m.hasNS {
		return m.namespace
	}
	ns := rt.newObject(mknull())
	no := rt.objPtr(ns)
	for _, name := range rt.exportNames(m, nil) {
		target, ambiguous := rt.resolveExport(m, name, nil)
		if ambiguous || !target.found() {
			continue // an ambiguous name is simply absent from the namespace
		}
		t, exported := target, name
		get := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if t.namespaceOf != nil {
				if t.deferred {
					return rt.deferredNamespace(t.namespaceOf), nil
				}
				return rt.moduleNamespace(t.namespaceOf), nil
			}
			v, _ := t.owner.exportValue(t.localName)
			if v.IsEmpty() {
				// The binding exists on the namespace but is still in its temporal
				// dead zone: the property is observable, reading it is not.
				return mkundef(), rt.referenceError("Cannot access '" + exported + "' before initialization")
			}
			return v, nil
		})
		// Enumerable, so Object.keys lists it; no setter, so a write is a no-op
		// in sloppy code and a TypeError in strict code.
		no.defineAccessor(name, get, mkundef(), true, false, attrEnumerable)
	}
	if rt.symToStringTag != 0 {
		no.defineOwnSymbol(rt.symToStringTag.handle(), rt.internString("Module"), 0)
	}
	no.flags.extensible = false
	if rt.moduleNamespaces == nil {
		rt.moduleNamespaces = map[*object]bool{}
	}
	rt.moduleNamespaces[no] = true
	m.namespace, m.hasNS = ns, true
	return ns
}

// importModuleNamespace is the runtime half of a STATIC import: it hands back
// the namespace object, which the importing frame keeps in a hidden local so
// every reference to an imported name reads through it. It must not evaluate
// anything — InnerModuleEvaluation has already run every requested module (or,
// in a cycle, left it half-evaluated on purpose so the bindings are live).
func (rt *Runtime) importModuleNamespace(spec, referrer string) (Value, *ThrowError) {
	if m, ok := rt.modules[rt.resolveSpecifier(spec, referrer)]; ok {
		if m.status == modErrored {
			return mkundef(), m.evalErr
		}
		return rt.moduleNamespace(m), nil
	}
	m, e := rt.loadModule(spec, referrer)
	if e != nil {
		return mkundef(), e
	}
	return rt.moduleNamespace(m), nil
}

// importModuleNamespaceDeferred is the runtime half of `import defer * as ns`.
// The module is in the registry already -- instantiating and linking the graph
// happens for a deferred request like any other -- so this only has to hand back
// the namespace that will run it.
func (rt *Runtime) importModuleNamespaceDeferred(spec, referrer string) (Value, *ThrowError) {
	m, ok := rt.modules[rt.resolveSpecifier(spec, referrer)]
	if !ok {
		var e *ThrowError
		if m, e = rt.loadModule(spec, referrer); e != nil {
			return mkundef(), e
		}
	}
	if m.status == modErrored {
		return mkundef(), m.evalErr
	}
	return rt.deferredNamespace(m), nil
}

// importModuleDynamic is ContinueDynamicImport: load and link the graph, then
// return a promise for the namespace that settles when evaluation does. Unlike
// a static import this must NOT drive the loop — the importer is itself running
// on it, and a nested drain would let the import preempt the depth-first order
// the rest of the graph is still being evaluated in.
func (rt *Runtime) importModuleDynamic(spec, referrer string) Value {
	promise, cap := rt.makePromise()
	// HostLoadImportedModule is asynchronous, so loading, linking and evaluating
	// all happen in a LATER job. That is not an implementation detail: it is what
	// stops a dynamic import from preempting the depth-first evaluation its own
	// importer is still in the middle of.
	rt.enqueueMicrotask(func() {
		m, se, e := rt.instantiateModule(spec, referrer)
		if e != nil {
			rt.rejectPromise(cap, e.Value)
			return
		}
		if se == nil {
			se = rt.linkModule(m, map[string]bool{})
		}
		if se != nil {
			rt.rejectPromise(cap, rt.syntaxError(se.Msg).Value)
			return
		}
		if he := rt.hoistModuleGraph(m, map[string]bool{}); he != nil {
			rt.rejectPromise(cap, he.Value)
			return
		}
		done := func() { rt.resolvePromise(promise, cap, rt.moduleNamespace(m)) }
		po := rt.objPtr(rt.moduleEvaluate(m))
		if po == nil || po.promise() == nil {
			done()
			return
		}
		onDone := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			done()
			return mkundef(), nil
		})
		onFail := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			rt.rejectPromise(cap, arg(a, 0))
			return mkundef(), nil
		})
		// Both settle the import()'s promise, and hold it in a Go closure the
		// collector cannot see into. See holdCaptures.
		rt.holdCaptures(onDone, []Value{promise})
		rt.holdCaptures(onFail, []Value{promise})
		rt.promiseThen(onDone, onFail, po)
	}, promise)
	return promise
}

// importModuleDeferDynamic is ContinueDynamicImport for the deferred phase. It
// loads and links exactly as a dynamic import does and then stops: what settles
// the promise is the deferred namespace, and the only thing evaluated is what
// cannot be left until later -- the module's asynchronous transitive
// dependencies, which need a turn of the event loop that a property read has
// not got.
func (rt *Runtime) importModuleDeferDynamic(spec, referrer string) Value {
	promise, cap := rt.makePromise()
	rt.enqueueMicrotask(func() {
		m, se, e := rt.instantiateModule(spec, referrer)
		if e != nil {
			rt.rejectPromise(cap, e.Value)
			return
		}
		if se == nil {
			se = rt.linkModule(m, map[string]bool{})
		}
		if se != nil {
			rt.rejectPromise(cap, rt.syntaxError(se.Msg).Value)
			return
		}
		if he := rt.hoistModuleGraph(m, map[string]bool{}); he != nil {
			rt.rejectPromise(cap, he.Value)
			return
		}
		done := func() { rt.resolvePromise(promise, cap, rt.deferredNamespace(m)) }
		var pend []Value
		for _, d := range rt.gatherAsyncDeps(m, map[*moduleRecord]bool{}) {
			pv := rt.moduleEvaluate(d)
			if po := rt.objPtr(pv); po != nil && po.promise() != nil {
				pend = append(pend, pv)
			}
		}
		if len(pend) == 0 {
			done()
			return
		}
		left := len(pend)
		onOne := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			if left--; left == 0 {
				done()
			}
			return mkundef(), nil
		})
		onFail := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, *ThrowError) {
			rt.rejectPromise(cap, arg(a, 0))
			return mkundef(), nil
		})
		rt.holdCaptures(onOne, []Value{promise})
		rt.holdCaptures(onFail, []Value{promise})
		for _, pv := range pend {
			rt.promiseThen(onOne, onFail, rt.objPtr(pv))
		}
	}, promise)
	return promise
}

// SetModuleBase sets the directory that import specifiers resolve against when
// the importer is not itself a module (a script calling import()).
func (rt *Runtime) SetModuleBase(dir string) { rt.moduleDir = dir }

// validateImportOptions checks the second argument of a dynamic import against
// the ImportCall algorithm and returns the "type" attribute it requests. An
// attribute the host cannot honour is rejected rather than silently ignored —
// asking for `{ type: 'json' }` and getting a JavaScript module back would be
// worse than a clear failure. All failures are TypeErrors, which import() turns
// into a rejected promise.
func (rt *Runtime) validateImportOptions(options Value) (string, *ThrowError) {
	if options.IsUndefined() {
		return "", nil
	}
	if !options.IsObjectLike() {
		return "", rt.typeError("import() options must be an object")
	}
	attrs, e := rt.getField(options, "with")
	if e != nil {
		return "", e
	}
	if attrs.IsUndefined() {
		return "", nil
	}
	if !attrs.IsObjectLike() {
		return "", rt.typeError("import() 'with' must be an object")
	}
	keys, e := rt.enumerableOwnKeysE(attrs)
	if e != nil {
		return "", e
	}
	typ := ""
	for _, k := range keys {
		v, e := rt.getField(attrs, k)
		if e != nil {
			return "", e
		}
		if v.Type() != TStr {
			return "", rt.typeError("import attribute '" + k + "' must be a string")
		}
		// "type" is the only attribute key the spec gives meaning to. Any other
		// key is one the host does not support, which the spec has it ignore.
		if k == "type" {
			typ = rt.strGo(v)
			if typ != "json" && typ != "text" {
				return "", rt.typeError("import attribute type '" + typ + "' is not supported")
			}
		}
	}
	return typ, nil
}

// namespaceDescriptor reports a module namespace export the way the spec
// requires — a writable, enumerable, non-configurable DATA property — even
// though it is stored as an accessor so that reads see the live binding. An
// export still in its temporal dead zone has no value to report, so the getter's
// throw propagates.
func (rt *Runtime) namespaceDescriptor(ns Value, name string) (Value, bool, *ThrowError) {
	o := rt.objPtr(ns)
	if o == nil || !rt.moduleNamespaces[o] {
		return mkundef(), false, nil
	}
	// Asked before hasOwn, because a key this namespace does not export is still
	// a question about it: what a deferred module answers is "no such export",
	// and it has to have run to say so.
	if e := rt.deferredAt(o, name); e != nil {
		return mkundef(), false, e
	}
	if !o.hasOwn(name) {
		return mkundef(), false, nil
	}
	v, e := rt.getField(ns, name)
	if e != nil {
		return mkundef(), true, e
	}
	d := rt.newPlainObject()
	do := rt.objPtr(d)
	do.defineOwn("value", v, attrDefault)
	do.defineOwn("writable", mkbool(true), attrDefault)
	do.defineOwn("enumerable", mkbool(true), attrDefault)
	do.defineOwn("configurable", mkbool(false), attrDefault)
	return d, true, nil
}

// namespaceTDZ performs the observable part of a module namespace's
// [[GetOwnProperty]](P) for callers that only need to know whether it succeeds:
// step 4 reads the binding through [[Get]], so an export still in its temporal
// dead zone makes the whole operation a ReferenceError. Every operation built on
// [[GetOwnProperty]] — hasOwnProperty, propertyIsEnumerable, Object.keys, for-in,
// a [[Set]] whose receiver is the namespace — inherits that.
func (rt *Runtime) namespaceTDZ(ns Value, name string) *ThrowError {
	o := rt.objPtr(ns)
	if o == nil || !rt.moduleNamespaces[o] {
		return nil
	}
	if e := rt.deferredAt(o, name); e != nil {
		return e
	}
	if !o.hasOwn(name) {
		return nil
	}
	_, e := rt.getField(ns, name)
	return e
}

// namespaceTDZAll is namespaceTDZ for every own export, in key order — what an
// operation that visits all the keys (Object.keys, for-in) must do.
func (rt *Runtime) namespaceTDZAll(ns Value) *ThrowError {
	o := rt.objPtr(ns)
	if o == nil || !rt.moduleNamespaces[o] {
		return nil
	}
	// [[OwnPropertyKeys]] asks about every key at once, so it triggers whatever
	// the keys turn out to be.
	if e := rt.deferredAt(o, ""); e != nil {
		return e
	}
	for _, k := range o.ownKeys() {
		if e := rt.namespaceTDZ(ns, k); e != nil {
			return e
		}
	}
	return nil
}

// isModuleNamespace reports whether v is a module namespace exotic object.
func (rt *Runtime) isModuleNamespace(v Value) bool {
	o := rt.objPtr(v)
	return o != nil && rt.moduleNamespaces[o]
}

// namespaceDefineProperty implements [[DefineOwnProperty]] for a module
// namespace object: it never adds or alters anything, and succeeds only when the
// descriptor is a no-op — one that matches the export's existing attributes
// (writable, enumerable, non-configurable) and, if a value is given, its current
// value. Anything else is rejected, which Reflect.defineProperty reports as
// false and Object.defineProperty throws.
func (rt *Runtime) namespaceDefineProperty(ns, key, descVal Value) *ThrowError {
	if rt.isModuleNamespace(ns) && !key.IsSymbol() {
		if e := rt.deferredAt(rt.objPtr(ns), rt.strGo(key)); e != nil {
			return e
		}
	}
	if !rt.isModuleNamespace(ns) || key.IsSymbol() {
		// A symbol key is not an export name: it takes the ordinary path, which is
		// what makes @@toStringTag definable.
		return nil
	}
	// Not key.IsString(): toPropertyKey leaves an array-index key numeric for the
	// fast paths, so an integer property name would slip past a string-only test.
	name, e := rt.propKeyString(key)
	if e != nil {
		return e
	}
	cur, isExport, e2 := rt.namespaceDescriptor(ns, name)
	e = e2
	if e != nil {
		return e
	}
	if !isExport {
		// A name the module does not export cannot be created on the namespace.
		return rt.rejectDefine("cannot define property '" + name + "' on a module namespace")
	}
	sameField := func(field string, want Value) (bool, *ThrowError) {
		has, e := rt.hasPropE(descVal, field)
		if e != nil || !has {
			return true, e // absent fields impose no constraint
		}
		got, e := rt.getField(descVal, field)
		if e != nil {
			return false, e
		}
		return rt.sameValue(got, want), nil
	}
	for _, f := range []string{"get", "set"} {
		has, e := rt.hasPropE(descVal, f)
		if e != nil {
			return e
		}
		if has { // a namespace export is never an accessor
			return rt.rejectDefine("cannot redefine property '" + name + "' on a module namespace")
		}
	}
	curo := rt.objPtr(cur)
	for _, f := range []string{"value", "writable", "enumerable", "configurable"} {
		want, _ := curo.getOwn(f)
		ok, e := sameField(f, want)
		if e != nil {
			return e
		}
		if !ok {
			return rt.rejectDefine("cannot redefine property '" + name + "' on a module namespace")
		}
	}
	return nil
}

// syntheticModule builds a module record whose exports come from a host-parsed
// resource rather than from JavaScript source. A JSON module has the single
// export "default" holding JSON.parse of its text (ParseJSONModule /
// CreateDefaultExportSyntheticModule); a text module has the text itself, and
// no other attribute type is supported.
//
// It has no *svFunc: nothing to link, nothing to evaluate. The record holds the
// value directly in its locals slice, which is what exportValue reads.
func (rt *Runtime) syntheticModule(key, typ, src string) (*moduleRecord, *SyntaxError, *ThrowError) {
	if typ == "text" {
		// CreateTextModule is the file, unread: whatever a text module holds is
		// already a string, so there is no parse here and nothing that could
		// fail as an early error. A .js file imported this way is its own
		// source, and a different module from the same file imported to run.
		return &moduleRecord{
			path:    key,
			exports: map[string]int{"default": 0},
			locals:  []Value{rt.newString(src)},
			status:  modEvaluated,
		}, nil, nil
	}
	if typ != "json" {
		return nil, nil, rt.typeError("import attribute type '" + typ + "' is not supported")
	}
	p := &jsonParser{rt: rt, src: src}
	v, _, perr := p.parse()
	if perr == nil {
		p.skipWS()
		if p.pos != len(p.src) {
			perr = errUnexpectedAfterJSON
		}
	}
	// Text that will not parse is reported like a module that will not parse: an
	// early error of the resolution phase, before anything in the graph runs.
	if perr != nil {
		return nil, &SyntaxError{Msg: perr.Error()}, nil
	}
	return &moduleRecord{
		path:    key,
		exports: map[string]int{"default": 0},
		locals:  []Value{v},
		status:  modEvaluated,
	}, nil, nil
}

// errUnexpectedAfterJSON is the parse failure for trailing non-whitespace, which
// the JSON parser itself does not report (JSON.parse checks it at the call site).
var errUnexpectedAfterJSON = errors.New("Unexpected non-whitespace character after JSON")
