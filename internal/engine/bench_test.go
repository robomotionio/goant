package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The workloads in bench/ are shared with cmd/goant-bench so the two harnesses
// measure the same thing. There, each file calls the global bench(work, units)
// and the harness times it; here, bench() just hands `work` back so the Go
// benchmark can drive it b.N times with -cpuprofile and -benchmem attached.
//
// ns/op is therefore the cost of one whole workload call. The comparable
// figure — the one goant-bench prints for node, deno and bun — is reported
// alongside it as ns/unit.
const benchCapture = `globalThis.__work = null;
globalThis.bench = function (work, units) { globalThis.__work = work; globalThis.__units = units; };
`

func loadWorkload(tb testing.TB, name string) (*Runtime, Value, int) {
	tb.Helper()
	path := filepath.Join("..", "..", "bench", name+".js")
	src, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("workload %s unavailable: %v", name, err)
	}
	rt := New()
	if _, err := rt.RunString(name+".js", benchCapture+string(src)); err != nil {
		tb.Fatalf("%s: %v", name, err)
	}
	work, e := rt.getField(rt.global, "__work")
	if e != nil || work.IsUndefined() || work.IsNull() {
		tb.Fatalf("%s: workload did not call bench()", name)
	}
	unitsV, e := rt.getField(rt.global, "__units")
	if e != nil {
		tb.Fatalf("%s: %v", name, e)
	}
	n, e := rt.toNumber(unitsV)
	if e != nil {
		tb.Fatalf("%s: %v", name, e)
	}
	units := int(n)
	if units <= 0 {
		tb.Fatalf("%s: bad unit count %d", name, units)
	}
	return rt, work, units
}

func runWorkload(b *testing.B, name string) {
	rt, work, units := loadWorkload(b, name)
	// One warm-up call so lazily built state (shapes, interned names, the
	// prototype chains the workload touches) is not charged to the first
	// iteration.
	if _, e := rt.callValue(work, mkundef(), nil); e != nil {
		b.Fatalf("%s: %v", name, e)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e := rt.callValue(work, mkundef(), nil); e != nil {
			b.StopTimer()
			b.Fatalf("%s: %v", name, e)
		}
	}
	b.StopTimer()
	// One iteration is one CALL, so ns/op is the whole workload. Divide it down
	// to the unit the workload declared, which is what the cross-engine numbers
	// are in. Letting b.N count units instead would break down whenever a single
	// call already exceeds b.N, which for these sizes is most of them.
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(units), "ns/unit")
	b.ReportMetric(0, "ns/op") // suppressed: the per-call figure is not comparable
}

func BenchmarkArith(b *testing.B)       { runWorkload(b, "arith") }
func BenchmarkArrayIndex(b *testing.B)  { runWorkload(b, "array-index") }
func BenchmarkClosureCall(b *testing.B) { runWorkload(b, "closure-call") }
func BenchmarkFib(b *testing.B)         { runWorkload(b, "fib") }
func BenchmarkForOf(b *testing.B)       { runWorkload(b, "for-of") }
func BenchmarkMapSet(b *testing.B)      { runWorkload(b, "map-set") }
func BenchmarkMethodCall(b *testing.B)  { runWorkload(b, "method-call") }
func BenchmarkObjectAlloc(b *testing.B) { runWorkload(b, "object-alloc") }
func BenchmarkPropPoly(b *testing.B)    { runWorkload(b, "prop-poly") }
func BenchmarkPropRead(b *testing.B)    { runWorkload(b, "prop-read") }
func BenchmarkPropWrite(b *testing.B)   { runWorkload(b, "prop-write") }
func BenchmarkStringOps(b *testing.B)   { runWorkload(b, "string-ops") }

// The front end is measured separately: an embedder that evaluates a lot of
// short scripts pays parse and compile on every one of them, and that cost is
// invisible in the steady-state numbers above.
const frontEndSource = `
function Point(x, y) { this.x = x; this.y = y; }
Point.prototype.dot = function (o) { return this.x * o.x + this.y * o.y; };
class Shape {
  #sides = 0;
  constructor(n) { this.#sides = n; }
  get sides() { return this.#sides; }
  static of(n) { return new Shape(n); }
}
const xs = [1, 2, 3].map(v => v * 2).filter(v => v > 2);
let { a = 1, ...rest } = { a: 5, b: 6, c: 7 };
for (const [k, v] of Object.entries(rest)) { console.log(` + "`${k}=${v}`" + `); }
async function load(u) { try { return await fetch(u); } catch { return null; } }
`

func BenchmarkParse(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Parse("bench.js", frontEndSource); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompile(b *testing.B) {
	prog, err := Parse("bench.js", frontEndSource)
	if err != nil {
		b.Fatal(err)
	}
	rt := New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.Compile(prog, "bench.js", frontEndSource); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNewRuntime is the cost an embedder pays per isolate — the figure
// that decides whether a runtime can be created per request or has to be
// pooled.
func BenchmarkNewRuntime(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = New()
	}
}

// TestWorkloadsRun keeps the suite honest: `go test` fails if a workload stops
// compiling or stops calling bench(), rather than the benchmark silently
// skipping months later.
func TestWorkloadsRun(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "bench"))
	if err != nil {
		t.Skipf("bench directory unavailable: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".js") || strings.HasPrefix(name, "_") {
			continue
		}
		name = strings.TrimSuffix(name, ".js")
		t.Run(name, func(t *testing.T) {
			rt, work, _ := loadWorkload(t, name)
			if _, e := rt.callValue(work, mkundef(), nil); e != nil {
				t.Fatalf("%v", e)
			}
		})
	}
}
