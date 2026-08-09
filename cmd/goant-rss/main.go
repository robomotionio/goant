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

	goant "github.com/robomotionio/goant"
)

func main() {
	n := flag.Int("n", 1000000, "records to build")
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
	src := `
		const records = [];
		for (let i = 0; i < N; i++) records.push({ id: i, amount: i * 1.5 });
		let sum = 0;
		records.forEach(r => { sum += r.amount; });
		sum;
	`
	src = strings.Replace(src, "N", strconv.Itoa(*n), 1)

	rt := goant.New()
	defer rt.Close()
	if _, err := rt.RunScript("rss.js", src); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}

	st := rt.Stats()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	fmt.Printf("RSSRESULT\t%s\t%d\tjsMB=%.1f\tcodeMB=%.2f\tcodeBlocks=%d\tgoHeapMB=%.1f\tgoSysMB=%.1f\tgcs=%d\tpeakRSSMB=%.1f\n",
		name, *n,
		float64(st.Bytes)/(1<<20),
		float64(st.CodeBytes)/(1<<20), st.CodeBlocks,
		float64(ms.HeapAlloc)/(1<<20),
		float64(ms.Sys)/(1<<20),
		ms.NumGC,
		float64(peakRSSkB())/1024)
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
