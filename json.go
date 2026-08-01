package goant

import (
	"errors"

	"github.com/robomotionio/goant/internal/engine"
)

// Byte-oriented JSON, for the host that speaks JSON to the script — which is
// most of them, and is exactly what a message pump does.
//
// The shape of a cgo binding's JSON support is dictated by its boundary:
// everything has to be marshalled across, so what it can offer is one big
// string in each direction, and a large message is copied several times:
//
//	inbound   []byte -> Go string -> engine string -> JSON.parse -> objects
//	outbound  objects -> JSON.stringify -> engine string -> Go string -> []byte
//
// Worse, a host with several outputs has to pack them into one value to bring
// them back in a single crossing, so it stringifies each result and then
// stringifies the array of those strings — escaping every quote in every
// payload a second time, and unescaping it again on the far side.
//
// With no boundary to cross, none of that is necessary. The parser reads the
// host's bytes in place and the serializer appends into the host's buffer, so
// each payload is written once and escaped once.

// ParseJSON parses b into a JavaScript value, with no intermediate string.
//
// This is JSON.parse without a reviver, and without the two copies that
// reaching JSON.parse from Go otherwise costs. b is not retained: the parser
// reads it in place, but every string and key in the result is freshly
// allocated, so the caller may reuse b as soon as this returns.
func (rt *Runtime) ParseJSON(b []byte) (Value, error) {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}, err
	}
	v, perr := e.JSONParseBytes(b)
	if perr != nil {
		return Value{}, rt.wrap(perr)
	}
	return rt.val(v), nil
}

// ParseJSONLazy parses b without building the value graph: objects and arrays
// come back knowing their own layout, and each property or element is built the
// first time something reads it. A value nobody reads is never built, and one
// the host serializes without touching goes back out as the bytes it came in as.
//
// For a message pump this is the difference between paying for the message and
// paying for the part of it the script uses. A pass-through costs a scan; a full
// traversal costs what the eager parse would have; anything in between lands in
// between. There is nothing to configure and nothing to predict.
//
// The whole document is still validated up front, so a syntax error belongs to
// the parse rather than to whichever read reaches it.
//
// One contract difference from ParseJSON: b IS retained. It must not be modified
// or reused while the returned value — or anything reachable from it — is alive.
func (rt *Runtime) ParseJSONLazy(b []byte) (Value, error) {
	e, err := rt.engineOf()
	if err != nil {
		return Value{}, err
	}
	v, perr := e.JSONParseBytesLazy(b)
	if perr != nil {
		return Value{}, rt.wrap(perr)
	}
	return rt.val(v), nil
}

// AppendJSONEach serializes each element of an array, appending every result to
// dst and returning the extended buffer together with the offset each element
// ends at. The caller slices the payloads apart without copying them.
//
//	buf, ends, err := arr.AppendJSONEach(nil, -1)
//	start := 0
//	for _, end := range ends {
//	    payload := buf[start:end:end]   // capped, so an append cannot reach the next
//	    start = end
//	}
//
// This is for a host that produces several outputs per run and wants each as
// its own []byte. Everything lands in one buffer, so it is one allocation
// rather than one per output, and each value is serialized exactly once.
//
// At most limit elements are serialized; pass a negative limit for all of them.
// A host that discards outputs beyond a fixed count should say so here rather
// than pay to build them.
//
// An element with no JSON form — undefined, a function, a symbol — contributes
// no bytes, so its span is empty. That is distinguishable from an element that
// serialized to the empty string, which is not valid JSON and cannot occur.
func (o *Object) AppendJSONEach(dst []byte, limit int) (out []byte, ends []int, err error) {
	if o == nil {
		return dst, nil, errNilObject
	}
	e, ok := o.live()
	if !ok {
		return dst, nil, ErrClosed
	}
	if !e.IsArray(o.v) {
		return dst, nil, errors.New("goant: value is not an array")
	}
	n, lerr := e.LengthOf(o.v)
	if lerr != nil {
		return dst, nil, o.rt.wrap(lerr)
	}
	if limit >= 0 && n > limit {
		n = limit
	}
	vals := make([]engine.Value, 0, n)
	for i := 0; i < n; i++ {
		ev, gerr := e.GetIndex(o.v, i)
		if gerr != nil {
			return dst, nil, o.rt.wrap(gerr)
		}
		vals = append(vals, ev)
	}
	out, ends, serr := e.JSONStringifyEachToBytes(vals, dst)
	if serr != nil {
		return out, ends, o.rt.wrap(serr)
	}
	return out, ends, nil
}

// SetBlobResolver installs the resolver a lazy parse uses for envelopes — the
// small stand-ins a host leaves in a message for data it keeps elsewhere.
//
// Resolving on first read rather than before the parse is what makes an
// envelope pay: a field the script never mentions is never fetched, so passing
// a message that references a hundred megabytes costs a few hundred bytes. A
// fetch the resolver cannot satisfy stops the script and is reported as the
// error from the run, rather than surfacing as a type error in the middle of
// someone's JavaScript.
//
// This only affects ParseJSONLazy.
func (rt *Runtime) SetBlobResolver(fn func(ref string) ([]byte, error)) {
	if e, err := rt.engineOf(); err == nil {
		e.SetBlobResolver(fn)
	}
}
