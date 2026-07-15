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
	isConst  bool // captures a const binding — assigning through it throws a TypeError
	selfName bool // captures a named-function-expression self-reference (immutable)
	name     string // the captured binding's source name (for direct-eval scope capture)
}

type svFunc struct {
	code      []byte
	constants []Value

	childFuncs []*svFunc   // nested functions, referenced by CLOSURE index
	upvalDescs []upvalDesc // how this function captures its upvalues

	name     string
	filename string
	source   string

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
	isMethod    bool // concise method / getter / setter: no [[Construct]], no .prototype
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

	// isDerivedCtor marks a derived class constructor: `this` starts in its TDZ
	// (thisSlot holds tEmpty until super() binds it), and OpReturn enforces
	// GetThisBinding / the object-or-undefined return-value rule.
	isDerivedCtor bool
	thisSlot      int // the *this* local slot for a non-arrow function (else 0)

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
	c.emitU16(0) // inline-cache slot placeholder
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
		c.syntaxErrorf("Private field '" + name + "' must be declared in an enclosing class")
		return
	}
	idx := c.constant(c.rt.internString(name))
	c.emit(op)
	c.emitU32(uint32(idx))
	c.emitU16(0) // inline-cache slot placeholder
}

// emitDefineField emits DEFINE_FIELD (u32 name-const, size 5).
func (c *compiler) emitDefineField(name string) {
	idx := c.constant(c.rt.internString(name))
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
func (c *compiler) constant(v Value) int {
	// Dedup by raw bits for non-strings; by content for strings.
	for i, e := range c.fn.constants {
		if e == v {
			return i
		}
		if v.IsString() && e.IsString() &&
			string(c.rt.strBytes(e)) == string(c.rt.strBytes(v)) {
			return i
		}
	}
	c.fn.constants = append(c.fn.constants, v)
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
