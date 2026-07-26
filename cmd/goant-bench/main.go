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
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
var candidates = []engine{
	{name: "goant", bin: "./goant"},
	{name: "node", bin: "node"},
	{name: "deno", bin: "deno", args: []string{"run", "--quiet", "--allow-read"}},
	{name: "bun", bin: "bun", args: []string{"run"}},
}

func main() {
	dir := flag.String("bench", "bench", "directory holding the workload scripts")
	runner := flag.String("runner", "./goant", "path to the goant binary")
	reps := flag.Int("n", 3, "runs per workload; the fastest is kept")
	only := flag.String("only", "", "run only workloads whose name contains this")
	flag.Parse()

	candidates[0].bin = *runner

	engines := available()
	if len(engines) == 0 {
		fmt.Fprintln(os.Stderr, "no JavaScript engine found")
		os.Exit(1)
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
		joined := filepath.Join(tmp, name+".js")
		if err := os.WriteFile(joined, append(append([]byte{}, preludeSrc...), src...), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		fmt.Printf("%-14s", name)
		times := map[string]float64{}
		for _, e := range engines {
			ns, err := timeRun(e, joined, *reps)
			if err != nil {
				fmt.Printf("  %12s", "error")
				continue
			}
			times[e.name] = ns
			fmt.Printf("  %12s", fmtNs(ns))
		}
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

// timeRun runs script under e reps times and returns the fastest nanoseconds
// per unit of work, which the script itself reports on its last line of stdout.
func timeRun(e engine, script string, reps int) (float64, error) {
	best := math.Inf(1)
	for i := 0; i < reps; i++ {
		args := append(append([]string{}, e.args...), script)
		cmd := exec.Command(e.bin, args...)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
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
