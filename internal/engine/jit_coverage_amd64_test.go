//go:build amd64

package engine

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// jitSupported is what the emitter has a template for. Kept here rather than
// derived from the emitter so that a diagnostic cannot silently agree with a
// bug in the thing it is measuring.
var jitSupported = map[Opcode]bool{
	OpThis: true, OpPutLocal: true, OpSetLocal: true, OpGetLocal: true,
	OpConst: true, OpConstI8: true, OpPop: true,
	OpAdd: true, OpSub: true, OpMul: true, OpDiv: true,
	OpLt: true, OpLe: true, OpGt: true, OpGe: true,
	OpJmp: true, OpJmpFalse: true, OpJmpTrue: true,
	OpReturn: true, OpReturnUndef: true,
}

// jitUnsupportedOps lists the distinct opcodes in fn that the tier has no
// template for, in no particular order.
func jitUnsupportedOps(fn *svFunc) []string {
	seen := map[string]bool{}
	code := fn.code
	for ip := fn.startIP; ip < len(code); {
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		if size <= 0 || ip+size > len(code) {
			return []string{"undecodable"}
		}
		if !jitSupported[op] {
			seen[opTable[op].Name] = true
		}
		ip += size
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	return out
}

// walkFuncs visits fn and every function nested in it.
func walkFuncs(fn *svFunc, visit func(*svFunc)) {
	visit(fn)
	for _, c := range fn.childFuncs {
		walkFuncs(c, visit)
	}
}

// TestJITCoverage reports how much of a real corpus this tier compiles, and what
// stops it from compiling the rest.
//
// Not an assertion — there is no correct number, and pinning one would only
// break every time the corpus or the tier moved. It exists because the tier has
// a dozen reasons to refuse and no way to tell which of them actually fire:
// guessing at the next opcode to implement is how effort goes into the wrong
// one. Run it with -v.
func TestJITCoverage(t *testing.T) {
	dir := filepath.Join("..", "..", "bench", "suites", "octane")
	files, err := filepath.Glob(filepath.Join(dir, "*.js"))
	if err != nil || len(files) == 0 {
		t.Skip("octane corpus not present; bench/suites/fetch.sh fetches it")
	}

	reasons := map[string]int{}
	var total, compiled int

	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rt := New()
		prog, err := Parse(f, string(src))
		if err != nil {
			continue // not every file in the corpus is a benchmark
		}
		top, err := rt.Compile(prog, f, string(src))
		if err != nil {
			continue
		}
		walkFuncs(top, func(fn *svFunc) {
			total++
			var why string
			if c := jitCompile(fn, &why); c != nil {
				compiled++
				c.free()
				return
			}
			reasons[why]++
		})
	}

	// The histogram above names the FIRST thing that stopped each function,
	// which flatters whatever the emitter happens to check earliest: a function
	// blocked by five different opcodes is charged entirely to one of them. This
	// second pass asks the more useful question — which functions are one
	// feature away — by collecting every unsupported opcode in a body and
	// counting only the bodies with exactly one.
	sole := map[string]int{}
	multi, clean := 0, 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rt := New()
		prog, err := Parse(f, string(src))
		if err != nil {
			continue
		}
		top, err := rt.Compile(prog, f, string(src))
		if err != nil {
			continue
		}
		walkFuncs(top, func(fn *svFunc) {
			var why string
			if c := jitCompile(fn, &why); c != nil {
				c.free()
				return
			}
			b := jitUnsupportedOps(fn)
			switch len(b) {
			case 0:
				clean++ // refused for a reason that is not an opcode
			case 1:
				sole[b[0]]++
			default:
				multi++
			}
		})
	}
	soleTotal := 0
	for _, n := range sole {
		soleTotal += n
	}
	t.Logf("of the refused: %d need one more opcode (%d distinct), %d need several, %d are blocked by something other than an opcode",
		soleTotal, len(sole), multi, clean)
	solerows := make([]struct {
		reason string
		n      int
	}, 0, len(sole))
	for r, n := range sole {
		solerows = append(solerows, struct {
			reason string
			n      int
		}{r, n})
	}
	sort.Slice(solerows, func(i, j int) bool { return solerows[i].n > solerows[j].n })
	for i, r := range solerows {
		if i >= 12 {
			break
		}
		t.Logf("  only %-22s would unblock %d", r.reason, r.n)
	}

	type row struct {
		reason string
		n      int
	}
	rows := make([]row, 0, len(reasons))
	for r, n := range reasons {
		rows = append(rows, row{r, n})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].n > rows[j].n })

	t.Logf("compiled %d of %d functions (%.1f%%)", compiled, total, 100*float64(compiled)/float64(total))
	for i, r := range rows {
		if i >= 20 {
			t.Logf("  ... and %d more reasons", len(rows)-i)
			break
		}
		t.Logf("  %-28s %d", r.reason, r.n)
	}
}
