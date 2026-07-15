package engine

// Object/array literal and member-access compilation (ant compiler.c
// compile_object / compile_array / compile_member / compile_call). Method calls
// route the receiver as `this`. Accessor properties, spread, and computed
// method names in literals are added as the port continues.

func (c *compiler) compileObject(n *Node) {
	// A CoverInitializedName (`{ x = 1 }`) is only valid once the object has been
	// reinterpreted as a destructuring pattern (which compiles via destructureObject,
	// not here); reaching compileObject means it is a plain object literal.
	if n.Flags&nodeHasCoverInit != 0 {
		c.syntaxErrorf("Invalid shorthand property initializer")
		return
	}
	c.emit(OpObject)
	for _, prop := range n.Args {
		if prop.Kind == NSpread {
			// {...src}: copy src's enumerable own props into the target.
			c.emit(OpDup)             // [target, target]
			c.compileExpr(prop.Right) // [target, target, src]
			c.emit(OpCopyDataProps)   // -> [target, target]
			c.emitByte(0)
			c.emit(OpPop) // discard the extra target left by COPY_DATA_PROPS
			continue
		}
		if prop.Flags&(fnGetter|fnSetter) != 0 {
			// Bit 4 marks an object-literal accessor as enumerable (class accessors,
			// emitted elsewhere without the bit, are non-enumerable).
			flags := byte(1) | 4 // getter, enumerable
			prefix := "get "
			if prop.Flags&fnSetter != 0 {
				flags = 2 | 4 // setter, enumerable
				prefix = "set "
			}
			name, ok := propKeyName(prop.Left)
			if prop.Flags&fnComputed != 0 || !ok {
				// Computed accessor: [obj, key, func] -> DEFINE_METHOD_COMP.
				c.emit(OpDup)
				c.compileExpr(prop.Left)
				c.compileFunc(prop.Right)
				c.emit(OpDefineMethodComp)
				c.emitByte(flags)
				c.emit(OpPop)
				continue
			}
			if prop.Right.Str == "" {
				prop.Right.Str = prefix + name
				prop.Right.Flags |= fnInferredName
			}
			c.compileFunc(prop.Right) // the accessor function
			idx := c.constant(c.rt.internString(name))
			c.emit(OpDefineMethod)
			c.emitU32(uint32(idx))
			c.emitByte(flags)
			continue
		}
		if prop.Flags&fnComputed != 0 {
			// { [expr]: v }: CreateDataProperty (enumerable own data), bypassing any
			// inherited setter (e.g. __proto__) that OpPutElem would trigger.
			c.emit(OpDup)
			c.compileExpr(prop.Left) // computed key
			c.compileExpr(prop.Right)
			// NamedEvaluation: an anonymous function/class value takes its name from
			// the computed key ("[desc]" for a symbol key).
			if isAnonFuncDef(prop.Right) {
				c.emit(OpSetNameComp)
			}
			c.emit(OpDefineMethodComp)
			c.emitByte(3)
			c.emit(OpPop) // DEFINE_METHOD_COMP leaves the target; drop the dup
			continue
		}
		name, ok := propKeyName(prop.Left)
		if !ok {
			c.errorf("unsupported object literal key (slice)")
			return
		}
		// `{__proto__: expr}` (colon form only) sets the [[Prototype]]; the
		// computed/shorthand/method forms create an ordinary "__proto__" property.
		if name == "__proto__" && prop.Flags&fnColon != 0 && prop.Flags&(fnGetter|fnSetter|fnMethod) == 0 {
			c.emit(OpDup)
			c.compileExpr(prop.Right)
			c.emit(OpSetProto) // obj proto -> obj
			c.emit(OpPop)
			continue
		}
		nameAnonExpr(prop.Right, name)
		before := len(c.fn.childFuncs)
		c.compileExpr(prop.Right)
		// A concise method that reads super needs its [[HomeObject]] set to the
		// object before it becomes an ordinary (enumerable) data property.
		if len(c.fn.childFuncs) > before && c.fn.childFuncs[len(c.fn.childFuncs)-1].usesSuper {
			c.emit(OpSetHomeObj) // [obj, method] -> [obj, method]
		}
		c.emitDefineField(name) // obj val -> obj
	}
}

func (c *compiler) compileArray(n *Node) {
	if hasSpread(n.Args) {
		c.buildSpreadArray(n.Args)
		return
	}
	for _, el := range n.Args {
		if el.Kind == NEmpty {
			c.emit(OpEmpty)
		} else {
			c.compileExpr(el)
		}
	}
	c.emit(OpArray)
	c.emitU16(uint16(len(n.Args)))
}

// compileMember compiles a member read (obj.name or obj[expr]).
func (c *compiler) compileMember(n *Node) {
	if n.Left != nil && n.Left.Kind == NIdent && n.Left.Str == "super" {
		c.compileSuperMember(n)
		return
	}
	c.compileExpr(n.Left)
	if n.Flags&1 != 0 { // computed
		c.compileExpr(n.Right)
		c.emit(OpGetElem)
		return
	}
	c.emitFieldOp(OpGetField, n.Right.Str)
}

// compileSuperMember compiles a super-property read `super.x` / `super[expr]`:
// the lookup starts at the home object's prototype (*superproto*) but the
// accessor receiver is the current `this`.
func (c *compiler) compileSuperMember(n *Node) {
	switch {
	case c.hasClassSuper():
		// Class element: the receiver is the captured *this*, the base is the
		// class's home binding (*superproto* / *superctor*).
		if !c.resolveClassBinding("*this*") {
			c.emit(OpUndef)
		}
		c.resolveClassBinding(c.superHomeBinding())
	case c.fn != nil && c.fn.isMethod && !c.fn.isArrow:
		// Object-literal method: the receiver is the dynamic `this`, the base is
		// the method's [[HomeObject]].[[Prototype]] (read at runtime via the
		// method's closure). Flag the method so it receives a home object.
		c.fn.usesSuper = true
		c.emit(OpThis)
		c.emit(OpGetSuper)
	default:
		c.syntaxErrorf("'super' keyword unexpected here")
		return
	}
	if n.Flags&1 != 0 { // computed
		c.compileExpr(n.Right)
	} else {
		c.emitConst(c.rt.internString(n.Right.Str))
	}
	c.emit(OpGetSuperVal)
}

// compileSuperMemberAssign compiles `super.x = v` / `super[k] = v`: it emits
// [receiver(this), base, key, val] and performs base.[[Set]](key, val, this).
func (c *compiler) compileSuperMemberAssign(n *Node) {
	member := n.Left
	c.emitSuperThis() // receiver
	if !c.emitSuperBase() {
		c.syntaxErrorf("'super' keyword unexpected here")
		return
	}
	if member.Flags&1 != 0 { // computed key
		c.compileExpr(member.Right)
	} else {
		c.emitConst(c.rt.internString(member.Right.Str))
	}
	c.compileExpr(n.Right) // value
	c.emit(OpPutSuperVal)  // [receiver, base, key, val] -> val
}

// tempLocal allocates a fresh anonymous local slot.
func (c *compiler) tempLocal() int { return c.addLocal("*tmp*", false) }

// loadMember reads the member into the accumulator, given the receiver in tSlot
// (and, for computed members, the key in kSlot).
func (c *compiler) loadMember(member *Node, tSlot, kSlot int) {
	c.emitOpU16(OpGetLocal, uint16(tSlot))
	if member.Flags&1 != 0 {
		c.emitOpU16(OpGetLocal, uint16(kSlot))
		c.emit(OpGetElem)
	} else {
		c.emitFieldOp(OpGetField, member.Right.Str)
	}
}

// compileMemberAssign compiles obj.name = v / obj[k] = v (and compound forms)
// as an expression, leaving the assigned value on the stack.
func (c *compiler) compileMemberAssign(n *Node) {
	member := n.Left
	computed := member.Flags&1 != 0

	// `super.x = v` / `super[k] = v` (plain assignment): a Super Reference write.
	if n.Op == TokAssign && member.Left != nil && member.Left.Kind == NIdent && member.Left.Str == "super" {
		c.compileSuperMemberAssign(n)
		return
	}

	if n.Op == TokAssign {
		c.compileExpr(member.Left) // obj
		if computed {              // obj[key] = v
			c.compileExpr(member.Right)
			c.compileExpr(n.Right)
			c.emit(OpInsert3) // obj key val -> val obj key val
			c.emit(OpPutElem)
			return
		}
		c.compileExpr(n.Right) // obj val
		c.emit(OpInsert2)      // obj val -> val obj val
		c.emitFieldOp(OpPutField, member.Right.Str)
		return
	}

	// Logical member assignment (obj.x &&=/||=/??= v): short-circuit so the RHS
	// and the [[Set]] (its setter) run only when needed.
	if jmpOp, isLogical := logicalAssignJmp(n.Op); isLogical {
		tSlot := c.tempLocal()
		kSlot := -1
		c.compileExpr(member.Left)
		c.emitOpU16(OpPutLocal, uint16(tSlot))
		if computed {
			kSlot = c.tempLocal()
			c.compileExpr(member.Right)
			c.emitOpU16(OpPutLocal, uint16(kSlot))
		}
		c.loadMember(member, tSlot, kSlot) // [old]
		c.emit(OpDup)                      // [old, old]
		skip := c.emitJump(jmpOp)          // short-circuit: leave [old]
		c.emit(OpPop)                      // []
		c.compileExpr(n.Right)             // [rhs]
		vSlot := c.tempLocal()
		c.emitOpU16(OpSetLocal, uint16(vSlot)) // keep [rhs]
		if computed {
			c.emitOpU16(OpGetLocal, uint16(tSlot))
			c.emitOpU16(OpGetLocal, uint16(kSlot))
			c.emitOpU16(OpGetLocal, uint16(vSlot))
			c.emit(OpPutElem)
		} else {
			c.emitOpU16(OpGetLocal, uint16(tSlot))
			c.emitOpU16(OpGetLocal, uint16(vSlot))
			c.emitFieldOp(OpPutField, member.Right.Str)
		}
		c.patchJump(skip)
		return
	}

	// Compound member assignment obj.x op= v via temp locals.
	op, ok := compoundOpcode(n.Op)
	if !ok {
		c.errorf("unsupported compound member assignment %v (slice)", n.Op)
		return
	}
	tSlot := c.tempLocal()
	kSlot := -1
	c.compileExpr(member.Left)
	c.emitOpU16(OpPutLocal, uint16(tSlot))
	if computed {
		kSlot = c.tempLocal()
		c.compileExpr(member.Right)
		c.emitOpU16(OpPutLocal, uint16(kSlot))
	}
	c.loadMember(member, tSlot, kSlot) // [old]
	c.compileExpr(n.Right)             // [old, rhs]
	c.emit(op)                         // [new]
	vSlot := c.tempLocal()
	c.emitOpU16(OpSetLocal, uint16(vSlot)) // keep new on stack, save to vSlot
	// store new into member
	if computed {
		c.emitOpU16(OpGetLocal, uint16(tSlot))
		c.emitOpU16(OpGetLocal, uint16(kSlot))
		c.emitOpU16(OpGetLocal, uint16(vSlot))
		c.emit(OpPutElem)
	} else {
		c.emitOpU16(OpGetLocal, uint16(tSlot))
		c.emitOpU16(OpGetLocal, uint16(vSlot))
		c.emitFieldOp(OpPutField, member.Right.Str)
	}
	// new value already on stack (from SetLocal above)
}

// compileMemberUpdate compiles obj.x++/--/++obj.x on a member target.
func (c *compiler) compileMemberUpdate(n *Node) {
	member := n.Right
	computed := member.Flags&1 != 0
	prefix := n.Flags == 1
	incOp := OpInc
	if n.Op == TokPostDec {
		incOp = OpDec
	}

	tSlot := c.tempLocal()
	kSlot := -1
	c.compileExpr(member.Left)
	c.emitOpU16(OpPutLocal, uint16(tSlot))
	if computed {
		kSlot = c.tempLocal()
		c.compileExpr(member.Right)
		c.emitOpU16(OpPutLocal, uint16(kSlot))
	}

	oldSlot := c.tempLocal()
	c.loadMember(member, tSlot, kSlot) // [val]
	c.emit(OpNeg)                      // ToNumeric(old): -(-x) preserves BigInt/fp
	c.emit(OpNeg)
	c.emitOpU16(OpPutLocal, uint16(oldSlot))

	newSlot := c.tempLocal()
	c.emitOpU16(OpGetLocal, uint16(oldSlot))
	c.emit(incOp)
	c.emitOpU16(OpPutLocal, uint16(newSlot))

	// store new into member
	if computed {
		c.emitOpU16(OpGetLocal, uint16(tSlot))
		c.emitOpU16(OpGetLocal, uint16(kSlot))
		c.emitOpU16(OpGetLocal, uint16(newSlot))
		c.emit(OpPutElem)
	} else {
		c.emitOpU16(OpGetLocal, uint16(tSlot))
		c.emitOpU16(OpGetLocal, uint16(newSlot))
		c.emitFieldOp(OpPutField, member.Right.Str)
	}

	// result: old (postfix) or new (prefix)
	if prefix {
		c.emitOpU16(OpGetLocal, uint16(newSlot))
	} else {
		c.emitOpU16(OpGetLocal, uint16(oldSlot))
	}
}

// propKeyName extracts a static property-key string from a literal key node.
func propKeyName(key *Node) (string, bool) {
	switch key.Kind {
	case NIdent, NString:
		return key.Str, true
	case NNumber:
		return numberToString(key.Num), true
	}
	return "", false
}
