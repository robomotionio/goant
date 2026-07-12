// Command goant-conf is the conformance harness. It mirrors ant's
// tests/harness/harness.js contract: spawn the runner binary once per test
// file; a test PASSES iff the process exits 0 within the timeout.
//
// It drives the corpus under conformance/ (es1/, es3/, es5/, compat-table/…)
// against a runner (the goant binary, or `node` as an oracle), reports a
// per-category X/Y table, and can diff the passing set against
// conformance/ant-results.txt (the 1511-name spec).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type result struct {
	name   string // e.g. "es1/Array.js"
	ok     bool
	reason string
}

var timeoutOverrideRe = regexp.MustCompile(`//\s*goant-timeout:\s*(\d+)`)

func main() {
	var (
		runner   = flag.String("runner", "./goant", "path to runner binary, or 'node'")
		profile  = flag.String("profile", "interp", "interp|jit|all")
		corpus   = flag.String("corpus", "conformance", "corpus root directory")
		only     = flag.String("only", "", "run only tests whose name contains this substring")
		category = flag.String("category", "", "restrict to a top-level category (es1,es3,es5,compat-table)")
		timeout  = flag.Duration("timeout", 10*time.Second, "per-test timeout")
		jobs     = flag.Int("j", 0, "worker count (0 = NumCPU)")
		results  = flag.String("results", "", "write results file (ant section order)")
		diffPath = flag.String("diff", "", "diff passing set against this results.txt")
		allow    = flag.String("allow", "", "allowlist file of expected-fail names")
		verbose  = flag.Bool("v", false, "print each failure")
	)
	flag.Parse()

	tests, err := discover(*corpus, *category, *only)
	if err != nil {
		fatal(err)
	}
	if len(tests) == 0 {
		fatal(fmt.Errorf("no tests found under %s", *corpus))
	}

	profiles := []string{*profile}
	if *profile == "all" {
		profiles = []string{"interp", "jit"}
	}

	if *jobs <= 0 {
		*jobs = min(16, max(1, runtime.NumCPU()))
	}

	// Run each profile; collect per-profile pass sets for cross-profile check.
	passByProfile := map[string]map[string]bool{}
	var allResults []result
	for _, prof := range profiles {
		res := runAll(*runner, prof, tests, *timeout, *jobs, *verbose)
		pass := map[string]bool{}
		for _, r := range res {
			if r.ok {
				pass[r.name] = true
			}
		}
		passByProfile[prof] = pass
		if prof == profiles[0] {
			allResults = res
		}
	}

	// Cross-profile differential gate (--profile all).
	crossFail := 0
	if len(profiles) > 1 {
		crossFail = crossProfileCheck(passByProfile, tests)
	}

	allowed := loadAllow(*allow)
	printSummary(allResults, tests)

	if *results != "" {
		if err := writeResults(*results, allResults); err != nil {
			fatal(err)
		}
	}

	exit := 0
	if *diffPath != "" {
		miss, extra := diffResults(allResults, *diffPath, allowed)
		if len(miss) > 0 || len(extra) > 0 {
			exit = 1
			fmt.Printf("\nDIFF vs %s: %d missing, %d unexpected\n", *diffPath, len(miss), len(extra))
			for _, n := range miss {
				fmt.Printf("  MISSING (expected OK, got fail): %s\n", n)
			}
			for _, n := range extra {
				fmt.Printf("  UNEXPECTED (passed, not in spec): %s\n", n)
			}
		} else {
			fmt.Printf("\nDIFF vs %s: clean (%d/%d)\n", *diffPath, countPass(allResults), len(tests))
		}
	}
	if crossFail > 0 {
		exit = 1
		fmt.Printf("\nCROSS-PROFILE MISMATCH: %d tests differ between interp and jit\n", crossFail)
	}
	os.Exit(exit)
}

// discover walks the corpus for *.js test files, returning names relative to
// the corpus root (the ant-results.txt naming convention).
func discover(corpus, category, only string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(corpus, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".js") {
			return nil
		}
		rel, err := filepath.Rel(corpus, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if category != "" && !strings.HasPrefix(rel, category+"/") {
			return nil
		}
		if only != "" && !strings.Contains(rel, only) {
			return nil
		}
		names = append(names, rel)
		return nil
	})
	sort.Strings(names)
	return names, err
}

func runAll(runner, profile string, tests []string, timeout time.Duration, jobs int, verbose bool) []result {
	corpus := "conformance"
	work := make(chan string)
	out := make(chan result, len(tests))
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range work {
				out <- runOne(runner, profile, corpus, name, timeout)
			}
		}()
	}
	go func() {
		for _, t := range tests {
			work <- t
		}
		close(work)
	}()
	go func() { wg.Wait(); close(out) }()

	res := make([]result, 0, len(tests))
	for r := range out {
		res = append(res, r)
		if verbose && !r.ok {
			fmt.Printf("  FAIL [%s] %s: %s\n", profile, r.name, r.reason)
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].name < res[j].name })
	return res
}

func runOne(runner, profile, corpus, name string, timeout time.Duration) result {
	path := filepath.Join(corpus, name)
	src, _ := os.ReadFile(path)
	if m := timeoutOverrideRe.FindSubmatch(src); m != nil {
		if ms := atoiSafe(string(m[1])); ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runner == "node" {
		cmd = exec.CommandContext(ctx, "node", path)
	} else {
		cmd = exec.CommandContext(ctx, runner, path)
	}
	// Deterministic, timezone-independent environment (Date conformance).
	cmd.Env = append(os.Environ(), "TZ=UTC")
	if profile == "jit" && runner != "node" {
		cmd.Env = append(cmd.Env, "GOANT_JIT=always")
	}
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return result{name, false, "timeout"}
	}
	if err != nil {
		return result{name, false, err.Error()}
	}
	return result{name, true, ""}
}

func crossProfileCheck(passByProfile map[string]map[string]bool, tests []string) int {
	n := 0
	for _, t := range tests {
		var first, seen bool
		for _, pass := range passByProfile {
			if !seen {
				first, seen = pass[t], true
			} else if pass[t] != first {
				n++
				break
			}
		}
	}
	return n
}

func printSummary(res []result, tests []string) {
	type cat struct{ pass, total int }
	cats := map[string]*cat{}
	var order []string
	for _, r := range res {
		c := category(r.name)
		if cats[c] == nil {
			cats[c] = &cat{}
			order = append(order, c)
		}
		cats[c].total++
		if r.ok {
			cats[c].pass++
		}
	}
	sort.Strings(order)
	fmt.Println("category                       pass/total")
	fmt.Println("----------------------------------------")
	total, pass := 0, 0
	for _, c := range order {
		fmt.Printf("%-30s %d/%d\n", c, cats[c].pass, cats[c].total)
		total += cats[c].total
		pass += cats[c].pass
	}
	fmt.Println("----------------------------------------")
	fmt.Printf("%-30s %d/%d\n", "TOTAL", pass, total)
}

// category returns the reporting bucket for a test name, splitting
// compat-table into its per-edition subdirs to mirror the plan's breakdown.
func category(name string) string {
	parts := strings.Split(name, "/")
	if parts[0] == "compat-table" && len(parts) > 2 {
		return "compat-table/" + parts[1]
	}
	return parts[0]
}

func countPass(res []result) int {
	n := 0
	for _, r := range res {
		if r.ok {
			n++
		}
	}
	return n
}

func writeResults(path string, res []result) error {
	var b strings.Builder
	for _, r := range res {
		status := "FAIL"
		if r.ok {
			status = "OK"
		}
		fmt.Fprintf(&b, "%s: %s\n", r.name, status)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// diffResults compares the passing set against a spec file of "name: OK" lines.
func diffResults(res []result, specPath string, allowed map[string]bool) (missing, extra []string) {
	spec, err := os.ReadFile(specPath)
	if err != nil {
		fatal(err)
	}
	want := map[string]bool{}
	for line := range strings.SplitSeq(string(spec), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := strings.SplitN(line, ":", 2)[0]
		want[name] = true
	}
	got := map[string]bool{}
	for _, r := range res {
		if r.ok {
			got[r.name] = true
		}
	}
	for name := range want {
		if !got[name] && !allowed[name] {
			missing = append(missing, name)
		}
	}
	for name := range got {
		if !want[name] && !allowed[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return
}

func loadAllow(path string) map[string]bool {
	m := map[string]bool{}
	if path == "" {
		return m
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m[line] = true
	}
	return m
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "goant-conf:", err)
	os.Exit(2)
}
