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
			if e := rt.objectDefinePropertyKey(obj, rt.toPropertyKeyValue(arg(args, 0)), desc); e != nil {
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
		if o == nil || o.proto.IsNull() || o.proto == 0 {
			return mknull(), nil
		}
		return o.proto, nil
	})
	setProto := rt.newNativeFunc("set __proto__", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if this.IsNullish() {
			return mkundef(), rt.typeError("Object.prototype.__proto__ called on null or undefined")
		}
		p := arg(args, 0)
		if o := rt.objPtr(this); o != nil && (p.IsObjectType() || p.IsNull()) {
			o.proto = p
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
		rt.defMethod(proto, name, 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
			s, e := rt.toStringValue(this)
			if e != nil {
				return mkundef(), e
			}
			text := string(rt.strBytes(s))
			open := "<" + tag
			if attr != "" {
				av, _ := rt.toStringValue(arg(args, 0))
				a := string(rt.strBytes(av))
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
		if o == nil {
			return mkundef(), rt.typeError("RegExp.prototype.compile on incompatible receiver")
		}
		pattern := ""
		if p := arg(args, 0); !p.IsUndefined() {
			if po := rt.objPtr(p); po != nil && po.regex != nil {
				sv, _ := rt.getField(p, "source")
				pattern = string(rt.strBytes(sv))
			} else {
				pv, e := rt.toStringValue(p)
				if e != nil {
					return mkundef(), e
				}
				pattern = string(rt.strBytes(pv))
			}
		}
		flags := ""
		if f := arg(args, 1); !f.IsUndefined() {
			fv, e := rt.toStringValue(f)
			if e != nil {
				return mkundef(), e
			}
			flags = string(rt.strBytes(fv))
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
		o.defineOwn("lastIndex", mknum(0), attrWritable)
		return this, nil
	})
}
