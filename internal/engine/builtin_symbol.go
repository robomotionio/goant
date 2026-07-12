package engine

// Symbol (ant modules/symbol.c). Symbols are unique primitive values with an
// optional description; well-known symbols (Symbol.iterator etc.) are shared
// singletons stored on the Symbol constructor. Symbol-keyed properties use the
// shape's symbol slots (see object.go / elements.go).

func (rt *Runtime) newSymbol(desc Value) Value {
	h, s := rt.symbols.alloc()
	s.desc = desc
	rt.symbolCounter++
	s.id = rt.symbolCounter
	return mkval(TSymbol, uint64(h))
}

// thisSymbol unwraps a Symbol.prototype receiver: a raw symbol, or a boxed
// Symbol wrapper object (from Object(symbol)).
func (rt *Runtime) thisSymbol(this Value) (Value, bool) {
	if this.IsSymbol() {
		return this, true
	}
	if o := rt.objPtr(this); o != nil && o.boxed.IsSymbol() {
		return o.boxed, true
	}
	return mkundef(), false
}

// symbolDesc returns a symbol's description Value (undefined if none).
func (rt *Runtime) symbolDesc(v Value) Value {
	if s := rt.symbols.get(Handle(v.handle())); s != nil {
		return s.desc
	}
	return mkundef()
}

func (rt *Runtime) initSymbolBuiltin() {
	proto := rt.newObject(rt.objectProto)
	rt.symbolProto = proto
	po := rt.objPtr(proto)

	rt.defMethod(po, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sym, ok := rt.thisSymbol(this)
		if !ok {
			return mkundef(), rt.typeError("Symbol.prototype.toString requires a symbol")
		}
		d := rt.symbolDesc(sym)
		ds := ""
		if d.IsString() {
			ds = string(rt.strBytes(d))
		}
		return rt.newString("Symbol(" + ds + ")"), nil
	})
	rt.defMethod(po, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sym, ok := rt.thisSymbol(this)
		if !ok {
			return mkundef(), rt.typeError("Symbol.prototype.valueOf requires a symbol")
		}
		return sym, nil
	})
	po.defineAccessor("description", rt.newNativeFunc("description", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		sym, ok := rt.thisSymbol(this)
		if !ok {
			return mkundef(), rt.typeError("Symbol.prototype.description requires a symbol")
		}
		return rt.symbolDesc(sym), nil
	}), mkundef(), true, false, attrConfigurable)

	// Symbol() constructor (not newable).
	ctor := rt.newNativeFunc("Symbol", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsObjectType() {
			return mkundef(), rt.typeError("Symbol is not a constructor")
		}
		desc := mkundef()
		if a := arg(args, 0); !a.IsUndefined() {
			s, e := rt.toStringValue(a)
			if e != nil {
				return mkundef(), e
			}
			desc = s
		}
		return rt.newSymbol(desc), nil
	})
	cobj := rt.objPtr(ctor)
	cobj.defineOwn("prototype", proto, 0)
	po.defineOwn("constructor", ctor, attrWritable|attrConfigurable)

	// Well-known symbols (shared singletons).
	wk := func(name string) Value {
		s := rt.newSymbol(rt.newString("Symbol." + name))
		cobj.defineOwn(name, s, 0)
		return s
	}
	rt.symIterator = wk("iterator")
	rt.symAsyncIterator = wk("asyncIterator")
	rt.symHasInstance = wk("hasInstance")
	rt.symToPrimitive = wk("toPrimitive")
	rt.symToStringTag = wk("toStringTag")
	rt.symIsConcatSpreadable = wk("isConcatSpreadable")
	rt.symSpecies = wk("species")
	rt.symMatch = wk("match")
	rt.symReplace = wk("replace")
	rt.symSearch = wk("search")
	rt.symSplit = wk("split")
	rt.symUnscopables = wk("unscopables")
	rt.symDispose = wk("dispose")
	rt.symAsyncDispose = wk("asyncDispose")

	// Symbol.for / Symbol.keyFor (global registry).
	rt.defMethod(cobj, "for", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		s, e := rt.toStringValue(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		key := string(rt.strBytes(s))
		if sym, ok := rt.symbolRegistry[key]; ok {
			return sym, nil
		}
		sym := rt.newSymbol(s)
		if rt.symbolRegistry == nil {
			rt.symbolRegistry = map[string]Value{}
		}
		rt.symbolRegistry[key] = sym
		return sym, nil
	})
	rt.defMethod(cobj, "keyFor", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		target := arg(args, 0)
		for k, sym := range rt.symbolRegistry {
			if sym == target {
				return rt.internString(k), nil
			}
		}
		return mkundef(), nil
	})

	rt.defGlobal("Symbol", ctor)
	// Symbol.prototype[@@toStringTag] === "Symbol" (data property).
	rt.setStringTag(proto, "Symbol")
	// Symbol.prototype[@@toPrimitive] returns the wrapped symbol (so a boxed
	// Symbol used as a property key coerces to the symbol, not "Symbol(...)").
	if rt.symToPrimitive != 0 {
		tp := rt.newNativeFunc("[Symbol.toPrimitive]", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			sym, ok := rt.thisSymbol(this)
			if !ok {
				return mkundef(), rt.typeError("Symbol.prototype[Symbol.toPrimitive] requires a symbol")
			}
			return sym, nil
		})
		po.defineOwnSymbol(rt.symToPrimitive.handle(), tp, attrConfigurable)
	}
}
