package engine

// Uint8Array base64/hex conversions (TC39 "Uint8Array to/from base64", stage 4):
// toBase64/fromBase64/setFromBase64 and toHex/fromHex/setFromHex.
//
// The decoders are written out rather than delegated to encoding/base64, because
// the specified behaviour differs from Go's in ways every option exercises:
// ASCII whitespace is skipped anywhere, the final chunk's treatment is chosen by
// lastChunkHandling, a `set*` decode stops once the target is full, and a decode
// that fails partway still writes the bytes it produced before the error.

const (
	b64Std = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

	// b64NoLimit is the "absent maxLength" of the spec's decoders: no cap.
	b64NoLimit = 1<<53 - 1
)

// b64Whitespace reports whether c is one of the ASCII whitespace code units the
// base64 decoder skips (tab, LF, FF, CR, space). Note this is NOT the full set
// of JS whitespace: only these five are ignored.
func b64Whitespace(c byte) bool {
	return c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' '
}

// b64Digit maps a base64 character to its 6-bit value, or -1 if it is not in the
// selected alphabet.
func b64Digit(c byte, url bool) int {
	alpha := b64Std
	if url {
		alpha = b64URL
	}
	for i := 0; i < len(alpha); i++ {
		if alpha[i] == c {
			return i
		}
	}
	return -1
}

// decodeB64Chunk decodes a partial or complete base64 chunk. n is the number of
// characters actually present (2, 3 or 4); the bytes produced are n-1 for a
// partial chunk and 3 for a complete one. strictBits rejects a partial chunk
// whose unused low bits are non-zero (lastChunkHandling "strict").
func decodeB64Chunk(chunk [4]int, n int, strictBits bool) ([]byte, bool) {
	switch n {
	case 2:
		v := chunk[0]<<6 | chunk[1]
		if strictBits && v&0xf != 0 {
			return nil, false
		}
		return []byte{byte(v >> 4)}, true
	case 3:
		v := chunk[0]<<12 | chunk[1]<<6 | chunk[2]
		if strictBits && v&0x3 != 0 {
			return nil, false
		}
		return []byte{byte(v >> 10), byte(v >> 2)}, true
	default:
		v := chunk[0]<<18 | chunk[1]<<12 | chunk[2]<<6 | chunk[3]
		return []byte{byte(v >> 16), byte(v >> 8), byte(v)}, true
	}
}

// fromBase64 implements the FromBase64 abstract operation. It returns the bytes
// decoded, how many code units of the source they consumed, and whether the
// decode ended in a SyntaxError — the caller writes the bytes out either way,
// since a `set*` decode is specified to keep everything it produced before the
// error.
//
// maxLength caps the output: a chunk that would overflow it is not decoded at
// all, and the decode stops there without an error (that is how setFromBase64
// reports a partially filled target).
func fromBase64(s []byte, url bool, lastChunk string, maxLength int) (bytes []byte, read int, bad bool) {
	if maxLength == 0 {
		return nil, 0, false
	}
	var chunk [4]int
	chunkLength := 0
	index := 0
	length := len(s)
	// fits reports whether n more bytes stay within maxLength.
	fits := func(n int) bool { return len(bytes)+n <= maxLength }
	for {
		for index < length && b64Whitespace(s[index]) {
			index++
		}
		if index == length {
			if chunkLength > 0 {
				switch lastChunk {
				case "stop-before-partial":
					return bytes, read, false
				case "strict":
					return bytes, read, true
				default: // "loose": the final chunk may omit its padding
					if chunkLength == 1 {
						return bytes, read, true
					}
					if !fits(chunkLength - 1) {
						return bytes, read, false
					}
					b, _ := decodeB64Chunk(chunk, chunkLength, false)
					bytes = append(bytes, b...)
				}
			}
			return bytes, length, false
		}
		c := s[index]
		index++
		if c == '=' {
			// A padded final chunk. One '=' completes a 3-character chunk, two
			// complete a 2-character one; anything else — including trailing
			// characters after the padding — is a SyntaxError.
			if chunkLength < 2 {
				return bytes, read, true
			}
			if chunkLength == 2 {
				for index < length && b64Whitespace(s[index]) {
					index++
				}
				if index == length || s[index] != '=' {
					// The padding itself is incomplete. "stop-before-partial" stops
					// here and reports what it decoded; the other modes reject.
					if lastChunk == "stop-before-partial" {
						return bytes, read, false
					}
					return bytes, read, true
				}
				index++
			}
			for index < length && b64Whitespace(s[index]) {
				index++
			}
			if index != length {
				return bytes, read, true
			}
			if !fits(chunkLength - 1) {
				return bytes, read, false
			}
			b, ok := decodeB64Chunk(chunk, chunkLength, lastChunk == "strict")
			if !ok {
				return bytes, read, true
			}
			return append(bytes, b...), length, false
		}
		d := b64Digit(c, url)
		if d < 0 {
			return bytes, read, true
		}
		chunk[chunkLength] = d
		chunkLength++
		if chunkLength == 4 {
			if !fits(3) {
				return bytes, read, false
			}
			b, _ := decodeB64Chunk(chunk, 4, false)
			bytes = append(bytes, b...)
			chunkLength = 0
			read = index
			if len(bytes) == maxLength {
				return bytes, read, false
			}
		}
	}
}

// fromHex implements the FromHex abstract operation: pairs of hex digits, no
// whitespace allowed, and an odd-length string decodes nothing at all.
func fromHex(s []byte, maxLength int) (bytes []byte, read int, bad bool) {
	length := len(s)
	if length%2 != 0 {
		return nil, 0, true
	}
	hexVal := func(c byte) int {
		switch {
		case c >= '0' && c <= '9':
			return int(c - '0')
		case c >= 'a' && c <= 'f':
			return int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			return int(c-'A') + 10
		}
		return -1
	}
	for read < length && len(bytes) < maxLength {
		hi, lo := hexVal(s[read]), hexVal(s[read+1])
		if hi < 0 || lo < 0 {
			return bytes, read, true
		}
		bytes = append(bytes, byte(hi<<4|lo))
		read += 2
	}
	return bytes, read, false
}

// encodeBase64 renders bytes in the selected alphabet, optionally without the
// '=' padding of a final partial group.
func encodeBase64(b []byte, url, omitPadding bool) string {
	alpha := b64Std
	if url {
		alpha = b64URL
	}
	out := make([]byte, 0, (len(b)+2)/3*4)
	i := 0
	for ; i+3 <= len(b); i += 3 {
		v := int(b[i])<<16 | int(b[i+1])<<8 | int(b[i+2])
		out = append(out, alpha[v>>18&63], alpha[v>>12&63], alpha[v>>6&63], alpha[v&63])
	}
	switch len(b) - i {
	case 1:
		v := int(b[i]) << 16
		out = append(out, alpha[v>>18&63], alpha[v>>12&63])
		if !omitPadding {
			out = append(out, '=', '=')
		}
	case 2:
		v := int(b[i])<<16 | int(b[i+1])<<8
		out = append(out, alpha[v>>18&63], alpha[v>>12&63], alpha[v>>6&63])
		if !omitPadding {
			out = append(out, '=')
		}
	}
	return string(out)
}

// encodeHex renders bytes as lowercase hex digit pairs.
func encodeHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}

// getOptionsObject implements GetOptionsObject: undefined yields a fresh
// null-prototype object (so every Get sees undefined), a non-object is a
// TypeError, and an object is used as given.
func (rt *Runtime) getOptionsObject(v Value) (Value, *ThrowError) {
	if v.IsUndefined() {
		return rt.newObject(mknull()), nil
	}
	if !v.IsObjectLike() || rt.objPtr(v) == nil {
		return mkundef(), rt.typeError("options must be an object")
	}
	return v, nil
}

// getStringOption reads a string-valued option that must be one of `allowed`.
// The value is NOT coerced: a String object or any other non-string is a
// TypeError before its toString could run.
func (rt *Runtime) getStringOption(opts Value, name, dflt string, allowed ...string) (string, *ThrowError) {
	v, e := rt.getField(opts, name)
	if e != nil {
		return "", e
	}
	if v.IsUndefined() {
		return dflt, nil
	}
	if !v.IsString() {
		return "", rt.typeError(name + " option must be a string")
	}
	s := rt.strGo(v)
	for _, a := range allowed {
		if s == a {
			return s, nil
		}
	}
	return "", rt.typeError("invalid " + name + " option")
}

// defUint8ArrayBase64Hex installs the Uint8Array base64/hex conversions.
func (rt *Runtime) defUint8ArrayBase64Hex(cobj, proto *object, kind taKind) {
	// requireU8 implements ValidateUint8Array: the receiver must be a Uint8Array.
	// The detached / out-of-bounds check is deliberately NOT here — it belongs
	// after the options object has been read, since an option getter may detach
	// the buffer and the spec observes that.
	requireU8 := func(this Value) (*object, *ThrowError) {
		o := rt.objPtr(this)
		if o == nil || o.ta == nil || o.ta.kind != taUint8 {
			return nil, rt.typeError("Uint8Array method called on an incompatible receiver")
		}
		return o, nil
	}
	// liveBytes re-checks the buffer and snapshots the receiver's elements
	// (GetUint8ArrayBytes), so an option getter's side effects on the receiver are
	// reflected in the result.
	liveBytes := func(this Value) ([]byte, *ThrowError) {
		o := rt.objPtr(this)
		if rt.taOutOfBounds(o) {
			return nil, rt.typeError("Cannot perform Uint8Array operation on a detached or out-of-bounds buffer")
		}
		n := rt.taLength(o)
		b := make([]byte, n)
		for i := 0; i < n; i++ {
			v, _ := rt.taGet(o, i)
			b[i] = byte(uint8(v.Number()))
		}
		return b, nil
	}
	newFrom := func(b []byte) Value {
		arrV, _ := rt.newTypedArray(kind, []Value{mknum(float64(len(b)))})
		ao := rt.objPtr(arrV)
		for i, by := range b {
			rt.taSet(ao, i, float64(by))
		}
		return arrV
	}
	// setResult writes the decoded bytes into the receiver and builds the
	// { read, written } result. The write happens even when the decode failed:
	// a partial decode keeps whatever it produced.
	setResult := func(this Value, decoded []byte, read int) Value {
		o := rt.objPtr(this)
		for i, b := range decoded {
			rt.taSet(o, i, float64(b))
		}
		res := rt.newObject(rt.objectProto)
		ro := rt.objPtr(res)
		ro.defineOwn("read", mknum(float64(read)), attrDefault)
		ro.defineOwn("written", mknum(float64(len(decoded))), attrDefault)
		return res
	}
	// decodeOptions reads the alphabet and lastChunkHandling options of a base64
	// decode, in that order.
	decodeOptions := func(args []Value) (url bool, lastChunk string, e *ThrowError) {
		opts, e := rt.getOptionsObject(arg(args, 1))
		if e != nil {
			return false, "", e
		}
		alphabet, e := rt.getStringOption(opts, "alphabet", "base64", "base64", "base64url")
		if e != nil {
			return false, "", e
		}
		lastChunk, e = rt.getStringOption(opts, "lastChunkHandling", "loose",
			"loose", "strict", "stop-before-partial")
		if e != nil {
			return false, "", e
		}
		return alphabet == "base64url", lastChunk, nil
	}

	rt.defMethod(proto, "toHex", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if _, e := requireU8(this); e != nil {
			return mkundef(), e
		}
		b, e := liveBytes(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(encodeHex(b)), nil
	})
	rt.defMethod(proto, "toBase64", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if _, e := requireU8(this); e != nil {
			return mkundef(), e
		}
		opts, e := rt.getOptionsObject(arg(args, 0))
		if e != nil {
			return mkundef(), e
		}
		alphabet, e := rt.getStringOption(opts, "alphabet", "base64", "base64", "base64url")
		if e != nil {
			return mkundef(), e
		}
		omitV, e := rt.getField(opts, "omitPadding")
		if e != nil {
			return mkundef(), e
		}
		b, e := liveBytes(this)
		if e != nil {
			return mkundef(), e
		}
		return rt.newString(encodeBase64(b, alphabet == "base64url", rt.toBoolean(omitV))), nil
	})
	rt.defMethod(cobj, "fromHex", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !arg(args, 0).IsString() {
			return mkundef(), rt.typeError("Uint8Array.fromHex requires a string argument")
		}
		b, _, bad := fromHex(rt.strBytes(arg(args, 0)), b64NoLimit)
		if bad {
			return mkundef(), rt.syntaxError("Invalid hex string")
		}
		return newFrom(b), nil
	})
	rt.defMethod(cobj, "fromBase64", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		if !arg(args, 0).IsString() {
			return mkundef(), rt.typeError("Uint8Array.fromBase64 requires a string argument")
		}
		url, lastChunk, e := decodeOptions(args)
		if e != nil {
			return mkundef(), e
		}
		b, _, bad := fromBase64(rt.strBytes(arg(args, 0)), url, lastChunk, b64NoLimit)
		if bad {
			return mkundef(), rt.syntaxError("Invalid base64 string")
		}
		return newFrom(b), nil
	})
	rt.defMethod(proto, "setFromHex", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := requireU8(this)
		if e != nil {
			return mkundef(), e
		}
		// ValidateUint8Array(into, ~write~): an immutable buffer cannot be written,
		// even by a decode that would produce no bytes.
		if e := rt.taWriteImmutable(this); e != nil {
			return mkundef(), e
		}
		if !arg(args, 0).IsString() {
			return mkundef(), rt.typeError("setFromHex requires a string argument")
		}
		if rt.taOutOfBounds(o) {
			return mkundef(), rt.typeError("Cannot perform Uint8Array operation on a detached or out-of-bounds buffer")
		}
		b, read, bad := fromHex(rt.strBytes(arg(args, 0)), rt.taLength(o))
		res := setResult(this, b, read)
		if bad {
			return mkundef(), rt.syntaxError("Invalid hex string")
		}
		return res, nil
	})
	rt.defMethod(proto, "setFromBase64", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		o, e := requireU8(this)
		if e != nil {
			return mkundef(), e
		}
		if e := rt.taWriteImmutable(this); e != nil {
			return mkundef(), e
		}
		if !arg(args, 0).IsString() {
			return mkundef(), rt.typeError("setFromBase64 requires a string argument")
		}
		url, lastChunk, e := decodeOptions(args)
		if e != nil {
			return mkundef(), e
		}
		// The out-of-bounds check follows the option getters, which may detach.
		if rt.taOutOfBounds(o) {
			return mkundef(), rt.typeError("Cannot perform Uint8Array operation on a detached or out-of-bounds buffer")
		}
		b, read, bad := fromBase64(rt.strBytes(arg(args, 0)), url, lastChunk, rt.taLength(o))
		res := setResult(this, b, read)
		if bad {
			return mkundef(), rt.syntaxError("Invalid base64 string")
		}
		return res, nil
	})
}
