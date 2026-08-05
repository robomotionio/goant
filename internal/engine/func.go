package engine

// svFunc is a compiled bytecode function (ant struct sv_func, include/silver/
// engine.h). The Phase 3 vertical slice populates a subset of these fields;
// remaining fields (upvalues, IC slots, type feedback, child funcs, srcpos)
// fill in as the full compiler port lands.
// upvalDesc describes how a closure captures one upvalue (ant sv_upval_desc):
// from the enclosing frame's local slot (isLocal) or the enclosing closure's
// upvalue array (!isLocal).
type upvalDesc struct {
	index    int
	isLocal  bool
	isConst  bool   // captures a const binding — assigning through it throws a TypeError
	selfName bool   // captures a named-function-expression self-reference (immutable)
	name     string // the captured binding's source name (for direct-eval scope capture)
}

type svFunc struct {
	code      []byte
	constants []Value

	// constNames is the Go text of every string constant, by the same index.
	//
	// The name of a property is a constant, and the interpreter needs it as a Go
	// string on every access that misses its inline cache. Going through the
	// Value meant resolving a handle — a chunk lookup, a bounds check and a
	// pointer chase — to reach a string that was decided at compile time and can
	// never change. It was 6% of Octane's Richards.
	constNames []string

	childFuncs []*svFunc   // nested functions, referenced by CLOSURE index
	upvalDescs []upvalDesc // how this function captures its upvalues

	name     string
	filename string
	source   string

	// jit is this function's tiering state: how often it has been entered, and
	// the compiled form once there is one. Compiled code is never released — a
	// function that got hot stays hot, and the block has to outlive every entry
	// into it, which nothing here can prove has ended.
	jit jitAttempt

	// elemKinds is the type feedback for element access, one byte per bytecode
	// offset, written by the interpreter and read by the emitter. Allocated only
	// for a function that actually indexes a TypedArray. See elemfeedback.go.
	elemKinds []uint8

	maxLocals  int
	maxStack   int
	paramCount int // positional parameter slots (used by frame init)
	fnLength   int // Function.length: params before the first default/rest
	srcStart   int
	srcEnd     int

	isStrict    bool
	isArrow     bool
	isAsync     bool
	isGenerator bool
	isClassCtor bool
	// usesAwait records that this function's OWN body suspends on an await —
	// written `await`, `for await`, or the implicit await of `await using`. For a
	// Module this is [[HasTLA]], which module evaluation must know STATICALLY:
	// `if (false) await x;` still makes the module an async-evaluating one.
	usesAwait bool

	// Module goal only: exported name -> top-level local slot, and the
	// specifiers of `export * from` re-exports.
	moduleExports  map[string]int
	moduleStarFrom []string
	moduleImports  []moduleImport
	moduleRequests []string // every specifier imported, bindings or not
	moduleIndirect map[string]indirectExport
	// moduleHoistFn is the module's InitializeEnvironment prologue split off as
	// its own function: the import bindings, the temporal dead zones, and the
	// hoisted function declarations. It runs when the module is LINKED, so a
	// cyclic importer whose body runs first still sees this module's functions.
	// startIP is where the remaining body then begins.
	moduleHoistFn *svFunc
	startIP       int
	isMethod      bool // concise method / getter / setter: no [[Construct]], no .prototype
	// isClassElement marks a class constructor / method / accessor / static block:
	// its `super` resolves via the class's captured *superproto* / *superctor*
	// bindings, distinguishing it from an object-literal method (whose super uses
	// a runtime [[HomeObject]]). Without this a *superproto* local left in an
	// enclosing scope by a sibling class would be mistaken for a super binding.
	isClassElement bool
	// classIsDerived marks a class element whose class has a heritage (`extends`).
	// Only then are the *superproto* / *superctor* bindings available, so a base
	// class element (like an object method) resolves super via its [[HomeObject]].
	classIsDerived bool
	// usesSuper marks a concise object-literal method whose body reads a super
	// property outside any class scope. Such a method's closure carries a
	// [[HomeObject]] (set when the method is defined on its object) so OpGetSuper
	// can start the lookup at the home object's prototype.
	usesSuper bool

	// capturesWith marks a function compiled lexically inside a `with` block: its
	// free names are emitted as OpWithGetVar/OpWithPutVar, and its closure snapshots
	// the enclosing with-object scope chain at creation (see OpClosure).
	capturesWith bool

	// dynamicVars marks a sloppy function whose body (or parameter list) contains
	// a direct eval. Such an eval creates its `var` bindings in THIS function's
	// variable environment at run time, where no compile-time slot exists for
	// them, so the frame gets a dynamic variable object at entry and the
	// function's free names resolve against it first — the same routing a `with`
	// uses (see nameIsWithRouted).
	dynamicVars bool

	// evalVarObj marks eval code compiled against a caller that has a dynamic
	// variable object: the eval frame adopts that same object, so a direct eval
	// nested inside this one still declares into the original function's variable
	// environment rather than starting a fresh one.
	evalVarObj bool

	// mappedArgs marks a function whose `arguments` object aliases its formal
	// parameters (10.4.4): non-strict, non-arrow, and a simple parameter list of
	// distinct names — the condition under which formal i owns frame slot i, which
	// is what the parameter map indexes.
	mappedArgs bool

	// globalLex maps this Script's top-level lexical names to the frame slots
	// holding them, so frame entry can publish them as global lexical bindings.
	globalLex map[string]globalLexDecl

	// metaCell is the shared cell holding this Module's `import.meta` object —
	// one per module, created on first access and seen by every function compiled
	// inside the module. nil outside module code.
	metaCell *Value

	// capturesHome marks an arrow function that reads `super` inherited from an
	// enclosing object-literal method: its closure takes that method's
	// [[HomeObject]], since an arrow has none of its own.
	capturesHome bool

	// isDerivedCtor marks a derived class constructor: `this` starts in its TDZ
	// (thisSlot holds tEmpty until super() binds it), and OpReturn enforces
	// GetThisBinding / the object-or-undefined return-value rule.
	isDerivedCtor bool
	thisSlot      int // the *this* local slot for a non-arrow function (else 0)

	// icCount is how many inline-cache slots this function's field opcodes were
	// assigned; ics is the array itself, allocated on first frame entry and kept
	// on the function so cached slots survive across calls (see propcache.go).
	icCount uint16
	ics     []propIC

	// evalScopes are the compile-time lexical snapshots for this function's direct
	// eval() call sites, indexed by the OpEval operand. Each records the caller
	// bindings a direct eval may borrow and which context constructs it may use.
	evalScopes []*evalScope
}

// ---- bytecode emission (compiler side) ----

func (c *compiler) emit(op Opcode) int {
	pos := len(c.fn.code)
	c.fn.code = append(c.fn.code, byte(op))
	return pos
}

func (c *compiler) emitByte(b byte) { c.fn.code = append(c.fn.code, b) }

func (c *compiler) emitU16(v uint16) {
	c.fn.code = append(c.fn.code, byte(v), byte(v>>8))
}

func (c *compiler) emitU32(v uint32) {
	c.fn.code = append(c.fn.code, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// emitConst pushes constant pool[idx].
func (c *compiler) emitConst(v Value) {
	idx := c.constant(v)
	c.emit(OpConst)
	c.emitU32(uint32(idx))
}

// emitOpU16 emits a one-operand (u16) opcode.
func (c *compiler) emitOpU16(op Opcode, v uint16) {
	c.emit(op)
	c.emitU16(v)
}

// emitGlobalGet reads a global by name (GET_GLOBAL: u32 name-const + u16 ic).
func (c *compiler) emitGlobalGet(name string) {
	idx := c.constant(c.rt.internString(name))
	c.emit(OpGetGlobal)
	c.emitU32(uint32(idx))
	c.emitU16(c.nextICSlot())
}

// emitGlobalPut writes a global by name (PUT_GLOBAL: u32 name-const).
func (c *compiler) emitGlobalPut(name string) {
	idx := c.constant(c.rt.internString(name))
	c.emit(OpPutGlobal)
	c.emitU32(uint32(idx))
}

// emitFieldOp emits a named-field opcode with a u32 name-const + u16 ic slot
// (GET_FIELD / GET_FIELD2 / PUT_FIELD, all size 7).
func (c *compiler) emitFieldOp(op Opcode, name string) {
	if len(name) > 0 && name[0] == '#' && !c.privateNameDeclared(name) {
		c.syntaxErrorf("Private field %s must be declared in an enclosing class", quotedName(name))
		return
	}
	idx := c.constant(c.rt.internString(c.privateKey(name)))
	c.emit(op)
	c.emitU32(uint32(idx))
	c.emitU16(c.nextICSlot())
}

// nextICSlot hands this field op its own inline-cache entry. Slots are per
// function and never reused, so two sites can never alias each other's shape.
func (c *compiler) nextICSlot() uint16 {
	if c.fn.icCount >= icNoSlot {
		return icNoSlot
	}
	slot := c.fn.icCount
	c.fn.icCount++
	return slot
}

// emitDefineField emits DEFINE_FIELD (u32 name-const, size 5).
func (c *compiler) emitDefineField(name string) {
	idx := c.constant(c.rt.internString(c.privateKey(name)))
	c.emit(OpDefineField)
	c.emitU32(uint32(idx))
}

// emitJump emits a branch opcode with a placeholder 32-bit absolute target,
// returning the operand offset to patch later.
func (c *compiler) emitJump(op Opcode) int {
	c.emit(op)
	pos := len(c.fn.code)
	c.emitU32(0)
	return pos
}

// patchJump writes the current code position as the absolute target of the
// jump whose operand starts at operandPos.
func (c *compiler) patchJump(operandPos int) {
	target := uint32(len(c.fn.code))
	c.fn.code[operandPos] = byte(target)
	c.fn.code[operandPos+1] = byte(target >> 8)
	c.fn.code[operandPos+2] = byte(target >> 16)
	c.fn.code[operandPos+3] = byte(target >> 24)
}

// constant interns a value into the constant pool with dedup for
// numbers/strings, returning its index.
// constant interns v in this function's constant pool and returns its index.
//
// Dedup by raw bits for non-strings; by content for strings, because the same
// text arrives here as more than one handle. Both go through an index rather
// than a scan of the pool: the scan was quadratic in the pool's size, and its
// string case dereferenced two handles per entry it passed over to compare
// them. That was eight percent of Octane's code-load, which does nothing but
// parse and compile.
func (c *compiler) constant(v Value) int {
	if v.IsString() {
		text := c.rt.strGo(v)
		if i, ok := c.strConsts[text]; ok {
			return i
		}
		if c.strConsts == nil {
			c.strConsts = make(map[string]int, 8)
		}
		c.strConsts[text] = len(c.fn.constants)
		c.fn.constants = append(c.fn.constants, v)
		c.fn.constNames = append(c.fn.constNames, text)
		return len(c.fn.constants) - 1
	}
	if i, ok := c.rawConsts[v]; ok {
		return i
	}
	if c.rawConsts == nil {
		c.rawConsts = make(map[Value]int, 8)
	}
	c.rawConsts[v] = len(c.fn.constants)
	c.fn.constants = append(c.fn.constants, v)
	c.fn.constNames = append(c.fn.constNames, "")
	return len(c.fn.constants) - 1
}

// ---- bytecode reading (interpreter side) ----

func readU16(code []byte, ip int) uint16 {
	return uint16(code[ip]) | uint16(code[ip+1])<<8
}

func readU32(code []byte, ip int) uint32 {
	return uint32(code[ip]) | uint32(code[ip+1])<<8 |
		uint32(code[ip+2])<<16 | uint32(code[ip+3])<<24
}

// quotedName renders an identifier for an error message. It exists so the
// message stays a constant format string, which `go vet` requires and which
// keeps a name that happens to contain a percent sign from being read as a verb.
func quotedName(name string) string { return "'" + name + "'" }
