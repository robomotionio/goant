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
	"encoding/json"
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

	"github.com/robomotionio/goant/internal/harness"
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
		core     = flag.Bool("core", false, "score only the ECMA-262 core: skip ECMA-402 (intl402), the\n\tnon-normative staging/ imports, staged proposals, and the tests that\n\tneed threads, a second realm, or web-legacy document.all")
		all      = flag.Bool("all", false, "score every test file, skipping nothing: no profile, no feature\n\texclusions, not even the host-capability ones. This is the number with\n\tno judgement calls in it — use it to audit what -core leaves out.\n\tOverrides -core.")
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

	coreOnly, skipNothing = *core, *all
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

// loadHarness indexes every helper by the path a test names it with, which for
// the staging tree means a subdirectory: SpiderMonkey's imports there include
// "sm/non262-shell.js", and reading only the top level left 164 of them
// unrunnable and counted as skips.
func loadHarness(dir string) (map[string]string, error) {
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".js") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		m[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("harness dir %s: %w", dir, err)
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
	// canBlockFalse is the CanBlockIsFalse flag: the test is written for a host
	// whose Agent Record has [[CanBlock]] false. See skipReason.
	canBlockFalse bool
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
	// A few tests use CR (or CRLF) line terminators throughout — the whole file is
	// then one LF-delimited "line", so normalise before splitting or every key
	// (including `includes`) is missed.
	block = strings.ReplaceAll(block, "\r\n", "\n")
	block = strings.ReplaceAll(block, "\r", "\n")
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
				case "CanBlockIsFalse":
					m.canBlockFalse = true
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
	"Atomics":                    true, // agent/SharedArrayBuffer host model
	"SharedArrayBuffer":          true,
	"Atomics.waitAsync":          true,
	"decorators":                 true,
	"import-assertions":          true, // the withdrawn `assert {…}` spelling
	"IsHTMLDDA":                  true,
	"source-phase-imports":       true,
	"source-phase-imports-typed": true,
}

// coreOnly restricts scoring to the ECMA-262 core (the -core flag).
var coreOnly bool

// skipNothing disables every exclusion below (the -all flag), so the score has
// no judgement calls in it: tests for features goant does not implement, and
// tests needing a host it does not provide, are run and counted as failures.
// The point is that -core's number stays auditable against this one.
var skipNothing bool

// coreSkipFeatures are the features outside a pure single-threaded ECMA-262
// engine: staged proposals that are not yet the language, tests needing a second
// realm the host must supply, and web-legacy document.all emulation.
//
// The proposal entries are the flags test262 lists above its "## Standard
// language features" header in features.txt. That header is the line to watch:
// when a proposal graduates it moves below it, and the tests stop being
// skippable and start being work. (iterator-chunking and iterator-includes
// crossed it in July 2026 and cost 109 failures until they were implemented.)
// The Atomics/SharedArrayBuffer group sits below the header and is excluded on
// scope, not on staging: it needs an agent model goant does not have.
var coreSkipFeatures = map[string]bool{
	"decorators":                 true,
	"source-phase-imports":       true,
	"source-phase-imports-typed": true,
	"import-defer":               true,
	"import-bytes":               true,
	"import-text":                true,
	"Iterator.prototype.join":    true,
	"import-attributes":          false, // implemented; scored
	"cross-realm":                true,
	"IsHTMLDDA":                  true,
	"Atomics":                    true,
	"Atomics.waitAsync":          true,
	"SharedArrayBuffer":          true,
}

// coreSkipDirs are the trees outside ECMA-262: ECMA-402 is a separate
// specification (and needs CLDR data), and staging/ holds unreviewed engine
// imports that are not normative.
var coreSkipDirs = []string{"intl402/", "staging/"}

func (m meta) skipReason() string {
	// CanBlockIsFalse names a property of the HOST's Agent Record, and a host has
	// one value for it: a browser's main thread cannot block, a shell agent can.
	// goant's can, so these two are not tests goant fails — they are tests
	// written for the other kind of host, and INTERPRETING.md says a host runs
	// only the variant that matches it.
	//
	// Deliberately ahead of the -all check. -all removes goant's judgement calls
	// about which features are in scope; this is not one of those, and pretending
	// to be both kinds of agent at once would make the number mean less, not more.
	if m.canBlockFalse {
		return "flag:CanBlockIsFalse (this agent can block)"
	}
	if skipNothing {
		return ""
	}
	for _, f := range m.features {
		if skipFeatures[f] {
			return "feature:" + f
		}
		if coreOnly && coreSkipFeatures[f] {
			return "non-core:" + f
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

// peakTop is how many of the hungriest tests to name. A run that dies for
// memory should say which test did it, in the run itself: chasing a 30 GB peak
// by bisecting the suite with address-space caps cost hours and produced three
// wrong answers before this existed.
const peakTop = 10

var (
	peakMu     sync.Mutex
	peakByTest = map[string]int64{}
)

func notePeak(path string, rss int64) {
	if rss == 0 {
		return
	}
	peakMu.Lock()
	if rss > peakByTest[path] {
		peakByTest[path] = rss
	}
	peakMu.Unlock()
}

// reportPeaks names the hungriest tests. Printed always, not only on failure:
// the number that matters is the LARGEST SINGLE CHILD, because that is what has
// to fit the machine, and a suite total hides it.
func reportPeaks() {
	peakMu.Lock()
	defer peakMu.Unlock()
	if len(peakByTest) == 0 {
		return
	}
	type kv struct {
		path string
		rss  int64
	}
	all := make([]kv, 0, len(peakByTest))
	for p, r := range peakByTest {
		all = append(all, kv{p, r})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].rss > all[j].rss })
	fmt.Printf("\npeak memory, hungriest %d of %d tests:\n", min(peakTop, len(all)), len(all))
	for _, e := range all[:min(peakTop, len(all))] {
		fmt.Printf("  %8.2f GB  %s\n", float64(e.rss)/(1<<30), e.path)
	}
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
	if coreOnly && !skipNothing {
		for _, d := range coreSkipDirs {
			if strings.HasPrefix(rel, "test/"+d) {
				return result{rel, outSkip, "non-core:" + strings.TrimSuffix(d, "/")}
			}
		}
	}
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
	// as a script prelude sharing the realm.
	if m.isModule {
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
		preFile := scratch + ".prelude.js"
		if err := os.WriteFile(preFile, []byte(pre.String()), 0o644); err != nil {
			return result{rel, outFail, "write: " + err.Error()}
		}
		// The module source is used verbatim (unlike a script, which is
		// concatenated with the harness), so the test file itself is handed to the
		// runner: its import specifiers must resolve against the real test
		// directory to find the sibling _FIXTURE modules.
		modFile := path
		// Flags must precede the positional file (flag parsing stops at the first
		// non-flag argument).
		ex := execRunnerArgs(runner, []string{"-module", "-prelude", preFile, modFile}, timeout)
		notePeak(rel, ex.maxRSS)
		if ok, reason := classify(m, ex); !ok {
			return result{rel, outFail, "module: " + reason}
		}
		return result{rel, outPass, ""}
	}

	// A script is concatenated with the harness into a scratch file, so its path
	// cannot locate the _FIXTURE modules an import() names: pass the test's own
	// directory as the resolution base.
	baseDir := filepath.Dir(path)
	for _, variant := range m.variants() {
		full := assemble(variant, m, string(src), harness, incSrc)
		if err := os.WriteFile(scratch, []byte(full), 0o644); err != nil {
			return result{rel, outFail, "write: " + err.Error()}
		}
		ex := execRunnerArgs(runner, []string{"-module-base", baseDir, scratch}, timeout)
		notePeak(rel, ex.maxRSS)
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
// The engine provides $262 itself now -- global, createRealm, evalScript, gc
// and detachArrayBuffer -- and its createRealm makes a real second realm with
// its own global lexical environment. The shim that used to stand in for it
// could only fake one, so a script evaluated in the "other" realm shared this
// realm's bindings and a test declaring the same name in both failed to parse.
var host262JS = ""

// jsQuote renders s as a JavaScript string literal.
func jsQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

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
	// maxRSS is the child's peak resident size, straight from the kernel via
	// wait4. Free -- the kernel has already counted it -- and it is the only way
	// to answer "which test used the memory" without bisecting the suite with
	// caps, which is what -peak reports.
	maxRSS int64
}

func execRunner(runner, file string, timeout time.Duration) execResult {
	return execRunnerArgs(runner, []string{file}, timeout)
}

func execRunnerArgs(runner string, args []string, timeout time.Duration) execResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, runner, args...)
	// $262 and $262.agent are off unless a host asks: both are capabilities a
	// production embedder must not hand a script, and the suite is the one thing
	// that needs them. An engine that does not know these variables ignores
	// them, so pointing -runner at another engine still works.
	cmd.Env = harness.PinJIT(append(os.Environ(), "TZ=UTC", "GOANT_262=1", "GOANT_AGENTS=1"))
	var so, se strings.Builder
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	r := execResult{stdout: so.String(), stderr: se.String(), maxRSS: childMaxRSS(cmd)}
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
	reportPeaks()
	// The score last, so `| tail -3` still shows it. The peak table is a
	// diagnostic and belongs above the headline, not in front of it.
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
