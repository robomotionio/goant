// Command gencldrlocale reads the per-locale CLDR data goant's formatters need
// and generates internal/engine/intl_locale_gen.go.
//
// Usage:
//
//	go run ./tools/gencldrlocale -out internal/engine/intl_locale_gen.go
//	go run ./tools/gencldrlocale -cldr <dir> -out ...   # from a local checkout
//
// Without -cldr the data is fetched from the cldr-json repository, which is
// where the JSON form of CLDR lives:
//
//	https://github.com/unicode-org/cldr-json/tree/main/cldr-json
//
// Five things are taken, for every locale internal/engine/intl_data.go formats
// natively:
//
//	numbers.json      compact patterns, the currency and percent patterns,
//	                  and how many digits a group needs before it is grouped
//	currencies.json   the symbols that are not just the code
//	units.json        the sanctioned units, in each of the three widths
//	listPatterns.json how a list of two, three or more is joined
//	dateFields.json   "in 3 days", "3 days ago", "yesterday"
//
// The generated file is committed; regenerate when bumping CLDR version.
//
// As in tools/gencldr, the tables are emitted as newline-separated string
// constants rather than map literals: a constant is read-only data in the
// binary, and the maps are built on first use, which for a host that never
// formats anything is never.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// locales is every tag internal/engine/intl_data.go carries a localeInfo for,
// plus the bare languages those tags are reached through. CLDR is keyed by
// language or by language-region, and not every region has its own file, so a
// tag with no file of its own falls back to its language.
var locales = []string{
	"af-ZA", "am-ET", "az-AZ", "bg-BG", "bs-BA", "ca-ES", "cs-CZ", "da-DK",
	"de-AT", "de-CH", "de-DE", "el-GR", "en-AU", "en-CA", "en-GB", "en-IE",
	"en-IN", "en-NZ", "en-PH", "en-SG", "en-US", "en-ZA", "es-AR", "es-CL",
	"es-CO", "es-ES", "es-MX", "es-PE", "es-VE", "et-EE", "fi-FI", "fil-PH",
	"fr-BE", "fr-CA", "fr-CH", "fr-FR", "gu-IN", "he-IL", "hi-IN", "hr-HR",
	"hu-HU", "id-ID", "it-CH", "it-IT", "ja-JP", "kn-IN", "ko-KR", "lt-LT",
	"lv-LV", "ml-IN", "ms-MY", "nb-NO", "nl-BE", "nl-NL", "pa-IN", "pl-PL",
	"pt-BR", "pt-PT", "ro-RO", "ru-RU", "sk-SK", "sl-SI", "sr-RS", "sv-SE",
	"sw-KE", "sw-TZ", "ta-IN", "te-IN", "tr-TR", "uk-UA", "ur-PK", "uz-UZ",
	"vi-VN", "zh-CN", "zh-HK", "zh-TW",
}

// cldrName is the directory cldr-json keeps a locale in. A few of the tags
// goant formats are spelled differently there, and the rest fall back to their
// language when the region has no file of its own.
var cldrName = map[string]string{
	"zh-CN": "zh", "zh-TW": "zh-Hant", "zh-HK": "zh-Hant-HK",
	"sr-RS": "sr", "az-AZ": "az", "uz-UZ": "uz", "he-IL": "he",
}

// magnitudes are the compact-pattern keys, from a thousand up. CLDR carries no
// pattern below a thousand because there is nothing there to shorten.
var magnitudes = []string{
	"1000", "10000", "100000", "1000000", "10000000", "100000000",
	"1000000000", "10000000000", "100000000000", "1000000000000",
	"10000000000000", "100000000000000",
}

// plurals are the categories a pattern may be given per, in the order the
// engine reads them back.
var plurals = []string{"zero", "one", "two", "few", "many", "other"}

// sanctionedUnits is the ECMA-402 list, spelled as CLDR spells it: the
// identifier the option takes is the second half of CLDR's category-unit name.
var sanctionedUnits = []string{
	"acre", "bit", "byte", "celsius", "centimeter", "day", "degree", "fahrenheit",
	"fluid-ounce", "foot", "gallon", "gigabit", "gigabyte", "gram", "hectare",
	"hour", "inch", "kilobit", "kilobyte", "kilogram", "kilometer", "liter",
	"megabit", "megabyte", "meter", "microsecond", "mile", "mile-scandinavian",
	"milliliter", "millimeter", "millisecond", "minute", "month", "nanosecond",
	"ounce", "percent", "petabyte", "pound", "second", "stone", "terabit",
	"terabyte", "week", "yard", "year",
}

// listTypes are the three kinds of list and the three widths each comes in.
var listTypes = []string{
	"standard", "standard-short", "standard-narrow",
	"or", "or-short", "or-narrow",
	"unit", "unit-short", "unit-narrow",
}

// relFields are the units Intl.RelativeTimeFormat takes, in each of the three
// widths. CLDR spells the widths as a suffix on the field name.
var relFields = []string{
	"year", "quarter", "month", "week", "day", "hour", "minute", "second",
}

type fetcher struct {
	dir   string
	cache map[string][]byte
}

func (f *fetcher) get(pkg, loc, file string) ([]byte, error) {
	key := pkg + "/" + loc + "/" + file
	if b, ok := f.cache[key]; ok {
		return b, nil
	}
	var b []byte
	var err error
	if f.dir != "" {
		b, err = os.ReadFile(filepath.Join(f.dir, pkg, "main", loc, file))
	} else {
		url := "https://raw.githubusercontent.com/unicode-org/cldr-json/main/cldr-json/" +
			pkg + "/main/" + loc + "/" + file
		b, err = httpGet(url)
	}
	if err != nil {
		return nil, err
	}
	f.cache[key] = b
	return b, nil
}

func httpGet(url string) ([]byte, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	for attempt := 0; ; attempt++ {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == 200 {
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			return b, err
		}
		if resp != nil {
			resp.Body.Close()
			if resp.StatusCode == 404 {
				return nil, fmt.Errorf("not found: %s", url)
			}
		}
		if attempt == 3 {
			return nil, fmt.Errorf("get %s: %v", url, err)
		}
		time.Sleep(time.Second << attempt)
	}
}

// localeJSON walks into main.<name>.<section>, whatever the file calls itself.
func localeJSON(b []byte, section ...string) (map[string]any, error) {
	var top struct {
		Main map[string]json.RawMessage `json:"main"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, err
	}
	for _, raw := range top.Main {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		cur := m
		for _, s := range section {
			next, ok := cur[s].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("no section %q", s)
			}
			cur = next
		}
		return cur, nil
	}
	return nil, fmt.Errorf("no locale in file")
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func sub(m map[string]any, key string) map[string]any {
	s, _ := m[key].(map[string]any)
	return s
}

func main() {
	dir := flag.String("cldr", "", "a local cldr-json checkout; fetched over the network when empty")
	out := flag.String("out", "internal/engine/intl_locale_gen.go", "file to write")
	flag.Parse()

	f := &fetcher{dir: *dir, cache: map[string][]byte{}}
	var numbers, compact, currencies, units, lists, reltime []string

	for _, tag := range locales {
		name := cldrName[tag]
		if name == "" {
			name = tag
		}
		got := false
		for _, try := range []string{name, strings.SplitN(name, "-", 2)[0]} {
			if collectNumbers(f, tag, try, &numbers, &compact, &currencies) == nil {
				got = true
				break
			}
		}
		if !got {
			fmt.Fprintf(os.Stderr, "warning: no numbers for %s\n", tag)
		}
		for _, try := range []string{name, strings.SplitN(name, "-", 2)[0]} {
			if collectUnits(f, tag, try, &units) == nil {
				break
			}
		}
		for _, try := range []string{name, strings.SplitN(name, "-", 2)[0]} {
			if collectLists(f, tag, try, &lists) == nil {
				break
			}
		}
		for _, try := range []string{name, strings.SplitN(name, "-", 2)[0]} {
			if collectRelative(f, tag, try, &reltime) == nil {
				break
			}
		}
		fmt.Fprintf(os.Stderr, "%s done\n", tag)
	}

	var b strings.Builder
	b.WriteString(header)
	emit(&b, "cldrNumberData", `one line per locale:
// tag, the currency pattern, the accounting pattern, the percent pattern, the
// fewest integer digits a number needs before its groups are separated, and the
// patterns for a range and for an approximate value.`, numbers)
	emit(&b, "cldrCompactData", `one line per locale and length ("s" or "l"),
// then twelve patterns from a thousand up, each a plural category and its
// pattern. An empty pattern means the locale does not shorten that magnitude.`, compact)
	emit(&b, "cldrCurrencyData", `one line per locale: the currency symbols that
// are not simply the code, and the narrow ones where they differ again.`, currencies)
	emit(&b, "cldrUnitData", `one line per locale, width and unit: the pattern
// per plural category, with {0} where the number goes. The unit named "per" is
// how a compound is joined.`, units)
	emit(&b, "cldrListData", `one line per locale and list type: the patterns
// for the start, the middle, the end, and a list of exactly two.`, lists)
	emit(&b, "cldrRelativeData", `one line per locale, width and unit: the past
// and future patterns per plural category, then the named offsets from two
// back to two forward where the locale has them.`, reltime)

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gofmt:", err)
		src = []byte(b.String())
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func collectNumbers(f *fetcher, tag, name string, numbers, compact, currencies *[]string) error {
	b, err := f.get("cldr-numbers-full", name, "numbers.json")
	if err != nil {
		return err
	}
	n, err := localeJSON(b, "numbers")
	if err != nil {
		return err
	}
	cur := sub(n, "currencyFormats-numberSystem-latn")
	pct := sub(n, "percentFormats-numberSystem-latn")
	minGroup := str(n, "minimumGroupingDigits")
	if minGroup == "" {
		minGroup = "1"
	}
	misc := sub(n, "miscPatterns-numberSystem-latn")
	*numbers = append(*numbers, strings.Join([]string{
		tag, str(cur, "standard"), str(cur, "accounting"), str(pct, "standard"), minGroup,
		str(misc, "range"), str(misc, "approximately"),
	}, "\t"))

	dec := sub(n, "decimalFormats-numberSystem-latn")
	for _, length := range []struct{ key, code string }{{"short", "s"}, {"long", "l"}} {
		df := sub(sub(dec, length.key), "decimalFormat")
		if df == nil {
			continue
		}
		var cells []string
		for _, mag := range magnitudes {
			var got []string
			for _, p := range plurals {
				if v := str(df, mag+"-count-"+p); v != "" {
					got = append(got, p+"="+v)
				}
			}
			cells = append(cells, strings.Join(got, ";"))
		}
		*compact = append(*compact, tag+"\t"+length.code+"\t"+strings.Join(cells, "\t"))
	}

	cb, err := f.get("cldr-numbers-full", name, "currencies.json")
	if err != nil {
		return nil // the symbols are a bonus; the patterns are the point
	}
	cm, err := localeJSON(cb, "numbers", "currencies")
	if err != nil {
		return nil
	}
	var codes []string
	for code := range cm {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	var syms []string
	for _, code := range codes {
		e := sub(cm, code)
		sym, narrow := str(e, "symbol"), str(e, "symbol-alt-narrow")
		if sym == code || (sym == "" && narrow == "") {
			if narrow == "" || narrow == code {
				continue
			}
		}
		cell := code + "=" + sym
		if narrow != "" && narrow != sym {
			cell += "/" + narrow
		}
		syms = append(syms, cell)
	}
	if len(syms) > 0 {
		*currencies = append(*currencies, tag+"\t"+strings.Join(syms, ","))
	}
	return nil
}

func collectUnits(f *fetcher, tag, name string, units *[]string) error {
	b, err := f.get("cldr-units-full", name, "units.json")
	if err != nil {
		return err
	}
	u, err := localeJSON(b, "units")
	if err != nil {
		return err
	}
	for _, width := range []string{"long", "short", "narrow"} {
		w := sub(u, width)
		if w == nil {
			continue
		}
		// The compound joiner first: "{0}/{1}" or "{0} pro {1}".
		if per := sub(w, "per"); per != nil {
			if p := str(per, "compoundUnitPattern"); p != "" {
				*units = append(*units, strings.Join([]string{tag, width, "per", "other=" + p}, "\t"))
			}
		}
		// CLDR names a unit by category and unit ("speed-kilometer-per-hour"),
		// and the option names it by the second half alone.
		byUnit := map[string]map[string]any{}
		for key, v := range w {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if i := strings.IndexByte(key, '-'); i > 0 {
				byUnit[key[i+1:]] = m
			}
		}
		for _, unit := range sanctionedUnits {
			m := byUnit[unit]
			if m == nil {
				continue
			}
			var got []string
			for _, p := range plurals {
				if v := str(m, "unitPattern-count-"+p); v != "" {
					got = append(got, p+"="+v)
				}
			}
			if len(got) == 0 {
				continue
			}
			*units = append(*units, strings.Join([]string{tag, width, unit, strings.Join(got, ";")}, "\t"))
		}
	}
	return nil
}

func collectLists(f *fetcher, tag, name string, lists *[]string) error {
	b, err := f.get("cldr-misc-full", name, "listPatterns.json")
	if err != nil {
		return err
	}
	l, err := localeJSON(b, "listPatterns")
	if err != nil {
		return err
	}
	for _, typ := range listTypes {
		m := sub(l, "listPattern-type-"+typ)
		if m == nil {
			continue
		}
		*lists = append(*lists, strings.Join([]string{
			tag, typ, str(m, "start"), str(m, "middle"), str(m, "end"), str(m, "2"),
		}, "\t"))
	}
	return nil
}

func collectRelative(f *fetcher, tag, name string, reltime *[]string) error {
	b, err := f.get("cldr-dates-full", name, "dateFields.json")
	if err != nil {
		return err
	}
	d, err := localeJSON(b, "dates", "fields")
	if err != nil {
		return err
	}
	for _, width := range []struct{ key, suffix string }{
		{"long", ""}, {"short", "-short"}, {"narrow", "-narrow"},
	} {
		for _, field := range relFields {
			m := sub(d, field+width.suffix)
			if m == nil {
				continue
			}
			var past, future []string
			for _, p := range plurals {
				if v := str(sub(m, "relativeTime-type-past"), "relativeTimePattern-count-"+p); v != "" {
					past = append(past, p+"="+v)
				}
				if v := str(sub(m, "relativeTime-type-future"), "relativeTimePattern-count-"+p); v != "" {
					future = append(future, p+"="+v)
				}
			}
			var named []string
			for _, off := range []string{"-2", "-1", "0", "1", "2"} {
				named = append(named, str(m, "relative-type-"+off))
			}
			*reltime = append(*reltime, strings.Join([]string{
				tag, width.key, field, strings.Join(past, ";"),
				strings.Join(future, ";"), strings.Join(named, "|"),
			}, "\t"))
		}
	}
	return nil
}

func emit(b *strings.Builder, name, doc string, lines []string) {
	fmt.Fprintf(b, "\n// %s is %s\nconst %s = \"\" +\n", name, doc, name)
	for _, l := range lines {
		fmt.Fprintf(b, "\t%s +\n", quote(l+"\n"))
	}
	b.WriteString("\t\"\"\n")
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

const header = `// Code generated by tools/gencldrlocale. DO NOT EDIT.

package engine

// Per-locale CLDR formatting data: the compact and currency patterns, the
// currency symbols, the unit patterns, the list patterns, and the relative-time
// patterns. See tools/gencldrlocale.
`
