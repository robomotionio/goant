package engine

// Annex B legacy web-compatibility builtins (ant annexb.c): Object.prototype
// __defineGetter__/__defineSetter__/__lookupGetter__/__lookupSetter__ and the
// __proto__ accessor; String.prototype trimLeft/trimRight aliases and the HTML
// wrapper methods; RegExp.prototype.compile.

func (rt *Runtime) initAnnexB() {
	rt.initAnnexBObject()
	rt.initAnnexBString()
	rt.initAnnexBRegExp()
}

func (rt *Runtime) initAnnexBObject() {
	proto := rt.objPtr(rt.objectProto)

	defineAccessorMethod := func(name string, wantGetter bool) {
		rt.defMethod(proto, name, 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			obj, e := rt.toObjectValue(this)
			if e != nil {
				return mkundef(), e
			}
			fn := arg(args, 1)
			if !rt.isCallable(fn) {
				return mkundef(), rt.typeError(name + ": target is not a function")
			}
			// {get|set, enumerable:true, configurable:true}
			desc := rt.newPlainObject()
			do := rt.objPtr(desc)
			key := "get"
			if !wantGetter {
				key = "set"
			}
			do.defineOwn(key, fn, attrDefault)
			do.defineOwn("enumerable", mktrue(), attrDefault)
			do.defineOwn("configurable", mktrue(), attrDefault)
			pk, e := rt.toPropertyKey(arg(args, 0)) // ToPropertyKey (propagating)
			if e != nil {
				return mkundef(), e
			}
			if e := rt.objectDefinePropertyKey(obj, pk, desc); e != nil {
				return mkundef(), e
			}
			return mkundef(), nil
		})
	}
	defineAccessorMethod("__defineGetter__", true)
	defineAccessorMethod("__defineSetter__", false)

	lookupAccessor := func(name string, wantGetter bool) {
		rt.defMethod(proto, name, 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			obj, e := rt.toObjectValue(this)
			if e != nil {
				return mkundef(), e
			}
			key := arg(args, 0)
			cur := obj
			for depth := 0; depth < maxProtoChainDepth; depth++ {
				o := rt.objPtr(cur)
				if o == nil {
					break
				}
				// A Proxy in the chain must have its [[GetOwnProperty]] and
				// [[GetPrototypeOf]] traps invoked (annex-b.Proxy.__lookupGetter__).
				if o.proxy != nil {
					descV, e := rt.proxyGetOwnPropertyDescriptor(o.proxy, key)
					if e != nil {
						return mkundef(), e
					}
					if !descV.IsUndefined() {
						g, _ := rt.getField(descV, "get")
						s, _ := rt.getField(descV, "set")
						if wantGetter && rt.isCallable(g) {
							return g, nil
						}
						if !wantGetter && rt.isCallable(s) {
							return s, nil
						}
						return mkundef(), nil // property found: shadows
					}
					proto, e := rt.proxyGetPrototypeOf(o.proxy)
					if e != nil {
						return mkundef(), e
					}
					cur = proto
					continue
				}
				var d ownDesc
				if key.IsSymbol() {
					d = o.ownDescriptorSym(key.handle())
				} else {
					nm, e := rt.propKeyString(key)
					if e != nil {
						return mkundef(), e
					}
					d = o.ownDescriptor(nm)
				}
				if d.exists {
					if d.isAccessor {
						if wantGetter && !d.getter.IsUndefined() {
							return d.getter, nil
						}
						if !wantGetter && !d.setter.IsUndefined() {
							return d.setter, nil
						}
					}
					return mkundef(), nil // own data property shadows
				}
				cur = o.proto
			}
			return mkundef(), nil
		})
	}
	lookupAccessor("__lookupGetter__", true)
	lookupAccessor("__lookupSetter__", false)

	// Object.prototype.__proto__ accessor (get/set [[Prototype]]).
	getProto := rt.newNativeFunc("get __proto__", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		obj, e := rt.toObjectValue(this)
		if e != nil {
			return mkundef(), e
		}
		o := rt.objPtr(obj)
		if o == nil {
			return mknull(), nil
		}
		if o.proxy != nil {
			return rt.proxyGetPrototypeOf(o.proxy)
		}
		if o.proto.IsNull() || o.proto == 0 {
			return mknull(), nil
		}
		return o.proto, nil
	})
	setProto := rt.newNativeFunc("set __proto__", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsNullish() {
			return mkundef(), rt.typeError("Object.prototype.__proto__ called on null or undefined")
		}
		p := arg(args, 0)
		if !p.IsObjectType() && !p.IsNull() {
			return mkundef(), nil // a primitive value is a silent no-op
		}
		if o := rt.objPtr(this); o != nil {
			if o.proxy != nil {
				_, e := rt.proxySetPrototypeOf(o.proxy, p)
				return mkundef(), e // __proto__ setter is a silent no-op on failure
			}
			if !rt.ordinarySetProto(o, p) {
				return mkundef(), rt.typeError("Cannot set the prototype (non-extensible, immutable, or cyclic)")
			}
		}
		return mkundef(), nil
	})
	proto.defineAccessor("__proto__", getProto, setProto, true, true, attrConfigurable)
}

func (rt *Runtime) initAnnexBString() {
	proto := rt.objPtr(rt.stringProto)

	// trimLeft / trimRight are aliases of trimStart / trimEnd.
	if ts, ok := proto.getOwn("trimStart"); ok {
		proto.defineOwn("trimLeft", ts, attrWritable|attrConfigurable)
	}
	if te, ok := proto.getOwn("trimEnd"); ok {
		proto.defineOwn("trimRight", te, attrWritable|attrConfigurable)
	}

	// HTML wrapper methods (String.prototype.anchor etc.).
	htmlMethod := func(name, tag, attr string) {
		// Only the attribute-taking wrappers (anchor/link/fontcolor/fontsize) have a
		// formal parameter; the plain tag wrappers (big/bold/…) take none, so their
		// .length is 0.
		length := 0
		if attr != "" {
			length = 1
		}
		rt.defMethod(proto, name, length, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			if this.IsNullish() { // RequireObjectCoercible before ToString(this)
				return mkundef(), rt.typeError("String.prototype." + name + " called on null or undefined")
			}
			s, e := rt.toStringValue(this)
			if e != nil {
				return mkundef(), e
			}
			text := rt.strGo(s)
			open := "<" + tag
			if attr != "" {
				av, ae := rt.toStringValue(arg(args, 0))
				if ae != nil { // a throwing ToString on the attribute value propagates
					return mkundef(), ae
				}
				a := rt.strGo(av)
				// Annex B escapes only double quotes in the attribute value.
				esc := ""
				for _, c := range a {
					if c == '"' {
						esc += "&quot;"
					} else {
						esc += string(c)
					}
				}
				open += " " + attr + "=\"" + esc + "\""
			}
			open += ">"
			return rt.newString(open + text + "</" + tag + ">"), nil
		})
	}
	htmlMethod("anchor", "a", "name")
	htmlMethod("link", "a", "href")
	htmlMethod("fontcolor", "font", "color")
	htmlMethod("fontsize", "font", "size")
	htmlMethod("big", "big", "")
	htmlMethod("blink", "blink", "")
	htmlMethod("bold", "b", "")
	htmlMethod("fixed", "tt", "")
	htmlMethod("italics", "i", "")
	htmlMethod("small", "small", "")
	htmlMethod("strike", "strike", "")
	htmlMethod("sub", "sub", "")
	htmlMethod("sup", "sup", "")
}

func (rt *Runtime) initAnnexBRegExp() {
	proto := rt.objPtr(rt.regexpProto)
	rt.defMethod(proto, "compile", 2, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o := rt.objPtr(this)
		// RequireInternalSlot(O, [[RegExpMatcher]]); and legacy features are enabled
		// only for a direct %RegExp% instance, not a subclass instance (approximated
		// by its [[Prototype]] being %RegExp.prototype%). Both cases are a TypeError.
		if o == nil || o.regex == nil || o.proto != rt.regexpProto {
			return mkundef(), rt.typeError("RegExp.prototype.compile called on an incompatible receiver")
		}
		var pattern, flags string
		p := arg(args, 0)
		if po := rt.objPtr(p); po != nil && po.regex != nil {
			// The pattern is a RegExp: its [[OriginalSource]]/[[OriginalFlags]] are
			// adopted and a supplied flags argument is a TypeError.
			if !arg(args, 1).IsUndefined() {
				return mkundef(), rt.typeError("RegExp.prototype.compile: flags may not be supplied when the pattern is a RegExp")
			}
			pattern = po.regex.Source
			flags = po.regex.Flags
		} else {
			if !p.IsUndefined() {
				pv, e := rt.toStringValue(p)
				if e != nil {
					return mkundef(), e
				}
				pattern = rt.strGo(pv)
			}
			if f := arg(args, 1); !f.IsUndefined() {
				fv, e := rt.toStringValue(f)
				if e != nil {
					return mkundef(), e
				}
				flags = rt.strGo(fv)
			}
		}
		nv, e := rt.newRegExp(pattern, flags)
		if e != nil {
			return mkundef(), e
		}
		// Adopt the freshly compiled regex + descriptor fields into `this`.
		no := rt.objPtr(nv)
		o.regex = no.regex
		for _, k := range []string{"source", "flags", "global", "ignoreCase", "multiline"} {
			if v, ok := no.getOwn(k); ok {
				o.defineOwn(k, v, 0)
			}
		}
		// Set(O, "lastIndex", 0, true): a non-writable lastIndex makes this a TypeError.
		if e := rt.setLastIndexOrThrow(this, 0); e != nil {
			return mkundef(), e
		}
		return this, nil
	})
}
