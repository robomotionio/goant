package engine

// Port of ant's dispatch loop (src/silver/engine.c) + the shared opcode bodies
// (src/silver/ops/*.h). The Phase 3 vertical slice implements the ops the slice
// compiler emits, proving execution end-to-end. Full opcode coverage, frames,
// closures, calls, exception unwinding, and TCO land as the port continues.
//
// DIVERGENCE (slice only): jumps use absolute byte targets; the full port
// adopts ant's exact branch encoding (reconciled via the bytecode-diff harness).

import (
	"math"
	"math/big"
)

// ThrowError wraps a thrown JS value surfaced as a Go error. When control is
// set it is a non-catchable control-flow signal (e.g. process.exit) that
// bypasses catch handlers.
type ThrowError struct {
	Value   Value
	rt      *Runtime
	control bool
	// rejected marks a [[DefineOwnProperty]]/[[Set]] rejection: DefinePropertyOrThrow
	// contexts throw it (it is a valid TypeError), but Reflect.defineProperty /
	// proxy invariant checks treat it as a boolean false instead of throwing.
	rejected bool
}

func (e *ThrowError) Error() string {
	if s, terr := e.rt.toStringValue(e.Value); terr == nil {
		return "Uncaught " + string(e.rt.strBytes(s))
	}
	return "Uncaught " + e.rt.inspect(e.Value, false)
}

// maxFrameDepth guards native recursion depth (ant maxFrames RangeError guard).
const maxFrameDepth = 8192

// execute runs the top-level script function, returning its completion value.
func (rt *Runtime) execute(fn *svFunc) (Value, error) {
	v, terr := rt.runFrame(fn, nil, mkundef(), rt.global, nil)
	if terr != nil {
		return mkundef(), terr
	}
	return v, nil
}

// runFrame executes a compiled function with a this-binding and arguments,
// returning its return value (ant sv_execute_frame). JS call depth maps onto
// the Go stack; open upvalues capturing this frame's locals are closed on exit.
func (rt *Runtime) runFrame(fn *svFunc, cl *closure, fnVal, thisVal Value, args []Value) (Value, *ThrowError) {
	rt.frameDepth++
	if rt.frameDepth > maxFrameDepth {
		rt.frameDepth--
		return mkundef(), rt.rangeError("Maximum call stack size exceeded")
	}
	defer func() { rt.frameDepth-- }()

	// Track the executing frame's strictness so a direct eval() (a native call,
	// so rt.frameStrict still reflects this frame) can inherit it.
	savedStrict := rt.frameStrict
	rt.frameStrict = fn.isStrict
	defer func() { rt.frameStrict = savedStrict }()

	// Frame state, declared up-front so a proper tail call (OP_TAIL_CALL) can reset
	// it and reuse this Go frame instead of recursing.
	var (
		code         []byte
		stack        []Value
		locals       []Value
		openUpvals   map[int]*upvalue
		handlers     []tryHandler
		pendingThrow Value
		withStack    []Value
		varObj       Value
		newTarget    Value
		privEnv      *privScope
		thrown       *ThrowError
		comp         completion
		ip           int
	)
	savedActiveNT := rt.activeNewTarget
	defer func() { rt.activeNewTarget = savedActiveNT }()

	push := func(v Value) { stack = append(stack, v) }
	pop := func() Value {
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		return v
	}
	peek := func() Value { return stack[len(stack)-1] }
	// captureUpvalue returns the open upvalue for a local slot, creating it on
	// first use so multiple closures over the same slot share one cell.
	captureUpvalue := func(slot int) *upvalue {
		if openUpvals == nil {
			openUpvals = map[int]*upvalue{}
		}
		if u, ok := openUpvals[slot]; ok {
			return u
		}
		u := &upvalue{location: &locals[slot]}
		openUpvals[slot] = u
		return u
	}
	closeAll := func() {
		// A module frame's locals ARE the module environment, and the record keeps
		// the slice alive past this frame. Closing would copy each captured binding
		// into its own cell, so a later write through a closure (an exported
		// function mutating an exported `let`) would no longer be visible to
		// importers — exactly the live-binding semantics modules require.
		if fn.moduleExports != nil {
			return
		}
		for _, u := range openUpvals {
			u.closeUp()
		}
	}
	// doReturn implements ant sv_vm_unwind_for_return: if a finally scope lies
	// between here and the frame exit, record a RETURN completion, unwind to it,
	// and return its finally-entry ip (ok=true). Otherwise the frame truly returns.
	doReturn := func(r Value) (int, bool) {
		for i := len(handlers) - 1; i >= 0; i-- {
			if handlers[i].kind != hTryFinally {
				continue
			}
			h := handlers[i]
			handlers = handlers[:i]
			stack = stack[:h.stackDepth]
			comp = completion{kind: compReturn, value: r}
			return h.catchIP, true
		}
		return 0, false
	}
	// doJump implements ant sv_vm_unwind_for_jump for break/continue crossing
	// finally scopes: run the nearest finally (recording a JUMP completion for the
	// rest), or, when none remain, pop the crossed handlers and fall through.
	doJump := func(target, nFin, nPop int) int {
		if nFin > 0 {
			for i := len(handlers) - 1; i >= 0; i-- {
				if handlers[i].kind != hTryFinally {
					continue
				}
				h := handlers[i]
				used := len(handlers) - i
				handlers = handlers[:i]
				stack = stack[:h.stackDepth]
				pops := nPop - used
				if pops < 0 {
					pops = 0
				}
				comp = completion{kind: compJump, jumpIP: target, jumpFin: nFin - 1, jumpPops: pops}
				return h.catchIP
			}
		}
		for nPop > 0 && len(handlers) > 0 {
			handlers = handlers[:len(handlers)-1]
			nPop--
		}
		comp = completion{}
		return -1
	}

restart:
	// OrdinaryCallBindThis for a non-strict function: a nullish `this` becomes the
	// global object, and a primitive `this` is boxed via ToObject. Strict
	// functions keep `this` as-is (strict.this-undefined-in-function).
	rt.frameStrict = fn.isStrict
	if !fn.isStrict {
		if thisVal.IsNullish() {
			thisVal = rt.global
		} else if !thisVal.IsObjectType() && thisVal.Type() != TTypedArray {
			thisVal, _ = rt.toObjectValue(thisVal) // non-nullish: cannot error
		}
	}
	// new.target for this invocation: set by construct just before the call and
	// consumed here so nested ordinary calls see undefined.
	newTarget = rt.pendingNewTarget
	rt.pendingNewTarget = mkundef()
	// A class constructor may only be invoked via `new` (new.target is set) — or
	// via super() from a derived constructor, which propagates new.target through
	// rt.activeNewTarget.
	if fn.isClassCtor {
		if newTarget.IsUndefined() {
			newTarget = rt.activeNewTarget
			if newTarget.IsUndefined() {
				return mkundef(), rt.typeError("Class constructor " + fn.name + " cannot be invoked without 'new'")
			}
		}
		rt.activeNewTarget = newTarget
	}
	code = fn.code
	stack = make([]Value, 0, fn.maxStack+16)
	// Locals start as undefined (the zero Value 0x0 would decode as the number
	// 0.0, so an unread local must not be left zeroed).
	locals = make([]Value, fn.maxLocals)
	for i := range locals {
		locals[i] = mkundef()
	}
	// Parameters occupy the first slots (ant frame arg layout).
	for i := 0; i < fn.paramCount && i < fn.maxLocals; i++ {
		if i < len(args) {
			locals[i] = args[i]
		}
	}
	// A module body hands its locals slice to the record being evaluated: those
	// slots *are* the module environment, so importers holding the slice see the
	// bindings live. Claimed here, at frame entry, before any nested import runs.
	if rt.pendingModule != nil && rt.pendingModule.fn == fn {
		rt.pendingModule.locals = locals
		rt.pendingModule = nil
	}
	// A Script's top-level lexical bindings become global ones here — before any
	// of them is initialised, so a read from a nested call reaches the temporal
	// dead zone rather than finding nothing at all.
	if fn.globalLex != nil {
		rt.registerGlobalLex(fn, locals)
	}
	openUpvals = nil
	handlers = nil
	pendingThrow = mkundef()
	// A function defined lexically inside a `with` seeds its scope chain from the
	// with-objects captured when its closure was created, so free names still
	// resolve against them (its bytecode reads them via OpWithGetVar).
	if cl != nil && len(cl.capturedWith) > 0 {
		withStack = append([]Value(nil), cl.capturedWith...)
	} else {
		withStack = nil
	}
	// The class-evaluation tag this function was created under: its private
	// accesses use the Private Names of that evaluation, not of whichever
	// evaluation happens to be running now.
	privEnv = nil
	if cl != nil {
		privEnv = cl.privEnv
	}
	// A function containing a direct eval carries a dynamic variable object: the
	// eval's `var` declarations that name nothing this function declared are
	// created on it, and this function's free names (compiled as with-routed
	// accesses) find them there. It is innermost of the captured with-objects but
	// outside this frame's own locals, and has a null prototype so nothing from
	// Object.prototype can shadow an outer binding.
	varObj = mkundef()
	switch {
	case fn.evalVarObj:
		// Eval code adopts the caller's variable object (already in its captured
		// with-chain); a direct eval nested here declares into the same one.
		varObj, rt.pendingVarObj = rt.pendingVarObj, mkundef()
	case fn.dynamicVars:
		varObj = rt.newObject(mknull())
		withStack = append(withStack, varObj)
	}
	ip = 0

	for {
		op := Opcode(code[ip])
		switch op {
		case OpUndef:
			push(mkundef())
			ip++
		case OpNull:
			push(mknull())
			ip++
		case OpGlobal:
			push(rt.global)
			ip++
		case OpEmpty:
			push(tEmpty)
			ip++
		case OpForIn:
			keys, e := rt.forInKeys(pop())
			if e != nil {
				thrown = e
				goto unwind
			}
			push(keys)
			ip++
		case OpHasPrivate:
			// Reused as the for-in re-validation check (see forInStillEnumerable):
			// [key, obj] -> bool. The opcode table entry is otherwise unclaimed and
			// its shape (pop 2, push 1) matches exactly.
			fiObj := pop()
			fiKey := pop()
			push(mkbool(rt.forInStillEnumerable(fiObj, fiKey)))
			ip++
		case OpForOf:
			vals, e := rt.iterableValues(pop())
			if e != nil {
				thrown = e
				goto unwind
			}
			arr := rt.newArray()
			ao := rt.objPtr(arr)
			ao.arr = vals
			ao.arrLen = uint32(len(vals))
			push(arr)
			ip++
		case OpIterCall:
			// Pop the source, push its (live) sync iterator. The lazy for-of loop
			// drives it with iter.next() and closes it on break.
			it, e := rt.getSyncIterator(pop())
			if e != nil {
				thrown = e
				goto unwind
			}
			push(it)
			ip += 2 // Size 2 (unused inline operand byte)
		case OpIterClose:
			// IteratorClose (7.4.8). GetMethod(iterator, "return"): undefined/null
			// leaves the iterator unclosed; a present-but-non-callable `return` is a
			// TypeError. Otherwise call return() and require an Object result.
			//
			// When the pending completion is itself a throw (the for-of finally sets
			// comp = compThrow before closing), any error raised while closing is
			// SUPPRESSED so the original exception wins (spec steps 6-7): return()
			// is still invoked for its side effects, but its abrupt result — a
			// non-callable method, a throw, or a non-Object result — does not
			// override the in-flight throw.
			iter := pop()
			suppress := comp.kind == compThrow
			if iter.IsObjectType() {
				rf, e := rt.getField(iter, "return")
				switch {
				case e != nil:
					if !suppress {
						thrown = e
						goto unwind
					}
				case rf.IsNullish():
					// no return method: nothing to close
				case !rt.isCallable(rf):
					if !suppress {
						thrown = rt.typeError("iterator 'return' property is not a function")
						goto unwind
					}
				default:
					res, e := rt.callValue(rf, iter, nil)
					switch {
					case e != nil:
						if !suppress {
							thrown = e
							goto unwind
						}
					case !res.IsObjectType():
						if !suppress {
							thrown = rt.typeError("iterator close: return() did not return an object")
							goto unwind
						}
					}
				}
			}
			ip++
		case OpForAwaitOf:
			// Pop the source, push its async iterator (GetAsyncIterator). The
			// caller-emitted loop then drives it with await iter.next().
			it, e := rt.getAsyncIterator(pop())
			if e != nil {
				thrown = e
				goto unwind
			}
			push(it)
			ip++
		case OpRegexp:
			flags := string(rt.strBytes(pop()))
			pattern := string(rt.strBytes(pop()))
			v, e := rt.newRegExp(pattern, flags)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(v)
			ip++
		case OpEnterWith:
			// `with (expr)` binds ToObject(expr): null/undefined throw a TypeError,
			// and a primitive is wrapped so property lookups resolve on the wrapper.
			obj, e := rt.toObjectValue(pop())
			if e != nil {
				thrown = e
				goto unwind
			}
			withStack = append(withStack, obj)
			ip++
		case OpExitWith:
			withStack = withStack[:len(withStack)-1]
			ip++
		case OpWithGetVar:
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			// Reference mode (high bit of the fallback-kind byte): also push the
			// resolved base beneath the value, so a paired OpWithPutVar can write
			// back to the same base (compound assignment; see emitWithVarRef).
			refMode := code[ip+7]&0x80 != 0
			// 0x40: a `typeof` read, whose global fallback yields undefined instead
			// of throwing for an unresolvable reference.
			lenient := code[ip+7]&0x40 != 0
			// 0x20 (reference mode only): resolve the base and push nothing else —
			// a plain assignment creates its Reference without reading through it.
			baseOnly := refMode && code[ip+7]&0x20 != 0
			// 0x10 (reference mode only): the base is a call's `this`, so an absent
			// one is undefined rather than the write-back marker.
			thisMode := refMode && code[ip+7]&0x10 != 0
			fbKind := code[ip+7] & 0x0f
			found := false
			for k := len(withStack) - 1; k >= 0; k-- {
				has, e := rt.hasPropE(withStack[k], name)
				if e != nil {
					thrown = e
					goto unwind
				}
				// @@unscopables is consulted only once the object actually has the
				// name (Object Environment Record HasBinding step 2 returns before the
				// @@unscopables Get when HasProperty is false).
				unscoped := false
				if has {
					u, ue := rt.isUnscopable(withStack[k], name)
					if ue != nil {
						thrown = ue
						goto unwind
					}
					unscoped = u
				}
				if has && !unscoped {
					if baseOnly {
						push(withStack[k])
						found = true
						break
					}
					// GetBindingValue performs its OWN HasProperty (step 2) before the
					// Get (step 4) — a second observable trap on a Proxy binding object,
					// distinct from the one HasBinding just did. If the binding is gone
					// by then (an @@unscopables getter deleted it), strict code throws a
					// ReferenceError and sloppy code reads undefined.
					still, e := rt.hasPropE(withStack[k], name)
					if e != nil {
						thrown = e
						goto unwind
					}
					if !still {
						if fn.isStrict {
							thrown = rt.referenceError(name + " is not defined")
							goto unwind
						}
						if refMode {
							push(withStack[k])
						}
						push(mkundef())
						found = true
						break
					}
					v, e := rt.getField(withStack[k], name)
					if e != nil {
						thrown = e
						goto unwind
					}
					if refMode {
						push(withStack[k]) // base
					}
					push(v)
					found = true
					break
				}
			}
			if !found {
				if refMode {
					if thisMode {
						push(mkundef()) // no binding object: `this` is undefined
					} else {
						push(tEmpty) // base marker: use the lexical fallback on write
					}
				}
				if baseOnly {
					ip += 8
					continue
				}
				// No with-object binds the name: fall back to the lexical resolution
				// the compiler baked into the spare operand bytes (kind@ip+7, index@ip+5).
				switch fbKind {
				case 1: // local slot
					lv := locals[readU16(code, ip+5)]
					if lv.IsEmpty() {
						thrown = rt.referenceError("Cannot access a lexical binding before initialization")
						goto unwind
					}
					push(lv)
				case 2: // upvalue
					uvv := cl.upvalues[readU16(code, ip+5)].get()
					if uvv.IsEmpty() {
						thrown = rt.referenceError("Cannot access a lexical binding before initialization")
						goto unwind
					}
					push(uvv)
				default: // global
					// GetValue on an unresolvable reference throws, exactly as a plain
					// OpGetGlobal would; `typeof` marks itself lenient instead.
					if !lenient && !rt.hasProp(rt.global, name) {
						thrown = rt.referenceError(name + " is not defined")
						goto unwind
					}
					v, ge := rt.getField(rt.global, name)
					if ge != nil {
						thrown = ge
						goto unwind
					}
					push(v)
				}
			}
			ip += 8
		case OpWithPutVar:
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			refMode := code[ip+7]&0x80 != 0
			fbKind := code[ip+7] & 0x7f
			val := pop()
			if refMode {
				// Reference mode: write to the base captured by the paired
				// OpWithGetVar (compound assignment). tEmpty means "use the lexical
				// fallback". The assignment value is left on the stack (it is an
				// expression).
				base := pop()
				if base.IsEmpty() {
					switch fbKind {
					case 1:
						locals[readU16(code, ip+5)] = val
					case 2:
						cl.upvalues[readU16(code, ip+5)].set(val)
					default:
						// A strict assignment to a name bound by no with-object and no
						// lexical binding is a ReferenceError (assignment to an undeclared
						// global), matching OpPutGlobal.
						if fn.isStrict && !rt.hasProp(rt.global, name) {
							thrown = rt.referenceError(name + " is not defined")
							goto unwind
						}
						if !rt.setProp(rt.global, name, val) && fn.isStrict {
							thrown = rt.typeError("Cannot assign to read only property '" + name + "'")
							goto unwind
						}
					}
				} else {
					// Object Environment Record SetMutableBinding: if the bound property
					// was deleted between the reference's read and this write (e.g. a
					// getter deleted it), a strict assignment is a ReferenceError rather
					// than re-creating the property.
					has, he := rt.hasPropE(base, name)
					if he != nil {
						thrown = he
						goto unwind
					}
					if !has && fn.isStrict {
						thrown = rt.referenceError(name + " is not defined")
						goto unwind
					}
					if e := rt.setField(base, name, val); e != nil {
						thrown = e
						goto unwind
					}
				}
				push(val)
				ip += 8
				continue
			}
			stored := false
			for k := len(withStack) - 1; k >= 0; k-- {
				has, e := rt.hasPropE(withStack[k], name)
				if e != nil {
					thrown = e
					goto unwind
				}
				// @@unscopables is consulted only once the object actually has the
				// name (Object Environment Record HasBinding step 2 returns before the
				// @@unscopables Get when HasProperty is false).
				unscoped := false
				if has {
					u, ue := rt.isUnscopable(withStack[k], name)
					if ue != nil {
						thrown = ue
						goto unwind
					}
					unscoped = u
				}
				if has && !unscoped {
					// SetMutableBinding re-checks HasProperty (step 1) before the Set
					// (step 3): a second observable trap, and the point at which a
					// strict write to a binding deleted since HasBinding is caught.
					stillExists, se := rt.hasPropE(withStack[k], name)
					if se != nil {
						thrown = se
						goto unwind
					}
					if !stillExists && fn.isStrict {
						thrown = rt.referenceError(name + " is not defined")
						goto unwind
					}
					if e := rt.setField(withStack[k], name, val); e != nil {
						thrown = e
						goto unwind
					}
					stored = true
					break
				}
			}
			if !stored {
				// Fall back to the compiler's lexical resolution (see OpWithGetVar).
				switch fbKind {
				case 1: // local slot
					locals[readU16(code, ip+5)] = val
				case 2: // upvalue
					cl.upvalues[readU16(code, ip+5)].set(val)
				default: // global
					// Strict assignment to an undeclared global is a ReferenceError
					// (matches OpPutGlobal); this surfaces when a strict function nested
					// in a `with` writes a name the with-object no longer binds.
					if fn.isStrict && !rt.hasProp(rt.global, name) {
						thrown = rt.referenceError(name + " is not defined")
						goto unwind
					}
					if !rt.setProp(rt.global, name, val) && fn.isStrict {
						thrown = rt.typeError("Cannot assign to read only property '" + name + "'")
						goto unwind
					}
				}
			}
			ip += 8
		case OpWithDelVar:
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			done := false
			for k := len(withStack) - 1; k >= 0; k-- {
				has, e := rt.hasPropE(withStack[k], name)
				if e != nil {
					thrown = e
					goto unwind
				}
				// @@unscopables is consulted only once the object actually has the
				// name (Object Environment Record HasBinding step 2 returns before the
				// @@unscopables Get when HasProperty is false).
				unscoped := false
				if has {
					u, ue := rt.isUnscopable(withStack[k], name)
					if ue != nil {
						thrown = ue
						goto unwind
					}
					unscoped = u
				}
				if has && !unscoped {
					ok, e := rt.deleteElement(withStack[k], rt.internString(name))
					if e != nil {
						thrown = e
						goto unwind
					}
					push(mkbool(ok))
					done = true
					break
				}
			}
			if !done {
				// Not bound by a with-object: fall back to a global-object delete (a
				// declared var/function is non-configurable → false, an implicit global
				// or absent name → true).
				ok, e := rt.deleteElement(rt.global, rt.internString(name))
				if e != nil {
					thrown = e
					goto unwind
				}
				push(mkbool(ok))
			}
			ip += 5
		case OpSpecialObj:
			kind := code[ip+1]
			switch kind {
			case 0: // arguments
				// The arguments object is an ordinary exotic object (NOT an Array), so
				// its own "length" data property is fixed at the argument count and does
				// not shift when an index past it is assigned.
				a := rt.newObject(rt.objectProto)
				ao := rt.objPtr(a)
				for i, v := range args {
					ao.defineOwn(numberToString(float64(i)), v, attrDefault)
				}
				if fn.mappedArgs {
					ao.argMap = newArgumentsMap(locals, fn.paramCount, len(args))
				}
				ao.defineOwn("length", mknum(float64(len(args))), attrWritable|attrConfigurable)
				if fn.isStrict {
					// Strict arguments: `callee` is a poison-pill accessor.
					ao.defineAccessor("callee", rt.poison, rt.poison, true, true, 0)
				} else {
					ao.defineOwn("callee", fnVal, attrWritable|attrConfigurable)
				}
				// The arguments object has its OWN @@iterator (%Array.prototype.values%).
				if rt.symIterator != 0 {
					if vals, e := rt.getField(rt.arrayProto, "values"); e == nil {
						ao.defineOwnSymbol(rt.symIterator.handle(), vals, attrWritable|attrConfigurable)
					}
				}
				push(a)
			case 1: // current function value (named function self-reference)
				push(fnVal)
			case 2: // new.target
				push(newTarget)
			case 3: // import.meta — one ordinary object per Module, created once
				cell := fn.metaCell
				if cell == nil {
					cell = &rt.importMeta
				}
				if *cell == 0 {
					*cell = rt.newObject(rt.objectProto)
				}
				push(*cell)
			case 4:
				// Enter a class body: push a fresh ClassPrivateEnvironment link. Every
				// closure created while it is in effect (methods, field initializers,
				// the constructor) captures the chain, which is what gives each
				// evaluation of the same class body its own Private Names — while a
				// nested class body still reaches the enclosing class's names.
				rt.privEnvSeq++
				privEnv = &privScope{tag: rt.privEnvSeq, parent: privEnv}
			case 5:
				// Leave a class body.
				if privEnv != nil {
					privEnv = privEnv.parent
				}
			default:
				push(mkundef())
			}
			ip += 2
		case OpThis:
			push(thisVal)
			ip++
		case OpSetProto:
			// obj proto -> obj
			proto := pop()
			objV := peek()
			if o := rt.objPtr(objV); o != nil && (proto.IsObjectType() || proto.IsNull()) {
				o.proto = proto
			}
			ip++
		case OpIsUndef:
			push(mkbool(pop().IsUndefined()))
			ip++
		case OpIsUndefOrNull:
			push(mkbool(pop().IsNullish()))
			ip++
		case OpGetArg:
			// Raw positional argument (undefined if not supplied), bypassing the
			// parameter local so it can be read while that local is in its TDZ.
			ai := int(readU16(code, ip+1))
			if ai < len(args) {
				push(args[ai])
			} else {
				push(mkundef())
			}
			ip += 3
		case OpRest:
			start := int(readU16(code, ip+1))
			restArr := rt.newArray()
			ro := rt.objPtr(restArr)
			for i := start; i < len(args); i++ {
				rt.arraySet(ro, uint32(i-start), args[i])
			}
			push(restArr)
			ip += 3
		case OpTrue:
			push(mktrue())
			ip++
		case OpFalse:
			push(mkfalse())
			ip++
		case OpConst:
			idx := readU32(code, ip+1)
			push(fn.constants[idx])
			ip += 5
		case OpConstI8:
			push(mknum(float64(int8(code[ip+1]))))
			ip += 2
		case OpPop:
			pop()
			ip++
		case OpDup:
			push(peek())
			ip++
		case OpInsert2:
			// obj a -> a obj a
			a := pop()
			obj := pop()
			push(a)
			push(obj)
			push(a)
			ip++
		case OpSwapUnder:
			// a b c -> b a c (swap the two values UNDER the top one)
			cv := pop()
			b := pop()
			a := pop()
			push(b)
			push(a)
			push(cv)
			ip++
		case OpInsert3:
			// obj prop a -> a obj prop a
			a := pop()
			prop := pop()
			obj := pop()
			push(a)
			push(obj)
			push(prop)
			push(a)
			ip++

		case OpObject:
			push(rt.newPlainObject())
			ip++
		case OpCopyDataProps:
			// Object spread: copy src's enumerable own props into target.
			// Stack: [target, src] -> [target]. With the inline flag set, an array
			// of already-bound keys sits under src (object rest): [target,
			// excludedArray, src].
			src := pop()
			var excluded []Value
			if code[ip+1] == 1 {
				if eo := rt.objPtr(pop()); eo != nil {
					excluded = eo.arr[:eo.arrLen]
				}
			}
			target := pop()
			if e := rt.copyDataPropsExcluding(target, src, excluded); e != nil {
				thrown = e
				goto unwind
			}
			push(target)
			ip += 2
		case OpArray:
			n := int(readU16(code, ip+1))
			arrv := rt.newArray()
			ao := rt.objPtr(arrv)
			ao.arr = make([]Value, n)
			ao.arrLen = uint32(n)
			for i := n - 1; i >= 0; i-- {
				ao.arr[i] = pop()
			}
			push(arrv)
			ip += 3
		case OpDefineField:
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			val := pop()
			if o := rt.objPtr(peek()); o != nil {
				if isPrivateKey(name) {
					if !o.flags.extensible {
						// PrivateFieldAdd step 1: a private field cannot be added to a
						// non-extensible object (a constructor return override can hand
						// the class one).
						thrown = rt.typeError("Cannot add private member " + privDisplay(name) + " to a non-extensible object")
						goto unwind
					}
					if !o.definePrivateField(name, privEnv, val) {
						thrown = rt.typeError("Cannot initialize private field " + privDisplay(name) + " twice on the same object")
						goto unwind
					}
				} else if o.proxy != nil || !o.flags.extensible {
					// A field / object-literal property is installed with
					// CreateDataPropertyOrThrow. For a Proxy receiver this must fire the
					// defineProperty trap, and for a non-extensible/frozen target adding
					// (or redefining a non-configurable) property throws — route through
					// the full [[DefineOwnProperty]] rather than the fast slot write.
					if e := rt.createDataProperty(peek(), rt.internString(name), val); e != nil {
						thrown = e
						goto unwind
					}
				} else {
					o.defineOwn(name, val, attrDefault)
				}
			}
			ip += 5
		case OpDefineMethod:
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			flags := code[ip+5]
			enumerable := flags&4 != 0 // bit 2: object-literal accessor (enumerable)
			shared := flags&8 != 0     // bit 3: shared private method, already homed
			flags &= 3
			accFn := pop()
			if !shared {
				rt.setMethodHome(accFn, peek()) // [[HomeObject]] for a super-using method
			}
			if isPrivateKey(name) {
				if o := rt.objPtr(peek()); o != nil {
					if !o.flags.extensible {
						// PrivateMethodOrAccessorAdd, like PrivateFieldAdd, refuses a
						// non-extensible object.
						thrown = rt.typeError("Cannot add private member " + privDisplay(name) + " to a non-extensible object")
						goto unwind
					}
					ok := true
					switch flags {
					case 1:
						ok = o.definePrivateAccessor(name, privEnv, accFn, true)
					case 2:
						ok = o.definePrivateAccessor(name, privEnv, accFn, false)
					default:
						ok = o.definePrivateMethod(name, privEnv, accFn)
					}
					if !ok {
						thrown = rt.typeError("Cannot install private method " + privDisplay(name) + " twice on the same object")
						goto unwind
					}
				}
				ip += 6
				break
			}
			if o := rt.objPtr(peek()); o != nil {
				switch flags {
				case 0: // data method: non-enumerable, writable, configurable
					o.defineOwn(name, accFn, attrWritable|attrConfigurable)
				default: // accessor: 1=getter, 2=setter (merging with existing)
					g, s := mkundef(), mkundef()
					hg, hs := false, false
					if d := o.ownDescriptor(name); d.exists && d.isAccessor {
						g, s = d.getter, d.setter
						hg, hs = !d.getter.IsUndefined(), !d.setter.IsUndefined()
					}
					if flags == 1 {
						g, hg = accFn, true
					} else {
						s, hs = accFn, true
					}
					attrs := uint8(attrConfigurable) // class accessors are non-enumerable
					if enumerable {
						attrs |= attrEnumerable
					}
					o.defineAccessor(name, g, s, hg, hs, attrs)
				}
			}
			ip += 6

		case OpDefineMethodComp:
			// [target, key, func] -> [target]. flags: 0=data method, 1=getter,
			// 2=setter, 3=data property. The key is any property key.
			flags := code[ip+1]
			accFn := pop()
			key := pop()
			rt.setMethodHome(accFn, peek()) // [[HomeObject]] for a super-using method
			// ToPropertyKey once (observable), then SetFunctionName on the anonymous
			// method/accessor from that key (a plain [k]:v data property is named by
			// OpSetNameComp instead). The coerced key is reused by the define below.
			pk, e := rt.toPropertyKey(key)
			if e != nil {
				thrown = e
				goto unwind
			}
			switch flags & 3 {
			case 0:
				rt.nameMethodFromKey(accFn, pk, "")
			case 1:
				rt.nameMethodFromKey(accFn, pk, "get ")
			case 2:
				rt.nameMethodFromKey(accFn, pk, "set ")
			}
			if e := rt.defineMethodComputed(peek(), pk, accFn, flags); e != nil {
				thrown = e
				goto unwind
			}
			ip += 2

		case OpGetField:
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			obj := pop()
			var v Value
			var e *ThrowError
			if isPrivateKey(name) {
				v, e = rt.getPrivate(obj, name, privEnv)
			} else {
				v, e = rt.getField(obj, name)
			}
			if e != nil {
				thrown = e
				goto unwind
			}
			push(v)
			ip += 7
		case OpGetField2:
			// obj -> obj val (keeps the receiver for a following method call)
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			obj := peek()
			var v Value
			var e *ThrowError
			if isPrivateKey(name) {
				v, e = rt.getPrivate(obj, name, privEnv)
			} else {
				v, e = rt.getField(obj, name)
			}
			if e != nil {
				thrown = e
				goto unwind
			}
			push(v)
			ip += 7
		case OpPutField:
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			val := pop()
			obj := pop()
			if isPrivateKey(name) {
				if e := rt.setPrivate(obj, name, privEnv, val); e != nil {
					thrown = e
					goto unwind
				}
				ip += 7
				break
			}
			ok, e := rt.setFieldR(obj, name, val)
			if e != nil {
				thrown = e
				goto unwind
			}
			if !ok && fn.isStrict {
				thrown = rt.typeError("Cannot assign to read only property '" + name + "'")
				goto unwind
			}
			ip += 7
		case OpGetElem:
			key := pop()
			v, e := rt.getElement(pop(), key)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(v)
			ip++
		case OpPutElem:
			val := pop()
			key := pop()
			obj := pop()
			ok, e := rt.setElementR(obj, key, val)
			if e != nil {
				thrown = e
				goto unwind
			}
			if !ok && fn.isStrict {
				thrown = rt.typeError("Cannot assign to read only property")
				goto unwind
			}
			ip++
		case OpGetLength:
			v, e := rt.getField(pop(), "length")
			if e != nil {
				thrown = e
				goto unwind
			}
			push(v)
			ip++
		case OpCallMethod:
			argc := int(readU16(code, ip+1))
			callArgs := make([]Value, argc)
			for i := argc - 1; i >= 0; i-- {
				callArgs[i] = pop()
			}
			fnVal := pop()
			thisArg := pop()
			ret, e := rt.callValue(fnVal, thisArg, callArgs)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(ret)
			ip += 3
		case OpGetGlobal:
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			// The global environment's declarative record is consulted first: a
			// Script-level let/const/class shadows a same-named global property.
			if b := rt.lookupGlobalLex(name); b != nil {
				v, e := rt.globalLexRead(b, name)
				if e != nil {
					thrown = e
					goto unwind
				}
				push(v)
				ip += 7
				continue
			}
			// GetValue on an unresolvable reference throws (a bare undeclared name);
			// typeof reads via GET_GLOBAL_UNDEF instead, so it never reaches here.
			if !rt.hasProp(rt.global, name) {
				thrown = rt.referenceError(name + " is not defined")
				goto unwind
			}
			// A global bound as an accessor property must invoke its getter (and
			// propagate an abrupt getter completion), so read via the ordinary
			// [[Get]], not the raw slot read getProp performs.
			v, ge := rt.getField(rt.global, name)
			if ge != nil {
				thrown = ge
				goto unwind
			}
			push(v)
			ip += 7
		case OpGetGlobalUndef:
			// Lenient global read (typeof of a possibly-undeclared global): absent
			// names yield undefined rather than a ReferenceError. A global lexical
			// binding is NOT absent, so its temporal dead zone still throws.
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			if b := rt.lookupGlobalLex(name); b != nil {
				v, e := rt.globalLexRead(b, name)
				if e != nil {
					thrown = e
					goto unwind
				}
				push(v)
				ip += 7
				continue
			}
			v, ge := rt.getField(rt.global, name)
			if ge != nil {
				thrown = ge
				goto unwind
			}
			push(v)
			ip += 7
		case OpPutGlobal:
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			val := pop()
			if b := rt.lookupGlobalLex(name); b != nil {
				if e := rt.globalLexWrite(b, name, val); e != nil {
					thrown = e
					goto unwind
				}
				ip += 5
				continue
			}
			if fn.isStrict && !rt.hasProp(rt.global, name) {
				thrown = rt.referenceError(name + " is not defined")
				goto unwind
			}
			ok := rt.setProp(rt.global, name, val)
			if !ok && fn.isStrict {
				thrown = rt.typeError("Cannot assign to read only property '" + name + "'")
				goto unwind
			}
			ip += 5
		case OpDeleteVar:
			// Reused as strict-mode ResolveBinding, emitted before a simple
			// assignment's right-hand side: it records whether the name bound to
			// anything, and the paired OpPutConst below throws afterwards if it did
			// not. Resolving first is observable — the RHS may create the binding.
			nm := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			push(mkbool(rt.lookupGlobalLex(nm) != nil || rt.hasProp(rt.global, nm)))
			ip += 5
		case OpPutConst:
			// Reused as PutValue for the reference OpDeleteVar resolved:
			// [resolvable, value] -> [value].
			pcVal := pop()
			pcOK := pop()
			pcName := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			if !rt.toBoolean(pcOK) {
				thrown = rt.referenceError(pcName + " is not defined")
				goto unwind
			}
			if b := rt.lookupGlobalLex(pcName); b != nil {
				if e := rt.globalLexWrite(b, pcName, pcVal); e != nil {
					thrown = e
					goto unwind
				}
			} else if !rt.hasProp(rt.global, pcName) {
				// Object Environment Record SetMutableBinding re-checks HasProperty:
				// a strict assignment whose right-hand side DELETED the global also
				// throws, even though the reference resolved.
				thrown = rt.referenceError(pcName + " is not defined")
				goto unwind
			} else if !rt.setProp(rt.global, pcName, pcVal) {
				thrown = rt.typeError("Cannot assign to read only property '" + pcName + "'")
				goto unwind
			}
			push(pcVal)
			ip += 5
		case OpGetLocal:
			lv := locals[readU16(code, ip+1)]
			if lv.IsEmpty() {
				// Reading a lexical binding still in its temporal dead zone.
				thrown = rt.referenceError("Cannot access a lexical binding before initialization")
				goto unwind
			}
			push(lv)
			ip += 3
		case OpPutLocal:
			locals[readU16(code, ip+1)] = pop()
			ip += 3
		case OpSetLocal:
			locals[readU16(code, ip+1)] = peek()
			ip += 3

		case OpAdd:
			b, a := pop(), pop()
			v, e := rt.jsAdd(a, b)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(v)
			ip++
		case OpSub, OpMul, OpDiv, OpMod, OpExp:
			b, a := pop(), pop()
			v, e := rt.jsArith(op, a, b)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(v)
			ip++
		case OpNeg:
			a, e := rt.toNumeric(pop())
			if e != nil {
				thrown = e
				goto unwind
			}
			if a.Type() == TBigInt {
				push(rt.newBigInt(new(big.Int).Neg(rt.bigIntVal(a))))
				ip++
				break
			}
			push(mknum(-a.Number()))
			ip++
		case OpUplus:
			a := pop()
			n, e := rt.toNumber(a)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(mknum(n))
			ip++
		case OpGetSuperVal:
			// [receiver(this), base(superproto), key] -> value. A super read starts
			// the lookup at the home object's prototype but keeps `this` as the
			// accessor receiver.
			key := pop()
			base := pop()
			recv := pop()
			v, e := rt.getSuperProp(base, key, recv)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(v)
			ip++
		case OpPutSuperVal:
			// [receiver(this), base(superproto), key, val] -> val. A super write
			// performs base.[[Set]](key, val, this): a setter on the super chain runs
			// with `this`, and a writable data property is created on `this`.
			val := pop()
			key := pop()
			base := pop()
			recv := pop()
			ok, e := rt.putSuperProp(base, key, val, recv)
			if e != nil {
				thrown = e
				goto unwind
			}
			if !ok && fn.isStrict {
				thrown = rt.typeError("Cannot assign to read only super property")
				goto unwind
			}
			push(val)
			ip++
		case OpChkCtor:
			// A class heritage must be null or a constructor (IsConstructor); an
			// arrow / async / generator / method function is callable but not a
			// constructor, so `class C extends thatFn {}` throws at definition.
			sc := pop()
			if !sc.IsNull() && !rt.isConstructorValue(sc) {
				thrown = rt.typeError("Class extends value is not a constructor or null")
				goto unwind
			}
			ip++
		case OpChkProto:
			// A superclass's .prototype (the protoParent) must be an Object or null
			// (e.g. a bound function is a constructor but has no "prototype").
			pv := pop()
			if !pv.IsNull() && !pv.IsObjectType() {
				thrown = rt.typeError("Class extends value does not have valid prototype property")
				goto unwind
			}
			ip++
		case OpSetHomeObj:
			// [obj, method] (unchanged): record the method's [[HomeObject]] as obj
			// so a super reference in the method resolves against obj's prototype.
			if len(stack) >= 2 {
				rt.setMethodHome(stack[len(stack)-1], stack[len(stack)-2])
			}
			ip++
		case OpGetSuper:
			// Push the [[Prototype]] of the current object-literal method's
			// HomeObject — the base for a super-property lookup outside a class.
			var base Value
			if cl != nil && cl.home.IsObjectType() {
				p, e := rt.getPrototypeOfValue(cl.home)
				if e != nil {
					thrown = e
					goto unwind
				}
				base = p
			}
			push(base)
			ip++
		case OpSetNameComp:
			// [key, func] (unchanged): NamedEvaluation for an anonymous function or
			// class in a computed property — set func.name from the property key
			// ("[desc]" for a symbol, the key string otherwise). Emitted only when
			// the compiler statically knows the value is an anonymous definition.
			if len(stack) >= 2 {
				rt.setInferredNameFromKey(stack[len(stack)-1], stack[len(stack)-2])
			}
			ip++
		case OpUsingPush, OpUsingPushAsync:
			// [entries, resource] -> [resource]: register the resource's disposer.
			resource := pop()
			entries := pop()
			r, e := rt.usingPush(entries, resource, op == OpUsingPushAsync)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(r)
			ip++
		case OpUsingDispose, OpUsingDisposeAsync:
			// [entries] -> [undefined]: dispose the block's resources on normal
			// completion; a disposal error becomes a throw. The async form awaits
			// each async disposer's result.
			entries := pop()
			var completion Value
			if op == OpUsingDisposeAsync {
				completion = rt.disposeEntriesAsync(entries, mkundef())
			} else {
				completion = rt.disposeEntries(entries, mkundef())
			}
			if !completion.IsUndefined() {
				thrown = &ThrowError{Value: completion, rt: rt}
				goto unwind
			}
			push(mkundef())
			ip++
		case OpUsingDisposeSuppressed, OpUsingDisposeAsyncSuppressed:
			// [entries, completion] -> [completion']: dispose on abrupt completion,
			// folding disposal errors into the pending one; the following OpThrow
			// re-raises the aggregate.
			completion := pop()
			entries := pop()
			if op == OpUsingDisposeAsyncSuppressed {
				push(rt.disposeEntriesAsync(entries, completion))
			} else {
				push(rt.disposeEntries(entries, completion))
			}
			ip++
		case OpToObject:
			// ToObject: null/undefined throw a TypeError; an Object (including a
			// TypedArray) is returned unchanged, so `ToObject(x) === x` is an
			// allocation-free test for "x is already an Object" — a primitive yields a
			// fresh wrapper and so compares unequal.
			ov, oe := rt.toObjectValue(pop())
			if oe != nil {
				thrown = oe
				goto unwind
			}
			push(ov)
			ip++
		case OpToPropkey:
			// Coerce to a property key (ToPrimitive with hint "string"). Used by
			// template substitution so `${obj}` prefers toString over valueOf, and
			// the following OpAdd concatenates the string primitive.
			a := pop()
			pk, e := rt.toPropertyKey(a)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(pk)
			ip++
		case OpInc:
			a := pop()
			if a.Type() == TBigInt {
				push(rt.newBigInt(new(big.Int).Add(rt.bigIntVal(a), big.NewInt(1))))
				ip++
			} else {
				n, e := rt.toNumber(a)
				if e != nil {
					thrown = e
					goto unwind
				}
				push(mknum(n + 1))
				ip++
			}
		case OpDec:
			a := pop()
			if a.Type() == TBigInt {
				push(rt.newBigInt(new(big.Int).Sub(rt.bigIntVal(a), big.NewInt(1))))
				ip++
			} else {
				n, e := rt.toNumber(a)
				if e != nil {
					thrown = e
					goto unwind
				}
				push(mknum(n - 1))
				ip++
			}
		case OpNot:
			push(mkbool(!rt.toBoolean(pop())))
			ip++
		case OpTypeof:
			push(rt.internString(rt.typeofString(pop())))
			ip++
		case OpVoid:
			pop()
			push(mkundef())
			ip++
		case OpImport:
			// Dynamic import(specifier, options). The specifier is coerced to a
			// string; import() never throws synchronously — a bad specifier or an
			// (as yet) unsupported module load rejects the returned promise instead.
			options := pop()
			specifier := pop()
			switch spec, e := rt.toStringValue(specifier); {
			case e != nil:
				push(rt.rejectedPromise(e.Value))
			default:
				typ, oe := rt.validateImportOptions(options)
				if oe != nil {
					push(rt.rejectedPromise(oe.Value))
				} else if ns, le := rt.importModuleNamespace(joinModuleKey(string(rt.strBytes(spec)), typ), fn.filename); le != nil {
					push(rt.rejectedPromise(le.Value))
				} else {
					push(rt.resolvedPromise(ns))
				}
			}
			ip++
		case OpImportSync:
			// Static import: load the requested module and leave its namespace
			// object on the stack for the importing frame to keep in a local.
			spec := pop()
			ns, e := rt.importModuleNamespace(string(rt.strBytes(spec)), fn.filename)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(ns)
			ip++
		case OpImportNamed:
			// Read one binding out of a namespace object. This runs on every
			// reference to an imported name, which is what keeps the binding live.
			name := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			ns := pop()
			v, e := rt.getField(ns, name)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(v)
			ip += 5
		case OpDelete:
			key := pop()
			obj := pop()
			ok, e := rt.deleteElement(obj, key)
			if e != nil {
				thrown = e
				goto unwind
			}
			if !ok && fn.isStrict {
				thrown = rt.typeError("Cannot delete property of a non-configurable object")
				goto unwind
			}
			push(mkbool(ok))
			ip++
		case OpIn:
			obj := pop()
			key := pop()
			res, e := rt.jsIn(key, obj, privEnv)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(mkbool(res))
			ip++
		case OpInstanceof:
			r := pop()
			l := pop()
			res, e := rt.jsInstanceof(l, r)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(mkbool(res))
			ip += 3
		case OpBnot:
			a, e := rt.toNumeric(pop())
			if e != nil {
				thrown = e
				goto unwind
			}
			if a.Type() == TBigInt {
				// BigInt::bitwiseNOT: ~x = -(x + 1).
				push(rt.newBigInt(new(big.Int).Not(rt.bigIntVal(a))))
				ip++
			} else {
				n, e := rt.toInt32(a)
				if e != nil {
					thrown = e
					goto unwind
				}
				push(mknum(float64(^n)))
				ip++
			}
		case OpBand, OpBor, OpBxor, OpShl, OpShr, OpUshr:
			b, a := pop(), pop()
			v, e := rt.jsBitwise(op, a, b)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(v)
			ip++

		case OpLt, OpLe, OpGt, OpGe:
			b, a := pop(), pop()
			v, e := rt.jsRelational(op, a, b)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(v)
			ip++
		case OpEq:
			b, a := pop(), pop()
			r, e := rt.abstractEquals(a, b)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(mkbool(r))
			ip++
		case OpNe:
			b, a := pop(), pop()
			r, e := rt.abstractEquals(a, b)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(mkbool(!r))
			ip++
		case OpSeq:
			b, a := pop(), pop()
			push(mkbool(rt.strictEquals(a, b)))
			ip++
		case OpSne:
			b, a := pop(), pop()
			push(mkbool(!rt.strictEquals(a, b)))
			ip++

		case OpJmp:
			ip = int(readU32(code, ip+1))
		case OpJmpFalse:
			if !rt.toBoolean(pop()) {
				ip = int(readU32(code, ip+1))
			} else {
				ip += 5
			}
		case OpJmpTrue:
			if rt.toBoolean(pop()) {
				ip = int(readU32(code, ip+1))
			} else {
				ip += 5
			}
		case OpJmpNotNullish:
			if !pop().IsNullish() {
				ip = int(readU32(code, ip+1))
			} else {
				ip += 5
			}

		case OpThrow:
			thrown = &ThrowError{Value: pop(), rt: rt}
			goto unwind

		case OpThrowError:
			// Throw a native error of the given kind (0=TypeError, 1=ReferenceError,
			// 2=SyntaxError, 3=RangeError) with a constant message.
			msg := string(rt.strBytes(fn.constants[readU32(code, ip+1)]))
			switch code[ip+5] {
			case 1:
				thrown = rt.referenceError(msg)
			case 2:
				thrown = rt.syntaxError(msg)
			case 3:
				thrown = rt.rangeError(msg)
			default:
				thrown = rt.typeError(msg)
			}
			goto unwind

		case OpYieldStarInit:
			// Lazy `yield* iterable`: drive the inner iterator, forwarding how the
			// outer generator is resumed (next/throw/return) to inner.next / throw /
			// return, and re-yielding each produced value. Leaves the delegate's
			// final value on the stack.
			// In an async generator `yield* x` delegates to x's async iterator
			// (falling back to a wrapped sync iterator) and awaits each inner result.
			var inner Value
			var e *ThrowError
			if fn.isAsync {
				inner, e = rt.getAsyncIterator(pop())
			} else {
				inner, e = rt.getSyncIterator(pop())
			}
			if e != nil {
				thrown = e
				goto unwind
			}
			// GetIterator caches the iterator's "next" method once; the delegation
			// CALLS the cached method each round rather than re-getting it (throw and
			// return are still looked up on demand, per the yield* algorithm).
			nextFn, e := rt.getField(inner, "next")
			if e != nil {
				thrown = e
				goto unwind
			}
			sent := mkundef()
			kind := genNext
			ipSet := false // set when a genReturn routes into an enclosing finally
		yieldStar:
			for {
				var result Value
				var re *ThrowError
				switch kind {
				case genThrow:
					throwFn, ge := rt.getField(inner, "throw")
					if ge != nil {
						thrown = ge
						goto unwind
					}
					if !rt.isCallable(throwFn) {
						// No `throw`: close the inner iterator with a NORMAL completion
						// and only then report the protocol violation. The close is a
						// `?` step, so an error it raises wins over the TypeError.
						if ce := rt.iteratorCloseE(inner); ce != nil {
							thrown = ce
							goto unwind
						}
						thrown = rt.typeError("The iterator does not provide a 'throw' method")
						goto unwind
					}
					result, re = rt.callValue(throwFn, inner, []Value{sent})
				case genReturn:
					returnFn, ge := rt.getField(inner, "return")
					if ge != nil {
						thrown = ge
						goto unwind
					}
					if !rt.isCallable(returnFn) {
						// No return method: propagate the return, running any finally.
						if fip, ok := doReturn(sent); ok {
							ip = fip
							ipSet = true
							break yieldStar
						}
						closeAll()
						return sent, nil
					}
					result, re = rt.callValue(returnFn, inner, []Value{sent})
				default:
					result, re = rt.callValue(nextFn, inner, []Value{sent})
				}
				if re != nil {
					thrown = re
					goto unwind
				}
				if fn.isAsync {
					// Await the inner iterator result (it is a promise). A throw/return
					// injected during the await propagates out of the delegation.
					ares, ainj := rt.suspend(result, true)
					if ainj != nil {
						if ainj.kind == genThrow {
							thrown = &ThrowError{Value: ainj.val, rt: rt}
							goto unwind
						}
						if fip, ok := doReturn(ainj.val); ok {
							ip = fip
							ipSet = true
							break yieldStar
						}
						closeAll()
						return ainj.val, nil
					}
					result = ares
				}
				if !result.IsObjectType() {
					thrown = rt.typeError("iterator result is not an object")
					goto unwind
				}
				doneV, de := rt.getField(result, "done") // IteratorComplete: ? Get(result, "done")
				if de != nil {
					thrown = de
					goto unwind
				}
				if rt.toBoolean(doneV) {
					value, ve := rt.getField(result, "value") // IteratorValue: ? Get(result, "value")
					if ve != nil {
						thrown = ve
						goto unwind
					}
					if kind == genReturn {
						// Inner honored return: propagate it, running any finally.
						if fip, ok := doReturn(value); ok {
							ip = fip
							ipSet = true
							break yieldStar
						}
						closeAll()
						return value, nil
					}
					push(value)
					break
				}
				// Not done. A SYNC generator re-yields the inner result OBJECT
				// unchanged (GeneratorYield(innerResult)) — its `value` is never read,
				// and a missing `done` stays missing. An ASYNC generator instead reads
				// the value and yields that (AsyncGeneratorYield(? IteratorValue(…))).
				var resumed Value
				var inject *genResume
				if fn.isAsync {
					value, ve := rt.getField(result, "value")
					if ve != nil {
						thrown = ve
						goto unwind
					}
					resumed, inject = rt.suspendYieldNoAwait(value)
				} else {
					resumed, inject = rt.suspendRaw(result)
				}
				sent, kind = resumed, genNext
				if inject != nil {
					sent, kind = inject.val, inject.kind
				}
			}
			if !ipSet {
				ip += 3
			}
		case OpYield, OpAwait:
			// Suspend this coroutine, handing the operand to its driver and
			// blocking until resumed. A throw/return injection unwinds instead of
			// producing a resume value. The await flag lets an async-generator
			// driver await the operand rather than re-yielding it.
			resumed, inject := rt.suspend(pop(), op == OpAwait)
			if inject != nil {
				switch inject.kind {
				case genThrow:
					thrown = &ThrowError{Value: inject.val, rt: rt}
					goto unwind
				case genReturn:
					// A forced return runs any enclosing finally before exiting.
					if fip, ok := doReturn(inject.val); ok {
						ip = fip
						continue
					}
					closeAll()
					return inject.val, nil
				}
			}
			push(resumed)
			ip++
		case OpTryPush:
			handlers = append(handlers, tryHandler{kind: hTry, catchIP: int(readU32(code, ip+1)), stackDepth: len(stack), privEnv: privEnv})
			ip += 5
		case OpTryPushFinally:
			handlers = append(handlers, tryHandler{kind: hTryFinally, catchIP: int(readU32(code, ip+1)), stackDepth: len(stack), privEnv: privEnv})
			ip += 5
		case OpTryPop:
			// Pop the innermost catch / try-finally handler (skipping any executing
			// finally handler, which is unwound by OP_FINALLY_RET).
			for i := len(handlers) - 1; i >= 0; i-- {
				if handlers[i].kind == hFinally {
					continue
				}
				handlers = append(handlers[:i], handlers[i+1:]...)
				break
			}
			ip++
		case OpCatch:
			push(pendingThrow)
			comp = completion{}
			ip += 5
		case OpFinally:
			// Enter a finally body: install a finally handler whose landing is the
			// code after OP_FINALLY_RET (the normal-completion continuation).
			handlers = append(handlers, tryHandler{kind: hFinally, catchIP: int(readU32(code, ip+1)), stackDepth: len(stack), privEnv: privEnv})
			ip += 5
		case OpFinallyDiscard:
			// Leave a finally body abnormally (break/continue): drop its handler and
			// any recorded completion.
			if len(handlers) > 0 && handlers[len(handlers)-1].kind == hFinally {
				handlers = handlers[:len(handlers)-1]
			}
			comp = completion{}
			ip++
		case OpFinallyRet:
			// Resume the completion recorded when the finally was entered.
			landing := ip + 1
			if len(handlers) > 0 && handlers[len(handlers)-1].kind == hFinally {
				landing = handlers[len(handlers)-1].catchIP
				handlers = handlers[:len(handlers)-1]
			}
			switch comp.kind {
			case compThrow:
				thrown = &ThrowError{Value: comp.value, rt: rt}
				comp = completion{}
				goto unwind
			case compReturn:
				r := comp.value
				comp = completion{}
				if fip, ok := doReturn(r); ok {
					ip = fip
					continue
				}
				if fn.isDerivedCtor {
					// Apply the [[Construct]] result rule now that every finally has
					// run; a non-object return's TypeError escapes the body.
					r2, e := rt.derivedCtorReturn(r, locals[fn.thisSlot])
					if e != nil {
						closeAll()
						return mkundef(), e
					}
					r = r2
				}
				closeAll()
				return r, nil
			case compJump:
				target, nFin, nPop := comp.jumpIP, comp.jumpFin, comp.jumpPops
				comp = completion{}
				if fip := doJump(target, nFin, nPop); fip >= 0 {
					ip = fip
				} else {
					ip = target
				}
			default:
				ip = landing
			}
		case OpUnwindJmp:
			target := int(readU32(code, ip+1))
			nFin := int(code[ip+5])
			nPop := int(code[ip+6])
			if fip := doJump(target, nFin, nPop); fip >= 0 {
				ip = fip
			} else {
				ip = target
			}
		case OpNipCatch:
			a := pop()
			pop()
			push(a)
			ip++

		case OpCloseUpval:
			// Close any open upvalue over this local slot (per-iteration lexical
			// binding: the next iteration captures a fresh cell).
			slot := int(readU16(code, ip+1))
			if openUpvals != nil {
				if u, ok := openUpvals[slot]; ok {
					u.closeUp()
					delete(openUpvals, slot)
				}
			}
			ip += 3
		case OpGetUpval:
			uv := cl.upvalues[readU16(code, ip+1)].get()
			if uv.IsEmpty() {
				thrown = rt.referenceError("Cannot access a lexical binding before initialization")
				goto unwind
			}
			push(uv)
			ip += 3
		case OpPutUpval:
			cl.upvalues[readU16(code, ip+1)].set(pop())
			ip += 3
		case OpSetUpval:
			cl.upvalues[readU16(code, ip+1)].set(peek())
			ip += 3

		case OpClosure:
			child := fn.childFuncs[readU32(code, ip+1)]
			upvals := make([]*upvalue, len(child.upvalDescs))
			for i, d := range child.upvalDescs {
				if d.isLocal {
					upvals[i] = captureUpvalue(d.index)
				} else {
					upvals[i] = cl.upvalues[d.index]
				}
			}
			fv := rt.newFunction(child, upvals)
			// A function compiled inside a `with` captures the current with-object
			// scope chain so its free names resolve against those objects when it is
			// later invoked (its bytecode uses OpWithGetVar for them).
			// An arrow that reads an inherited `super` takes the enclosing method's
			// [[HomeObject]]: it has none of its own, and OpGetSuper reads it off the
			// running closure.
			if child.capturesHome && cl != nil && cl.home.IsObjectType() {
				if ncl := rt.closureOf(fv); ncl != nil {
					ncl.home = cl.home
				}
			}
			if child.capturesWith && len(withStack) > 0 {
				if ncl := rt.closureOf(fv); ncl != nil {
					ncl.capturedWith = append([]Value(nil), withStack...)
				}
			}
			// A function created inside a class body belongs to that evaluation's
			// private environment (0 — the overwhelmingly common case — costs nothing).
			if privEnv != nil {
				if ncl := rt.closureOf(fv); ncl != nil {
					ncl.privEnv = privEnv
				}
			}
			push(fv)
			ip += 5

		case OpCall:
			argc := int(readU16(code, ip+1))
			callArgs := make([]Value, argc)
			for i := argc - 1; i >= 0; i-- {
				callArgs[i] = pop()
			}
			fnVal := pop()
			ret, e := rt.callValue(fnVal, mkundef(), callArgs)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(ret)
			ip += 3
		case OpEval:
			// Direct eval site. Stack: [callee, arg0, ...]. If the callee is no
			// longer the intrinsic %eval% (the binding was reassigned), this is an
			// ordinary call; otherwise evaluate the source string.
			scopeIdx := int(readU16(code, ip+1))
			// The high bit marks a call site in tail position (evalTailFlag).
			evalTail := scopeIdx&evalTailFlag != 0
			scopeIdx &^= evalTailFlag
			argc := int(readU16(code, ip+3))
			evalArgs := make([]Value, argc)
			for i := argc - 1; i >= 0; i-- {
				evalArgs[i] = pop()
			}
			callee := pop()
			var (
				ret Value
				e   *ThrowError
			)
			switch {
			case rt.evalFn == 0 || callee != rt.evalFn:
				// Not the intrinsic after all: an ordinary call, and in tail position a
				// proper one — reuse this frame exactly as OpTailCall does.
				if evalTail {
					if o := rt.objPtr(callee); o != nil && o.native == nil && o.proxy == nil {
						if cl2 := rt.closures.get(o.closure); cl2 != nil &&
							!cl2.fn.isGenerator && !cl2.fn.isAsync && !cl2.fn.isClassCtor {
							closeAll()
							fn, cl, fnVal, thisVal, args = cl2.fn, cl2, callee, mkundef(), evalArgs
							goto restart
						}
					}
				}
				ret, e = rt.callValue(callee, mkundef(), evalArgs)
			case argc == 0:
				ret = mkundef()
			case !evalArgs[0].IsString():
				ret = evalArgs[0]
			default:
				var sc *evalScope
				if scopeIdx < len(fn.evalScopes) {
					sc = fn.evalScopes[scopeIdx]
				}
				// Hand the eval this frame's dynamic scope: the with-objects it must
				// resolve free names against, and the variable object its own `var`
				// declarations create bindings on. Saved and restored so a nested eval
				// (or a call made from the eval body) cannot see a stale chain.
				savedVarObj, savedWithStack := rt.callerVarObj, rt.callerWithStack
				savedPrivEnv := rt.callerPrivEnv
				rt.callerVarObj, rt.callerWithStack, rt.callerPrivEnv = varObj, withStack, privEnv
				ret, e = rt.performDirectEval(string(rt.strBytes(evalArgs[0])), sc, cl,
					thisVal, newTarget, captureUpvalue)
				rt.callerVarObj, rt.callerWithStack, rt.callerPrivEnv = savedVarObj, savedWithStack, savedPrivEnv
			}
			if e != nil {
				thrown = e
				goto unwind
			}
			push(ret)
			ip += 5
		case OpTailCall, OpTailCallMethod:
			// Proper tail call: `return f(args)` in strict code. If the callee is an
			// ordinary compiled function we reset this frame and reuse the Go stack
			// slot (goto restart) instead of recursing, giving bounded stack growth.
			argc := int(readU16(code, ip+1))
			callArgs := make([]Value, argc)
			for i := argc - 1; i >= 0; i-- {
				callArgs[i] = pop()
			}
			callee := pop()
			tailThis := mkundef()
			if op == OpTailCallMethod {
				tailThis = pop()
			}
			if o := rt.objPtr(callee); o != nil && o.native == nil && o.proxy == nil {
				if cl2 := rt.closures.get(o.closure); cl2 != nil &&
					!cl2.fn.isGenerator && !cl2.fn.isAsync && !cl2.fn.isClassCtor {
					closeAll()
					fn, cl, fnVal, thisVal, args = cl2.fn, cl2, callee, tailThis, callArgs
					goto restart
				}
			}
			ret, e := rt.callValue(callee, tailThis, callArgs)
			if e != nil {
				thrown = e
				goto unwind
			}
			closeAll()
			return ret, nil
		case OpSuperApply:
			// super(...): [superctor, argsArray] -> constructed object. The parent
			// is [[Construct]]ed with the derived class's new.target, and the result
			// becomes `this` (the compiler stores it into *this*).
			argsArr := pop()
			// GetSuperConstructor: the [[Prototype]] of the running constructor. A
			// class constructor is never a proxy, so reading it here — after
			// ArgumentListEvaluation, which the spec performs first — is not
			// observable, and the IsConstructor check that follows is in order.
			classCtor := pop()
			superNewTarget := pop()
			superctor, sce := rt.getPrototypeOfValue(classCtor)
			if sce != nil {
				thrown = sce
				goto unwind
			}
			if !rt.isConstructorValue(superctor) {
				thrown = rt.typeError("Super constructor is not a constructor")
				goto unwind
			}
			var callArgs []Value
			if ao := rt.objPtr(argsArr); ao != nil {
				callArgs = make([]Value, ao.arrLen)
				for i := uint32(0); i < ao.arrLen; i++ {
					if int(i) < len(ao.arr) {
						callArgs[i] = ao.arr[i]
					}
				}
			}
			ret, e := rt.constructWithTarget(superctor, callArgs, superNewTarget)
			if e != nil {
				thrown = e
				goto unwind
			}
			// BindThisValue: the constructor's `this` must still be uninitialised.
			// The compiler names the binding in the inline operand (see
			// superThisLocal / superThisUpval), read raw because "uninitialised" IS
			// the empty value here.
			if ref := readU16(code, ip+1); ref != 0 {
				idx := int(ref & (superThisIndexMax - 1))
				bound := false
				switch ref & superThisKindMask {
				case superThisLocal:
					bound = idx < len(locals) && !locals[idx].IsEmpty()
				case superThisUpval:
					bound = cl != nil && idx < len(cl.upvalues) && !cl.upvalues[idx].get().IsEmpty()
				}
				if bound {
					thrown = rt.referenceError("Super constructor may only be called once per derived class constructor")
					goto unwind
				}
			}
			push(ret)
			ip += 3
		case OpApply:
			// func this argsArray -> result
			argsArr := pop()
			thisArg := pop()
			fnVal := pop()
			var callArgs []Value
			if ao := rt.objPtr(argsArr); ao != nil {
				callArgs = make([]Value, ao.arrLen)
				for i := uint32(0); i < ao.arrLen; i++ {
					if int(i) < len(ao.arr) {
						callArgs[i] = ao.arr[i]
					}
				}
			}
			ret, e := rt.callValue(fnVal, thisArg, callArgs)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(ret)
			ip += 3
		case OpNewApply:
			// constructor argsArray -> result (`new C(...spread)`).
			argsArr := pop()
			ctorVal := pop()
			var callArgs []Value
			if ao := rt.objPtr(argsArr); ao != nil {
				callArgs = make([]Value, ao.arrLen)
				for i := uint32(0); i < ao.arrLen; i++ {
					if int(i) < len(ao.arr) {
						callArgs[i] = ao.arr[i]
					}
				}
			}
			ret, e := rt.construct(ctorVal, callArgs)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(ret)
			ip += 3
		case OpSpread:
			// arr iterable -> (appends iterable's values to arr)
			iterable := pop()
			arrV := pop()
			vals, e := rt.iterableValues(iterable)
			if e != nil {
				thrown = e
				goto unwind
			}
			ao := rt.objPtr(arrV)
			for _, v := range vals {
				rt.arraySet(ao, ao.arrLen, v)
			}
			ip++
		case OpNew:
			argc := int(readU16(code, ip+1))
			callArgs := make([]Value, argc)
			for i := argc - 1; i >= 0; i-- {
				callArgs[i] = pop()
			}
			fnVal := pop()
			ret, e := rt.construct(fnVal, callArgs)
			if e != nil {
				thrown = e
				goto unwind
			}
			push(ret)
			ip += 3

		case OpReturn:
			r := pop()
			if fip, ok := doReturn(r); ok {
				ip = fip
				continue
			}
			if fn.isDerivedCtor {
				// The [[Construct]] result rule runs as the constructor completes,
				// after every try/catch/finally of the body — so its TypeError (a
				// non-object return) escapes the body rather than being caught by it.
				r2, e := rt.derivedCtorReturn(r, locals[fn.thisSlot])
				if e != nil {
					closeAll()
					return mkundef(), e
				}
				r = r2
			}
			closeAll()
			return r, nil
		case OpReturnUndef:
			r := mkundef()
			if fip, ok := doReturn(r); ok {
				ip = fip
				continue
			}
			if fn.isDerivedCtor {
				r2, e := rt.derivedCtorReturn(r, locals[fn.thisSlot])
				if e != nil {
					closeAll()
					return mkundef(), e
				}
				r = r2
			}
			closeAll()
			return r, nil
		case OpHalt:
			closeAll()
			return mkundef(), nil
		default:
			closeAll()
			return mkundef(), &ThrowError{Value: rt.newString("InternalError: unimplemented opcode " + op.Name()), rt: rt}
		}
		continue

	unwind:
		// Route the thrown value to the innermost catch / try-finally handler in
		// this frame (skipping executing finally bodies); if there is none, or this
		// is a control-flow signal, propagate up.
		if !thrown.control {
			for len(handlers) > 0 {
				h := handlers[len(handlers)-1]
				handlers = handlers[:len(handlers)-1]
				if h.kind == hFinally {
					// A throw inside a finally body overrides its pending completion
					// and propagates to the next outer handler.
					comp = completion{}
					continue
				}
				stack = stack[:h.stackDepth]
				privEnv = h.privEnv
				if h.kind == hTryFinally {
					// Run the finally with the throw pending; OP_FINALLY_RET re-raises.
					comp = completion{kind: compThrow, value: thrown.Value}
					thrown = nil
					ip = h.catchIP
					goto resumed
				}
				pendingThrow = thrown.Value
				thrown = nil
				ip = h.catchIP
				goto resumed
			}
		}
		closeAll()
		return mkundef(), thrown
	resumed:
		continue
	}
}

// OpSuperApply's inline operand names the binding holding the constructor's
// `this`, so it can perform BindThisValue's already-initialised check: the top
// two bits select local or upvalue, the rest is the index. Zero means "no
// binding found", which only happens for a super() the compiler could not
// resolve (and which is an early error anyway).
const (
	superThisKindMask uint16 = 0xC000
	superThisLocal    uint16 = 0x4000
	superThisUpval    uint16 = 0x8000
	superThisIndexMax        = 0x4000
)

// tryHandler records a live catch/finally handler (ant handler stack).
type tryHandler struct {
	kind       uint8  // hTry / hTryFinally / hFinally
	catchIP    int    // landing ip (catch tag, finally entry, or post-finally)
	stackDepth int    // stack length to restore on entry
	privEnv    *privScope // class private environment to restore on entry
}

// handler kinds (ant SV_HANDLER_*).
const (
	hTry        uint8 = iota // catch handler
	hTryFinally              // try protected by a finally (abrupt exits run finally)
	hFinally                 // an executing finally body
)

// completion kinds recorded when an abrupt exit enters a finally (ant
// SV_COMPLETION_*); the finally's OP_FINALLY_RET resumes it.
const (
	compNone uint8 = iota
	compThrow
	compReturn
	compJump
)

// completion is the pending abrupt completion a finally must resume.
type completion struct {
	kind     uint8
	value    Value
	jumpIP   int
	jumpFin  int // remaining finallies for a pending break/continue jump
	jumpPops int // remaining handler pops for a pending break/continue jump
}

// ---- operator semantics ----

func (rt *Runtime) toNumber(v Value) (float64, *ThrowError) {
	if v.IsObjectType() || v.Type() == TTypedArray {
		p, e := rt.toPrimitive(v, "number")
		if e != nil {
			return 0, e
		}
		v = p
	}
	n, ok := rt.toNumberPrimitive(v)
	if !ok {
		if v.IsSymbol() {
			return 0, rt.typeError("cannot convert a Symbol value to a number")
		}
		return 0, rt.typeError("cannot convert value to number")
	}
	return n, nil
}

func (rt *Runtime) toInt32(v Value) (int32, *ThrowError) {
	n, e := rt.toNumber(v)
	if e != nil {
		return 0, e
	}
	return toInt32(n), nil
}

func (rt *Runtime) toUint32(v Value) (uint32, *ThrowError) {
	n, e := rt.toNumber(v)
	if e != nil {
		return 0, e
	}
	return toUint32(n), nil
}

// jsAdd implements the ECMAScript "+" operator for primitive operands: string
// concatenation if either side is a string, otherwise numeric addition.
func (rt *Runtime) jsAdd(a, b Value) (Value, *ThrowError) {
	pa, e := rt.toPrimitive(a, "default")
	if e != nil {
		return mkundef(), e
	}
	pb, e := rt.toPrimitive(b, "default")
	if e != nil {
		return mkundef(), e
	}
	if pa.IsString() || pb.IsString() {
		sa, e := rt.toStringValue(pa)
		if e != nil {
			return mkundef(), e
		}
		sb, e := rt.toStringValue(pb)
		if e != nil {
			return mkundef(), e
		}
		return rt.newStringBytes(concatWTF8(rt.strBytes(sa), rt.strBytes(sb))), nil
	}
	if pa.Type() == TBigInt || pb.Type() == TBigInt {
		if pa.Type() != TBigInt || pb.Type() != TBigInt {
			return mkundef(), rt.typeError("Cannot mix BigInt and other types, use explicit conversions")
		}
		return rt.bigIntBinaryOp(OpAdd, rt.bigIntVal(pa), rt.bigIntVal(pb))
	}
	na, ea := rt.toNumberPrimitive(pa)
	if !ea {
		return mkundef(), rt.typeError("cannot convert to number")
	}
	nb, eb := rt.toNumberPrimitive(pb)
	if !eb {
		return mkundef(), rt.typeError("cannot convert to number")
	}
	return mknum(na + nb), nil
}

func (rt *Runtime) jsArith(op Opcode, a, b Value) (Value, *ThrowError) {
	if a.Type() == TBigInt || b.Type() == TBigInt {
		pa, e := rt.toPrimitive(a, "number")
		if e != nil {
			return mkundef(), e
		}
		pb, e := rt.toPrimitive(b, "number")
		if e != nil {
			return mkundef(), e
		}
		if pa.Type() != TBigInt || pb.Type() != TBigInt {
			return mkundef(), rt.typeError("Cannot mix BigInt and other types, use explicit conversions")
		}
		return rt.bigIntBinaryOp(op, rt.bigIntVal(pa), rt.bigIntVal(pb))
	}
	na, ea := rt.toNumber(a)
	if ea != nil {
		return mkundef(), ea
	}
	nb, eb := rt.toNumber(b)
	if eb != nil {
		return mkundef(), eb
	}
	switch op {
	case OpSub:
		return mknum(na - nb), nil
	case OpMul:
		return mknum(na * nb), nil
	case OpDiv:
		return mknum(na / nb), nil
	case OpMod:
		return mknum(jsMod(na, nb)), nil
	case OpExp:
		return mknum(jsExp(na, nb)), nil
	}
	return mkundef(), rt.typeError("bad arithmetic op")
}

// toNumeric implements ToNumeric(value): ToPrimitive with hint number, then keep
// a BigInt or coerce the primitive to a Number. Bitwise and arithmetic operators
// apply it to each operand BEFORE dispatching the BigInt-vs-Number path, so an
// object whose valueOf/@@toPrimitive yields a BigInt is not misread as a mix.
func (rt *Runtime) toNumeric(v Value) (Value, *ThrowError) {
	p, e := rt.toPrimitive(v, "number")
	if e != nil {
		return mkundef(), e
	}
	if p.Type() == TBigInt {
		return p, nil
	}
	n, e := rt.toNumber(p)
	if e != nil {
		return mkundef(), e
	}
	return mknum(n), nil
}

func (rt *Runtime) jsBitwise(op Opcode, a, b Value) (Value, *ThrowError) {
	var e *ThrowError
	if a, e = rt.toNumeric(a); e != nil {
		return mkundef(), e
	}
	if b, e = rt.toNumeric(b); e != nil {
		return mkundef(), e
	}
	if a.Type() == TBigInt || b.Type() == TBigInt {
		if a.Type() != TBigInt || b.Type() != TBigInt {
			return mkundef(), rt.typeError("Cannot mix BigInt and other types, use explicit conversions")
		}
		if op == OpUshr {
			return mkundef(), rt.typeError("BigInts have no unsigned right shift, use >> instead")
		}
		return rt.bigIntBinaryOp(op, rt.bigIntVal(a), rt.bigIntVal(b))
	}
	if op == OpUshr {
		ua, e := rt.toUint32(a)
		if e != nil {
			return mkundef(), e
		}
		sb, e := rt.toUint32(b)
		if e != nil {
			return mkundef(), e
		}
		return mknum(float64(ua >> (sb & 31))), nil
	}
	ia, e := rt.toInt32(a)
	if e != nil {
		return mkundef(), e
	}
	ib, e := rt.toInt32(b)
	if e != nil {
		return mkundef(), e
	}
	switch op {
	case OpBand:
		return mknum(float64(ia & ib)), nil
	case OpBor:
		return mknum(float64(ia | ib)), nil
	case OpBxor:
		return mknum(float64(ia ^ ib)), nil
	case OpShl:
		return mknum(float64(ia << (uint32(ib) & 31))), nil
	case OpShr:
		return mknum(float64(ia >> (uint32(ib) & 31))), nil
	}
	return mkundef(), rt.typeError("bad bitwise op")
}

// jsRelational implements abstract relational comparison.
func (rt *Runtime) jsRelational(op Opcode, a, b Value) (Value, *ThrowError) {
	// IsLessThan begins with ToPrimitive(hint number) on both operands, always
	// coercing the SOURCE left operand first (`a > b` is IsLessThan(b, a, false),
	// whose "right first" is this a). Two String results then compare as strings —
	// which is how `function(){} > {}` compares their source texts rather than
	// coercing both to NaN.
	if a.IsObjectLike() {
		pa, e := rt.toPrimitive(a, "number")
		if e != nil {
			return mkundef(), e
		}
		a = pa
	}
	if b.IsObjectLike() {
		pb, e := rt.toPrimitive(b, "number")
		if e != nil {
			return mkundef(), e
		}
		b = pb
	}
	if a.Type() == TBigInt && b.Type() == TBigInt {
		cmp := rt.bigIntVal(a).Cmp(rt.bigIntVal(b))
		switch op {
		case OpLt:
			return mkbool(cmp < 0), nil
		case OpLe:
			return mkbool(cmp <= 0), nil
		case OpGt:
			return mkbool(cmp > 0), nil
		case OpGe:
			return mkbool(cmp >= 0), nil
		}
	}
	if a.IsString() && b.IsString() {
		cmp := compareStrings(rt.strBytes(a), rt.strBytes(b))
		switch op {
		case OpLt:
			return mkbool(cmp < 0), nil
		case OpLe:
			return mkbool(cmp <= 0), nil
		case OpGt:
			return mkbool(cmp > 0), nil
		case OpGe:
			return mkbool(cmp >= 0), nil
		}
	}
	// A BigInt operand mixed with a String or Number is compared mathematically
	// (spec Abstract Relational Comparison), rather than ToNumber'd (which throws
	// on a BigInt). An invalid BigInt string, or a NaN Number, makes the result
	// false.
	if a.Type() == TBigInt && b.IsString() {
		n, ok := stringToBigInt(string(rt.strBytes(b)))
		if !ok {
			return mkfalse(), nil
		}
		return relBool(op, rt.bigIntVal(a).Cmp(n)), nil
	}
	if a.IsString() && b.Type() == TBigInt {
		n, ok := stringToBigInt(string(rt.strBytes(a)))
		if !ok {
			return mkfalse(), nil
		}
		return relBool(op, n.Cmp(rt.bigIntVal(b))), nil
	}
	if a.Type() == TBigInt {
		n, e := rt.toNumber(b) // b is Number/Boolean (String handled above)
		if e != nil {
			return mkundef(), e
		}
		cmp, ok := cmpBigIntNumber(rt.bigIntVal(a), n)
		if !ok {
			return mkfalse(), nil
		}
		return relBool(op, cmp), nil
	}
	if b.Type() == TBigInt {
		n, e := rt.toNumber(a)
		if e != nil {
			return mkundef(), e
		}
		cmp, ok := cmpBigIntNumber(rt.bigIntVal(b), n)
		if !ok {
			return mkfalse(), nil
		}
		return relBool(op, -cmp), nil
	}
	na, ea := rt.toNumber(a)
	if ea != nil {
		return mkundef(), ea
	}
	nb, eb := rt.toNumber(b)
	if eb != nil {
		return mkundef(), eb
	}
	if math.IsNaN(na) || math.IsNaN(nb) {
		return mkfalse(), nil // any relation with NaN is false
	}
	switch op {
	case OpLt:
		return mkbool(na < nb), nil
	case OpLe:
		return mkbool(na <= nb), nil
	case OpGt:
		return mkbool(na > nb), nil
	case OpGe:
		return mkbool(na >= nb), nil
	}
	return mkfalse(), nil
}

// abstractEquals implements the ECMAScript "==" algorithm for primitives.
// relBool maps a comparison sign (-1/0/1) to the boolean for a relational op.
func relBool(op Opcode, cmp int) Value {
	switch op {
	case OpLt:
		return mkbool(cmp < 0)
	case OpLe:
		return mkbool(cmp <= 0)
	case OpGt:
		return mkbool(cmp > 0)
	default: // OpGe
		return mkbool(cmp >= 0)
	}
}

// cmpBigIntNumber compares a BigInt with a Number mathematically, returning the
// sign of (bi − n) and whether the comparison is defined (false for a NaN n).
func cmpBigIntNumber(bi *big.Int, n float64) (int, bool) {
	switch {
	case math.IsNaN(n):
		return 0, false
	case math.IsInf(n, 1):
		return -1, true // bi < +Infinity
	case math.IsInf(n, -1):
		return 1, true // bi > -Infinity
	}
	return new(big.Float).SetInt(bi).Cmp(big.NewFloat(n)), true
}

func (rt *Runtime) abstractEquals(a, b Value) (bool, *ThrowError) {
	ta, tb := a.Type(), b.Type()
	if ta == tb {
		return rt.strictEquals(a, b), nil
	}
	// null == undefined
	if (ta == TNull && tb == TUndef) || (ta == TUndef && tb == TNull) {
		return true, nil
	}
	// number == string
	if ta == TNum && tb == TStr {
		return a.Number() == stringToNumber(string(rt.strBytes(b))), nil
	}
	if ta == TStr && tb == TNum {
		return stringToNumber(string(rt.strBytes(a))) == b.Number(), nil
	}
	// boolean coerces to number, then re-compare
	if ta == TBool {
		return rt.abstractEquals(mknum(boolToNum(a)), b)
	}
	if tb == TBool {
		return rt.abstractEquals(a, mknum(boolToNum(b)))
	}
	// BigInt vs Number: equal iff the Number is the BigInt's exact (integer) value.
	if ta == TBigInt && tb == TNum {
		cmp, ok := cmpBigIntNumber(rt.bigIntVal(a), b.Number())
		return ok && cmp == 0, nil
	}
	if ta == TNum && tb == TBigInt {
		cmp, ok := cmpBigIntNumber(rt.bigIntVal(b), a.Number())
		return ok && cmp == 0, nil
	}
	// BigInt vs String: parse the string as a BigInt (invalid → not equal).
	if ta == TBigInt && tb == TStr {
		n, ok := stringToBigInt(string(rt.strBytes(b)))
		return ok && rt.bigIntVal(a).Cmp(n) == 0, nil
	}
	if ta == TStr && tb == TBigInt {
		n, ok := stringToBigInt(string(rt.strBytes(a)))
		return ok && rt.bigIntVal(b).Cmp(n) == 0, nil
	}
	// object vs primitive (number/string/bigint/symbol): ToPrimitive the object
	// side, then re-compare (ES abstract equality steps 10-11). A throw from
	// ToPrimitive (e.g. a valueOf/@@toPrimitive that throws) propagates.
	if a.IsObjectLike() && (tb == TNum || tb == TStr || tb == TBigInt || tb == TSymbol) {
		pa, e := rt.toPrimitive(a, "")
		if e != nil {
			return false, e
		}
		return rt.abstractEquals(pa, b)
	}
	if b.IsObjectLike() && (ta == TNum || ta == TStr || ta == TBigInt || ta == TSymbol) {
		pb, e := rt.toPrimitive(b, "")
		if e != nil {
			return false, e
		}
		return rt.abstractEquals(a, pb)
	}
	return false, nil
}

func boolToNum(v Value) float64 {
	if v.Bool() {
		return 1
	}
	return 0
}

// ---- numeric helpers (ant numbers.cc) ----

// toInt32 implements ECMAScript ToInt32.
func toInt32(d float64) int32 {
	if math.IsNaN(d) || math.IsInf(d, 0) {
		return 0
	}
	return int32(uint32(int64(math.Trunc(d))))
}

// toUint32 implements ECMAScript ToUint32.
func toUint32(d float64) uint32 {
	if math.IsNaN(d) || math.IsInf(d, 0) {
		return 0
	}
	return uint32(int64(math.Trunc(d)))
}

// jsMod implements the ECMAScript "%" (remainder) operator.
func jsMod(a, b float64) float64 {
	if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || b == 0 {
		return math.NaN()
	}
	if math.IsInf(b, 0) {
		return a
	}
	if a == 0 {
		return a
	}
	return math.Mod(a, b)
}

// jsExp implements the ECMAScript "**" operator (differs from math.Pow for a
// few NaN edge cases mandated by the spec).
func jsExp(base, exp float64) float64 {
	if math.IsNaN(exp) {
		return math.NaN()
	}
	if exp == 0 {
		return 1
	}
	// base = ±1, exp = ±Inf → NaN (spec), unlike C pow which returns 1.
	if math.IsInf(exp, 0) && (base == 1 || base == -1) {
		return math.NaN()
	}
	return math.Pow(base, exp)
}

// compareStrings compares two WTF-8 strings by UTF-16 code unit (JS ordering).
func compareStrings(a, b []byte) int {
	la, lb := utf16Len(a), utf16Len(b)
	n := min(la, lb)
	for i := 0; i < n; i++ {
		ca := utf16CodeUnitAt(a, i)
		cb := utf16CodeUnitAt(b, i)
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	switch {
	case la < lb:
		return -1
	case la > lb:
		return 1
	}
	return 0
}

func (rt *Runtime) typeError(msg string) *ThrowError {
	return &ThrowError{Value: rt.makeError(rt.errors.typeProto, "TypeError", msg), rt: rt}
}

func (rt *Runtime) rangeError(msg string) *ThrowError {
	return &ThrowError{Value: rt.makeError(rt.errors.rangeProto, "RangeError", msg), rt: rt}
}

// rejectDefine is a [[DefineOwnProperty]] rejection (a TypeError that
// DefinePropertyOrThrow throws, but Reflect.defineProperty reports as false).
func (rt *Runtime) rejectDefine(msg string) *ThrowError {
	e := rt.typeError(msg)
	e.rejected = true
	return e
}

func (rt *Runtime) referenceError(msg string) *ThrowError {
	ev, _ := rt.construct(rt.errors.refErr, []Value{rt.newString(msg)})
	return &ThrowError{Value: ev, rt: rt}
}
