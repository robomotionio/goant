// Command goant-t262 runs the official ECMAScript conformance suite (Test262,
// github.com/tc39/test262) against the goant interpreter. Unlike the curated
// compat-table corpus (feature-presence testing), Test262 is adversarial,
// spec-algorithm-level conformance: ~53k files, most run in both strict and
// sloppy mode.
//
// It parses each test's YAML frontmatter (flags / includes / negative /
// features), assembles the harness (assert.js + sta.js + any includes, plus
// doneprintHandle.js for async tests), runs the required strict/sloppy variants
// through `./goant <tmpfile>`, and classifies the result:
//
//   - positive test  -> pass iff exit==0 (assert.js throws Test262Error on fail)
//   - async test     -> pass iff stdout has "Test262:AsyncTestComplete"
//   - negative parse -> pass iff a parse-time SyntaxError (goant: "…: Type:",
//     not "Uncaught")
//   - negative runtime -> pass iff "Uncaught <Type>" on stderr
//
// A test passes only if every variant it requires passes.
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
		root     = flag.String("t262", "../test262", "path to a test262 checkout")
		runner   = flag.String("runner", "./goant", "path to the goant runner binary")
		only     = flag.String("only", "", "run only tests whose path contains this substring")
		dir      = flag.String("dir", "", "restrict to this subdirectory under test/ (e.g. language/statements/const)")
		jobs     = flag.Int("j", 0, "worker count (0 = min(16,NumCPU))")
		timeout  = flag.Duration("timeout", 10*time.Second, "per-execution timeout")
		maxN     = flag.Int("max", 0, "cap number of test files (0 = all)")
		verbose  = flag.Bool("v", false, "print every failure with a one-line reason")
		showSkip = flag.Bool("show-skip", false, "also list skipped tests")
		failFile = flag.String("failures", "", "write the sorted list of failing test paths here")
	)
	flag.Parse()

	harnessDir := filepath.Join(*root, "harness")
	testDir := filepath.Join(*root, "test")
	if *dir != "" {
		testDir = filepath.Join(testDir, *dir)
	}
	if _, err := os.Stat(testDir); err != nil {
		fatal(fmt.Errorf("test dir %s: %w", testDir, err))
	}

	harness, err := loadHarness(harnessDir)
	if err != nil {
		fatal(err)
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

	results := runAll(*runner, *root, tests, harness, *timeout, *jobs)

	report(results, *verbose, *showSkip, *failFile)
}

// ---- discovery ----

// discover walks testDir for *.js files, excluding the _FIXTURE.js helper files
// (imported by module tests, never run directly). Paths are returned relative to
// the test262 root so they read like the canonical test names.
func discover(testDir, only string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(testDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".js") {
			return nil
		}
		if strings.HasSuffix(path, "_FIXTURE.js") {
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

// ---- harness ----

func loadHarness(dir string) (map[string]string, error) {
	m := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("harness dir %s: %w", dir, err)
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		m[e.Name()] = string(b)
	}
	return m, nil
}

// ---- frontmatter ----

type meta struct {
	includes []string
	features []string
	negType  string
	negPhase string
	isRaw    bool
	isModule bool
	isAsync  bool
	onlyStr  bool
	noStrict bool
}

// parseMeta extracts the /*--- … ---*/ YAML frontmatter. Test262's frontmatter is
// a small, regular subset of YAML, so a targeted line parser (no external dep,
// keeping the pure-Go build) handles the keys we care about: flags, includes,
// features, and the nested negative{phase,type}.
func parseMeta(src string) meta {
	var m meta
	start := strings.Index(src, "/*---")
	if start < 0 {
		return m
	}
	end := strings.Index(src[start:], "---*/")
	if end < 0 {
		return m
	}
	block := src[start+len("/*---") : start+end]
	lines := strings.Split(block, "\n")

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if indentOf(line) != 0 {
			continue
		}
		key, val, ok := splitKey(line)
		if !ok {
			continue
		}
		switch key {
		case "flags":
			for _, f := range parseList(val, lines, &i) {
				switch f {
				case "raw":
					m.isRaw = true
				case "module":
					m.isModule = true
				case "async":
					m.isAsync = true
				case "onlyStrict":
					m.onlyStr = true
				case "noStrict":
					m.noStrict = true
				}
			}
		case "includes":
			m.includes = append(m.includes, parseList(val, lines, &i)...)
		case "features":
			m.features = append(m.features, parseList(val, lines, &i)...)
		case "negative":
			// Nested block: indented phase:/type: lines follow.
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == "" {
					continue
				}
				if indentOf(lines[j]) == 0 {
					break
				}
				nk, nv, ok := splitKey(lines[j])
				if !ok {
					continue
				}
				switch nk {
				case "phase":
					m.negPhase = strings.TrimSpace(nv)
				case "type":
					m.negType = strings.TrimSpace(nv)
				}
				i = j
			}
		}
	}
	return m
}

func indentOf(s string) int {
	n := 0
	for _, c := range s {
		if c != ' ' {
			break
		}
		n++
	}
	return n
}

func splitKey(line string) (key, val string, ok bool) {
	t := strings.TrimSpace(line)
	c := strings.IndexByte(t, ':')
	if c < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(t[:c])
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	return key, strings.TrimSpace(t[c+1:]), true
}

// parseList reads either an inline list (`[a, b]`) from val, or a following
// block of `- item` lines, advancing *i past any consumed block lines.
func parseList(val string, lines []string, i *int) []string {
	val = strings.TrimSpace(val)
	if strings.HasPrefix(val, "[") {
		val = strings.TrimPrefix(val, "[")
		val = strings.TrimSuffix(val, "]")
		var out []string
		for _, p := range strings.Split(val, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	// Block list: consume indented `- item` lines.
	var out []string
	for j := *i + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		if indentOf(lines[j]) == 0 {
			break
		}
		item := strings.TrimSpace(lines[j])
		if !strings.HasPrefix(item, "- ") {
			break
		}
		out = append(out, strings.TrimSpace(item[2:]))
		*i = j
	}
	return out
}

// ---- feature skip-list ----
//
// Host capabilities goant intentionally does not provide (multi-realm/agent
// harness hooks, unimplemented staged proposals). Tests tagged with these are
// reported SKIP rather than FAIL so the pass rate reflects the language subset
// goant targets, not missing host glue.
var skipFeatures = map[string]bool{
	"cross-realm":                true, // needs $262.createRealm
	"Atomics":                    true, // agent/SharedArrayBuffer host model
	"SharedArrayBuffer":          true,
	"Atomics.waitAsync":          true,
	"Temporal":                   true, // staged proposal, not implemented
	"decorators":                 true,
	"import-assertions":          true,
	"import-attributes":          true,
	"IsHTMLDDA":                  true,
	"source-phase-imports":       true,
	"source-phase-imports-typed": true,
}

func (m meta) skipReason() string {
	for _, f := range m.features {
		if skipFeatures[f] {
			return "feature:" + f
		}
	}
	return ""
}

// staticImportRE matches a static ImportDeclaration or a re-export
// (export … from / export *), which need module linking not yet implemented.
// Dynamic import() is fine (it is an expression, handled by the engine).
var staticImportRE = regexp.MustCompile(`(?m)^\s*import\s+[^(]|(?m)^\s*export\s+(\*|\{[^}]*\}\s+from|.*\bfrom\b)`)

func moduleNeedsLinking(src string) bool {
	return staticImportRE.MatchString(src)
}

// ---- execution ----

type outcome int

const (
	outPass outcome = iota
	outFail
	outSkip
)

type result struct {
	path    string
	outcome outcome
	reason  string
}

func runAll(runner, root string, tests []string, harness map[string]string, timeout time.Duration, jobs int) []result {
	tmpDir, err := os.MkdirTemp("", "goant-t262-")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	work := make(chan string)
	out := make(chan result, len(tests))
	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			scratch := filepath.Join(tmpDir, fmt.Sprintf("w%d.js", id))
			for path := range work {
				out <- runOne(runner, root, path, harness, timeout, scratch)
			}
		}(w)
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
	}
	sort.Slice(res, func(i, j int) bool { return res[i].path < res[j].path })
	return res
}

func runOne(runner, root, path string, harness map[string]string, timeout time.Duration, scratch string) result {
	rel, _ := filepath.Rel(root, path)
	rel = filepath.ToSlash(rel)

	src, err := os.ReadFile(path)
	if err != nil {
		return result{rel, outFail, "read: " + err.Error()}
	}
	m := parseMeta(string(src))
	if r := m.skipReason(); r != "" {
		return result{rel, outSkip, r}
	}

	// Resolve includes up front so a missing harness file is a clear skip.
	var incSrc []string
	for _, inc := range m.includes {
		h, ok := harness[inc]
		if !ok {
			return result{rel, outSkip, "missing include " + inc}
		}
		incSrc = append(incSrc, h)
	}

	// A Module runs in the Module goal (implicitly strict, this===undefined) and
	// cannot be concatenated with the script harness; the harness is instead run
	// as a script prelude sharing the realm. Static imports need linking (not yet
	// implemented), so those are still skipped.
	if m.isModule {
		if moduleNeedsLinking(string(src)) {
			return result{rel, outSkip, "module (needs linking)"}
		}
		var pre strings.Builder
		pre.WriteString(harness["assert.js"])
		pre.WriteByte('\n')
		pre.WriteString(harness["sta.js"])
		pre.WriteByte('\n')
		pre.WriteString(host262JS)
		if m.isAsync {
			pre.WriteString(harness["doneprintHandle.js"])
			pre.WriteByte('\n')
		}
		for _, h := range incSrc {
			pre.WriteString(h)
			pre.WriteByte('\n')
		}
		preFile, modFile := scratch+".prelude.js", scratch+".module.mjs"
		if err := os.WriteFile(preFile, []byte(pre.String()), 0o644); err != nil {
			return result{rel, outFail, "write: " + err.Error()}
		}
		if err := os.WriteFile(modFile, src, 0o644); err != nil {
			return result{rel, outFail, "write: " + err.Error()}
		}
		// Flags must precede the positional file (flag parsing stops at the first
		// non-flag argument).
		ex := execRunnerArgs(runner, []string{"-module", "-prelude", preFile, modFile}, timeout)
		if ok, reason := classify(m, ex); !ok {
			return result{rel, outFail, "module: " + reason}
		}
		return result{rel, outPass, ""}
	}

	for _, variant := range m.variants() {
		full := assemble(variant, m, string(src), harness, incSrc)
		if err := os.WriteFile(scratch, []byte(full), 0o644); err != nil {
			return result{rel, outFail, "write: " + err.Error()}
		}
		ex := execRunner(runner, scratch, timeout)
		if ok, reason := classify(m, ex); !ok {
			return result{rel, outFail, variant + ": " + reason}
		}
	}
	return result{rel, outPass, ""}
}

// host262JS defines the Test262 host object $262. evalScript is indirect
// (global-scope) eval; global is the global object (indirect eval's `this`);
// detachArrayBuffer uses the engine's transferring ArrayBuffer.prototype.transfer;
// gc is a no-op. createRealm/agent are unsupported (their tests are not covered).
// $262.global uses globalThis rather than (0,eval)("this") so the harness does
// not reference `eval` at the script's top level — a test that declares
// `let eval` would otherwise put that reference in its temporal dead zone.
const host262JS = "var $262 = { global: globalThis, evalScript: function (s) { return (0, eval)(s); }, gc: function () {}, detachArrayBuffer: function (b) { try { b.transfer(); } catch (e) {} } };\n"

func (m meta) variants() []string {
	switch {
	case m.isRaw:
		return []string{"raw"}
	case m.onlyStr:
		return []string{"strict"}
	case m.noStrict:
		return []string{"sloppy"}
	default:
		return []string{"strict", "sloppy"}
	}
}

func assemble(variant string, m meta, testSrc string, harness map[string]string, incSrc []string) string {
	var b strings.Builder
	if variant == "strict" {
		b.WriteString("\"use strict\";\n")
	}
	if variant != "raw" {
		b.WriteString(harness["assert.js"])
		b.WriteByte('\n')
		b.WriteString(harness["sta.js"])
		b.WriteByte('\n')
		b.WriteString(host262JS)
		if m.isAsync {
			b.WriteString(harness["doneprintHandle.js"])
			b.WriteByte('\n')
		}
		for _, h := range incSrc {
			b.WriteString(h)
			b.WriteByte('\n')
		}
	}
	b.WriteString(testSrc)
	return b.String()
}

type execResult struct {
	exit     int
	stdout   string
	stderr   string
	timedOut bool
}

func execRunner(runner, file string, timeout time.Duration) execResult {
	return execRunnerArgs(runner, []string{file}, timeout)
}

func execRunnerArgs(runner string, args []string, timeout time.Duration) execResult {
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

// classify decides whether one variant's execution meets the test's expectation.
func classify(m meta, ex execResult) (bool, string) {
	if ex.timedOut {
		return false, "timeout"
	}
	// Negative test: an expected parse-time or runtime error of a given type.
	if m.negPhase != "" {
		if ex.exit == 0 {
			return false, "expected " + m.negType + " (" + m.negPhase + "), exit 0"
		}
		switch m.negPhase {
		case "parse", "resolution", "early":
			// A parse-time error: goant prints "<file>:<line>: SyntaxError: …",
			// distinct from a runtime "Uncaught …". Require the former.
			if strings.Contains(ex.stderr, "Uncaught") {
				return false, "expected parse " + m.negType + ", got runtime error"
			}
			if !strings.Contains(ex.stderr, m.negType) {
				return false, "expected parse " + m.negType + ", stderr: " + firstLine(ex.stderr)
			}
			return true, ""
		default: // runtime
			if !strings.Contains(ex.stderr, m.negType) {
				return false, "expected runtime " + m.negType + ", stderr: " + firstLine(ex.stderr)
			}
			return true, ""
		}
	}
	// Async test: completion is signalled on stdout by doneprintHandle.js.
	if m.isAsync {
		if strings.Contains(ex.stdout, "Test262:AsyncTestFailure") {
			return false, "async failure: " + asyncFailLine(ex.stdout)
		}
		if !strings.Contains(ex.stdout, "Test262:AsyncTestComplete") {
			return false, "async never completed: " + firstLine(ex.stderr)
		}
		return true, ""
	}
	// Positive test: assert.js throws Test262Error (→ nonzero exit) on any failure.
	if ex.exit != 0 {
		return false, firstLine(ex.stderr)
	}
	return true, ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func asyncFailLine(stdout string) string {
	for _, ln := range strings.Split(stdout, "\n") {
		if strings.Contains(ln, "Test262:AsyncTestFailure") {
			return strings.TrimSpace(ln)
		}
	}
	return ""
}

// ---- reporting ----

func report(res []result, verbose, showSkip bool, failFile string) {
	var pass, fail, skip int
	// Bucket by the first two path segments under test/ for a readable breakdown.
	type bucket struct{ pass, fail, skip int }
	buckets := map[string]*bucket{}
	var failing []result

	for _, r := range res {
		key := bucketKey(r.path)
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
			failing = append(failing, r)
		case outSkip:
			skip++
			b.skip++
		}
	}

	if verbose {
		for _, r := range failing {
			fmt.Printf("FAIL %s\n       %s\n", r.path, r.reason)
		}
	}
	if showSkip {
		for _, r := range res {
			if r.outcome == outSkip {
				fmt.Printf("SKIP %s  (%s)\n", r.path, r.reason)
			}
		}
	}

	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Println()
	fmt.Printf("%-46s %8s %8s %8s %7s\n", "bucket", "pass", "fail", "skip", "rate")
	fmt.Println(strings.Repeat("-", 80))
	for _, k := range keys {
		b := buckets[k]
		run := b.pass + b.fail
		rate := 100.0
		if run > 0 {
			rate = 100 * float64(b.pass) / float64(run)
		}
		fmt.Printf("%-46s %8d %8d %8d %6.1f%%\n", k, b.pass, b.fail, b.skip, rate)
	}
	fmt.Println(strings.Repeat("-", 80))
	run := pass + fail
	rate := 0.0
	if run > 0 {
		rate = 100 * float64(pass) / float64(run)
	}
	fmt.Printf("%-46s %8d %8d %8d %6.1f%%\n", "TOTAL", pass, fail, skip, rate)
	fmt.Printf("\n%d/%d passing (of %d run; %d skipped)\n", pass, run, run, skip)

	if failFile != "" {
		var b strings.Builder
		for _, r := range failing {
			fmt.Fprintf(&b, "%s\t%s\n", r.path, r.reason)
		}
		if err := os.WriteFile(failFile, []byte(b.String()), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "goant-t262: writing failures:", err)
		} else {
			fmt.Printf("wrote %d failures to %s\n", len(failing), failFile)
		}
	}
}

// bucketKey groups a test path (test262-relative, e.g.
// "test/language/statements/const/x.js") by its first three components under
// test/ for a legible per-area breakdown.
func bucketKey(rel string) string {
	rel = strings.TrimPrefix(rel, "test/")
	parts := strings.Split(rel, "/")
	if len(parts) > 3 {
		parts = parts[:3]
	} else if len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, "/")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "goant-t262:", err)
	os.Exit(2)
}
