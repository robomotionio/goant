// Command compatgen generates the compat-table conformance corpus
// (conformance/compat-table/<cat>/*.js) from the pinned kangax/compat-table data
// files (see TODO 0.3). It runs the node extract step (extract.cjs), maps each
// (feature, subtest) exec to one of the authoritative target names in
// conformance/ant-results.txt, and writes a self-contained runnable test.
//
// Fidelity by construction: a record is emitted only when its computed leaf name
// matches a real target leaf, so compatgen can never invent an off-spec name. It
// reports coverage (targets hit / total) and every unmapped target, which drives
// the curation of overrides in mapping.json.
//
// Usage:
//
//	go run ./tools/compatgen            # generate into conformance/compat-table
//	go run ./tools/compatgen -report    # print coverage without writing files
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// record is one extracted exec (matches extract.cjs output).
type record struct {
	DataFile string `json:"dataFile"`
	Category string `json:"category"`
	Feature  string `json:"feature"`
	Subtest  string `json:"subtest"`
	Exec     string `json:"exec"`
	IsAsync  bool   `json:"isAsync"`
}

func main() {
	var (
		report  = flag.Bool("report", false, "print coverage report only; do not write files")
		automap = flag.Bool("automap", false, "greedily match unmapped targets to records by token similarity, merge into mapping.json, then exit")
		thresh  = flag.Float64("thresh", 0.34, "minimum jaccard token similarity for -automap")
		outRoot = flag.String("out", "conformance/compat-table", "output root for generated tests")
		results = flag.String("results", "conformance/ant-results.txt", "authoritative target-name spec")
	)
	flag.Parse()

	pin := strings.TrimSpace(readFile("tools/compatgen/COMPAT_TABLE_COMMIT"))
	recs := runExtract()
	targets := loadTargets(*results) // leaf -> full "compat-table/<cat>/<leaf>.js"
	overrides := loadOverrides("tools/compatgen/mapping.json")

	if *automap {
		autoMap(recs, targets, overrides, *thresh)
		return
	}

	// Assign records to target leaves. A target is claimed by at most one record;
	// conflicting claims are reported and dropped (curate via mapping.json).
	claimedBy := map[string]*record{} // leaf -> record
	conflicts := map[string]int{}     // leaf -> claim count
	for i := range recs {
		r := &recs[i]
		leaf := mapLeaf(r, overrides)
		if leaf == "" {
			continue
		}
		if _, ok := targets[leaf]; !ok {
			continue
		}
		if _, taken := claimedBy[leaf]; taken {
			conflicts[leaf]++
			continue
		}
		claimedBy[leaf] = r
	}

	// Coverage.
	var hit, async int
	for leaf, r := range claimedBy {
		_ = leaf
		if r.IsAsync {
			async++
		}
		hit++
	}
	fmt.Fprintf(os.Stderr, "compatgen: %d/%d target leaves mapped (%d async deferred), %d conflicts\n",
		hit, len(targets), async, len(conflicts))

	if *report {
		reportUnmapped(targets, claimedBy)
		return
	}

	// Emit tests for claimed targets (sync + async).
	written := 0
	for leaf, r := range claimedBy {
		full := targets[leaf] // compat-table/<cat>/<leaf>.js
		rel := strings.TrimPrefix(full, "compat-table/")
		dst := filepath.Join(*outRoot, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(dst, []byte(genTest(r, pin)), 0o644); err != nil {
			fatal(err)
		}
		written++
	}
	fmt.Fprintf(os.Stderr, "compatgen: wrote %d files to %s\n", written, *outRoot)
}

// mapLeaf computes the target leaf name for a record: an explicit override wins,
// then an exact subtest/feature name (the cases where compat-table already uses
// the dotted path ant adopted).
func mapLeaf(r *record, overrides map[string]string) string {
	key := r.Feature
	if r.Subtest != "" {
		key = r.Feature + " :: " + r.Subtest
	}
	if o, ok := overrides[key]; ok {
		return o
	}
	if r.Subtest != "" {
		return r.Subtest // exact-match layer
	}
	return r.Feature
}

// recordKey is the mapping.json override key for a record.
func recordKey(r *record) string {
	if r.Subtest != "" {
		return r.Feature + " :: " + r.Subtest
	}
	return r.Feature
}

func tokenize(s string) map[string]bool {
	set := map[string]bool{}
	cur := []rune{}
	flush := func() {
		if len(cur) > 0 {
			set[string(cur)] = true
			cur = cur[:0]
		}
	}
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			cur = append(cur, c)
		} else {
			flush()
		}
	}
	flush()
	return set
}

func jaccard(a, b map[string]bool) float64 {
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// autoMap greedily assigns unmapped target leaves to unmapped records by token
// similarity (best pairs first, 1:1), merging results into mapping.json. Manual
// overrides already present are preserved and their targets/records excluded.
func autoMap(recs []record, targets map[string]string, overrides map[string]string, thresh float64) {
	usedLeaf := map[string]bool{}
	usedRec := map[string]bool{}

	// Exact layer + existing overrides already claim some targets/records.
	for i := range recs {
		r := &recs[i]
		leaf := mapLeaf(r, overrides)
		if _, ok := targets[leaf]; ok && !usedLeaf[leaf] {
			usedLeaf[leaf] = true
			usedRec[recordKey(r)] = true
		}
	}

	// Precompute record token sets for the still-unused records.
	type rtok struct {
		r *record
		t map[string]bool
	}
	var pool []rtok
	for i := range recs {
		r := &recs[i]
		if usedRec[recordKey(r)] {
			continue
		}
		pool = append(pool, rtok{r, tokenize(r.Feature + " " + r.Subtest)})
	}

	type pair struct {
		leaf  string
		key   string
		score float64
	}
	var pairs []pair
	for leaf := range targets {
		if usedLeaf[leaf] {
			continue
		}
		lt := tokenize(leaf)
		for _, rt := range pool {
			s := jaccard(lt, rt.t)
			if s >= thresh {
				pairs = append(pairs, pair{leaf, recordKey(rt.r), s})
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].score > pairs[j].score })

	added := 0
	for _, p := range pairs {
		if usedLeaf[p.leaf] || usedRec[p.key] {
			continue
		}
		usedLeaf[p.leaf] = true
		usedRec[p.key] = true
		overrides[p.key] = p.leaf
		added++
	}

	out, _ := json.MarshalIndent(overrides, "", "  ")
	if err := os.WriteFile("tools/compatgen/mapping.json", out, 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "automap: added %d mappings (%d total); wrote mapping.json\n", added, len(overrides))
}

// ---- generated test source ----

const preamble = `// Code generated by tools/compatgen from kangax/compat-table; DO NOT EDIT.
// SPDX-License-Identifier: MIT  (https://github.com/kangax/compat-table)
var global = this;
if (typeof globalThis !== "undefined") { global = globalThis; }
global.global = global;
global.__createIterableObject = function (arr, methods) {
  methods = methods || {};
  if (typeof Symbol !== "function" || !Symbol.iterator) { return {}; }
  arr.length++;
  var iterator = {
    next: function () { return { value: arr.shift(), done: arr.length <= 0 }; },
    "return": methods["return"],
    "throw": methods["throw"]
  };
  var iterable = {};
  iterable[Symbol.iterator] = function () { return iterator; };
  return iterable;
};
`

func genTest(r *record, pin string) string {
	var b strings.Builder
	b.WriteString(preamble)
	fmt.Fprintf(&b, "// pin: %s\n// feature: %s\n", pin, r.Feature)
	if r.Subtest != "" {
		fmt.Fprintf(&b, "// subtest: %s\n", r.Subtest)
	}
	if r.IsAsync {
		// Async: the exec signals success via asyncTestPassed(); if the pending
		// flag is still set after the microtask queue drains, it failed (handled
		// by the host in RunString).
		b.WriteString("globalThis.__asyncTestPending = true;\n")
		b.WriteString("global.asyncTestPassed = function () { globalThis.__asyncTestPending = false; };\n")
		b.WriteString("(function () {")
		b.WriteString(r.Exec)
		b.WriteString("\n})();\n")
		return b.String()
	}
	b.WriteString("var __ok = (function () {")
	b.WriteString(r.Exec)
	b.WriteString("\n})();\n")
	// compat-table's pass criterion is truthiness (execs often return 1 via `&=`).
	b.WriteString("if (!__ok) { throw new Error(\"compat-table check failed: \" + __ok); }\n")
	return b.String()
}

// ---- inputs ----

func runExtract() []record {
	cmd := exec.Command("node", "tools/compatgen/extract.cjs")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fatal(fmt.Errorf("extract.cjs: %w", err))
	}
	var recs []record
	if err := json.Unmarshal(out, &recs); err != nil {
		fatal(fmt.Errorf("parse extract output: %w", err))
	}
	return recs
}

// loadTargets returns leaf -> full compat-table name from the spec file.
func loadTargets(path string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(readFile(path), "\n") {
		name := strings.SplitN(strings.TrimSpace(line), ":", 2)[0]
		if !strings.HasPrefix(name, "compat-table/") {
			continue
		}
		leaf := name
		if i := strings.Index(strings.TrimPrefix(name, "compat-table/"), "/"); i >= 0 {
			leaf = strings.TrimPrefix(name, "compat-table/")[i+1:]
		}
		leaf = strings.TrimSuffix(leaf, ".js")
		m[leaf] = name
	}
	return m
}

func loadOverrides(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		fatal(fmt.Errorf("parse %s: %w", path, err))
	}
	return m
}

func reportUnmapped(targets map[string]string, claimed map[string]*record) {
	var miss []string
	for leaf := range targets {
		if _, ok := claimed[leaf]; !ok {
			miss = append(miss, leaf)
		}
	}
	sort.Strings(miss)
	fmt.Fprintf(os.Stderr, "compatgen: %d unmapped targets:\n", len(miss))
	for _, m := range miss {
		fmt.Println(m)
	}
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	return string(b)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "compatgen:", err)
	os.Exit(1)
}
