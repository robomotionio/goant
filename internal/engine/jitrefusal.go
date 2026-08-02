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

// jitNoteRefusal records why fn will be interpreted from here on, and whether
// that reason is the only thing in its way.
//
// The second part is what makes the output actionable. A first-blocker count
// answers "what does the emitter trip over first", which is not the same
// question: GET_FIELD2 is what `o.m()` compiles to and CALL is always the
// instruction after it, so a template for GET_FIELD2 alone would move the
// refusal one opcode along and unblock nothing. A sole-blocker count says how
// much running code one template would actually reach.
func jitNoteRefusal(fn *svFunc, why string) {
	if !jitStats.enabled {
		return
	}
	if why == "" {
		why = "unstated"
	}
	id := jitReasonID(why)
	fn.jit.why = id

	// Sole when the emitter's complaint is the whole story: either the one
	// opcode it has no template for, or — with no template missing at all — the
	// structural reason it gave. Anything else needs at least two pieces of work
	// and is charged to neither.
	missing := jitMissingTemplates(fn)
	switch {
	case len(missing) == 0:
		fn.jit.sole = id
	case len(missing) == 1 && why == "op:"+missing[0]:
		fn.jit.sole = id
	}
	jitRefusals.funcs = append(jitRefusals.funcs, fn)
}

func jitReasonID(why string) uint32 {
	if id, ok := jitRefusals.index[why]; ok {
		return id
	}
	id := uint32(len(jitRefusals.names))
	jitRefusals.names = append(jitRefusals.names, why)
	jitRefusals.index[why] = id
	return id
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

// jitNoteInstruction charges one interpreted bytecode instruction to fn.
//
// Frame entries were the wrong denominator, and it took a profile of compiled
// code to see it: DeltaBlue runs 72% of its frame *entries* as machine code and
// that code is 11% of its CPU time, because the functions the tier still refuses
// are the ones with the long bodies. A count of entries says a function that is
// called once and runs for a second costs nothing.
//
// One branch and one increment on the interpreter's instruction dispatch, and
// only when GOANT_JIT_STATS is set. It slows a diagnostic run and leaves the
// proportions it is measuring intact, which is the whole job.
func jitNoteInstruction(fn *svFunc) {
	fn.jit.insns++
}

// JITRefusalWeight is one refusal reason and what it costs.
//
// Insns is the one to read: how many bytecode instructions the interpreter
// executed inside functions refused for this reason. Entries counts frame
// entries, which flatters a function called often over one that runs long, and
// Unblocks is how much of Entries this reason alone would release.
type JITRefusalWeight struct {
	Reason   string
	Entries  uint64
	Unblocks uint64
	Insns    uint64
	Funcs    int
}

// JITRefusalWeights reports the refusal reasons by what they would unblock,
// heaviest first.
func JITRefusalWeights() []JITRefusalWeight {
	byReason := map[string]*JITRefusalWeight{}
	at := func(id uint32) *JITRefusalWeight {
		name := jitRefusals.names[id]
		r := byReason[name]
		if r == nil {
			r = &JITRefusalWeight{Reason: name}
			byReason[name] = r
		}
		return r
	}
	for _, fn := range jitRefusals.funcs {
		r := at(fn.jit.why)
		r.Entries += uint64(fn.jit.entries)
		r.Insns += fn.jit.insns
		r.Funcs++
		if fn.jit.sole != 0 {
			at(fn.jit.sole).Unblocks += uint64(fn.jit.entries)
		}
	}
	out := make([]JITRefusalWeight, 0, len(byReason))
	for _, r := range byReason {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Insns != out[j].Insns {
			return out[i].Insns > out[j].Insns
		}
		if out[i].Unblocks != out[j].Unblocks {
			return out[i].Unblocks > out[j].Unblocks
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
