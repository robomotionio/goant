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
	indirect  map[string]indirectExport // `export … from` re-exports
	starFrom  []string                  // specifiers of `export * from` re-exports
	namespace Value
	hasNS     bool // namespace built; the zero Value decodes as 0, not undefined
	status    moduleStatus
	evalErr   *ThrowError
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
			return exportTarget{namespaceOf: dep}, false
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
}

func (t exportTarget) found() bool { return t.owner != nil || t.namespaceOf != nil }

// resolveSpecifier turns an import specifier into an absolute path, relative to
// the importing module's directory (or the process cwd at the entry point).
func (rt *Runtime) resolveSpecifier(spec, referrer string) string {
	if filepath.IsAbs(spec) {
		return filepath.Clean(spec)
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
	return filepath.Clean(filepath.Join(base, spec))
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
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, rt.typeError("cannot resolve module '" + spec + "'")
	}
	prog, perr := parseMode(path, string(src), true, true)
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
	for _, spec := range m.fn.moduleRequests {
		dep, ok := rt.modules[rt.resolveSpecifier(spec, m.path)]
		if !ok {
			return &SyntaxError{Msg: "cannot resolve module '" + spec + "'"}
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
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, spec := range m.fn.moduleRequests {
		add(spec)
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
	// Dependencies evaluate first (post-order): a module reached only through a
	// re-export emits no import opcode, so nothing else would ever run its body.
	// The modEvaluating status above makes a cycle stop here rather than recurse.
	for _, req := range m.requestedSpecifiers() {
		if dep, ok := rt.modules[rt.resolveSpecifier(req, m.path)]; ok {
			if e := rt.evaluateModule(dep); e != nil {
				m.status = modErrored
				m.evalErr = e
				return e
			}
		}
	}
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
		target, ambiguous := rt.resolveExport(m, name, nil)
		if ambiguous || !target.found() {
			continue // an ambiguous name is simply absent from the namespace
		}
		t, exported := target, name
		get := rt.newNativeFunc("get "+name, 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if t.namespaceOf != nil {
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

// SetModuleBase sets the directory that import specifiers resolve against when
// the importer is not itself a module (a script calling import()).
func (rt *Runtime) SetModuleBase(dir string) { rt.moduleDir = dir }

// validateImportOptions checks the second argument of a dynamic import against
// the ImportCall algorithm. goant supports no module types, so any attribute the
// host would have to honour is rejected rather than silently ignored — importing
// `{ type: 'json' }` and getting a JavaScript module back would be worse than a
// clear failure. All failures are TypeErrors, which import() turns into a
// rejected promise.
func (rt *Runtime) validateImportOptions(options Value) *ThrowError {
	if options.IsUndefined() {
		return nil
	}
	if !options.IsObjectLike() {
		return rt.typeError("import() options must be an object")
	}
	attrs, e := rt.getField(options, "with")
	if e != nil {
		return e
	}
	if attrs.IsUndefined() {
		return nil
	}
	if !attrs.IsObjectLike() {
		return rt.typeError("import() 'with' must be an object")
	}
	keys, e := rt.enumerableOwnKeysE(attrs)
	if e != nil {
		return e
	}
	for _, k := range keys {
		v, e := rt.getField(attrs, k)
		if e != nil {
			return e
		}
		if v.Type() != TStr {
			return rt.typeError("import attribute '" + k + "' must be a string")
		}
		// "type" is the only attribute key the spec gives meaning to; every value
		// of it names a module type goant cannot produce.
		if k == "type" {
			return rt.typeError("import attribute type '" + string(rt.strBytes(v)) + "' is not supported")
		}
	}
	return nil
}

// namespaceDescriptor reports a module namespace export the way the spec
// requires — a writable, enumerable, non-configurable DATA property — even
// though it is stored as an accessor so that reads see the live binding. An
// export still in its temporal dead zone has no value to report, so the getter's
// throw propagates.
func (rt *Runtime) namespaceDescriptor(ns Value, name string) (Value, bool, *ThrowError) {
	o := rt.objPtr(ns)
	if o == nil || !rt.moduleNamespaces[o] || !o.hasOwn(name) {
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
