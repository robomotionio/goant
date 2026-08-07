// Command goant-soak runs the tier the way a host runs it: pooled Runtimes,
// many short invocations, for hours.
//
// Everything else that exercises the tier gets it wrong in the same way. The
// unit tests are one Runtime on one goroutine for milliseconds. The fuzzer
// builds a FRESH Runtime per input, which is the one shape a host never uses.
// test262 runs a process per test. None of them reach the combination a
// long-running embedder is actually made of:
//
//   - a Runtime that lives for days, so compiled code is compiled once and
//     entered millions of times, across invocations that know nothing about it;
//   - Invocation.Release, which is REGION RECLAMATION — it rewinds the object,
//     string, closure and bigint pools, recycling cells that compiled code's
//     inline caches were filled from. A call site holds a callee handle and a
//     raw entry address; a jitCallee holds a *closure straight into the pool
//     being rewound. The epoch bump in End is what retires all of that, and the
//     ordering it depends on (retire, THEN truncate) is the sort of invariant
//     that holds until someone reorders two lines;
//   - several of those at once, on different goroutines, with the collector
//     unmapping the code of whatever has been dropped.
//
// So this is not a benchmark and not a correctness suite. It is the question
// "does it still give the right answer after nine hours of that", asked
// continuously, with the numbers a host would alarm on printed as it goes.
//
//	goant-soak -hours 12 -workers 4
//	goant-soak -hours 1 -workers 2 -report 30s
//
// Every flow is deterministic and its expected answer is computed once at
// startup WITH THE TIER OFF, so the oracle is the interpreter, exactly as in
// the fuzzer. A mismatch is a miscompilation or a pool-recycling bug, and the
// process exits non-zero naming the flow and the iteration.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robomotionio/goant/internal/engine"
)

// flows are meant to look like work, not like a fuzzer's output. Each is
// deterministic, allocates a message-shaped graph, and returns a string —
// which matters for Release, since every Value a run created is invalid
// afterwards and only bytes extracted beforehand survive.
var flows = []struct {
	name string
	src  string
}{
	{"json-roundtrip", `
		(function () {
			var rows = [];
			for (var i = 0; i < 200; i++) {
				rows.push({ id: i, name: "item-" + i, tags: ["a", "b"], score: i * 1.5, ok: i % 3 === 0 });
			}
			var s = JSON.stringify({ rows: rows });
			var back = JSON.parse(s);
			var t = 0;
			for (var i = 0; i < back.rows.length; i++) t += back.rows[i].score + back.rows[i].id;
			return "" + back.rows.length + "|" + t + "|" + s.length;
		})();`},
	{"string-build", `
		(function () {
			var out = "";
			for (var i = 0; i < 2000; i++) out += (i % 10);
			var parts = out.split("9");
			var n = 0;
			for (var i = 0; i < parts.length; i++) n += parts[i].length;
			return "" + out.length + "|" + parts.length + "|" + n + "|" + out.slice(0, 12);
		})();`},
	{"array-pipeline", `
		(function () {
			var a = [];
			for (var i = 0; i < 1000; i++) a.push(i);
			var b = a.filter(function (x) { return x % 3 === 0; })
					 .map(function (x) { return x * x; })
					 .reduce(function (s, x) { return s + x; }, 0);
			a.sort(function (x, y) { return y - x; });
			return "" + b + "|" + a[0] + "|" + a[999] + "|" + a.length;
		})();`},
	{"object-shapes", `
		(function () {
			function P(a, b) { this.a = a; this.b = b; }
			P.prototype.sum = function () { return this.a + this.b; };
			var t = 0, os = [];
			for (var i = 0; i < 500; i++) {
				var o = new P(i, i * 2);
				if (i % 5 === 0) o.extra = i;    // a shape transition partway
				os.push(o);
			}
			for (var i = 0; i < os.length; i++) t += os[i].sum() + (os[i].extra || 0);
			return "" + t + "|" + os.length;
		})();`},
	{"numeric-kernel", `
		(function () {
			var a = new Float64Array(256), b = new Int32Array(256);
			for (var i = 0; i < 256; i++) { a[i] = i * 0.5; b[i] = i; }
			var s = 0;
			for (var r = 0; r < 40; r++) {
				for (var i = 1; i < 255; i++) {
					a[i] = (a[i - 1] + a[i] + a[i + 1]) / 3;
					b[i] = (b[i - 1] ^ b[i] ^ b[i + 1]) & 0xffff;
					s += a[i] + b[i];
				}
			}
			return "" + s.toFixed(6) + "|" + a[128].toFixed(6) + "|" + b[128];
		})();`},
	{"recursion-and-closures", `
		(function () {
			function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2); }
			var fs = [];
			for (let i = 0; i < 20; i++) fs.push(function () { return i * fib(10); });
			var t = 0;
			for (var i = 0; i < fs.length; i++) t += fs[i]();
			return "" + fib(20) + "|" + t;
		})();`},
	{"regexp-and-text", `
		(function () {
			var text = "";
			for (var i = 0; i < 100; i++) text += "line " + i + ": value=" + (i * 7) + ";\n";
			var re = /value=(\d+)/g, m, t = 0, n = 0;
			while ((m = re.exec(text)) !== null) { t += parseInt(m[1], 10); n++; }
			var cleaned = text.replace(/line \d+: /g, "").replace(/;\n/g, ",");
			return "" + t + "|" + n + "|" + cleaned.length;
		})();`},
	{"exceptions", `
		(function () {
			var caught = 0, t = 0;
			for (var i = 0; i < 300; i++) {
				try {
					if (i % 7 === 0) throw new TypeError("no " + i);
					t += i;
				} catch (e) { caught++; t -= 1; }
				finally { t += 0.5; }
			}
			return "" + caught + "|" + t;
		})();`},
}

func main() {
	var (
		hours    = flag.Float64("hours", 12, "how long to run")
		workers  = flag.Int("workers", 4, "concurrent pooled Runtimes")
		report   = flag.Duration("report", time.Minute, "how often to print the numbers")
		relFreq  = flag.Int("release-every", 3, "call Invocation.Release instead of End on every Nth run (0 = never)")
		freshEvy = flag.Int("fresh-runtime-every", 0, "discard and rebuild each worker's Runtime every N runs (0 = never)")
	)
	flag.Parse()

	if !engine.JITIsEnabled() {
		fmt.Fprintln(os.Stderr, "goant-soak: the tier is OFF; set GOANT_JIT=1 or this measures the interpreter")
	}

	// The oracle, computed once with the tier off. Same design as the fuzzer:
	// the interpreter is the answer key, so a mismatch is the tier's fault by
	// construction rather than a judgement call.
	want := make([]string, len(flows))
	func() {
		defer engine.JITSetEnabled(engine.JITSetEnabled(false))
		for i, f := range flows {
			rt := engine.New()
			v, err := rt.RunString(f.name+".js", f.src)
			if err != nil {
				fatal("flow %q failed interpreted: %v", f.name, err)
			}
			s, err := rt.ToString(v)
			if err != nil {
				fatal("flow %q result not a string: %v", f.name, err)
			}
			want[i] = s
			runtime.KeepAlive(rt)
		}
	}()

	fmt.Printf("goant-soak: %s/%s, %d workers, %.1fh, tier=%v, release every %d, fresh Runtime every %d\n",
		runtime.GOOS, runtime.GOARCH, *workers, *hours, engine.JITIsEnabled(), *relFreq, *freshEvy)
	fmt.Printf("%d flows, answers pinned against the interpreter\n\n", len(flows))

	var runs, released, rebuilt, dirty atomic.Int64
	var failures atomic.Int64
	deadline := time.Now().Add(time.Duration(*hours * float64(time.Hour)))
	stop := make(chan struct{})

	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rt, scripts := newWorker()
			for n := 0; time.Now().Before(deadline); n++ {
				select {
				case <-stop:
					return
				default:
				}
				fi := n % len(flows)

				inv := rt.BeginInvocation()
				v, err := rt.RunScript(scripts[fi])
				if err != nil {
					failures.Add(1)
					fmt.Printf("FAIL worker %d run %d flow %q: %v\n", id, n, flows[fi].name, err)
					inv.End()
					close(stop)
					return
				}
				// Extracted BEFORE Release: every Value the run made is invalid
				// afterwards, and reading one is the documented way to get a
				// wrong answer out of this API.
				got, err := rt.ToString(v)
				if err != nil {
					failures.Add(1)
					fmt.Printf("FAIL worker %d run %d flow %q: result: %v\n", id, n, flows[fi].name, err)
					inv.End()
					close(stop)
					return
				}
				if got != want[fi] {
					failures.Add(1)
					fmt.Printf("FAIL worker %d run %d flow %q:\n  got  %q\n  want %q\n",
						id, n, flows[fi].name, got, want[fi])
					inv.End()
					close(stop)
					return
				}
				if inv.Dirty() {
					dirty.Add(1)
				}
				// Release is the interesting path: it rewinds the pools that
				// compiled code's caches were filled from. End is the fallback,
				// and Release falls back to it by itself when a run touched
				// shared state.
				if *relFreq > 0 && n%*relFreq == 0 {
					if inv.Release() {
						released.Add(1)
					}
				} else {
					inv.End()
				}
				runs.Add(1)

				if *freshEvy > 0 && n > 0 && n%*freshEvy == 0 {
					rt, scripts = newWorker()
					rebuilt.Add(1)
				}
			}
		}(w)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(*report)
		defer t.Stop()
		start := time.Now()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				printStatus(start, deadline, runs.Load(), released.Load(), rebuilt.Load(), dirty.Load())
				if time.Now().After(deadline) {
					return
				}
			}
		}
	}()

	wg.Wait()
	select {
	case <-stop:
	default:
		close(stop)
	}
	<-done

	fmt.Println()
	printStatus(time.Now(), deadline, runs.Load(), released.Load(), rebuilt.Load(), dirty.Load())
	if n := failures.Load(); n > 0 {
		fmt.Printf("\nSOAK FAILED: %d mismatches\n", n)
		os.Exit(1)
	}
	fmt.Printf("\nSOAK CLEAN: %d runs, no disagreement with the interpreter\n", runs.Load())
}

// newWorker is one pooled host: a Runtime that will live for the whole run, with
// every flow compiled once. Compiling up front is the point — a host does not
// re-parse a flow it has already loaded, and it is what makes the tier compile
// once and be entered forever.
func newWorker() (*engine.Runtime, []*engine.Script) {
	rt := engine.New()
	scripts := make([]*engine.Script, len(flows))
	for i, f := range flows {
		sc, err := rt.CompileScript(f.name+".js", f.src)
		if err != nil {
			fatal("compiling %q: %v", f.name, err)
		}
		scripts[i] = sc
	}
	return rt, scripts
}

// printStatus prints what a host would alarm on. Code memory is separate from
// the Go heap on purpose: they are the two numbers that moved independently
// when this last went wrong, and a single total would have hidden it.
func printStatus(start, deadline time.Time, runs, released, rebuilt, dirty int64) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	blocks, bytes, peak := engine.JITCodeMemory()
	left := time.Until(deadline).Truncate(time.Second)
	if left < 0 {
		left = 0
	}
	fmt.Printf("%s  runs %-9d released %-8d rebuilt %-5d dirty %-6d | "+
		"code %5.1f MB / %-6d blocks (peak %5.1f) | go heap %5.1f MB  sys %5.1f MB | %s left\n",
		time.Now().UTC().Format("15:04:05"), runs, released, rebuilt, dirty,
		float64(bytes)/(1<<20), blocks, float64(peak)/(1<<20),
		float64(ms.HeapAlloc)/(1<<20), float64(ms.Sys)/(1<<20), left)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "goant-soak: "+format+"\n", a...)
	os.Exit(1)
}
