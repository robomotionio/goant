// Command goant-rss answers one question: when the tier is on, where does the
// extra resident memory go?
//
// It runs one workload and then reports the three quantities separately,
// because they are three different things and a single RSS figure cannot tell
// them apart:
//
//   - the JavaScript heap the engine accounts for (cells and payload bytes),
//   - the executable memory the tier holds, which lives outside that
//     accounting and outside SetHeapLimit,
//   - Go's own heap, and the peak resident set of the whole process.
//
// The distinction that matters: a peak that moves with GOGC is the Go
// collector pacing itself, and a peak that does not is something being kept.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	goant "github.com/robomotionio/goant"
)

func main() {
	n := flag.Int("n", 1000000, "records to build")
	reps := flag.Int("reps", 1, "how many scopes to run, one after another")
	work := flag.String("workload", "build", "build | orders")
	limitMB := flag.Int("limitMB", 0, "heap limit in MB; 0 for none")
	label := flag.String("label", "", "row label; defaults to the tier state")
	flag.Parse()

	name := *label
	if name == "" {
		name = "goant"
		if os.Getenv("GOANT_JIT") == "1" {
			name = "goant+jit"
		}
		if g := os.Getenv("GOGC"); g != "" {
			name += "/GOGC=" + g
		}
	}

	// Built in JavaScript rather than handed in as a host message, so that what
	// is measured is the engine and not an embedder's message path.
	src := strings.Replace(buildSrc, "N", strconv.Itoa(*n), 1)
	var msg []byte
	if *work == "orders" {
		src = ordersSrc
		msg = orderJSON(*n)
	}

	// A limit is not just a ceiling: it switches the collector onto the byte
	// budget, so collections are scheduled by payload rather than by cell count.
	// Every host that runs this engine in production sets one — the Function
	// node passes 2 GiB — and a measurement taken without one is measuring a
	// configuration nobody ships.
	var opts []goant.Option
	if *limitMB > 0 {
		opts = append(opts, goant.WithMemoryLimit(uint64(*limitMB)<<20))
		name += fmt.Sprintf("/limit=%dMB", *limitMB)
	}
	rt := goant.New(opts...)
	defer rt.Close()

	// Compiled once and run many times, which is what a Function node does and
	// what the tier needs to exist at all: a function is compiled on its eighth
	// entry, and the counter belongs to the compiled function, not to the
	// source. Re-parsing per message makes a fresh one every time, so the
	// counter restarts at zero, nothing ever reaches eight, and the goant+jit
	// column quietly measures the interpreter. It did, for a while.
	prog, err := rt.Compile("rss.js", src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "compile:", err)
		os.Exit(1)
	}

	// One Scope per repetition, which is the shape a host actually runs: a
	// pooled Runtime handed a fresh global per message, with the region freed in
	// between. A single run measures a cold heap and misses everything that only
	// appears once the pools have been through a peak already — and it is the
	// only shape in which the tier is even on, since a function is compiled on
	// its eighth entry and one repetition never reaches it.
	polluted := 0
	start := time.Now()
	for i := 0; i < *reps; i++ {
		s, err := rt.NewScope()
		if err != nil {
			fmt.Fprintln(os.Stderr, "scope:", err)
			os.Exit(1)
		}
		if msg != nil {
			// Lazily, which is how the Function node hands a message over: the
			// document is scanned and each value built on the read that needs
			// it. What the loop touches it pays for, and the pattern of when
			// those allocations happen is exactly what is under investigation.
			v, perr := rt.ParseJSONLazy(msg)
			if perr != nil {
				fmt.Fprintln(os.Stderr, "parse:", perr)
				os.Exit(1)
			}
			if serr := s.Set("msg", v); serr != nil {
				fmt.Fprintln(os.Stderr, "set:", serr)
				os.Exit(1)
			}
		}
		if _, err := s.RunProgram(prog); err != nil {
			fmt.Fprintln(os.Stderr, "run:", err)
			os.Exit(1)
		}
		if s.Polluted() {
			polluted++
		}
		s.Close()
	}

	per := time.Since(start) / time.Duration(*reps)

	st := rt.Stats()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	fmt.Printf("RSSRESULT\t%s\t%d\tjsMB=%.1f\treservedCells=%d\treservedMB=%.1f\tjsGCs=%d\tcodeMB=%.2f\tcodeBlocks=%d\tgoHeapMB=%.1f\tgoSysMB=%.1f\tgcs=%d\tpeakRSSMB=%.1f\tmsPerRep=%.1f\tpolluted=%d\n",
		name, *n,
		float64(st.Bytes)/(1<<20),
		st.ReservedCells,
		float64(st.ReservedBytes)/(1<<20),
		st.Collections,
		float64(st.CodeBytes)/(1<<20), st.CodeBlocks,
		float64(ms.HeapAlloc)/(1<<20),
		float64(ms.Sys)/(1<<20),
		ms.NumGC,
		float64(peakRSSkB())/1024,
		float64(per.Microseconds())/1000, polluted)
}

// buildSrc allocates its own working set: N objects into an array, then a pass
// over them. Every cell is the engine's own, and nothing arrives from a host.
const buildSrc = `
	const records = [];
	for (let i = 0; i < N; i++) records.push({ id: i, amount: i * 1.5 });
	let sum = 0;
	records.forEach(r => { sum += r.amount; });
	sum;
`

// ordersSrc is the deskbot Function node, wrapped as a function so the tier
// compiles it the way it compiles a real one — on the eighth message.
//
// It is here verbatim rather than paraphrased because the paraphrase is what
// hid the problem: the synthetic loop above allocates one object per iteration
// and nothing else, and it showed the two tier arms agreeing to within a
// megabyte at every size. This one allocates two strings per row as well
// (toUpperCase, trim), reads its rows out of a lazily parsed document so each
// one is built on the read, and rolls the results up through map/sort/join. The
// difference between the two is the whole investigation.
const ordersSrc = `
	function main(msg) {
		const rows = msg.records;
		const byCustomer = {};
		let flagged = 0;

		for (let i = 0; i < rows.length; i++) {
			const r = rows[i];
			const amount = r.qty * r.price * (1 - (r.discount || 0));
			const key = r.customer.toUpperCase().trim();

			let acc = byCustomer[key];
			if (!acc) {
				acc = byCustomer[key] = { customer: key, total: 0, count: 0, max: 0, skus: [] };
			}
			acc.total += amount;
			acc.count += 1;
			if (amount > acc.max) acc.max = amount;
			if (amount > 1000 || r.qty > 50) {
				flagged += 1;
				acc.skus.push(r.sku);
			}
		}

		const summary = Object.keys(byCustomer).map(k => {
			const a = byCustomer[k];
			return {
				customer: a.customer,
				total: Math.round(a.total * 100) / 100,
				count: a.count,
				avg: Math.round((a.total / a.count) * 100) / 100,
				max: Math.round(a.max * 100) / 100,
				skus: a.skus.sort().join(","),
			};
		}).sort((x, y) => y.total - x.total);

		return { summary: summary, flagged: flagged, customers: summary.length };
	}
	main(msg).customers;
`

// orderJSON builds the message the node is given: n order lines, five
// customers, the same document the deskbot benchmark sends.
func orderJSON(n int) []byte {
	customers := []string{"acme", "globex", "initech", "umbrella", "soylent"}
	var b strings.Builder
	b.WriteString(`{"records":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"sku":"SKU-%05d","customer":%q,"qty":%d,"price":%.2f,"discount":%.2f}`,
			i, customers[i%len(customers)], 1+i%60,
			float64(5+i%97)+0.99, float64(i%5)/20.0)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// peakRSSkB reads VmHWM: the high-water mark, not the current figure, which has
// usually fallen back by the time anyone reads it.
func peakRSSkB() int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		return v
	}
	return 0
}
