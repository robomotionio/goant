package main

// The comparison engines are a baseline, not a measurement.
//
// goant is the only column that ever moves between two runs of this tool.
// Re-running goja, ant, duktape and node alongside it measures nothing and is
// not cheap: goja's zlib run takes over five minutes against goant's fifty
// seconds, and before -timeout existed ant would sit on mandreel for a quarter
// of an hour. A table of five engines cost the wall clock of its slowest four
// columns, every single time, to reprint numbers that had not changed.
//
// So every cell is measured once and written to a file, and a later run reads
// it back. What makes that safe rather than merely fast is what the key is made
// of:
//
//   - the machine, because a laptop score and a bench-VM score are not the same
//     measurement, and mixing them silently would be worse than not caching;
//   - the script, hashed, so changing a workload or the harness that assembles
//     it invalidates every engine's number for it;
//   - and, for goant, the binary, hashed, so a rebuild invalidates the goant
//     column and nothing else.
//
// That last one is also what makes a run resumable. A table takes hours and a
// dropped ssh connection kills it; restarting reads back every cell already
// measured and carries on at the first one that is not. Nothing is re-timed
// that has not changed, and nothing changed is ever read from the file.
// -refresh forces the lot to be re-measured anyway.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// baselineFile is where the cached comparison scores live, next to the
// workloads they describe.
const baselineFile = "baseline.json"

// baseline is the cached result for every engine but goant. Values are the
// number the table prints — a score for octane, nanoseconds for micro — and
// errFailed marks a cell that failed or timed out, so a workload an engine
// cannot run is not retried for the timeout on every future run.
type baseline struct {
	// Host is where these numbers were taken. A cache from another machine is
	// ignored rather than merged: cross-machine numbers are not comparable.
	Host   string             `json:"host"`
	Scores map[string]float64 `json:"scores"` // "suite/workload/engine" -> value

	path   string
	dirty  bool
	reused map[string]bool // engines whose numbers came from the file
}

// errFailed is the cached stand-in for an engine that could not run a workload.
// Remembering a failure matters as much as remembering a score: duktape cannot
// compile mandreel and ant cannot load half the suite, and without this each of
// those cells would be re-attempted — up to the whole -timeout — on every run.
const errFailed = -1

// cell names one square of the table: which suite and workload, and the hash of
// the exact script that was handed to the engines.
type cell struct {
	suite, workload, script string
}

// cachedRun produces one cell. It is run only when the file has nothing for it,
// which is once per machine per version of the thing being measured — including
// the first time an engine appears at all, so adding a runtime to the table
// measures that runtime and nothing else.
func cachedRun(bl *baseline, refresh bool, e engine, c cell, run func() (float64, error)) (float64, error) {
	name := engineKey(e)
	if !refresh {
		if v, ok := bl.get(c, name); ok {
			bl.reused[e.name] = true
			if v == errFailed {
				return 0, fmt.Errorf("%s: cached failure", e.name)
			}
			return v, nil
		}
	}
	v, err := run()
	if err != nil {
		bl.put(c, name, errFailed)
		return 0, err
	}
	bl.put(c, name, v)
	return v, nil
}

// goantBuild is the goant binary's content hash, filled in by hashBinary. It is
// part of goant's cache key, so the column is reused across a restart and
// dropped the moment the binary is rebuilt.
var goantBuild string

// engineKey is the engine's name, with the build stamped on for goant — the one
// engine that is expected to differ from run to run.
func engineKey(e engine) string {
	if e.name == "goant" && goantBuild != "" {
		return "goant@" + goantBuild
	}
	return e.name
}

// hashBinary is the short content hash of a file, or "" if it cannot be read —
// in which case goant is simply never cached, which is the safe direction.
func hashBinary(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

// hashBytes is hashBinary for a script this tool assembled in memory.
func hashBytes(b []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(b))[:12]
}

// note says which columns were read rather than measured, so a table is never
// silently older than it looks.
func (b *baseline) note() {
	if len(b.reused) == 0 {
		return
	}
	names := make([]string, 0, len(b.reused))
	for n := range b.reused {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("%s read from %s (measured on %s); -refresh to re-measure\n",
		strings.Join(names, ", "), b.path, b.Host)
}

// pick narrows the engines to a comma-separated list of names. The list is
// honoured exactly — asking for goant alone is the point of it, and it is the
// difference between timing a change and timing four engines that did not
// change.
func pick(engines []engine, want string) []engine {
	if want == "" {
		return engines
	}
	keep := map[string]bool{}
	for _, n := range strings.Split(want, ",") {
		keep[strings.TrimSpace(n)] = true
	}
	var out []engine
	for _, e := range engines {
		if keep[e.name] {
			out = append(out, e)
		}
	}
	return out
}

func loadBaseline(dir string) *baseline {
	b := &baseline{
		Scores: map[string]float64{},
		path:   filepath.Join(dir, baselineFile),
		reused: map[string]bool{},
	}
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	data, err := os.ReadFile(b.path)
	if err != nil {
		b.Host = host
		return b
	}
	var stored baseline
	if err := json.Unmarshal(data, &stored); err != nil || stored.Host != host {
		// Unreadable, or taken somewhere else. Start again here rather than
		// reporting one machine's numbers beside another's.
		b.Host = host
		return b
	}
	b.Host, b.Scores = stored.Host, stored.Scores
	if b.Scores == nil {
		b.Scores = map[string]float64{}
	}
	return b
}

func (c cell) key(engine string) string {
	return c.suite + "/" + c.workload + "/" + c.script + "/" + engine
}

// get returns a cached value and whether there was one. ok is true even for a
// remembered failure, which the caller tells apart by the value.
func (b *baseline) get(c cell, engine string) (float64, bool) {
	v, ok := b.Scores[c.key(engine)]
	return v, ok
}

func (b *baseline) put(c cell, engine string, v float64) {
	b.Scores[c.key(engine)] = v
	b.dirty = true
	// Drop what this supersedes: the same square measured against an older
	// script, or against an older build of goant. Without it the file grows by a
	// full table every time anything is rebuilt.
	prefix := c.suite + "/" + c.workload + "/"
	keep := c.key(engine)
	for k := range b.Scores {
		if k != keep && strings.HasPrefix(k, prefix) && supersedes(k, c, engine) {
			delete(b.Scores, k)
		}
	}
}

// supersedes reports whether the new entry for engine in c replaces the stored
// key k — same square and same engine, but a stale script or build.
func supersedes(k string, c cell, engine string) bool {
	parts := strings.Split(k, "/")
	if len(parts) != 4 {
		return false
	}
	oldEngine := parts[3]
	if oldEngine == engine {
		return true // same engine, different script hash
	}
	// goant@<build>: the same engine under a build that no longer exists.
	return strings.HasPrefix(engine, "goant@") && strings.HasPrefix(oldEngine, "goant@")
}

// save writes the cache back, if anything was added. Called after every
// workload so a run that is interrupted still leaves what it learned behind.
func (b *baseline) save() {
	if !b.dirty {
		return
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(b.path, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "could not write %s: %v\n", b.path, err)
		return
	}
	b.dirty = false
}
