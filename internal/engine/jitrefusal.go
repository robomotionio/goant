package engine

import "sort"

// What the tier is actually losing, weighted by how often the function runs.
//
// The static histogram — how many functions in a corpus contain an opcode the
// emitter has no template for — has now pointed at the wrong work twice. It said
// the numeric operators were not worth building and they were; it said PUT_FIELD
// was the largest single blocker in the corpus by a factor of seven, and
// clearing it moved static coverage from 460 functions to 497 and moved Octane
// by nothing at all. Both times the reason was the same: a program's time is not
// spread evenly over its functions, so counting functions counts the wrong
// thing.
//
// This counts frame entries instead. Every entry into a function the tier
// refused is charged to the reason it was refused for, so the output says how
// much of the running program each missing template would reach — which is the
// number the static one was standing in for.
//
// It is a diagnostic: everything here is behind GOANT_JIT_STATS, nothing is
// synchronised, and the two fields it needs on jitAttempt are never written
// otherwise. A Runtime per goroutine would undercount, which is the right
// trade for keeping a hash lookup off the interpreter's frame entry.

// jitRefusals is the reason table. Index 0 is "not refused", so a function's
// recorded reason can be zero-valued.
var jitRefusals = struct {
	names []string
	index map[string]uint32
	funcs []*svFunc
}{names: []string{""}, index: map[string]uint32{}}

// jitWhy returns the destination for a refusal reason, or nil when nobody is
// collecting them — which is what keeps the emitter from formatting a string on
// every refusal in an ordinary run.
func jitWhy(dst *string) *string {
	if !jitStats.enabled {
		return nil
	}
	return dst
}

// jitNoteRefusal records why fn will be interpreted from here on.
func jitNoteRefusal(fn *svFunc, why string) {
	if !jitStats.enabled {
		return
	}
	if why == "" {
		why = "unstated"
	}
	id, ok := jitRefusals.index[why]
	if !ok {
		id = uint32(len(jitRefusals.names))
		jitRefusals.names = append(jitRefusals.names, why)
		jitRefusals.index[why] = id
	}
	fn.jit.why = id
	jitRefusals.funcs = append(jitRefusals.funcs, fn)
}

// jitNoteEntry charges one interpreted frame entry to fn.
//
// Saturating rather than wrapping: a counter that wraps turns the hottest
// function in the program into the coldest, which is the one mistake this whole
// diagnostic exists to avoid.
func jitNoteEntry(fn *svFunc) {
	if fn.jit.entries != ^uint32(0) {
		fn.jit.entries++
	}
}

// JITRefusalWeights reports, per refusal reason, how many frame entries went to
// the interpreter because of it — heaviest first.
//
// A reason with a large count and few functions is a small amount of work that
// would reach a lot of running code, which is exactly what the static histogram
// cannot distinguish from a large amount of work that would reach none.
func JITRefusalWeights() []struct {
	Reason  string
	Entries uint64
	Funcs   int
} {
	type row = struct {
		Reason  string
		Entries uint64
		Funcs   int
	}
	byReason := map[string]*row{}
	for _, fn := range jitRefusals.funcs {
		name := jitRefusals.names[fn.jit.why]
		r := byReason[name]
		if r == nil {
			r = &row{Reason: name}
			byReason[name] = r
		}
		r.Entries += uint64(fn.jit.entries)
		r.Funcs++
	}
	out := make([]row, 0, len(byReason))
	for _, r := range byReason {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Entries != out[j].Entries {
			return out[i].Entries > out[j].Entries
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
