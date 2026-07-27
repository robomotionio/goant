package engine

// Lazy JSON: parse a document by scanning it, not by building it.
//
// The eager parser turns every byte of a message into engine objects before
// the script runs, which costs several times the document in live memory and
// is pure waste for a script that reads two fields and returns. The usual
// alternative — leave the bytes alone and answer each property read from the
// host — trades that for a full document walk per read, which is quadratic
// over a loop. Neither is a good default, and the thing that decides which is
// right (how much of the message the script touches) is not known until after
// the script has run.
//
// This is the third option, and it does not require the choice. A lazily
// parsed value knows its own layout — for an object, its keys and where each
// value starts; for an array, where each element starts — and nothing below
// that. Reading a slot parses exactly the value at that offset and writes the
// result back, so a second read is an ordinary inline-cached slot load. A
// value nobody reads is never built.
//
// The cost of a script is then proportional to what it touches, on both axes:
// a pass-through parses nothing and serialises the original bytes, a full
// traversal pays roughly the eager parse spread out over the reads, and
// anything between lands between. There is no crossover to predict and no
// flag to set.
//
// The whole document is validated up front (lazyScan below, one allocation-free
// pass), so a syntax error is still reported by the parse and not by whichever
// property read happens to reach it.

import (
	"errors"
	"strconv"
	"strings"
	"unsafe"
)

// maxLazyDoc is the largest document a lazy parse will take. Offsets ride in
// the 46-bit payload of a sentinel Value, so the real ceiling is far higher;
// this one keeps the arithmetic in int32 range on 32-bit hosts and stops a
// pathological input long before the payload could overflow.
const maxLazyDoc = 1 << 32

// lazyDoc is one document's backing bytes, shared by every lazy node parsed
// out of it. src aliases the host's buffer: nothing here copies it, which is
// the point.
type lazyDoc struct {
	rt  *Runtime
	src string

	// resolver fetches the bytes behind a content-addressed reference, for a
	// host that stores large fields outside the message and leaves an envelope
	// in their place. Nil when the host has no such store, which is the case
	// this file was written for and still the common one.
	//
	// Resolving on first read rather than before the parse is what makes the
	// envelope worth having: a field the script never mentions is never
	// fetched, so a message that carries a reference to a hundred megabytes
	// costs a few hundred bytes to pass through.
	resolver BlobResolver

	// blobs caches what has already been fetched, keyed by reference. Two
	// envelopes naming the same blob are common — a message that fans out and
	// rejoins — and re-fetching would decompress it twice.
	blobs map[string][]byte

	// pending is set on the document belonging to an array that stands for a
	// blob nobody has read yet. Such a document has no src: its elements are
	// indices, not offsets, until the blob arrives and it adopts the blob's.
	pending *pendingBlob

	// keys holds engine-owned copies of the property names seen so far. A key
	// is interned, and the intern table keeps the Go string itself, so handing
	// it a view of the host's buffer would leave the table pointing at memory
	// the host is free to overwrite. Records in a message overwhelmingly repeat
	// the same few field names, so deduping here makes it one copy per distinct
	// key rather than one per occurrence.
	keys map[string]string
}

// ownKey returns an engine-owned copy of a key parsed out of the host buffer.
func (d *lazyDoc) ownKey(k string) string {
	if owned, ok := d.keys[k]; ok {
		return owned
	}
	owned := strings.Clone(k)
	if d.keys == nil {
		d.keys = make(map[string]string, 8)
	}
	d.keys[owned] = owned
	return owned
}

// JSONParseBytesLazy parses JSON directly from b without building the value
// graph. Objects and arrays come back knowing their own layout and nothing
// deeper; each property or element is parsed the first time it is read.
//
// This is JSON.parse without a reviver, with the same result for any script
// that can observe it, and with cost proportional to what the script reads
// rather than to the size of the document.
//
// b must not be modified or reused for as long as the returned value is
// reachable. That window is longer than JSONParseBytes's: the result aliases
// b for its whole life, not just for the duration of the parse.
func (rt *Runtime) JSONParseBytesLazy(b []byte) (Value, error) {
	if len(b) == 0 {
		return mkundef(), &jsonError{"Unexpected end of JSON input"}
	}
	if len(b) >= maxLazyDoc {
		// Too large to address with a span. Fall back rather than refuse: the
		// eager parser has no offset limit, and a host that got here would
		// rather have a slow answer than an error.
		return rt.JSONParseBytes(b)
	}
	src := unsafe.String(unsafe.SliceData(b), len(b))

	// Validate once, up front, so a malformed document is a parse error rather
	// than a surprise on whichever read reaches the damage. This is the only
	// full pass over the bytes a lazy parse makes, and it allocates nothing.
	start := skipSpace(src, 0)
	end, err := lazyScan(src, start)
	if err != nil {
		return mkundef(), err
	}
	for end < len(src) && isJSONSpace(src[end]) {
		end++
	}
	if end != len(src) {
		return mkundef(), &jsonError{"Unexpected non-whitespace character after JSON"}
	}

	d := &lazyDoc{rt: rt, src: src, resolver: rt.blobResolver}
	return d.value(start), nil
}

// value materialises the single JSON value starting at off. Containers come
// back lazy — knowing their layout, holding spans for their contents — and
// primitives come back as themselves.
//
// The document was validated by lazyScan before any of this ran, so nothing
// here can fail; a malformed input would have been rejected by the parse.
func (d *lazyDoc) value(off int) Value {
	switch c := d.src[off]; c {
	case '{':
		return d.object(off)
	case '[':
		return d.array(off)
	case '"':
		s, _, _ := lazyString(d.src, off)
		return d.rt.newString(s)
	case 't':
		return mktrue()
	case 'f':
		return mkfalse()
	case 'n':
		return mknull()
	default:
		end, _ := lazyNumber(d.src, off)
		f, err := strconv.ParseFloat(d.src[off:end], 64)
		if err != nil {
			// Only ErrRange survives validation, and ParseFloat has already
			// returned the correct ±Inf for it.
			if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
				return mkundef()
			}
		}
		return mknum(f)
	}
}

// BlobResolver returns the bytes behind a content-addressed reference. An
// error stops the script the way a heap-limit breach does — the read cannot
// produce a value, and carrying on would hand the script an envelope where it
// expected data, which surfaces as a type error somewhere unrelated.
type BlobResolver func(ref string) ([]byte, error)

// SetBlobResolver installs the resolver used by JSONParseBytesLazy for
// envelopes it encounters. Pass nil to disable, which is the default.
func (rt *Runtime) SetBlobResolver(r BlobResolver) { rt.blobResolver = r }

// BlobResolveError returns the failure that stopped the last script, if it was
// stopped by a blob that could not be fetched.
func (rt *Runtime) BlobResolveError() error { return rt.blobErr }

// Envelope marker keys. An envelope is recognised by carrying both, with
// __magic holding the agreed constant — the same test the host makes, done
// here so the check costs a shape lookup rather than a document scan.
const (
	blobKeyRef   = "__ref"
	blobKeyMagic = "__magic"
	blobKeyType  = "__type"
	blobKeyLen   = "__len"
	blobMagic    = 20260301
)

// maxDeferredElems caps how long an array an envelope may claim to be before
// the claim stops being taken on trust.
//
// __len travels in the message rather than in the content-addressed blob, so
// unlike the data it is not self-verifying. Believing it is what makes reading
// a length free; the exposure is one slice of that many Values, so the bound is
// set where pre-sizing stops being cheaper than reading the blob and counting.
// Past it, the envelope resolves the old way.
const maxDeferredElems = 1 << 22 // 4M elements, 32 MB of spans

// errBlobShape reports a blob that did not hold the array its envelope
// described. It is a corrupt store rather than a script error, so it stops the
// script rather than surfacing as elements that read as undefined.
var errBlobShape = errors.New("goant: blob does not hold the array its envelope describes")

// pendingBlob is an array envelope that has been stood up but not read. It
// holds what is needed to fetch the blob on the first element access, and where
// the envelope's own text sits in the referring document, so an array that
// nobody touched can be written back out as the reference it arrived as.
type pendingBlob struct {
	ref    string
	elems  int
	envDoc *lazyDoc
	envOff int
}

// blobRef reports whether o is an envelope and, if so, what it refers to.
//
// The probe is two shape lookups, which is why it can afford to run on every
// object materialised: an ordinary record fails the first one. Only an object
// carrying both keys pays for reading them, and by then it is almost certainly
// an envelope.
// elems is the number of elements the envelope says it stands for, or -1 when
// it describes something other than an array, omits the count, or gives one
// that cannot be taken on trust.
func (d *lazyDoc) blobRef(o *object) (ref string, elems int, ok bool) {
	magicSlot := o.shape.lookupInterned(blobKeyMagic)
	if magicSlot < 0 {
		return "", -1, false
	}
	refSlot := o.shape.lookupInterned(blobKeyRef)
	if refSlot < 0 {
		return "", -1, false
	}
	magic := o.slotGet(uint32(magicSlot))
	if magic.Type() != TNum || magic.Number() != blobMagic {
		return "", -1, false
	}
	rv := o.slotGet(uint32(refSlot))
	if rv.Type() != TStr {
		return "", -1, false
	}
	s := d.rt.strGo(rv)
	if s == "" {
		return "", -1, false
	}
	return s, d.envelopeElems(o), true
}

// envelopeElems reads the element count an array envelope declares.
//
// __len is only an element count when __type says "array" — for a string
// envelope the same key holds a character count — so both are read or neither
// is believed.
func (d *lazyDoc) envelopeElems(o *object) int {
	typeSlot := o.shape.lookupInterned(blobKeyType)
	if typeSlot < 0 {
		return -1
	}
	tv := o.slotGet(uint32(typeSlot))
	if tv.Type() != TStr || d.rt.strGo(tv) != "array" {
		return -1
	}
	lenSlot := o.shape.lookupInterned(blobKeyLen)
	if lenSlot < 0 {
		return -1
	}
	lv := o.slotGet(uint32(lenSlot))
	if lv.Type() != TNum {
		return -1
	}
	n := lv.Number()
	elems := int(n)
	if float64(elems) != n || elems < 1 || elems > maxDeferredElems {
		return -1
	}
	return elems
}

// deferredArray stands an array envelope up at the length the envelope
// declares, without fetching anything.
//
// The blob is read on the first access to an element, and not before. That is
// what makes `msg.rows.length` cost a field read rather than a file read, a
// zstd decompress and a scan of the result — the answer was in the message the
// whole time, and resolving to find it was reading the book to count its pages.
//
// Every element is an unresolved span, so arrLen equals len(arr) and the array
// is indistinguishable from any other lazily parsed one. Nothing that walks an
// array has to know this one is different; only forceElem does, and only
// because it is the place the blob is finally needed.
func (d *lazyDoc) deferredArray(ref string, elems, envOff int) Value {
	// The cache is shared with the document that referred here, so a second
	// envelope naming the same blob finds it already fetched.
	if d.blobs == nil {
		d.blobs = make(map[string][]byte, 4)
	}
	arr := d.rt.newArray()
	o := d.rt.objPtr(arr)
	o.lazy = &lazyDoc{
		rt:       d.rt,
		resolver: d.resolver,
		blobs:    d.blobs,
		pending:  &pendingBlob{ref: ref, elems: elems, envDoc: d, envOff: envOff},
	}
	o.arr = make([]Value, elems)
	for i := range o.arr {
		o.arr[i] = mklazy(i)
	}
	o.arrLen = uint32(elems)
	return arr
}

// fetchPendingBlob reads the blob this array stands for and adopts its layout,
// keeping the array's own identity — a script may already be holding it.
//
// A failure stops the script, the same way resolveBlob's does and for the same
// reason: there is no value to carry on with, and leaving the elements reading
// as undefined would turn a missing blob into a wrong answer.
func (o *object) fetchPendingBlob() bool {
	d := o.lazy
	p := d.pending
	b, ok := d.blobs[p.ref]
	if !ok {
		var err error
		b, err = d.resolver(p.ref)
		if err != nil {
			d.rt.blobErr = err
			if d.rt.interrupt != nil {
				d.rt.interrupt.flag.Store(interruptBlob)
			}
			return false
		}
		d.blobs[p.ref] = b
	}
	src := d.rt.objPtr(d.rt.lazyRoot(b, d.resolver, d.blobs))
	if src == nil || src.typeTag != TArr || src.lazy == nil {
		d.rt.blobErr = errBlobShape
		if d.rt.interrupt != nil {
			d.rt.interrupt.flag.Store(interruptBlob)
		}
		return false
	}
	// The blob is authoritative about its own contents, so its length wins over
	// the envelope's claim if the two ever disagree.
	o.lazy = src.lazy
	o.arr = src.arr
	o.arrLen = src.arrLen
	return true
}

// resolveBlob fetches a reference and parses its contents lazily, so a blob
// that turns out to hold an array of records is itself only built as far as
// the script reads it.
func (d *lazyDoc) resolveBlob(ref string, envelope Value) Value {
	if b, ok := d.blobs[ref]; ok {
		return d.rt.lazyRoot(b, d.resolver, d.blobs)
	}
	b, err := d.resolver(ref)
	if err != nil {
		// No value can be produced, and returning the envelope would hand the
		// script an object where it expected the data. Stop instead, the same
		// way the heap limit does, and let the host report what happened.
		d.rt.blobErr = err
		if d.rt.interrupt != nil {
			d.rt.interrupt.flag.Store(interruptBlob)
		}
		return envelope
	}
	if d.blobs == nil {
		d.blobs = make(map[string][]byte, 4)
	}
	d.blobs[ref] = b
	return d.rt.lazyRoot(b, d.resolver, d.blobs)
}

// lazyRoot parses b as a lazily built document sharing a resolver and blob
// cache with the document that referred to it.
func (rt *Runtime) lazyRoot(b []byte, r BlobResolver, blobs map[string][]byte) Value {
	if len(b) == 0 || len(b) >= maxLazyDoc {
		return mknull()
	}
	src := unsafe.String(unsafe.SliceData(b), len(b))
	start := skipSpace(src, 0)
	if _, err := lazyScan(src, start); err != nil {
		return mknull()
	}
	d := &lazyDoc{rt: rt, src: src, resolver: r, blobs: blobs}
	return d.value(start)
}

// object builds a lazy object: every key interned and installed in the shape,
// every value a span. The shape is real, so key-level operations — ownKeys,
// `in`, for-in, delete, Object.keys — need to know nothing about laziness.
// Only reading a value goes through slotGet, which is the one hook.
func (d *lazyDoc) object(off int) Value {
	obj := d.rt.newObject(d.rt.objectProto)
	o := d.rt.objPtr(obj)
	o.lazy = d

	i := skipSpace(d.src, off+1)
	if d.src[i] == '}' {
		return obj
	}
	for {
		key, next, _ := lazyString(d.src, i)
		i = skipSpace(d.src, next)
		i = skipSpace(d.src, i+1) // ':'
		// Last duplicate key wins, exactly as the eager parser's defineOwn does.
		o.defineOwn(d.ownKey(key), mklazy(i), attrDefault)
		i, _ = lazyScan(d.src, i)
		i = skipSpace(d.src, i)
		if d.src[i] == ',' {
			i = skipSpace(d.src, i+1)
			continue
		}
		// An envelope stands in for data the host keeps outside the message.
		// Now that the keys are in the shape, recognising one costs a lookup,
		// and the value the script gets is the blob rather than the reference.
		if d.resolver != nil {
			if ref, elems, ok := d.blobRef(o); ok {
				// An envelope that says how many elements it stands for can be
				// answered about without being opened.
				if elems > 0 {
					return d.deferredArray(ref, elems, off)
				}
				return d.resolveBlob(ref, obj)
			}
		}
		return obj // '}'
	}
}

// array builds a lazy array: one span per element, so the element list costs
// eight bytes an element and a single scan of the array's bytes, whatever the
// elements are. This is the same trick ForEach's ArrayCursor plays on the host
// side, moved inside the engine where the result survives being indexed twice.
func (d *lazyDoc) array(off int) Value {
	arr := d.rt.newArray()
	o := d.rt.objPtr(arr)
	o.lazy = d

	i := skipSpace(d.src, off+1)
	if d.src[i] == ']' {
		return arr
	}
	for {
		o.arr = append(o.arr, mklazy(i))
		o.arrLen++
		i, _ = lazyScan(d.src, i)
		i = skipSpace(d.src, i)
		if d.src[i] == ',' {
			i = skipSpace(d.src, i+1)
			continue
		}
		return arr // ']'
	}
}

// forceSlot parses the span in a named slot and writes the value back. Called
// from slotGet, so it runs inside whichever opcode read the property; the
// collector only runs at interpreter safepoints, which is what lets this
// allocate freely without rooting the intermediate result.
func (o *object) forceSlot(slot uint32, span Value) Value {
	if o.lazy == nil {
		return mkundef()
	}
	v := o.lazy.value(span.lazyOffset())
	o.slotSet(slot, v)
	return v
}

// forceElem is forceSlot for a fast-array element.
func (o *object) forceElem(idx uint32, span Value) Value {
	if o.lazy == nil {
		return mkundef()
	}
	// An array standing in for a blob reads it here — on the first element
	// anyone asks for, having answered for its length until now without one.
	if o.lazy.pending != nil {
		if !o.fetchPendingBlob() {
			return mkundef()
		}
		// The blob's own length won, so an index the envelope promised may not
		// be there. Reading past the end is undefined, not a panic.
		if int(idx) >= len(o.arr) {
			return mkundef()
		}
		return o.arrAt(idx)
	}
	v := o.lazy.value(span.lazyOffset())
	o.arr[idx] = v
	return v
}

// arrAt reads element idx of o's fast-array storage, parsing it first if it is
// still a span. Every read of o.arr that a script can reach goes through here;
// the direct o.arr[i] reads that remain are on arrays the engine built itself
// (arguments objects, disposal stacks, iterator results), which never hold one.
func (o *object) arrAt(idx uint32) Value {
	v := o.arr[idx]
	if v.isLazy() {
		return o.forceElem(idx, v)
	}
	return v
}

// lazyRawSafe reports whether splicing untouched spans can still produce what
// building them would have.
//
// The one thing that can change the answer is a toJSON on Object.prototype or
// Array.prototype: SerializeJSONProperty looks it up on every object it
// serializes, so a spliced span would skip a call the built value would have
// made. Nothing else about an unparsed span is observable, since by definition
// nothing has looked at it. One lookup per serialization settles it.
func (rt *Runtime) lazyRawSafe() bool {
	for _, proto := range [...]Value{rt.objectProto, rt.arrayProto} {
		if tj, _ := rt.getField(proto, "toJSON"); rt.isCallable(tj) {
			return false
		}
	}
	return true
}

// lazySpanRaw returns the source text of holder[key] when that property is
// still an unparsed span, so a serializer can copy the bytes straight through
// rather than building a value only to write it back out.
//
// This is what makes a pass-through free: a script that returns the message
// untouched serializes to the bytes it arrived as, at no allocation and no
// parse, whatever the message weighs.
func (rt *Runtime) lazySpanRaw(holder Value, key string) (string, bool) {
	o := rt.objPtr(holder)
	if o == nil || o.lazy == nil || o.proxy != nil {
		return "", false
	}
	var v Value
	if o.typeTag == TArr {
		idx, ok := canonicalIndex(key)
		if !ok || idx >= o.arrLen || int(idx) >= len(o.arr) {
			return "", false
		}
		v = o.arr[idx]
	} else {
		slot := o.shape.lookupInterned(key)
		if slot < 0 || o.isAccessorSlot(uint32(slot)) {
			return "", false
		}
		v = o.slotGetRaw(uint32(slot))
	}
	if !v.isLazy() {
		// An array standing in for a blob nobody read is still exactly the
		// envelope it was built from, so write that rather than fetching the
		// data in order to write it out again.
		return rt.untouchedEnvelopeRaw(v)
	}
	off := v.lazyOffset()
	end, err := lazyScan(o.lazy.src, off)
	if err != nil {
		return "", false
	}
	return o.lazy.src[off:end], true
}

// untouchedEnvelopeRaw returns the envelope text an array was stood up from,
// when the array still stands for the whole of it.
//
// "Still" is checked, not tracked. Clearing a flag on every write would only be
// as good as the list of writes someone remembered to find, and a missed one
// writes a reference where the script left data — a silent wrong answer in the
// message, which is the worst failure this code can have. Walking the elements
// is one comparison each over a slice already in cache, and it cannot be
// fooled: an element that was read is no longer a span, a push or a truncation
// changes the length, and anything defined on the array itself lands in its
// shape.
func (rt *Runtime) untouchedEnvelopeRaw(v Value) (string, bool) {
	o := rt.objPtr(v)
	if o == nil || o.typeTag != TArr || o.lazy == nil || o.lazy.pending == nil {
		return "", false
	}
	p := o.lazy.pending
	if o.arrLen != uint32(p.elems) || len(o.arr) != p.elems {
		return "", false
	}
	if o.shape != nil && len(o.shape.props) > 0 {
		return "", false
	}
	for i, e := range o.arr {
		if !e.isLazy() || e.lazyOffset() != i {
			return "", false
		}
	}
	end, err := lazyScan(p.envDoc.src, p.envOff)
	if err != nil {
		return "", false
	}
	return p.envDoc.src[p.envOff:end], true
}

// ---- scanning ----
//
// One iterative validator does all the structural work. It is used twice: once
// over the whole document to reject malformed input, and once per container
// value to find where that value ends. Iterative rather than recursive so a
// deeply nested document costs heap rather than the Go stack.

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func skipSpace(src string, i int) int {
	for i < len(src) && isJSONSpace(src[i]) {
		i++
	}
	return i
}

var errJSONEOF = &jsonError{"Unexpected end of JSON input"}
var errJSONToken = &jsonError{"Unexpected token in JSON"}

// lazyScan validates the JSON value starting at i and returns the index just
// past it. i must already be at the value's first byte.
func lazyScan(src string, i int) (int, error) {
	// stack holds the open containers, '{' or '['. Its depth is the nesting
	// depth, and an empty stack after a completed value means we are done.
	var stack []byte

	for {
		// --- a value is expected at i ---
		if i >= len(src) {
			return 0, errJSONEOF
		}
		switch c := src[i]; {
		case c == '{':
			stack = append(stack, '{')
			i = skipSpace(src, i+1)
			if i >= len(src) {
				return 0, errJSONEOF
			}
			if src[i] == '}' {
				i++
				stack = stack[:len(stack)-1]
				goto closed
			}
			// A non-empty object expects "key" : value.
			var err error
			if i, err = lazyKey(src, i); err != nil {
				return 0, err
			}
			continue
		case c == '[':
			stack = append(stack, '[')
			i = skipSpace(src, i+1)
			if i >= len(src) {
				return 0, errJSONEOF
			}
			if src[i] == ']' {
				i++
				stack = stack[:len(stack)-1]
				goto closed
			}
			continue
		case c == '"':
			var err error
			if _, i, err = lazyString(src, i); err != nil {
				return 0, err
			}
		case c == 't':
			if !strings.HasPrefix(src[i:], "true") {
				return 0, errJSONToken
			}
			i += 4
		case c == 'f':
			if !strings.HasPrefix(src[i:], "false") {
				return 0, errJSONToken
			}
			i += 5
		case c == 'n':
			if !strings.HasPrefix(src[i:], "null") {
				return 0, errJSONToken
			}
			i += 4
		case c == '-' || (c >= '0' && c <= '9'):
			var err error
			if i, err = lazyNumber(src, i); err != nil {
				return 0, err
			}
		default:
			return 0, errJSONToken
		}

		// --- a value has just completed at i; close containers or advance ---
	closed:
		for {
			if len(stack) == 0 {
				return i, nil
			}
			i = skipSpace(src, i)
			if i >= len(src) {
				return 0, errJSONEOF
			}
			open := stack[len(stack)-1]
			c := src[i]
			if c == ',' {
				i = skipSpace(src, i+1)
				if open == '{' {
					var err error
					if i, err = lazyKey(src, i); err != nil {
						return 0, err
					}
				}
				break // back to expecting a value
			}
			if (open == '{' && c == '}') || (open == '[' && c == ']') {
				i++
				stack = stack[:len(stack)-1]
				continue // the container itself is now a completed value
			}
			if open == '{' {
				return 0, &jsonError{"Expected ',' or '}' in JSON object"}
			}
			return 0, &jsonError{"Expected ',' or ']' in JSON array"}
		}
	}
}

// lazyKey consumes a `"key" :` pair and returns the index of the value's first
// byte.
func lazyKey(src string, i int) (int, error) {
	if i >= len(src) || src[i] != '"' {
		return 0, &jsonError{"Expected string key in JSON object"}
	}
	_, i, err := lazyString(src, i)
	if err != nil {
		return 0, err
	}
	i = skipSpace(src, i)
	if i >= len(src) || src[i] != ':' {
		return 0, &jsonError{"Expected ':' in JSON object"}
	}
	return skipSpace(src, i+1), nil
}

// lazyString reads the JSON string beginning at the quote at i, returning its
// decoded value and the index just past the closing quote.
//
// The common case is a string with no escapes, which is returned as a view
// into src — the caller either interns an owned copy (a key) or hands it to
// newString, which copies. Only an escaped string is rebuilt.
func lazyString(src string, i int) (string, int, error) {
	i++ // opening quote
	start := i
	for i < len(src) {
		c := src[i]
		switch {
		case c == '"':
			return src[start:i], i + 1, nil
		case c == '\\':
			return lazyStringEscaped(src, start, i)
		case c < 0x20:
			return "", 0, &jsonError{"Bad control character in JSON string"}
		}
		i++
	}
	return "", 0, &jsonError{"Unterminated JSON string"}
}

// lazyStringEscaped finishes a string that turned out to contain an escape.
// start is the first byte after the opening quote and i is at the backslash.
func lazyStringEscaped(src string, start, i int) (string, int, error) {
	var b strings.Builder
	b.WriteString(src[start:i])
	for i < len(src) {
		c := src[i]
		switch {
		case c == '"':
			return b.String(), i + 1, nil
		case c < 0x20:
			return "", 0, &jsonError{"Bad control character in JSON string"}
		case c != '\\':
			b.WriteByte(c)
			i++
			continue
		}
		i++
		if i >= len(src) {
			return "", 0, &jsonError{"Unterminated JSON string"}
		}
		switch src[i] {
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case '/':
			b.WriteByte('/')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			if i+4 >= len(src) {
				return "", 0, &jsonError{"Invalid unicode escape"}
			}
			var cp uint32
			for k := 1; k <= 4; k++ {
				h := src[i+k]
				if !isXDigitByte(h) {
					return "", 0, &jsonError{"Invalid unicode escape in JSON string"}
				}
				cp = cp<<4 | hexVal(h)
			}
			// Written exactly as the eager parser writes it, surrogates and
			// all. Any divergence here would be a difference a script could
			// see between the two parses, which is the one thing laziness is
			// not allowed to cost.
			b.WriteString(string(rune(cp)))
			i += 4
		default:
			return "", 0, &jsonError{"Invalid escape in JSON string"}
		}
		i++
	}
	return "", 0, &jsonError{"Unterminated JSON string"}
}

// lazyNumber validates the JSON number at i and returns the index just past it.
func lazyNumber(src string, i int) (int, error) {
	bad := func() (int, error) { return 0, &jsonError{"Invalid number in JSON"} }
	digit := func() bool { return i < len(src) && src[i] >= '0' && src[i] <= '9' }
	if i < len(src) && src[i] == '-' {
		i++
	}
	// JSON forbids leading zeros: a single 0, or a nonzero digit run.
	if i < len(src) && src[i] == '0' {
		i++
	} else if digit() {
		for digit() {
			i++
		}
	} else {
		return bad()
	}
	if i < len(src) && src[i] == '.' {
		i++
		if !digit() {
			return bad()
		}
		for digit() {
			i++
		}
	}
	if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
		i++
		if i < len(src) && (src[i] == '+' || src[i] == '-') {
			i++
		}
		if !digit() {
			return bad()
		}
		for digit() {
			i++
		}
	}
	return i, nil
}
