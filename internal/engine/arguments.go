package engine

// The mapped arguments object (10.4.4). In a non-strict function with a simple
// parameter list, `arguments[i]` and the i-th formal parameter are two views of
// one binding: writing either is visible through the other.
//
// goant models the [[ParameterMap]] as a side table rather than the spec's inner
// object. The arguments object still carries an ordinary data property per
// index — so attributes, enumeration and the ordinary define/delete algorithms
// all work unchanged — and the map says which of those indices additionally read
// and write through to a frame local. The frame's locals slice is allocated once
// and never reallocated, so holding it here aliases the live bindings.
//
// An index leaves the map when the spec says the two stop being one binding:
// it is redefined as an accessor, made non-writable, or deleted.

// argumentsMap is the [[ParameterMap]] of one mapped arguments object.
type argumentsMap struct {
	// locals is the owning frame's locals slice. A simple parameter list binds
	// formal i to slot i, so index i maps to locals[i].
	locals []Value
	// n is the number of indices mapped at creation: min(parameter count,
	// argument count). Indices at or past it were never mapped.
	n int
	// gone marks the indices that have since left the map.
	gone []bool
}

// newArgumentsMap builds the map for a call with the given number of formals and
// arguments. It returns nil when nothing would be mapped.
func newArgumentsMap(locals []Value, paramCount, argCount int) *argumentsMap {
	n := paramCount
	if argCount < n {
		n = argCount
	}
	if n <= 0 {
		return nil
	}
	return &argumentsMap{locals: locals, n: n, gone: make([]bool, n)}
}

// index resolves a property key to a mapped parameter index, or -1.
func (m *argumentsMap) index(key string) int {
	if m == nil {
		return -1
	}
	idx, ok := canonicalIndex(key)
	if !ok || int(idx) >= m.n || m.gone[idx] {
		return -1
	}
	return int(idx)
}

func (m *argumentsMap) get(i int) Value    { return m.locals[i] }
func (m *argumentsMap) set(i int, v Value) { m.locals[i] = v }

// unmap removes a key from the map, if it is in it. The ordinary property keeps
// whatever value it holds, which is why callers that unmap on a value-less
// redefinition must first copy the mapped value across.
func (m *argumentsMap) unmap(key string) {
	if i := m.index(key); i >= 0 {
		m.gone[i] = true
	}
}
