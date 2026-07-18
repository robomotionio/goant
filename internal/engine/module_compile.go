package engine

// Compile-time half of ES module linking: the import prologue that loads each
// requested module into a hidden local, and the export table that maps an
// exported name to the top-level slot holding it.

// moduleImport is one requested binding of a static import, kept so the link
// phase can resolve it (and reject an unresolvable or ambiguous one) before the
// module is evaluated.
type moduleImport struct {
	specifier  string
	importName string // "" for `import * as ns`
	local      string
}

// importBinding records where a locally-bound imported name reads from.
type importBinding struct {
	// modName is the hidden local holding the source module's namespace object.
	// It is kept as a name, not a slot, so a reference from a nested function
	// resolves through the ordinary upvalue-capture machinery.
	modName    string
	exportName string // "" for a namespace import (`import * as ns`)
}

// emitImportPrologue loads every statically imported module before the body
// runs, leaving each namespace object in a hidden local, and registers the local
// names those imports bind.
func (c *compiler) emitImportPrologue(stmts []*Node) {
	for _, s := range stmts {
		if s == nil || s.Kind != NImportDecl || s.Right == nil {
			continue
		}
		spec := s.Right.Str
		modName := "*mod:" + spec + "*"
		slot := c.addLocal(modName, false)
		c.emit(OpConst)
		c.emitU32(uint32(c.constant(c.rt.internString(spec))))
		c.emit(OpImportSync)
		c.emitOpU16(OpPutLocal, uint16(slot))
		for _, sp := range s.Args {
			if sp == nil || sp.Right == nil {
				continue
			}
			b := importBinding{modName: modName}
			if sp.Flags&importBindNamespace == 0 {
				b.exportName = "default"
				if sp.Left != nil {
					b.exportName = sp.Left.Str
				}
			}
			if c.importBindings == nil {
				c.importBindings = map[string]importBinding{}
			}
			c.importBindings[sp.Right.Str] = b
			c.fn.moduleImports = append(c.fn.moduleImports, moduleImport{
				specifier: spec, importName: b.exportName, local: sp.Right.Str,
			})
		}
	}
}

// compileImportRead emits the read of an imported binding: fetch the source
// module's namespace from its hidden local, then (unless this is a namespace
// import) the named export off it.
func (c *compiler) compileImportRead(b importBinding) {
	if slot := c.resolveLocal(b.modName); slot >= 0 {
		c.emitOpU16(OpGetLocal, uint16(slot))
	} else if uv := c.resolveUpvalue(b.modName); uv >= 0 {
		c.emitOpU16(OpGetUpval, uint16(uv))
	} else {
		c.errorf("unresolved module binding %s", b.modName)
		return
	}
	if b.exportName == "" {
		return
	}
	c.emit(OpImportNamed)
	c.emitU32(uint32(c.constant(c.rt.internString(b.exportName))))
}

// moduleExportEntries maps each exported name to the local binding that backs
// it. Re-export forms (`export … from`, `export *`) have no local and are
// resolved through the source module at run time instead.
func moduleExportEntries(stmts []*Node) map[string]string {
	out := map[string]string{}
	for _, s := range stmts {
		if s == nil || s.Kind != NExport {
			continue
		}
		switch {
		case s.Flags&exDefault != 0:
			if d := s.Left; d != nil && (d.Kind == NFunc || d.Kind == NClass) && d.Str != "" {
				out["default"] = d.Str
			} else {
				out["default"] = "*default*"
			}
		case s.Flags&exDecl != 0:
			names := map[string]bool{}
			moduleLocalNames(s.Left, names)
			for n := range names {
				out[n] = n
			}
		case s.Flags&exFrom == 0 && s.Flags&exNamed != 0:
			for _, spec := range s.Args {
				if spec == nil || spec.Right == nil || spec.Left == nil {
					continue
				}
				out[spec.Right.Str] = spec.Left.Str
			}
		}
	}
	return out
}

// indirectExport is an export forwarded from another module: `export { a as b }
// from "m"` re-exports m's `a`, and `export * as ns from "m"` exports m's whole
// namespace. Neither creates a local binding, so these are resolved through the
// source module at run time.
type indirectExport struct {
	specifier  string
	importName string // "*" for a namespace re-export
}

// moduleIndirectExports maps each re-exported name to where it comes from.
func moduleIndirectExports(stmts []*Node) map[string]indirectExport {
	out := map[string]indirectExport{}
	for _, s := range stmts {
		if s == nil || s.Kind != NExport || s.Flags&exFrom == 0 || s.Right == nil {
			continue
		}
		spec := s.Right.Str
		// `export * as ns from "m"` names the whole namespace; a bare `export *`
		// has no name of its own and is handled as a star re-export instead.
		if s.Flags&exStar != 0 {
			if s.Flags&exNamespace != 0 && len(s.Args) > 0 && s.Args[0].Right != nil {
				out[s.Args[0].Right.Str] = indirectExport{specifier: spec, importName: "*"}
			}
			continue
		}
		for _, spc := range s.Args {
			if spc == nil || spc.Right == nil || spc.Left == nil {
				continue
			}
			out[spc.Right.Str] = indirectExport{specifier: spec, importName: spc.Left.Str}
		}
	}
	return out
}

// moduleStarSpecifiers lists the `export * from "…"` specifiers, whose exports
// this module forwards.
func moduleStarSpecifiers(stmts []*Node) []string {
	var out []string
	for _, s := range stmts {
		// `export * as ns from "m"` names m's namespace; it does NOT forward m's
		// individual export names, so only a bare `export *` counts here.
		if s != nil && s.Kind == NExport && s.Flags&exStar != 0 &&
			s.Flags&exNamespace == 0 && s.Right != nil {
			out = append(out, s.Right.Str)
		}
	}
	return out
}

// lookupImport finds an imported binding by local name, searching outward: a
// nested function inside a module sees the module's imports.
func (c *compiler) lookupImport(name string) (importBinding, bool) {
	for e := c; e != nil; e = e.enclosing {
		if b, ok := e.importBindings[name]; ok {
			return b, true
		}
	}
	return importBinding{}, false
}

// isImportName reports whether a name is bound by a static import in this
// module (or an enclosing one, for a nested function).
func (c *compiler) isImportName(name string) bool {
	_, ok := c.lookupImport(name)
	return ok
}
