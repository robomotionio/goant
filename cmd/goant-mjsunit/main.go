// Command goant-mjsunit runs V8's mjsunit suite (v8/test/mjsunit) against the
// goant interpreter. Test262 checks goant against the spec clause by clause;
// mjsunit checks it against fifteen years of real bug reports and the usage
// patterns that produced them, so the two find different things. It is pure
// JavaScript — no npm, no node builtins — which is why it is usable here at all.
//
// A test is assembled the way d8 runs one: the shell shim below, then the
// mjsunit.js assertion harness, then anything the test names in a `// Files:`
// header or loads with d8.file.execute, then the test itself. mjsunit reports
// failure by throwing, so the pass criterion is d8's: exit 0 with nothing on
// stderr.
//
// Roughly half the suite is not runnable by any engine but V8:
//
//   - `%Natives()` syntax is a parse error everywhere else
//   - wasm/, sandbox/, d8/ and shared-memory/ test the shell, not the language
//   - WebAssembly / Worker / Realm need host facilities goant does not provide
//
// Those are reported SKIP, not FAIL, so the score reflects the JavaScript goant
// is actually being asked to run.
package main

import (
	"context"
	"errors"
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

func main() {
	var (
		root     = flag.String("mjsunit", "../v8/test/mjsunit", "path to a v8 test/mjsunit checkout")
		runner   = flag.String("runner", "./goant", "path to the goant runner binary")
		only     = flag.String("only", "", "run only tests whose path contains this substring")
		dir      = flag.String("dir", "", "restrict to this subdirectory under test/mjsunit (e.g. es6)")
		jobs     = flag.Int("j", 0, "worker count (0 = min(16,NumCPU))")
		timeout  = flag.Duration("timeout", 10*time.Second, "per-execution timeout")
		maxN     = flag.Int("max", 0, "cap number of test files (0 = all)")
		verbose  = flag.Bool("v", false, "print every failure with a one-line reason")
		showSkip = flag.Bool("show-skip", false, "also list skipped tests")
		failFile = flag.String("failures", "", "write the sorted list of failing test paths here")
	)
	flag.Parse()

	testDir := *root
	if *dir != "" {
		testDir = filepath.Join(testDir, *dir)
	}
	if _, err := os.Stat(testDir); err != nil {
		fatal(fmt.Errorf("mjsunit dir %s: %w", testDir, err))
	}
	harness, err := os.ReadFile(filepath.Join(*root, "mjsunit.js"))
	if err != nil {
		fatal(fmt.Errorf("mjsunit.js: %w", err))
	}

	tests, err := discover(testDir, *only)
	if err != nil {
		fatal(err)
	}
	if *maxN > 0 && len(tests) > *maxN {
		tests = tests[:*maxN]
	}
	if len(tests) == 0 {
		fatal(fmt.Errorf("no tests found under %s", testDir))
	}
	if *jobs <= 0 {
		*jobs = min(16, max(1, runtime.NumCPU()))
	}

	results := runAll(*runner, *root, tests, string(harness), *timeout, *jobs)
	report(results, *verbose, *showSkip, *failFile)
}

// ---- discovery ----

// discover walks testDir for *.js files. mjsunit.js is the harness, and the
// mjsunit-*.js siblings next to it are helper libraries that tests pull in by
// name; neither is a test.
func discover(testDir, only string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(testDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".js") {
			return nil
		}
		if base := filepath.Base(path); base == "mjsunit.js" || strings.HasPrefix(base, "mjsunit-") {
			return nil
		}
		if only != "" && !strings.Contains(path, only) {
			return nil
		}
		out = append(out, path)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// ---- skip rules ----

// nativesRE matches V8's `%Runtime()` intrinsic syntax. It is enabled by
// --allow-natives-syntax and is a SyntaxError in every other engine, so those
// tests cannot be scored here at all. The `%` must be followed by an identifier
// and a call to avoid matching a modulo expression.
var nativesRE = regexp.MustCompile(`%[A-Z][A-Za-z0-9_]*\(`)

// hostOnlyRE matches the host facilities goant deliberately does not provide.
// Realm and Worker need multiple agents; the externalize/serializer/FastCAPI
// hooks are d8 test plumbing with no JavaScript meaning.
var hostOnlyRE = regexp.MustCompile(`\bWebAssembly\b|\bnew Worker\b|\bRealm\.|\bexternalizeString\b|` +
	`\bcreateExternalizable|\bgetV8Statistics\b|\bSharedStructType\b|\bcputracemark\b|` +
	`d8\.(serializer|wasm|dom|test|profiler|debugger)\b|\basync_hooks\b`)

// skipDirs are the trees that test the shell or the engine's internals rather
// than the JavaScript language.
var skipDirs = []string{"wasm", "sandbox", "d8", "shared-memory", "tools", "async-hooks"}

// skipReason reports why a test cannot be scored, or "" if it can be.
func skipReason(rel, src string) string {
	top, _, _ := strings.Cut(rel, string(filepath.Separator))
	for _, d := range skipDirs {
		if top == d {
			return "host-only dir:" + d
		}
	}
	if nativesRE.MatchString(src) {
		return "natives-syntax"
	}
	if m := hostOnlyRE.FindString(src); m != "" {
		return "host-only:" + strings.TrimSuffix(strings.TrimPrefix(m, "\\b"), "\\b")
	}
	return ""
}

// ---- assembly ----

// filesRE captures a `// Files: a.js b.js` header. mjsunit's own runner loads
// each named file before the test.
var filesRE = regexp.MustCompile(`(?m)^// Files:\s*(.*)$`)

// executeRE captures d8.file.execute('…'), which is d8's `load`. goant has no
// host file loader, so the runner inlines the named source instead and leaves
// the call itself to the no-op stub in the shim.
var executeRE = regexp.MustCompile(`d8\.file\.execute\(\s*['"]([^'"]+)['"]\s*\)`)

// moduleRE detects a test that must run in the module goal. mjsunit marks these
// with a `// MODULE` header; a bare top-level import/export would otherwise be a
// SyntaxError in the script goal.
var moduleRE = regexp.MustCompile(`(?m)^// MODULE\s*$`)

// preloads returns the sources a test pulls in before its own body, in order:
// the `// Files:` header first, then every d8.file.execute target. Paths in
// both are written relative to the v8 checkout root, so the test/mjsunit/
// prefix is stripped. A name that does not resolve is skipped rather than
// failed — the test will report the missing symbol itself, which is a clearer
// diagnostic than a harness error.
func preloads(mjsRoot, src string) []string {
	var names []string
	if m := filesRE.FindStringSubmatch(src); m != nil {
		names = append(names, strings.Fields(m[1])...)
	}
	for _, m := range executeRE.FindAllStringSubmatch(src, -1) {
		names = append(names, m[1])
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimPrefix(n, "test/mjsunit/")
		if seen[n] {
			continue
		}
		seen[n] = true
		if b, err := os.ReadFile(filepath.Join(mjsRoot, n)); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}

// shimJS is the d8 shell surface mjsunit tests assume. It is deliberately
// nothing but host glue: no shim here may change JavaScript semantics, so a
// failure still means goant got the language wrong. gc() is a no-op because
// goant's collector is not observable from JavaScript and no mjsunit assertion
// depends on collection having happened.
const shimJS = `
globalThis.print = function () { console.log(Array.prototype.join.call(arguments, ' ')); };
globalThis.printErr = globalThis.print;
globalThis.write = globalThis.print;
globalThis.alert = globalThis.print;
globalThis.gc = function () {};
globalThis.quit = function (code) { process.exit(code | 0); };
globalThis.version = function () { return 'goant'; };
globalThis.load = function () {};
globalThis.readbuffer = function () { return new ArrayBuffer(0); };
globalThis.d8 = {
  file: { execute: function () {}, read: function () { return ''; } },
  test: {}, terminate: function () {},
};
`

func assemble(harness, src string, pre []string) string {
	var b strings.Builder
	b.WriteString(shimJS)
	b.WriteByte('\n')
	b.WriteString(harness)
	b.WriteByte('\n')
	for _, p := range pre {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString(src)
	return b.String()
}

// ---- execution ----

type outcome int

const (
	outPass outcome = iota
	outFail
	outSkip
)

type result struct {
	name    string
	outcome outcome
	reason  string
}

type execResult struct {
	stdout, stderr string
	exit           int
	timedOut       bool
}

func runAll(runner, mjsRoot string, tests []string, harness string, timeout time.Duration, jobs int) []result {
	results := make([]result, len(tests))
	scratchDir, err := os.MkdirTemp("", "goant-mjsunit-")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(scratchDir)

	var wg sync.WaitGroup
	work := make(chan int)
	for w := range jobs {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			scratch := filepath.Join(scratchDir, fmt.Sprintf("w%d.js", w))
			for i := range work {
				results[i] = runOne(runner, mjsRoot, tests[i], harness, scratch, timeout)
			}
		}(w)
	}
	for i := range tests {
		work <- i
	}
	close(work)
	wg.Wait()
	return results
}

func runOne(runner, mjsRoot, path, harness, scratch string, timeout time.Duration) result {
	rel, _ := filepath.Rel(mjsRoot, path)
	src, err := os.ReadFile(path)
	if err != nil {
		return result{rel, outFail, "read: " + err.Error()}
	}
	if r := skipReason(rel, string(src)); r != "" {
		return result{rel, outSkip, r}
	}

	// A module test is handed to the runner as-is: its own import specifiers must
	// resolve against the real test directory, and the harness rides along as a
	// prelude script rather than being concatenated in front of it.
	if moduleRE.MatchString(string(src)) {
		pre := shimJS + "\n" + harness + "\n" + strings.Join(preloads(mjsRoot, string(src)), "\n")
		preFile := scratch + ".prelude.js"
		if err := os.WriteFile(preFile, []byte(pre), 0o644); err != nil {
			return result{rel, outFail, "write: " + err.Error()}
		}
		ex := execRunner(runner, []string{"-module", "-prelude", preFile, path}, timeout)
		if ok, reason := classify(ex); !ok {
			return result{rel, outFail, reason}
		}
		return result{rel, outPass, ""}
	}

	full := assemble(harness, string(src), preloads(mjsRoot, string(src)))
	if err := os.WriteFile(scratch, []byte(full), 0o644); err != nil {
		return result{rel, outFail, "write: " + err.Error()}
	}
	ex := execRunner(runner, []string{"-module-base", filepath.Dir(path), scratch}, timeout)
	if ok, reason := classify(ex); !ok {
		return result{rel, outFail, reason}
	}
	return result{rel, outPass, ""}
}

func execRunner(runner string, args []string, timeout time.Duration) execResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, runner, args...)
	cmd.Env = append(os.Environ(), "TZ=UTC")
	var so, se strings.Builder
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	r := execResult{stdout: so.String(), stderr: se.String()}
	if ctx.Err() == context.DeadlineExceeded {
		r.timedOut = true
		r.exit = -1
		return r
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			r.exit = ee.ExitCode()
		} else {
			r.exit = -1
			r.stderr += "\n[exec] " + err.Error()
		}
	}
	return r
}

// classify applies d8's criterion: an mjsunit test that finishes without
// throwing has passed. assertX failures surface as an uncaught MjsUnitAssertionError.
func classify(ex execResult) (bool, string) {
	if ex.timedOut {
		return false, "timeout"
	}
	if ex.exit != 0 {
		if line := firstLine(ex.stderr); line != "" {
			return false, line
		}
		return false, fmt.Sprintf("exit %d", ex.exit)
	}
	if s := strings.TrimSpace(ex.stderr); s != "" {
		return false, firstLine(s)
	}
	return true, ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// ---- reporting ----

func report(res []result, verbose, showSkip bool, failFile string) {
	var pass, fail, skip int
	type bucket struct{ pass, fail, skip int }
	buckets := map[string]*bucket{}
	var failures []string

	for _, r := range res {
		key := "(root)"
		if i := strings.IndexByte(r.name, filepath.Separator); i >= 0 {
			key = r.name[:i]
		}
		b := buckets[key]
		if b == nil {
			b = &bucket{}
			buckets[key] = b
		}
		switch r.outcome {
		case outPass:
			pass++
			b.pass++
		case outFail:
			fail++
			b.fail++
			failures = append(failures, r.name+"\t"+r.reason)
		case outSkip:
			skip++
			b.skip++
		}
	}

	if verbose {
		for _, r := range res {
			if r.outcome == outFail {
				fmt.Printf("FAIL %s\n     %s\n", r.name, r.reason)
			}
		}
	}
	if showSkip {
		for _, r := range res {
			if r.outcome == outSkip {
				fmt.Printf("SKIP %s (%s)\n", r.name, r.reason)
			}
		}
	}

	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Println(strings.Repeat("-", 80))
	fmt.Printf("%-46s %8s %8s %8s %7s\n", "bucket", "pass", "fail", "skip", "rate")
	fmt.Println(strings.Repeat("-", 80))
	for _, k := range keys {
		b := buckets[k]
		rate := 100.0
		if run := b.pass + b.fail; run > 0 {
			rate = 100 * float64(b.pass) / float64(run)
		}
		fmt.Printf("%-46s %8d %8d %8d %6.1f%%\n", k, b.pass, b.fail, b.skip, rate)
	}
	fmt.Println(strings.Repeat("-", 80))
	run := pass + fail
	rate := 100.0
	if run > 0 {
		rate = 100 * float64(pass) / float64(run)
	}
	fmt.Printf("%-46s %8d %8d %8d %6.1f%%\n", "TOTAL", pass, fail, skip, rate)
	fmt.Printf("\n%d/%d passing (of %d run; %d skipped)\n", pass, run, run, skip)

	if failFile != "" {
		sort.Strings(failures)
		if err := os.WriteFile(failFile, []byte(strings.Join(failures, "\n")+"\n"), 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote %d failures to %s\n", len(failures), failFile)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "goant-mjsunit:", err)
	os.Exit(1)
}
