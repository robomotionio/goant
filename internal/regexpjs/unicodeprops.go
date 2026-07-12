package regexpjs

import (
	"fmt"
	"strings"
	"unicode"
)

// translateUnicodeProps rewrites ECMAScript Unicode property escapes
// (\p{…} / \P{…}, valid only under the `u`/`v` flag) into explicit code-point
// character classes, since the underlying regexp2 engine does not understand the
// ES property syntax. Properties Go's unicode package cannot resolve (e.g. Emoji
// data, scripts newer than the toolchain's Unicode version) yield an error, so
// the regex simply fails to compile as it did before.
func translateUnicodeProps(pattern string) (string, error) {
	var out strings.Builder
	inClass := false
	for i := 0; i < len(pattern); {
		c := pattern[i]
		if c == '\\' && i+1 < len(pattern) {
			n := pattern[i+1]
			if (n == 'p' || n == 'P') && i+2 < len(pattern) && pattern[i+2] == '{' {
				rel := strings.IndexByte(pattern[i+3:], '}')
				if rel < 0 {
					return "", fmt.Errorf("incomplete \\p{X} character escape in `%s`", pattern)
				}
				name := pattern[i+3 : i+3+rel]
				rt, ok := resolveUnicodeProperty(name)
				if !ok {
					return "", fmt.Errorf("unknown unicode category or property `%s`", name)
				}
				out.WriteString(rangeTableToClass(rt, n == 'P', inClass))
				i += 3 + rel + 1
				continue
			}
			// Any other escape: copy both bytes verbatim.
			out.WriteByte(c)
			out.WriteByte(n)
			i += 2
			continue
		}
		switch c {
		case '[':
			inClass = true
		case ']':
			inClass = false
		}
		out.WriteByte(c)
		i++
	}
	return out.String(), nil
}

// resolveUnicodeProperty maps an ES property name — "Script=Greek", "gc=Lu",
// a bare general category ("Lu"/"Letter"), a binary property ("White_Space"),
// or a bare script ("Greek") — to a Unicode RangeTable.
func resolveUnicodeProperty(name string) (*unicode.RangeTable, bool) {
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		prop := strings.TrimSpace(name[:eq])
		val := strings.TrimSpace(name[eq+1:])
		switch prop {
		case "Script", "sc", "Script_Extensions", "scx":
			rt, ok := unicode.Scripts[val]
			return rt, ok
		case "General_Category", "gc":
			return lookupCategory(val)
		}
		return nil, false
	}
	if rt, ok := lookupCategory(name); ok {
		return rt, true
	}
	if rt, ok := unicode.Properties[name]; ok {
		return rt, true
	}
	if rt, ok := emojiProperties[name]; ok {
		return rt, true
	}
	if rt, ok := unicode.Scripts[name]; ok {
		return rt, true
	}
	return nil, false
}

// lookupCategory resolves a general-category value by its abbreviated name (Lu)
// or its ES long alias (Uppercase_Letter).
func lookupCategory(name string) (*unicode.RangeTable, bool) {
	if rt, ok := unicode.Categories[name]; ok {
		return rt, true
	}
	if short, ok := categoryAliases[name]; ok {
		if rt, ok := unicode.Categories[short]; ok {
			return rt, true
		}
	}
	return nil, false
}

var categoryAliases = map[string]string{
	"Letter": "L", "Cased_Letter": "LC", "Uppercase_Letter": "Lu",
	"Lowercase_Letter": "Ll", "Titlecase_Letter": "Lt", "Modifier_Letter": "Lm",
	"Other_Letter": "Lo", "Mark": "M", "Nonspacing_Mark": "Mn",
	"Spacing_Mark": "Mc", "Enclosing_Mark": "Me", "Number": "N",
	"Decimal_Number": "Nd", "Letter_Number": "Nl", "Other_Number": "No",
	"Punctuation": "P", "Connector_Punctuation": "Pc", "Dash_Punctuation": "Pd",
	"Open_Punctuation": "Ps", "Close_Punctuation": "Pe", "Initial_Punctuation": "Pi",
	"Final_Punctuation": "Pf", "Other_Punctuation": "Po", "Symbol": "S",
	"Math_Symbol": "Sm", "Currency_Symbol": "Sc", "Modifier_Symbol": "Sk",
	"Other_Symbol": "So", "Separator": "Z", "Space_Separator": "Zs",
	"Line_Separator": "Zl", "Paragraph_Separator": "Zp", "Other": "C",
	"Control": "Cc", "Format": "Cf", "Surrogate": "Cs", "Private_Use": "Co",
	"Unassigned": "Cn",
}

// rangeTableToClass renders a RangeTable as a regexp2 character class. When
// inClass is true the ranges are emitted bare (no surrounding brackets) so they
// can be spliced into an existing [...] class. Negated escapes are only handled
// outside a class.
func rangeTableToClass(rt *unicode.RangeTable, negate, inClass bool) string {
	var b strings.Builder
	if !inClass {
		if negate {
			b.WriteString("[^")
		} else {
			b.WriteByte('[')
		}
	}
	for _, r := range rt.R16 {
		writeClassRange(&b, uint32(r.Lo), uint32(r.Hi), uint32(r.Stride))
	}
	for _, r := range rt.R32 {
		writeClassRange(&b, r.Lo, r.Hi, r.Stride)
	}
	if !inClass {
		b.WriteByte(']')
	}
	return b.String()
}

// esc renders a code point as a regexp2-accepted escape: \uXXXX for the BMP,
// \u{…} (valid under the Unicode flag) for astral code points.
func esc(c uint32) string {
	if c <= 0xFFFF {
		return fmt.Sprintf(`\u%04X`, c)
	}
	return fmt.Sprintf(`\u{%X}`, c)
}

func writeClassRange(b *strings.Builder, lo, hi, stride uint32) {
	if stride <= 1 {
		if lo == hi {
			b.WriteString(esc(lo))
		} else {
			b.WriteString(esc(lo) + "-" + esc(hi))
		}
		return
	}
	for c := lo; c <= hi; c += stride {
		b.WriteString(esc(c))
	}
}
