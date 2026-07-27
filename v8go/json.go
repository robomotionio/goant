package v8go

import (
	"errors"
	"strconv"

	"github.com/robomotionio/goant/internal/engine"
)

// Byte-oriented JSON, for the host that speaks JSON to the script — which is
// most of them, and is exactly what a message pump does.
//
// These are extensions beyond the V8 binding's surface, not part of the
// compatibility shim. They exist because the binding's shape is dictated by
// cgo: everything has to be marshalled across the boundary, so the API it can
// offer is one big string in each direction. Without that boundary the copies
// are avoidable, and on a large message they are the whole cost:
//
//	inbound   []byte -> Go string -> engine string -> JSON.parse -> objects
//	outbound  objects -> JSON.stringify -> engine string -> Go string -> []byte
//
// Worse, a host with several outputs has to pack them into one value to bring
// them back in a single crossing, so it stringifies each result and then
// stringifies the array of strings — escaping every quote in every payload a
// second time, and unescaping it again on the far side.
//
// Here the parse reads the host's bytes directly and the serializer appends
// into the host's buffer, so each payload is written once and never escaped
// twice.

// ParseJSONBytes parses b into a value, with no intermediate JS string.
//
// This is JSON.parse without a reviver, and without the two copies that
// reaching JSON.parse from Go otherwise costs. b is not retained: the parser
// reads it in place, but every string and key in the result is freshly
// allocated, so the caller may reuse b as soon as this returns.
func (c *Context) ParseJSONBytes(b []byte) (*Value, error) {
	if c == nil {
		return nil, errors.New("v8go: nil context")
	}
	v, err := c.r.JSONParseBytes(b)
	if err != nil {
		return nil, asJSError(c.r, err)
	}
	return wrap(c.r, v), nil
}

// ParseJSONBytesLazy parses b without building the value graph: objects and
// arrays come back knowing their own layout, and each property or element is
// built the first time something reads it. A value nobody reads is never
// built, and one the host serializes without touching goes back out as the
// bytes it came in as.
//
// For a message pump this is the difference between paying for the message and
// paying for the part of it the script uses. A pass-through costs a scan; a
// full traversal costs what the eager parse would have; anything in between
// lands in between. There is nothing to configure and nothing to predict.
//
// The one contract difference from ParseJSONBytes: b IS retained. It must not
// be modified or reused while the returned value — or any value reachable from
// it — is still alive.
func (c *Context) ParseJSONBytesLazy(b []byte) (*Value, error) {
	if c == nil {
		return nil, errors.New("v8go: nil context")
	}
	v, err := c.r.JSONParseBytesLazy(b)
	if err != nil {
		return nil, asJSError(c.r, err)
	}
	return wrap(c.r, v), nil
}

// JSONElementsToBytes serializes each element of an array value, appending
// every result to dst and returning the extended buffer together with the
// offset each element ends at. The caller slices the payloads apart without
// copying them.
//
// At most limit elements are serialized; pass a negative limit for all of them.
// A host that discards outputs beyond a fixed count should say so here rather
// than pay to build them.
//
// An element that serializes to nothing — undefined, a function, a symbol —
// contributes no bytes, so its span is empty and is distinguishable from an
// element that serialized to the empty string.
func (v *Value) JSONElementsToBytes(dst []byte, limit int) (out []byte, ends []int, err error) {
	if v == nil || v.r == nil {
		return dst, nil, errors.New("v8go: nil value")
	}
	if !v.r.IsArray(v.rt) {
		return dst, nil, errors.New("v8go: value is not an array")
	}
	lenVal, err := v.r.GetProp(v.rt, "length")
	if err != nil {
		return dst, nil, asJSError(v.r, err)
	}
	n, err := v.r.ToNumber(lenVal)
	if err != nil {
		return dst, nil, asJSError(v.r, err)
	}
	count := int(n)
	if count < 0 {
		count = 0
	}
	if limit >= 0 && count > limit {
		count = limit
	}

	vals := make([]engine.Value, 0, count)
	for i := 0; i < count; i++ {
		e, err := v.r.GetProp(v.rt, strconv.Itoa(i))
		if err != nil {
			return dst, nil, asJSError(v.r, err)
		}
		vals = append(vals, e)
	}
	out, ends, serr := v.r.JSONStringifyEachToBytes(vals, dst)
	if serr != nil {
		return out, ends, asJSError(v.r, serr)
	}
	return out, ends, nil
}
