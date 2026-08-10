// Command goant is the CLI entry point for the pure-Go ant port.
//
//	goant file.js        run a script
//	goant -e '<code>'    evaluate a string
//	goant --parse f.js   parse only, report syntax errors (Phase 1)
//	goant --disasm f.js   disassemble compiled bytecode (Phase 3)
//	goant --version      print the version and exit
//	goant -jit=false f.js run with the compiled tier off
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"

	"github.com/robomotionio/goant/internal/engine"
)

func main() {
	var (
		eval    = flag.String("e", "", "evaluate code from the command line")
		parse   = flag.Bool("parse", false, "parse only; report syntax errors")
		disasm  = flag.Bool("disasm", false, "disassemble compiled bytecode")
		module  = flag.Bool("module", false, "run the file as a Module (strict, this===undefined)")
		prelude = flag.String("prelude", "", "comma-separated script files to run (as scripts) before the module")
		modBase = flag.String("module-base", "", "directory that import specifiers resolve against (for scripts using import())")
		cpuProf = flag.String("cpuprofile", "", "write a CPU profile to this file")
		jit     = flag.Bool("jit", true, "run the compiled tier (GOANT_JIT overrides; an explicit -jit wins over both)")
		showVer = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(versionString())
		return
	}

	// Where the compiled tier actually ran, for GOANT_JIT_STATS=1. Deferred
	// rather than printed at the end of main because the interesting exits —
	// a thrown error, an explicit process.exit — do not reach it.
	if os.Getenv("GOANT_JIT_STATS") != "" {
		defer dumpJITStats()
	}

	if *cpuProf != "" {
		f, err := os.Create(*cpuProf)
		if err != nil {
			fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			fatal(err)
		}
		// Every exit below goes through os.Exit, which skips defers, so the
		// profile has to be flushed on the way out of each of them instead.
		defer stopProfile()
		profiling = true
	}

	var src, name string
	switch {
	case *eval != "":
		src, name = *eval, "<eval>"
	case flag.NArg() >= 1:
		name = flag.Arg(0)
		data, err := os.ReadFile(name)
		if err != nil {
			fatal(err)
		}
		src = string(data)
	default:
		fmt.Fprintln(os.Stderr, "usage: goant [flags] file.js")
		flag.PrintDefaults()
		os.Exit(2)
	}

	// The compiled tier is ON in this binary and OFF for a host that links the
	// package. The two defaults answer different questions. An embedder inherits
	// whatever the process is doing and should get the conservative path until it
	// asks; someone running `goant script.js` has asked for this engine to run a
	// script, and handing them a third of the speed it has is not conservative,
	// it is just wrong. External benchmark harnesses invoke the bare binary with
	// no flags and no environment, so this is also the number the world measures.
	//
	// Precedence: an explicit -jit wins, then GOANT_JIT, then on. GOANT_JIT still
	// decides in BOTH directions when it is set, because every A/B of the tier in
	// this repo is spelled that way -- see jit_tier.go on what reading presence
	// rather than value once cost.
	//
	// Anything that shells out to this binary must now say which tier it wants
	// rather than let the child pick. goant-t262, goant-mjsunit, goant-bench and
	// goant-conf all route their child environment through harness.PinJIT.
	switch {
	case flagWasSet("jit"):
		engine.JITSetEnabled(*jit)
	case os.Getenv("GOANT_JIT") != "":
		// jit_tier.go already parsed it at init, in both directions.
	default:
		engine.JITSetEnabled(true)
	}

	rt := engine.New()
	// $262 is a set of capabilities, not language features: detachArrayBuffer
	// invalidates a buffer's bytes, createRealm allocates realms without bound,
	// and reading IsHTMLDDA turns the compiled tier off for the realm. A host
	// asks for them explicitly, and the conformance runner is the only thing
	// here that does. See Runtime.EnableHostAPI.
	if os.Getenv("GOANT_262") == "1" {
		rt.EnableHostAPI()
	}
	// $262.agent is a separate grant, for the same reason: it starts goroutines
	// that run scripts. See Runtime.EnableAgents.
	if os.Getenv("GOANT_AGENTS") == "1" {
		rt.EnableAgents()
	}
	// GOANT_HEAP_LIMIT bounds the live heap, in megabytes, for a host that runs
	// this binary rather than links the package -- which for us means the
	// conformance harness, one process per test.
	//
	// It exists because a single test262 file can ask for an unbounded amount:
	// measured, `-all` peaked at 30.6 GB in ONE child, which fits our 31 GB
	// bench box by 400 MB and cannot fit a 16 GB CI runner at all. The runner
	// there does not fail the test, it dies, and the job reports "the runner has
	// received a shutdown signal" with no indication that memory was the reason.
	if mb := os.Getenv("GOANT_HEAP_LIMIT"); mb != "" {
		n, err := strconv.ParseUint(mb, 10, 64)
		if err != nil || n == 0 {
			fmt.Fprintf(os.Stderr, "goant: GOANT_HEAP_LIMIT must be a positive number of megabytes, got %q\n", mb)
			os.Exit(2)
		}
		rt.SetHeapLimit(n << 20)
	}
	// GOANT_ADDR_LIMIT is the hard counterpart, in megabytes: a kernel cap on
	// the address space rather than a budget the engine charges itself. The two
	// are not interchangeable — see setAddressSpaceLimit.
	if mb := os.Getenv("GOANT_ADDR_LIMIT"); mb != "" {
		n, err := strconv.ParseUint(mb, 10, 64)
		if err != nil || n == 0 {
			fmt.Fprintf(os.Stderr, "goant: GOANT_ADDR_LIMIT must be a positive number of megabytes, got %q\n", mb)
			os.Exit(2)
		}
		if err := setAddressSpaceLimit(n << 20); err != nil {
			fmt.Fprintf(os.Stderr, "goant: could not cap the address space: %v\n", err)
			os.Exit(2)
		}
	}
	if *modBase != "" {
		rt.SetModuleBase(*modBase)
	}

	switch {
	case *parse:
		if err := engine.ParseOnly(name, src); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case *disasm:
		if err := engine.DisasmOnly(name, src); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case *module:
		// Run prelude scripts (harness globals) into the same realm first.
		if *prelude != "" {
			for _, pf := range strings.Split(*prelude, ",") {
				if pf == "" {
					continue
				}
				data, err := os.ReadFile(pf)
				if err != nil {
					fatal(err)
				}
				if _, err := rt.RunString(pf, string(data)); err != nil {
					reportAndExit(err)
				}
			}
		}
		if _, err := rt.RunModule(name, src); err != nil {
			reportAndExit(err)
		}
	default:
		if _, err := rt.RunString(name, src); err != nil {
			reportAndExit(err)
		}
	}
}

// profiling records whether a CPU profile is running, so the exit paths know
// they have to flush it before os.Exit discards the deferred stop.
var profiling bool

// flagWasSet reports whether a flag was given on the command line, as opposed
// to holding its default. flag.Visit walks only the flags that were set, which
// is the only way to tell `-jit=true` from an unmentioned -jit -- and telling
// them apart is what lets the environment decide in the middle case.
func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// version is the release this binary was cut from, stamped at link time:
//
//	go build -ldflags "-X main.version=0.1.0" ./cmd/goant
//
// Empty means an unstamped build, which says 0.0.0 rather than inventing a
// number it cannot back up.
var version = ""

// versionString is one line, and deliberately one line only.
//
// It is read by machines as well as people: the js-engine-benchmark harness
// picks the version out of `--version` with a regex over the whole output, so a
// second line beginning with a v and a digit would be a coin toss. The shape is
// `goant vX.Y.Z-<sha>` because that is what their generic pattern extracts
// intact, commit included -- an engine that printed the commit on its own line
// got a blank version column and an issue asking for it back.
func versionString() string {
	v := version
	if v == "" {
		v = "0.0.0"
	}
	rev, dirty := vcsRevision()
	if rev == "" {
		return "goant v" + v
	}
	if dirty {
		rev += ".dirty"
	}
	return "goant v" + v + "-" + rev
}

// vcsRevision returns the short commit this binary was built from, and whether
// the tree was dirty, from the stamps the Go toolchain records for a build made
// inside a repository. Absent for a build from an exported tree or under
// -buildvcs=false, which is why a release stamps -X main.version as well.
func vcsRevision() (rev string, dirty bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
			if len(rev) > 7 {
				rev = rev[:7]
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return rev, dirty
}

func dumpJITStats() {
	c, d, in := engine.JITStats()
	total := c + d + in
	if total == 0 {
		total = 1
	}
	fmt.Fprintf(os.Stderr, "jit: %d compiled (%.1f%% of frame entries), %d declined, %d interpreted\n",
		c, 100*float64(c)/float64(total), d, in)
	hit, miss := engine.JITPropertyStats()
	if reads := hit + miss; reads > 0 {
		fmt.Fprintf(os.Stderr, "jit: %d property reads, %d served by the compiled cache (%.1f%%)\n",
			reads, hit, 100*float64(hit)/float64(reads))
	}
	if n := engine.JITNarrowStats(); n > 0 {
		fmt.Fprintf(os.Stderr, "jit:   of those misses, %d were served by the cache anyway (the emitted guard is narrower)\n", n)
	}
	phit, pmiss := engine.JITStoreStats()
	if stores := phit + pmiss; stores > 0 {
		fmt.Fprintf(os.Stderr, "jit: %d property stores, %d served by the compiled cache (%.1f%%)\n",
			stores, phit, 100*float64(phit)/float64(stores))
	}
	ghit, gmiss := engine.JITGlobalStats()
	if reads := ghit + gmiss; reads > 0 {
		fmt.Fprintf(os.Stderr, "jit: %d global reads, %d served by the compiled cache (%.1f%%)\n",
			reads, ghit, 100*float64(ghit)/float64(reads))
	}
	ehit, emiss := engine.JITElementStats()
	if reads := ehit + emiss; reads > 0 {
		fmt.Fprintf(os.Stderr, "jit: %d element reads, %d served by the emitted guard chain (%.1f%%)\n",
			reads, ehit, 100*float64(ehit)/float64(reads))
	}
	// Why the reads that reached the runtime did. "full" is the only one more
	// ways would serve; it is the number that decides whether icWays is worth
	// widening, and it has been 0.6% or less everywhere it has been measured.
	if h, e, r, f := engine.ICMissReasons(); h+e+r+f > 0 {
		tot := float64(h + e + r + f)
		fmt.Fprintf(os.Stderr, "jit: %d cache consults after an emitted miss: %d served here (%.1f%%), %d empty site (%.1f%%), %d room to spare (%.1f%%), %d full (%.1f%%)\n",
			uint64(tot), h, 100*float64(h)/tot, e, 100*float64(e)/tot,
			r, 100*float64(r)/tot, f, 100*float64(f)/tot)
	}
	shit, smiss := engine.JITElementStoreStats()
	if writes := shit + smiss; writes > 0 {
		fmt.Fprintf(os.Stderr, "jit: %d element stores, %d served by the emitted guard chain (%.1f%%)\n",
			writes, shit, 100*float64(shit)/float64(writes))
	}
	cfast, cslow := engine.JITCallStats()
	if calls := cfast + cslow; calls > 0 {
		fmt.Fprintf(os.Stderr, "jit: %d calls from compiled code, %d made by the call site itself (%.1f%%)\n",
			calls, cfast, 100*float64(cfast)/float64(calls))
	}
	// Why compiled code left, heaviest first. This is the number that says what
	// is worth speculating on next: a helper is a round trip, and the round trip
	// is the cost — see the note on JITHelperStats.
	if h := engine.JITHelperStats(); len(h) > 0 {
		var total uint64
		for _, e := range h {
			total += e.Count
		}
		fmt.Fprintf(os.Stderr, "jit: %d calls out of compiled code, by reason:\n", total)
		for i, e := range h {
			if i == 12 {
				fmt.Fprintf(os.Stderr, "jit:   … and %d more reasons\n", len(h)-i)
				break
			}
			fmt.Fprintf(os.Stderr, "jit:   %-14s %12d  %5.1f%%\n",
				e.Name, e.Count, 100*float64(e.Count)/float64(total))
		}
	}
	fast, slow := engine.JITOperatorStats()
	if ops := fast + slow; ops > 0 {
		fmt.Fprintf(os.Stderr, "jit: %d untyped operators, %d took the machine instruction (%.1f%%)\n",
			ops, fast, 100*float64(fast)/float64(ops))
	}
	// What the tier lost, weighted by how often the function ran rather than by
	// how many functions there were. Only the top few: the tail is long and
	// every entry in it is, by construction, cold.
	if w := engine.JITRefusalWeights(); len(w) > 0 {
		fmt.Fprintln(os.Stderr, "jit: interpreted work by refusal, heaviest first (insns is the one to read):")
		for i, r := range w {
			if i == 8 || (r.Insns == 0 && r.Entries == 0) {
				break
			}
			fmt.Fprintf(os.Stderr, "jit:   %-22s %13d insns %11d entries %11d unblocks (%d fns)\n",
				r.Reason, r.Insns, r.Entries, r.Unblocks, r.Funcs)
		}
	}
}

func stopProfile() {
	if profiling {
		pprof.StopCPUProfile()
		profiling = false
	}
}

func reportAndExit(err error) {
	stopProfile()
	var exit *engine.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.Code)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func fatal(err error) {
	stopProfile()
	fmt.Fprintln(os.Stderr, "goant:", err)
	os.Exit(1)
}
