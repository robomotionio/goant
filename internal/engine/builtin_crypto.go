package engine

// The Crypto interface (WHATWG Web Crypto): the way a script asks for bytes
// nobody can predict.
//
// Math.random is not that and was never meant to be. It is a 64-bit xorshift,
// and a handful of its outputs give the state away — so an id minted from it is
// an id that anyone holding a few earlier ones can mint again. Leaving this
// interface out did not steer scripts away from that; it steered them into it,
// because every id library in circulation feature-detects getRandomValues and
// silently falls back to Math.random when it is missing. The weak path was not
// the one nobody took, it was the only one on offer.
//
// Both methods read the operating system's CSPRNG on the call. There is no
// user-space pool and no per-Runtime state, and that is the design rather than
// an omission: nothing to seed, nothing to duplicate when a realm is built, and
// nothing an invocation could rewind into repeating itself.
//
// subtle is absent rather than stubbed. It is an asynchronous API over key
// material — import, derive, sign, encrypt — and a stand-in that answered
// anything at all would be worse than the property not being there, because a
// script tests for it and takes a different path when it is missing. Same for
// the Crypto constructor: browsers expose it so `crypto instanceof Crypto`
// works, and exposing one that cannot construct anything buys the check its
// answer at the price of a global that does nothing.

import (
	cryptorand "crypto/rand"
	"strconv"
)

// randomValuesQuota is the per-call ceiling the spec puts on getRandomValues.
// It is a limit on how much a single call may ask for, not on how much entropy
// exists; a caller wanting more calls again.
const randomValuesQuota = 65536

func (rt *Runtime) initCrypto() {
	c := rt.newObject(rt.objectProto)
	co := rt.objPtr(c)

	// crypto.getRandomValues(view): fill view with random bytes and return it.
	//
	// The write goes to the bytes directly rather than element by element,
	// which is right for every kind it accepts — a random element and a random
	// byte pattern are the same thing — but it does mean stepping around the
	// store path, so the two things that path does have to be done here: the
	// window is recomputed from the view rather than trusted (a resizable
	// buffer may have shrunk under it), and a write to a view older than this
	// invocation is reported, or a host pooling Runtimes would rewind a region
	// that shared bytes had been written into.
	co.defineOwn("getRandomValues", rt.newNativeFunc("getRandomValues", 1, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		v := arg(args, 0)
		o := rt.objPtr(v)
		if o == nil || (o.ta == nil && o.dv() == nil) {
			return mkundef(), rt.typeError(
				"Failed to execute 'getRandomValues' on 'Crypto': parameter 1 is not of type 'ArrayBufferView'.")
		}
		if o.ta == nil || !isIntegerTAKind(o.ta.kind) {
			return mkundef(), rt.typeError(
				"Failed to execute 'getRandomValues' on 'Crypto': The provided ArrayBufferView is of type '" +
					viewTypeName(o) + "', which is not an integer array type.")
		}

		// A detached or out-of-bounds view reports zero length and is filled
		// with nothing, which is what the spec's byte-length step amounts to
		// and what a browser does. Refusing it would be a second failure mode
		// for the same condition every other TypedArray read already treats as
		// empty.
		n := rt.taCurrentLen(o) * o.ta.size()
		if n > randomValuesQuota {
			return mkundef(), rt.typeError(
				"Failed to execute 'getRandomValues' on 'Crypto': The ArrayBufferView's byte length (" +
					strconv.Itoa(n) + ") exceeds the number of bytes of entropy available via this API (" +
					strconv.Itoa(randomValuesQuota) + ").")
		}
		if n > 0 {
			if b := rt.taBytes(o.ta); b != nil {
				rt.noteSharedMutationOf(o)
				off := o.ta.byteOffset
				cryptorand.Read(b[off : off+n])
			}
		}
		return v, nil
	}), attrWritable|attrConfigurable)

	// crypto.randomUUID(): a version 4 UUID, from the same source.
	//
	// This is the function the customer script that started all this was
	// hand-rolling out of Math.random, and hand-rolling it is how the fixed
	// seed became visible as one UUID forever. Sixteen bytes, six bits spent
	// on the version and variant, and no way for it to repeat.
	co.defineOwn("randomUUID", rt.newNativeFunc("randomUUID", 0, func(rt *Runtime, this Value, args []Value) (Value, *ThrowError) {
		return rt.newString(randomUUID()), nil
	}), attrWritable|attrConfigurable)

	rt.setStringTag(c, "Crypto")
	rt.objPtr(rt.global).defineOwn("crypto", c, attrWritable|attrConfigurable)
}

// isIntegerTAKind reports whether getRandomValues accepts this element kind.
// The float kinds are excluded because there is no bit pattern that is a random
// float in any useful sense — many of them are not even distinct values, since
// every NaN payload reads back as one NaN. BigInt64 and BigUint64 are integer
// types and are accepted, as they are in a browser.
func isIntegerTAKind(k taKind) bool {
	switch k {
	case taFloat16, taFloat32, taFloat64:
		return false
	}
	return true
}

// viewTypeName is what a browser calls this view in the type-mismatch message:
// the element kind without the "Array" suffix, or "DataView".
func viewTypeName(o *object) string {
	if o.ta == nil {
		return "DataView"
	}
	name := taKinds[o.ta.kind].name
	if len(name) > 5 && name[len(name)-5:] == "Array" {
		return name[:len(name)-5]
	}
	return name
}

// randomUUID builds a version 4 UUID (RFC 9562 §5.4) from 16 CSPRNG bytes.
func randomUUID() string {
	var b [16]byte
	cryptorand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant 10xx

	const hex = "0123456789abcdef"
	var out [36]byte
	j := 0
	for i, c := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[j] = '-'
			j++
		}
		out[j] = hex[c>>4]
		out[j+1] = hex[c&0x0f]
		j += 2
	}
	return string(out[:])
}
