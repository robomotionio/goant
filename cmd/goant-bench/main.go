// goant-bench times the bench/ workloads under goant and under whichever other
// JavaScript engines are installed, and prints how far apart they are.
//
// The workloads time THEMSELVES (see bench/_prelude.js) and print nanoseconds
// per unit of work. Timing them from out here would work for goant, which
// starts in about two milliseconds, but node, deno and bun take tens of
// milliseconds to start — more than a JITted workload takes to run, so startup
// noise would be the measurement. Each script is run several times and the
// fastest result kept, since the fastest observation is the one least polluted
// by whatever else the machine was doing.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// engine is one JavaScript implementation to measure. args are whatever has to
// precede the script path.
type engine struct {
	name string
	bin  string
	args []string
}

// candidates lists the engines worth comparing against, in the order they are
// reported. One that is not installed is skipped rather than failing the run.
// goja is the comparison that matters most, being the other pure-Go engine and
// so the one measured under the same constraints. It is reached through a
// separate `goja-run` binary rather than linked in, which keeps goja out of
// this module's dependencies — see bench/README.md for how to build it.
var candidates = []engine{
	{name: "goant", bin: "./goant"},
	// The four interpreters worth standing beside. goja is the other pure-Go
	// engine; ant is the C engine goant is a port of, so it is the answer to
	// "what did the port cost"; quickjs is the one most embedders actually
	// reach for; duktape is the reference embeddable interpreter. None of the
	// four has a JIT, which is what makes them the honest comparison — node,
	// deno and bun are in the table to show the distance to one.
	{name: "goja", bin: "goja-run"},
	{name: "ant", bin: "ant"},
	{name: "quickjs", bin: "qjs"},
	{name: "duktape", bin: "duk"},
	{name: "node", bin: "node"},
	{name: "deno", bin: "deno", args: []string{"run", "--quiet", "--allow-read"}},
	{name: "bun", bin: "bun", args: []string{"run"}},
}

// octaneBenchmarks lists Octane 2.0's suites and the files each one needs,
// concatenated in order ahead of the driver. Octane's own runner needs a shell
// `load()` builtin that none of these four engines has, so the pieces are
// joined into one script instead.
//
// They are ordered smallest-first: goant is currently far slower than a JIT, so
// the later entries take minutes rather than seconds. Use -only to pick.
var octaneBenchmarks = []struct {
	name  string
	files []string
}{
	{"richards", []string{"richards.js"}},
	{"deltablue", []string{"deltablue.js"}},
	{"crypto", []string{"crypto.js"}},
	{"raytrace", []string{"raytrace.js"}},
	{"navier-stokes", []string{"navier-stokes.js"}},
	{"splay", []string{"splay.js"}},
	{"regexp", []string{"regexp.js"}},
	{"earley-boyer", []string{"earley-boyer.js"}},
	{"code-load", []string{"code-load.js"}},
	{"box2d", []string{"box2d.js"}},
	{"gbemu", []string{"gbemu-part1.js", "gbemu-part2.js"}},
	{"zlib", []string{"zlib.js", "zlib-data.js"}},
	{"pdfjs", []string{"pdfjs.js"}},
	{"mandreel", []string{"mandreel.js"}},
	{"typescript", []string{"typescript.js", "typescript-input.js", "typescript-compiler.js"}},
}

func main() {
	dir := flag.String("bench", "bench", "directory holding the workload scripts")
	suite := flag.String("suite", "micro", "which suite to run: micro (bench/*.js) or octane")
	runner := flag.String("runner", "./goant", "path to the goant binary")
	reps := flag.Int("n", 3, "runs per workload; the fastest is kept")
	only := flag.String("only", "", "run only workloads whose name contains this")
	limit := flag.Duration("timeout", 10*time.Minute, "give up on one engine's run after this long")
	which := flag.String("engines", "", "run only these engines (comma-separated); default is every one installed")
	refresh := flag.Bool("refresh", false, "re-measure the comparison engines instead of reading "+baselineFile)
	flag.Parse()

	runLimit = *limit

	candidates[0].bin = *runner
	// goant's own results are cached too, under the binary's hash: a restart
	// after a dropped connection picks up where it left off, and a rebuild
	// invalidates the column rather than reusing it.
	goantBuild = hashBinary(*runner)

	engines := pick(available(), *which)
	if len(engines) == 0 {
		fmt.Fprintln(os.Stderr, "no JavaScript engine found")
		os.Exit(1)
	}
	bl := loadBaseline(*dir)

	if *suite == "octane" {
		runOctane(engines, *dir, *only, *reps, bl, *refresh)
		return
	}

	prelude := filepath.Join(*dir, "_prelude.js")
	preludeSrc, err := os.ReadFile(prelude)
	if err != nil {
		fmt.Fprintf(os.Stderr, "missing harness %s: %v\n", prelude, err)
		os.Exit(1)
	}

	works, err := workloads(*dir, *only)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(works) == 0 {
		fmt.Fprintln(os.Stderr, "no workloads matched")
		os.Exit(1)
	}

	tmp, err := os.MkdirTemp("", "goant-bench")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	printHeader(engines)

	var ratios []float64
	for _, w := range works {
		name := strings.TrimSuffix(filepath.Base(w), ".js")
		src, err := os.ReadFile(w)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		// The harness and the workload are concatenated into one script rather
		// than imported, so this works identically under an engine that treats a
		// bare .js file as a module and one that treats it as a script.
		script := append(append([]byte{}, preludeSrc...), src...)
		joined := filepath.Join(tmp, name+".js")
		if err := os.WriteFile(joined, script, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		c := cell{suite: "micro", workload: name, script: hashBytes(script)}

		fmt.Printf("%-14s", name)
		times := map[string]float64{}
		for _, e := range engines {
			ns, err := cachedRun(bl, *refresh, e, c, func() (float64, error) {
				return timeRun(e, joined, *reps)
			})
			if err != nil {
				fmt.Printf("  %12s", "error")
				continue
			}
			times[e.name] = ns
			fmt.Printf("  %12s", fmtNs(ns))
		}
		bl.save()
		if base, ok := fastestOther(engines, times); ok && times["goant"] > 0 && base > 0 {
			r := times["goant"] / base
			ratios = append(ratios, r)
			fmt.Printf("   %7.0fx", r)
		}
		fmt.Println()
	}

	if len(ratios) > 0 {
		sort.Float64s(ratios)
		fmt.Printf("\ngoant vs the fastest other engine: %.0fx median, %.0fx best, %.0fx worst (%d workloads)\n",
			median(ratios), ratios[0], ratios[len(ratios)-1], len(ratios))
	}
	bl.note()
}

// available returns the candidate engines that are actually runnable here.
func available() []engine {
	var out []engine
	for _, e := range candidates {
		if strings.ContainsRune(e.bin, filepath.Separator) {
			if _, err := os.Stat(e.bin); err != nil {
				continue
			}
		} else if _, err := exec.LookPath(e.bin); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

// workloads lists the scripts to run, excluding the ones whose name starts with
// '_' (the startup baseline and any other harness file).
func workloads(dir, only string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".js") || strings.HasPrefix(n, "_") {
			continue
		}
		if only != "" && !strings.Contains(n, only) {
			continue
		}
		out = append(out, filepath.Join(dir, n))
	}
	sort.Strings(out)
	return out, nil
}

// runLimit is how long one engine gets for one run before it is given up on.
// Without it a single engine that wedges — duktape and ant have both sat on an
// Octane workload for a quarter of an hour — holds up the whole table, and a
// table that never finishes says less than one with a cell marked error.
var runLimit = 10 * time.Minute

// runEngine runs one script under one engine and returns its stdout.
func runEngine(e engine, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	args := append(append([]string{}, e.args...), script)
	cmd := exec.CommandContext(ctx, e.bin, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: gave up after %s", e.name, runLimit)
	}
	return out, err
}

// timeRun runs script under e reps times and returns the fastest nanoseconds
// per unit of work, which the script itself reports on its last line of stdout.
func timeRun(e engine, script string, reps int) (float64, error) {
	best := math.Inf(1)
	for i := 0; i < reps; i++ {
		out, err := runEngine(e, script)
		if err != nil {
			return 0, err
		}
		lines := strings.Fields(strings.TrimSpace(string(out)))
		if len(lines) == 0 {
			return 0, fmt.Errorf("%s: no timing on stdout", e.name)
		}
		ns, err := strconv.ParseFloat(lines[len(lines)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("%s: unparsable timing %q", e.name, lines[len(lines)-1])
		}
		if ns < best {
			best = ns
		}
	}
	return best, nil
}

// fastestOther returns the best time among engines other than goant.
func fastestOther(engines []engine, times map[string]float64) (float64, bool) {
	best, ok := 0.0, false
	for _, e := range engines {
		if e.name == "goant" {
			continue
		}
		d, has := times[e.name]
		if !has || d <= 0 {
			continue
		}
		if !ok || d < best {
			best, ok = d, true
		}
	}
	return best, ok
}

func printHeader(engines []engine) {
	fmt.Printf("%-14s", "workload")
	for _, e := range engines {
		fmt.Printf("  %12s", e.name)
	}
	fmt.Printf("   %8s\n", "vs best")
	fmt.Println(strings.Repeat("-", 14+14*len(engines)+11))
}

// fmtNs renders nanoseconds per unit of work.
func fmtNs(ns float64) string {
	switch {
	case ns >= 1000:
		return fmt.Sprintf("%.1fµs", ns/1000)
	case ns >= 10:
		return fmt.Sprintf("%.0fns", ns)
	default:
		return fmt.Sprintf("%.2fns", ns)
	}
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// runOctane scores each Octane benchmark under every engine. Octane reports a
// SCORE, where higher is faster — the opposite direction from the
// microbenchmarks, so the comparison column is the other engine over goant.
func runOctane(engines []engine, benchDir, only string, reps int, bl *baseline, refresh bool) {
	src := filepath.Join(benchDir, "suites", "octane")
	if _, err := os.Stat(filepath.Join(src, "base.js")); err != nil {
		fmt.Fprintf(os.Stderr, "octane not fetched: run %s\n", filepath.Join(benchDir, "suites", "fetch.sh"))
		os.Exit(1)
	}
	base, err := os.ReadFile(filepath.Join(src, "base.js"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	driver, err := os.ReadFile(filepath.Join(benchDir, "suites", "_octane-driver.js"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// The shell globals Octane expects and not every engine has — see the file.
	shell, err := os.ReadFile(filepath.Join(benchDir, "suites", "_octane-shell.js"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	tmp, err := os.MkdirTemp("", "goant-octane")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	printHeader(engines)

	var ratios []float64
	for _, b := range octaneBenchmarks {
		if only != "" && !strings.Contains(b.name, only) {
			continue
		}
		// .cjs, not .js: Octane is 2013-era sloppy-mode code and crypto.js
		// assigns an implicit global (`setupEngine = function …`), which is a
		// ReferenceError under strict mode. deno and bun treat a .js file as a
		// module and so run it strict, node does not — which would score the
		// choice of goal rather than the engine. The extension pins every
		// engine to the script goal Octane was written for.
		joined := filepath.Join(tmp, b.name+".cjs")
		var buf []byte
		buf = append(buf, shell...)
		buf = append(buf, base...)
		for _, f := range b.files {
			part, err := os.ReadFile(filepath.Join(src, f))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			buf = append(buf, '\n')
			buf = append(buf, part...)
		}
		buf = append(buf, '\n')
		buf = append(buf, driver...)
		if err := os.WriteFile(joined, buf, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		c := cell{suite: "octane", workload: b.name, script: hashBytes(buf)}

		fmt.Printf("%-14s", b.name)
		scores := map[string]float64{}
		for _, e := range engines {
			// Octane already repeats internally to reach a stable score, so one run
			// per engine is enough; -n still raises it for a noisy machine.
			s, err := cachedRun(bl, refresh, e, c, func() (float64, error) {
				return bestScore(e, joined, reps)
			})
			if err != nil {
				fmt.Printf("  %12s", "error")
				continue
			}
			scores[e.name] = s
			fmt.Printf("  %12.0f", s)
		}
		bl.save()
		if best, ok := bestOtherScore(engines, scores); ok && scores["goant"] > 0 && best > 0 {
			r := best / scores["goant"]
			ratios = append(ratios, r)
			fmt.Printf("   %7.0fx", r)
		}
		fmt.Println()
	}

	if len(ratios) > 0 {
		sort.Float64s(ratios)
		fmt.Printf("\ngoant vs the fastest other engine: %.0fx median, %.0fx best, %.0fx worst (%d benchmarks)\n",
			median(ratios), ratios[0], ratios[len(ratios)-1], len(ratios))
	}
	bl.note()
}

// bestScore runs an Octane benchmark reps times and keeps the highest score.
func bestScore(e engine, script string, reps int) (float64, error) {
	best := 0.0
	for i := 0; i < reps; i++ {
		out, err := runEngine(e, script)
		if err != nil {
			return 0, err
		}
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) == 0 {
			return 0, fmt.Errorf("%s: no score on stdout", e.name)
		}
		s, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("%s: unparsable score %q", e.name, fields[len(fields)-1])
		}
		if s > best {
			best = s
		}
	}
	return best, nil
}

// bestOtherScore returns the highest score among engines other than goant.
func bestOtherScore(engines []engine, scores map[string]float64) (float64, bool) {
	best, ok := 0.0, false
	for _, e := range engines {
		if e.name == "goant" {
			continue
		}
		s, has := scores[e.name]
		if !has || s <= 0 {
			continue
		}
		if s > best {
			best, ok = s, true
		}
	}
	return best, ok
}
