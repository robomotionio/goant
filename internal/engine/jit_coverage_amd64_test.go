//go:build amd64

package engine

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

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
