package engine

import (
	"os"
	"path/filepath"
	"sort"
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

const (
	modNew moduleStatus = iota
	modEvaluating
	modEvaluated
	modErrored
)

type moduleRecord struct {
	path   string // resolved absolute path; the registry key
	fn     *svFunc
	locals []Value // the evaluated frame's locals, kept live for importers
	// exports maps an export name to the local slot holding it. Slots are
	// resolved at compile time from the module's top-level bindings.
	exports   map[string]int
	starFrom  []string // specifiers of `export * from` re-exports
	namespace Value
	hasNS     bool // namespace built; the zero Value decodes as 0, not undefined
	status    moduleStatus
	evalErr   *ThrowError
}

// exportValue reads an export's current value, or undefined before evaluation
// assigns it (a binding in its temporal dead zone reads as undefined here; the
// namespace object is the only observer and ES only requires a throw for a `let`
// accessed before initialisation, which the local slot cannot distinguish).
func (m *moduleRecord) exportValue(name string) (Value, bool) {
	slot, ok := m.exports[name]
	if !ok {
		return mkundef(), false
	}
	if slot < 0 || slot >= len(m.locals) {
		return mkundef(), true
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
	names := make([]string, 0, len(m.exports))
	for n := range m.exports {
		names = append(names, n)
	}
	for _, spec := range m.starFrom {
		dep, err := rt.loadModule(spec, m.path)
		if err != nil {
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
// `export * from` chains.
func (rt *Runtime) resolveExport(m *moduleRecord, name string, seen map[string]bool) (*moduleRecord, bool) {
	if seen == nil {
		seen = map[string]bool{}
	}
	if seen[m.path] {
		return nil, false
	}
	seen[m.path] = true
	if _, ok := m.exports[name]; ok {
		return m, true
	}
	if name == "default" {
		return nil, false // `export *` never forwards default
	}
	for _, spec := range m.starFrom {
		dep, err := rt.loadModule(spec, m.path)
		if err != nil {
			continue
		}
		if owner, ok := rt.resolveExport(dep, name, seen); ok {
			return owner, true
		}
	}
	return nil, false
}

// resolveSpecifier turns an import specifier into an absolute path, relative to
// the importing module's directory (or the process cwd at the entry point).
func (rt *Runtime) resolveSpecifier(spec, referrer string) string {
	if filepath.IsAbs(spec) {
		return filepath.Clean(spec)
	}
	base := rt.moduleDir
	if referrer != "" {
		base = filepath.Dir(referrer)
	}
	return filepath.Clean(filepath.Join(base, spec))
}

// loadModule resolves, compiles and evaluates a module, returning the cached
// record on any later request for the same path.
func (rt *Runtime) loadModule(spec, referrer string) (*moduleRecord, *ThrowError) {
	path := rt.resolveSpecifier(spec, referrer)
	if rt.modules == nil {
		rt.modules = map[string]*moduleRecord{}
	}
	if m, ok := rt.modules[path]; ok {
		if m.status == modErrored {
			return nil, m.evalErr
		}
		return m, nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, rt.typeError("cannot resolve module '" + spec + "'")
	}
	prog, perr := parseMode(path, string(src), true, true)
	if perr != nil {
		return nil, rt.syntaxError(perr.Error())
	}
	fn, cerr := rt.CompileModule(prog, path, string(src))
	if cerr != nil {
		return nil, rt.syntaxError(cerr.Error())
	}
	m := &moduleRecord{path: path, fn: fn, exports: fn.moduleExports, starFrom: fn.moduleStarFrom}
	if m.exports == nil {
		m.exports = map[string]int{}
	}
	rt.modules[path] = m
	if e := rt.evaluateModule(m); e != nil {
		return nil, e
	}
	return m, nil
}

// evaluateModule runs a module body once. A module already being evaluated is a
// cycle: its (partly initialised) bindings are returned as-is, which is what
// makes circular imports observe live bindings rather than deadlock.
func (rt *Runtime) evaluateModule(m *moduleRecord) *ThrowError {
	if m.status != modNew {
		if m.status == modErrored {
			return m.evalErr
		}
		return nil
	}
	m.status = modEvaluating
	// runFrame publishes its locals slice to the pending record as the frame
	// starts — before any nested import can run and claim the slot.
	rt.pendingModule = m
	promise := rt.runAsync(m.fn, nil, mkundef(), mkundef(), nil)
	rt.pendingModule = nil
	rt.runEventLoop()
	if po := rt.objPtr(promise); po != nil && po.promise != nil && po.promise.state == 2 {
		m.status = modErrored
		m.evalErr = &ThrowError{Value: po.promise.value, rt: rt}
		return m.evalErr
	}
	m.status = modEvaluated
	return nil
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
		owner, ok := rt.resolveExport(m, name, nil)
		if !ok {
			continue
		}
		n, o := name, owner
		get := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			v, _ := o.exportValue(n)
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
	m.namespace, m.hasNS = ns, true
	return ns
}

// importModuleNamespace is the runtime half of a static import: it loads the
// module and hands back its namespace object, which the importing frame keeps in
// a hidden local so every reference to an imported name reads through it.
func (rt *Runtime) importModuleNamespace(spec, referrer string) (Value, *ThrowError) {
	m, e := rt.loadModule(spec, referrer)
	if e != nil {
		return mkundef(), e
	}
	return rt.moduleNamespace(m), nil
}
