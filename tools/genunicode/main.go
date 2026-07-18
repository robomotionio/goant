// Command genunicode reads a Unicode Character Database (UCD) directory and
// generates internal/regexpjs/unicode17_gen.go: the RangeTables for every
// ECMAScript-permitted Unicode property escape (\p{…}) at the Unicode version of
// the input files. Test262's property-escapes tests are generated against a
// specific Unicode version (currently 17.0.0), newer than the Go toolchain's
// bundled unicode package, so the property data must come from the real UCD.
//
// Usage:
//
//	go run ./tools/genunicode -ucd <dir> -out internal/regexpjs/unicode17_gen.go
//
// The <dir> must contain (downloaded from https://www.unicode.org/Public/<ver>/ucd/):
//
//	extracted/DerivedGeneralCategory.txt  Scripts.txt  ScriptExtensions.txt
//	PropList.txt  DerivedCoreProperties.txt  DerivedNormalizationProps.txt
//	PropertyValueAliases.txt  UnicodeData.txt  emoji/emoji-data.txt
//
// The generated file is committed; regenerate only when bumping Unicode version.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type rng struct{ lo, hi rune }

// coalesce sorts and merges adjacent/overlapping ranges into a minimal set.
func coalesce(rs []rng) []rng {
	if len(rs) == 0 {
		return nil
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].lo < rs[j].lo })
	out := []rng{rs[0]}
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.lo <= last.hi+1 {
			if r.hi > last.hi {
				last.hi = r.hi
			}
		} else {
			out = append(out, r)
		}
	}
	return out
}

// complement returns the ranges in [0, 0x10FFFF] not covered by rs.
func complement(rs []rng) []rng {
	c := coalesce(rs)
	var out []rng
	next := rune(0)
	for _, r := range c {
		if r.lo > next {
			out = append(out, rng{next, r.lo - 1})
		}
		if r.hi+1 > next {
			next = r.hi + 1
		}
	}
	if next <= 0x10FFFF {
		out = append(out, rng{next, 0x10FFFF})
	}
	return out
}

// parseCodePoints parses "AAAA" or "AAAA..BBBB" into a range.
func parseCodePoints(s string) (rng, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ".."); i >= 0 {
		lo, err := strconv.ParseInt(s[:i], 16, 32)
		if err != nil {
			return rng{}, err
		}
		hi, err := strconv.ParseInt(s[i+2:], 16, 32)
		if err != nil {
			return rng{}, err
		}
		return rng{rune(lo), rune(hi)}, nil
	}
	v, err := strconv.ParseInt(s, 16, 32)
	if err != nil {
		return rng{}, err
	}
	return rng{rune(v), rune(v)}, nil
}

// forEachDataLine calls fn(cols) for every non-comment, non-blank line, where
// cols are the ';'-separated fields with the trailing comment stripped.
func forEachDataLine(path string, fn func(cols []string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cols := strings.Split(line, ";")
		for i := range cols {
			cols[i] = strings.TrimSpace(cols[i])
		}
		fn(cols)
	}
	return sc.Err()
}

// A propData collects ranges keyed by property (or property value) name.
type propData map[string][]rng

func (p propData) add(name string, r rng) { p[name] = append(p[name], r) }

func main() {
	ucd := flag.String("ucd", "", "path to a UCD directory")
	out := flag.String("out", "", "output Go file")
	pkg := flag.String("pkg", "regexpjs", "package name")
	flag.Parse()
	if *ucd == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: genunicode -ucd <dir> -out <file>")
		os.Exit(2)
	}

	p := filepath.Join
	// --- General_Category (extracted/DerivedGeneralCategory.txt) ---
	cats := propData{}
	must(forEachDataLine(p(*ucd, "extracted", "DerivedGeneralCategory.txt"), func(c []string) {
		if len(c) < 2 {
			return
		}
		r, err := parseCodePoints(c[0])
		must(err)
		cats.add(c[1], r)
	}))
	// Derive the group categories (L, LC, M, N, P, S, Z, C) from the specific ones.
	groups := map[string][]string{
		"L":  {"Lu", "Ll", "Lt", "Lm", "Lo"},
		"LC": {"Lu", "Ll", "Lt"},
		"M":  {"Mn", "Mc", "Me"},
		"N":  {"Nd", "Nl", "No"},
		"P":  {"Pc", "Pd", "Ps", "Pe", "Pi", "Pf", "Po"},
		"S":  {"Sm", "Sc", "Sk", "So"},
		"Z":  {"Zs", "Zl", "Zp"},
		"C":  {"Cc", "Cf", "Cs", "Co", "Cn"},
	}
	for g, members := range groups {
		for _, m := range members {
			cats[g] = append(cats[g], cats[m]...)
		}
	}
	// Assigned = every code point whose gc is not Cn.
	var assigned []rng
	for name, rs := range cats {
		if name == "Cn" || len(name) != 2 {
			continue // skip Cn and the derived group names
		}
		assigned = append(assigned, rs...)
	}

	// --- Scripts (Scripts.txt) → keyed by full script name ---
	scripts := propData{}
	must(forEachDataLine(p(*ucd, "Scripts.txt"), func(c []string) {
		if len(c) < 2 {
			return
		}
		r, err := parseCodePoints(c[0])
		must(err)
		scripts.add(c[1], r)
	}))

	// --- Value aliases (from PropertyValueAliases.txt) ---
	// Every alias column maps to the canonical name: for scripts the long name
	// (column 3, matching Scripts.txt), for general categories the short code
	// (column 2, matching u17Categories' keys). Deprecated aliases (Qaac→Coptic,
	// Qaai→Inherited) and the punctuation-style ones (cntrl, digit, punct) live in
	// the trailing columns, so all of them must be captured.
	scAlias := map[string]string{}  // any script alias → full name
	catAlias := map[string]string{} // any gc alias → short code
	must(forEachDataLine(p(*ucd, "PropertyValueAliases.txt"), func(c []string) {
		if len(c) < 3 {
			return
		}
		switch c[0] {
		case "sc":
			full := c[2]
			for _, a := range c[1:] {
				scAlias[a] = full
			}
		case "gc":
			short := c[1]
			for _, a := range c[1:] {
				catAlias[a] = short
			}
		}
	}))

	// --- Script_Extensions (ScriptExtensions.txt + Scripts default) ---
	// scx(cp) = the explicit short-code list if listed, else {Script(cp)}.
	explicit := map[rune]bool{}
	scx := propData{}
	must(forEachDataLine(p(*ucd, "ScriptExtensions.txt"), func(c []string) {
		if len(c) < 2 {
			return
		}
		r, err := parseCodePoints(c[0])
		must(err)
		codes := strings.Fields(c[1])
		for cp := r.lo; cp <= r.hi; cp++ {
			explicit[cp] = true
		}
		for _, code := range codes {
			full := scAlias[code]
			if full == "" {
				full = code
			}
			scx.add(full, r)
		}
	}))
	// Add the default contribution: cps with Script==S and not explicitly listed.
	for name, rs := range scripts {
		for _, r := range rs {
			for cp := r.lo; cp <= r.hi; cp++ {
				if !explicit[cp] {
					scx.add(name, rng{cp, cp})
				}
			}
		}
	}
	// Script=Unknown (Zzzz) covers every code point with no assigned script — the
	// complement of the union of all script ranges. A code point outside
	// ScriptExtensions.txt (all of these are) has scx == {Script}, so scx=Unknown
	// is the same set.
	var allScript []rng
	for _, rs := range scripts {
		allScript = append(allScript, rs...)
	}
	unknown := complement(allScript)
	scripts["Unknown"] = unknown
	scx["Unknown"] = unknown
	scAlias["Zzzz"] = "Unknown"
	scAlias["Unknown"] = "Unknown"

	// --- Binary properties ---
	bin := propData{}
	// Simple list files: name is column 1, only keep ES-permitted properties.
	collect := func(file string, want map[string]bool) {
		must(forEachDataLine(p(*ucd, file), func(c []string) {
			if len(c) < 2 || !want[c[1]] {
				return
			}
			r, err := parseCodePoints(c[0])
			must(err)
			bin.add(c[1], r)
		}))
	}
	collect("PropList.txt", map[string]bool{
		"ASCII_Hex_Digit": true, "Bidi_Control": true, "Dash": true,
		"Deprecated": true, "Diacritic": true, "Extender": true,
		"Hex_Digit": true, "Ideographic": true, "IDS_Binary_Operator": true,
		"IDS_Trinary_Operator": true, "Join_Control": true,
		"Logical_Order_Exception": true, "Noncharacter_Code_Point": true,
		"Pattern_Syntax": true, "Pattern_White_Space": true,
		"Quotation_Mark": true, "Radical": true, "Regional_Indicator": true,
		"Sentence_Terminal": true, "Soft_Dotted": true,
		"Terminal_Punctuation": true, "Unified_Ideograph": true,
		"Variation_Selector": true, "White_Space": true,
	})
	collect("DerivedCoreProperties.txt", map[string]bool{
		"Alphabetic": true, "Cased": true, "Case_Ignorable": true,
		"Changes_When_Casefolded": true, "Changes_When_Casemapped": true,
		"Changes_When_Lowercased": true, "Changes_When_Titlecased": true,
		"Changes_When_Uppercased": true, "Default_Ignorable_Code_Point": true,
		"Grapheme_Base": true, "Grapheme_Extend": true, "ID_Continue": true,
		"ID_Start": true, "Lowercase": true, "Math": true, "Uppercase": true,
		"XID_Continue": true, "XID_Start": true,
	})
	collect("DerivedNormalizationProps.txt", map[string]bool{
		"Changes_When_NFKC_Casefolded": true,
	})
	collect(filepath.Join("emoji", "emoji-data.txt"), map[string]bool{
		"Emoji": true, "Emoji_Component": true, "Emoji_Modifier": true,
		"Emoji_Modifier_Base": true, "Emoji_Presentation": true,
		"Extended_Pictographic": true,
	})
	// Bidi_Mirrored comes from UnicodeData.txt field 9 ("Y"), with First/Last
	// range pairs spelled across two consecutive lines.
	var firstBM rune = -1
	must(forEachDataLine(p(*ucd, "UnicodeData.txt"), func(c []string) {
		if len(c) < 10 {
			return
		}
		cp, err := strconv.ParseInt(c[0], 16, 32)
		must(err)
		name := c[1]
		mirrored := c[9] == "Y"
		switch {
		case strings.HasSuffix(name, ", First>"):
			if mirrored {
				firstBM = rune(cp)
			}
		case strings.HasSuffix(name, ", Last>"):
			if firstBM >= 0 {
				bin.add("Bidi_Mirrored", rng{firstBM, rune(cp)})
				firstBM = -1
			}
		default:
			if mirrored {
				bin.add("Bidi_Mirrored", rng{rune(cp), rune(cp)})
			}
		}
	}))

	// --- Emoji "properties of strings" (v-flag) ---
	// Each maps to a regexp2 alternation of the literal code-point sequences the
	// property matches. RGI_Emoji is the union of all the others (including
	// keycaps), per UTS #51.
	sets := map[string]*seqSet{}
	seqOf := func(field string) *seqSet {
		if sets[field] == nil {
			sets[field] = &seqSet{}
		}
		return sets[field]
	}
	readSeqs := func(file string, want map[string]bool) {
		must(forEachDataLine(p(*ucd, file), func(c []string) {
			if len(c) < 2 || !want[c[1]] {
				return
			}
			addEmojiField(seqOf(c[1]), c[0])
		}))
	}
	emojiTypes := map[string]bool{
		"Basic_Emoji": true, "Emoji_Keycap_Sequence": true,
		"RGI_Emoji_Flag_Sequence": true, "RGI_Emoji_Tag_Sequence": true,
		"RGI_Emoji_Modifier_Sequence": true,
	}
	readSeqs(filepath.Join("emoji", "emoji-sequences.txt"), emojiTypes)
	readSeqs(filepath.Join("emoji", "emoji-zwj-sequences.txt"), map[string]bool{"RGI_Emoji_ZWJ_Sequence": true})
	// RGI_Emoji = union of every component set.
	rgi := &seqSet{}
	for _, s := range sets {
		rgi.seqs = append(rgi.seqs, s.seqs...)
		rgi.single = append(rgi.single, s.single...)
	}
	sets["RGI_Emoji"] = rgi
	stringProps := map[string]string{}
	for name, s := range sets {
		stringProps[name] = emojiPattern(s)
	}

	// --- Emit ---
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by tools/genunicode from the Unicode 17.0.0 UCD. DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", *pkg)
	fmt.Fprintf(&b, "import \"unicode\"\n\n")

	emitMap(&b, "u17Categories", cats)
	emitTable(&b, "u17Assigned", assigned)
	emitMap(&b, "u17Scripts", scripts)
	emitMap(&b, "u17ScriptExtensions", scx)
	emitMap(&b, "u17Binary", bin)
	emitStrMap(&b, "u17ScriptAlias", scAlias)
	emitStrMap(&b, "u17CategoryAlias", catAlias)
	emitStrMap(&b, "u17StringProperties", stringProps)

	src, err := format.Source(b.Bytes())
	if err != nil {
		// Emit unformatted so the error is inspectable.
		os.WriteFile(*out, b.Bytes(), 0o644)
		must(err)
	}
	must(os.WriteFile(*out, src, 0o644))
	fmt.Printf("wrote %s: %d categories, %d scripts, %d scx, %d binary props\n",
		*out, len(cats), len(scripts), len(scx), len(bin))
}

func emitMap(b *bytes.Buffer, name string, data propData) {
	fmt.Fprintf(b, "var %s = map[string]*unicode.RangeTable{\n", name)
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%q: ", k)
		writeTable(b, coalesce(data[k]))
		b.WriteString(",\n")
	}
	b.WriteString("}\n\n")
}

func emitTable(b *bytes.Buffer, name string, rs []rng) {
	fmt.Fprintf(b, "var %s = ", name)
	writeTable(b, coalesce(rs))
	b.WriteString("\n\n")
}

func emitStrMap(b *bytes.Buffer, name string, m map[string]string) {
	fmt.Fprintf(b, "var %s = map[string]string{\n", name)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%q: %q,\n", k, m[k])
	}
	b.WriteString("}\n\n")
}

// writeTable emits a *unicode.RangeTable literal, splitting ranges at U+FFFF
// into R16 (uint16) and R32 (uint32) with stride 1.
func writeTable(b *bytes.Buffer, rs []rng) {
	var r16, r32 [][2]rune
	for _, r := range rs {
		lo, hi := r.lo, r.hi
		if lo <= 0xFFFF {
			h := hi
			if h > 0xFFFF {
				h = 0xFFFF
			}
			r16 = append(r16, [2]rune{lo, h})
		}
		if hi > 0xFFFF {
			l := lo
			if l < 0x10000 {
				l = 0x10000
			}
			r32 = append(r32, [2]rune{l, hi})
		}
	}
	b.WriteString("&unicode.RangeTable{")
	if len(r16) > 0 {
		b.WriteString("R16: []unicode.Range16{")
		for _, r := range r16 {
			fmt.Fprintf(b, "{0x%04X,0x%04X,1},", r[0], r[1])
		}
		b.WriteString("},")
	}
	if len(r32) > 0 {
		b.WriteString("R32: []unicode.Range32{")
		for _, r := range r32 {
			fmt.Fprintf(b, "{0x%X,0x%X,1},", r[0], r[1])
		}
		b.WriteString("},")
	}
	b.WriteString("}")
}

// seqSet collects the members of an emoji "property of strings": multi-code-point
// sequences plus single code points (which coalesce into a compact class).
type seqSet struct {
	seqs   [][]rune
	single []rng
}

// addEmojiField parses one code-point field ("231A", "231A..231B", or
// "0023 FE0F 20E3") into a seqSet.
func addEmojiField(s *seqSet, field string) {
	if strings.Contains(field, "..") {
		r, err := parseCodePoints(field)
		must(err)
		s.single = append(s.single, r)
		return
	}
	parts := strings.Fields(field)
	if len(parts) == 1 {
		v, err := strconv.ParseInt(parts[0], 16, 32)
		must(err)
		s.single = append(s.single, rng{rune(v), rune(v)})
		return
	}
	seq := make([]rune, len(parts))
	for i, part := range parts {
		v, err := strconv.ParseInt(part, 16, 32)
		must(err)
		seq[i] = rune(v)
	}
	s.seqs = append(s.seqs, seq)
}

// emojiPattern renders a seqSet as a regexp2 alternation, longest sequences
// first so a greedy `+` matches the intended (longer) sequence before a prefix.
func emojiPattern(s *seqSet) string {
	seqs := append([][]rune(nil), s.seqs...)
	sort.SliceStable(seqs, func(i, j int) bool { return len(seqs[i]) > len(seqs[j]) })
	var alts []string
	for _, seq := range seqs {
		var sb strings.Builder
		for _, r := range seq {
			sb.WriteString(escRune(r))
		}
		alts = append(alts, sb.String())
	}
	if len(s.single) > 0 {
		var sb strings.Builder
		sb.WriteByte('[')
		for _, r := range coalesce(s.single) {
			if r.lo == r.hi {
				sb.WriteString(escRune(r.lo))
			} else {
				sb.WriteString(escRune(r.lo) + "-" + escRune(r.hi))
			}
		}
		sb.WriteByte(']')
		alts = append(alts, sb.String())
	}
	return "(?:" + strings.Join(alts, "|") + ")"
}

// escRune renders a code point as a regexp2 \u escape (\uXXXX for the BMP,
// \u{…} for astral code points, matching regexpjs.esc).
func escRune(r rune) string {
	if r <= 0xFFFF {
		return fmt.Sprintf(`\u%04X`, r)
	}
	return fmt.Sprintf(`\u{%X}`, r)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
