package engine

// Byte-oriented JSON entry points for embedders.
//
// A host that speaks JSON to a script — which is most of them — otherwise pays
// for a round trip through JS strings in both directions:
//
//	inbound:  []byte -> Go string -> engine string -> JSON.parse -> objects
//	outbound: objects -> JSON.stringify -> engine string -> Go string -> []byte
//
// Every arrow that is not the parse or the serialize itself is a full copy of
// the payload, and on a message pump that is the dominant cost. These entry
// points remove them: parse reads the host's bytes directly, serialize appends
// into the host's buffer.
//
// This is only possible without a foreign-function boundary. A cgo binding has
// to marshal across regardless, which is why the API it inherits is shaped
// around one big string in each direction.

import "unsafe"

// JSONParseBytes parses JSON directly from b, with no intermediate JS string.
//
// b must not be modified while the returned value is reachable: the parser
// takes a string view over it rather than copying. Strings *inside* the result
// are freshly allocated, so only the top-level buffer is aliased.
//
// This is JSON.parse without a reviver. A host that needs one should go through
// the builtin, since the reviver has to observe source text.
func (rt *Runtime) JSONParseBytes(b []byte) (Value, error) {
	if len(b) == 0 {
		return mkundef(), &jsonError{"Unexpected end of JSON input"}
	}
	// A string view over the caller's bytes. Safe because the parser only reads,
	// and because every string it produces is a fresh allocation — parseString
	// unescapes into a new buffer, and newString copies.
	src := unsafe.String(unsafe.SliceData(b), len(b))
	p := &jsonParser{rt: rt, src: src, aliased: true}
	v, _, err := p.parse()
	if err != nil {
		return mkundef(), err
	}
	p.skipWS()
	if p.pos != len(p.src) {
		return mkundef(), &jsonError{"Unexpected non-whitespace character after JSON"}
	}
	return v, nil
}

// JSONStringifyToBytes serializes v as JSON, appending to dst and returning the
// extended slice. Pass nil to allocate, or a reused buffer to avoid allocating
// at all.
//
// ok is false when v serializes to nothing — undefined, a function, a symbol.
// That is not the string "undefined" and not an empty string; it is the absence
// of a value, and a caller that flattens the distinction corrupts its output.
// dst is returned unchanged in that case.
//
// The value is serialized once, straight into dst. The pattern this replaces —
// stringify in JS, hand back a JS string, copy it to Go, and for multiple
// outputs stringify the whole lot a second time so it can travel as one value —
// costs several full passes over the payload and escapes it twice.
func (rt *Runtime) JSONStringifyToBytes(v Value, dst []byte) (out []byte, ok bool, err error) {
	// Serialize straight into the caller's buffer: the stringifier appends, so
	// handing it dst means the document is built once, in place, with no
	// intermediate string and no final copy.
	st := &jsonStringifier{rt: rt, buf: dst, rawSpans: rt.lazyRawSafe()}
	holder := rt.newPlainObject()
	rt.objPtr(holder).defineOwn("", v, attrDefault)
	ok, terr := st.str("", holder, "")
	if terr != nil {
		return dst, false, terr
	}
	if !ok {
		// Nothing was appended, but the stringifier may have grown the buffer;
		// return it truncated to what the caller gave us.
		return st.buf[:len(dst)], false, nil
	}
	return st.buf, true, nil
}

// JSONStringifyEachToBytes serializes each of vals in turn, appending every
// result to dst and returning the extended buffer plus the offset each value
// ends at, so a caller can slice them apart without a second pass.
//
// This exists for the multi-output case, which is the one that made the old
// arrangement expensive: N results had to be packed into a single JS value to
// come back across in one piece, so each was stringified and then the array of
// strings was stringified again — escaping every quote in every payload a
// second time. Here they are simply written one after another.
//
// A value that serializes to nothing contributes no bytes; its offset equals
// the previous one, so an empty span is distinguishable from a written one.
func (rt *Runtime) JSONStringifyEachToBytes(vals []Value, dst []byte) (out []byte, ends []int, err error) {
	ends = make([]int, len(vals))
	for i, v := range vals {
		var ok bool
		dst, ok, err = rt.JSONStringifyToBytes(v, dst)
		if err != nil {
			return dst, ends, err
		}
		_ = ok
		ends[i] = len(dst)
	}
	return dst, ends, nil
}
