#!/usr/bin/env python3
"""Generate goant's Intl tables from the 26.7.3 v8go fork, used as an oracle.

Rather than fitting patterns to rendered strings, this asks ICU for the
structure directly via Intl.*.formatToParts, so the tables are exact by
construction. Two tables come out:

  * zoneDisplayNames -- ICU's long time-zone name per IANA zone (standard and
    daylight), which Date.prototype.toString() prints in parentheses.
  * localeTable      -- date/time patterns, day-period markers and number
    separators per locale.

Run:  python3 gen.py > ../intl_data.go
"""
import subprocess, sys, os, re, json, datetime, zoneinfo

SP = os.path.dirname(os.path.abspath(__file__))
ORACLE = os.environ.get("ORACLE", os.path.join(SP, "probe-v8-2673"))
ZONEDUMP = os.path.join(SP, "zonedump.tsv")

# Locales worth shipping: Gregorian calendar and ASCII digits. Others (th-TH
# Buddhist, fa-IR Persian, ar-* / bn-IN native digits) are deliberately left out
# so they fall back to en-US rather than render subtly wrong; the fork itself
# falls back for tags its ICU data does not carry.
LOCALES = """en-US en-GB en-AU en-CA en-IN en-NZ en-ZA en-IE en-SG en-PH
tr-TR de-DE de-AT de-CH fr-FR fr-CA fr-BE fr-CH es-ES es-MX es-AR es-CO es-CL
es-PE es-VE it-IT it-CH pt-BR pt-PT nl-NL nl-BE pl-PL ru-RU uk-UA cs-CZ sk-SK
hu-HU ro-RO bg-BG el-GR sv-SE da-DK fi-FI nb-NO nn-NO is-IS et-EE lv-LV lt-LT
sl-SI hr-HR sr-RS bs-BA mk-MK sq-AL mt-MT ga-IE cy-GB ca-ES gl-ES eu-ES
ja-JP ko-KR zh-CN zh-TW zh-HK vi-VN id-ID ms-MY fil-PH hi-IN ta-IN te-IN
mr-IN gu-IN kn-IN ml-IN pa-IN he-IL ur-PK az-AZ uz-UZ kk-KZ ky-KG hy-AM ka-GE
be-BY sw-KE sw-TZ am-ET af-ZA zu-ZA xh-ZA mn-MN ne-NP si-LK km-KH lo-LA""".split()

PROBE = r"""
var out = [];
var LOC = %s;

// Two instants: 09:04:05 exercises the AM/one-digit forms, 15:07:09 the PM/
// two-digit forms. Both are read with timeZone:'UTC' so the host zone cannot
// leak into the generated table.
var AM = new Date(Date.UTC(2026, 0, 2, 9, 4, 5));
var PM = new Date(Date.UTC(2026, 10, 22, 15, 7, 9));
// Midnight and noon pin down the hour cycle: h11 shows 0, h12 shows 12, h23
// shows 00 and h24 shows 24. Nothing else distinguishes them.
var MIDNIGHT = new Date(Date.UTC(2026, 0, 2, 0, 0, 0));
var NOON = new Date(Date.UTC(2026, 0, 2, 12, 0, 0));
// 13:00 is what separates a 12-hour clock from a 24-hour one. The day-period
// marker cannot do it: es-AR renders 13:00 as 01:00:00 and prints no marker at
// all, so keying off the marker's presence reads it as 24-hour.
var AFTERNOON = new Date(Date.UTC(2026, 0, 2, 13, 0, 0));

var DATE_OPT = {year:'numeric', month:'numeric', day:'numeric', timeZone:'UTC'};
var TIME_OPT = {hour:'numeric', minute:'numeric', second:'numeric', timeZone:'UTC'};
var BOTH_OPT = {year:'numeric', month:'numeric', day:'numeric',
                hour:'numeric', minute:'numeric', second:'numeric', timeZone:'UTC'};

function parts(loc, opt, d) {
  return (new Intl.DateTimeFormat(loc, opt)).formatToParts(d)
    .map(function (p) { return p.type + '\x1f' + p.value; }).join('\x1e');
}
function numParts(loc, n) {
  return (new Intl.NumberFormat(loc)).formatToParts(n)
    .map(function (p) { return p.type + '\x1f' + p.value; }).join('\x1e');
}

for (var i = 0; i < LOC.length; i++) {
  var L = LOC[i], rec = ['ERR'];
  try {
    var ro = (new Intl.DateTimeFormat(L, TIME_OPT)).resolvedOptions();
    rec = [
      L, ro.locale, String(ro.hourCycle), String(ro.numberingSystem),
      parts(L, DATE_OPT, AM), parts(L, DATE_OPT, PM),
      parts(L, TIME_OPT, AM), parts(L, TIME_OPT, PM),
      parts(L, BOTH_OPT, AM),
      parts(L, TIME_OPT, MIDNIGHT), parts(L, TIME_OPT, NOON),
      parts(L, TIME_OPT, AFTERNOON),
      // NaN, infinity and the negative prefix are localised too ("epäluku" in
      // Finnish; an LRM before the minus in Hebrew), so take them verbatim.
      (1).toLocaleString(L), (-1).toLocaleString(L),
      (NaN).toLocaleString(L), (Infinity).toLocaleString(L),
      numParts(L, 1234567.891), numParts(L, 1234), numParts(L, 12345),
      numParts(L, -0.5), numParts(L, 123456789),
      // the actual strings, used to self-check the reconstruction
      AM.toLocaleDateString(L, {timeZone:'UTC'}),
      AM.toLocaleTimeString(L, {timeZone:'UTC'}),
      AM.toLocaleString(L, {timeZone:'UTC'}),
      PM.toLocaleDateString(L, {timeZone:'UTC'}),
      PM.toLocaleTimeString(L, {timeZone:'UTC'}),
      PM.toLocaleString(L, {timeZone:'UTC'})
    ];
  } catch (e) { rec = [L, 'ERR', String(e)]; }
  out.push(rec.join('\x1d'));
}
out.join('\n')
"""


def run_oracle(js):
    p = subprocess.run([ORACLE, "/dev/stdin"], input=js, capture_output=True,
                       text=True, env={**os.environ, "TZ": "UTC", "LANG": "C",
                                       "LC_ALL": "C"})
    if p.returncode != 0:
        sys.exit("oracle failed:\n" + p.stdout + p.stderr)
    return p.stdout


def decode_parts(s):
    return [tuple(f.split("\x1f", 1)) for f in s.split("\x1e")] if s else []


# Field part type -> (token, token when zero-padded to two digits)
DATE_TOK = {"year": "Y", "month": "M", "day": "D"}
TIME_TOK = {"hour": "H", "minute": "m", "second": "s"}


def build_pattern(parts_am, parts_pm, toks):
    """Turn two formatToParts renderings into one token pattern.

    Zero-padding is read off the values rather than assumed: a field is padded
    when some sample renders a value below ten in two characters, and unpadded
    when some sample renders one below ten in a single character. The two
    instants are chosen so every field is single-digit in one and double-digit
    in the other, which is what lets `d` and `dd` be told apart. A field that
    claims both is a contradiction and fails the fit rather than guessing.
    """
    if len(parts_am) != len(parts_pm):
        return None
    out = []
    for (ta, va), (tp, vp) in zip(parts_am, parts_pm):
        if ta != tp:
            return None
        if ta == "literal":
            if va != vp:
                return None
            out.append(va)
        elif ta in toks:
            if ta == "year":
                out.append("{Y}")
                continue
            padded = unpadded = False
            for v in (va, vp):
                if not v.isdigit():
                    return None
                if int(v) < 10:
                    if len(v) == 2:
                        padded = True
                    elif len(v) == 1:
                        unpadded = True
            if padded and unpadded:
                return None
            tok = toks[ta]
            out.append("{%s}" % (tok * 2 if padded else tok))
        elif ta == "dayPeriod":
            out.append("{P}")
        else:
            return None
    return "".join(out)


def hour_cycle(parts_midnight, parts_noon, parts_afternoon):
    """h11/h12/h23/h24, read off what midnight, noon and 13:00 render as.

    13:00 decides 12-hour versus 24-hour (it shows as 1 or as 13), and midnight
    then decides which of the pair: a 12-hour clock shows 0 for h11 and 12 for
    h12, a 24-hour one shows 0 for h23 and 24 for h24.
    """
    def hour(parts):
        return ("".join(v for t, v in parts if t == "hour").lstrip("0") or "0")
    mid, aft = hour(parts_midnight), hour(parts_afternoon)
    if aft == "1":
        return "h11" if mid == "0" else "h12"
    return "h24" if mid == "24" else "h23"


def build_glue(both, date_str, time_str):
    """The date/time connector, as '{d}<literal>{t}' (or the reverse).

    Taken by removing the standalone date and time renderings from the combined
    one. Reading it off formatToParts instead would mis-attribute a date pattern
    that ends in a literal (hu-HU's trailing '.') to the connector.
    """
    if not date_str or not time_str:
        return None
    di, ti = both.find(date_str), both.find(time_str)
    if di < 0 or ti < 0:
        return None
    if di < ti:
        if di != 0 or ti + len(time_str) != len(both):
            return None
        return "{d}%s{t}" % both[di + len(date_str):ti]
    if ti != 0 or di + len(date_str) != len(both):
        return None
    return "{t}%s{d}" % both[ti + len(time_str):di]


def number_info(big, n1234, n12345, neg, n123456789, s_one, s_minus_one):
    """Group/decimal marks, minus sign, and the grouping shape."""
    def pick(parts, want):
        return "".join(v for t, v in parts if t == want)

    group = pick(big, "group")[:1] or ","
    decimal = pick(big, "decimal")[:1] or "."
    # Taken as the whole prefix that formatting -1 adds over 1, not just the
    # minusSign part: Hebrew puts a left-to-right mark in front of it.
    minus = s_minus_one[:-len(s_one)] if s_one and s_minus_one.endswith(s_one) else "-"
    # minimumGroupingDigits: es-ES prints 1234 ungrouped but 12345 grouped
    min_group = 1 if any(t == "group" for t, _ in n1234) else 2
    # Indian grouping puts the first separator after three digits, then every two
    ints = "".join(v for t, v in n123456789 if t in ("integer", "group"))
    indian = bool(re.match(r"^\d{1,2}" + re.escape(group) + r"\d{2}" +
                           re.escape(group) + r"\d{2}" + re.escape(group) +
                           r"\d{3}$", ints))
    return dict(group=group, decimal=decimal, minus=minus,
                minGroup=min_group, indian=indian)


def gen_locales():
    raw = run_oracle(PROBE % json.dumps(LOCALES))
    table, skipped = {}, []
    for line in raw.strip().split("\n"):
        f = line.split("\x1d")
        if len(f) < 27 or f[1] == "ERR":
            skipped.append((f[0], "unsupported"))
            continue
        (tag, resolved, _declared_cycle, numbering,
         d_am, d_pm, t_am, t_pm, both, t_midnight, t_noon, t_afternoon,
         s_one, s_minus_one, s_nan, s_inf,
         n_big, n_1234, n_12345, n_neg, n_123456789,
         s_date_am, s_time_am, s_both_am,
         s_date_pm, s_time_pm, s_both_pm) = f[:27]
        # resolvedOptions().hourCycle disagrees with the pattern ICU actually
        # picks for some locales (es-AR reports h12 but its skeleton is not),
        # so the cycle is derived from rendered hours instead.
        cycle = hour_cycle(decode_parts(t_midnight), decode_parts(t_noon),
                           decode_parts(t_afternoon))

        if numbering != "latn":
            skipped.append((tag, "numbering=" + numbering))
            continue
        # The fork silently falls back for tags its ICU data lacks; shipping our
        # own entry for those would be inventing data it never produced.
        if resolved != tag and resolved != tag.split("-")[0]:
            skipped.append((tag, "fork falls back to " + resolved))
            continue

        date_pat = build_pattern(decode_parts(d_am), decode_parts(d_pm), DATE_TOK)
        time_pat = build_pattern(decode_parts(t_am), decode_parts(t_pm), TIME_TOK)
        if not date_pat or not time_pat:
            skipped.append((tag, "pattern fit failed"))
            continue
        glue = build_glue(s_both_am, s_date_am, s_time_am)
        if glue is None:
            skipped.append((tag, "glue fit failed"))
            continue

        am = "".join(v for t, v in decode_parts(t_am) if t == "dayPeriod")
        pm = "".join(v for t, v in decode_parts(t_pm) if t == "dayPeriod")

        num = number_info(decode_parts(n_big), decode_parts(n_1234),
                          decode_parts(n_12345), decode_parts(n_neg),
                          decode_parts(n_123456789), s_one, s_minus_one)

        table[tag] = dict(date=date_pat, time=time_pat, glue=glue,
                          hourCycle=cycle, am=am, pm=pm,
                          nan=s_nan, inf=s_inf,
                          expect=dict(date_am=s_date_am, time_am=s_time_am,
                                      both_am=s_both_am, date_pm=s_date_pm,
                                      time_pm=s_time_pm, both_pm=s_both_pm,
                                      num="".join(v for _, v in decode_parts(n_big))),
                          **num)
    return table, skipped


def gen_zones():
    rows = {}
    with open(ZONEDUMP) as f:
        for line in f:
            p = line.rstrip("\n").split("\t")
            if len(p) == 4:
                rows.setdefault(p[0], {})[p[1]] = p[3]

    jan = datetime.datetime(2026, 1, 15, tzinfo=datetime.timezone.utc)
    jul = datetime.datetime(2026, 7, 15, tzinfo=datetime.timezone.utc)
    table = {}
    for zone, r in rows.items():
        if "JAN" not in r or "JUL" not in r:
            continue
        try:
            tz = zoneinfo.ZoneInfo(zone)
        except Exception:
            continue
        std = dst = None
        for when, when_dt in (("JAN", jan), ("JUL", jul)):
            if when_dt.astimezone(tz).dst():
                dst = r[when]
            else:
                std = r[when]
        std = std or dst
        dst = dst or std
        if std:
            table[zone] = (std, dst)
    return table


GO_HEADER = '''// Code generated by tools/intlgen. DO NOT EDIT.
//
// The tables below are extracted from the V8 build the robot shipped through
// 26.7.4 (robomotionio/v8go, V8 14.7 with ICU), by asking it for structure via
// Intl.DateTimeFormat.prototype.formatToParts rather than by fitting patterns to
// rendered strings. Regenerate with tools/intlgen when that oracle changes.

package engine

// zoneDisplayNames maps an IANA zone to the long names ICU prints in the
// parenthesised tail of Date.prototype.toString(), as {standard, daylight}. A
// zone that is absent falls back to the GMT+HH:MM form, which is what ICU itself
// does for zones CLDR carries no name for.
var zoneDisplayNames = map[string][2]string{
'''


def go_str(s):
    return json.dumps(s, ensure_ascii=False)


def emit(locales, zones, skipped, langs):
    w = sys.stdout.write
    w(GO_HEADER)
    for zone in sorted(zones):
        std, dst = zones[zone]
        w("\t%s: {%s, %s},\n" % (go_str(zone), go_str(std), go_str(dst)))
    w("}\n\n")

    w("""// localeInfo describes one locale's formatting, in the shape goant's own
// formatter consumes. Patterns use {Y} {M} {MM} {D} {DD} for dates, {H} {HH}
// {m} {mm} {s} {ss} {P} for times, and {d}/{t} in glue; everything else is a
// literal.
type localeInfo struct {
\tdate, time, glue string
\thourCycle        string
\tam, pm           string
\tgroup, decimal   string
\tminus            string // may carry a bidi mark, as in he-IL
\tnan, inf         string // localised too: NaN is "epäluku" in fi-FI
\tminGroup         int  // minimum integer digits before grouping kicks in
\tindian           bool // 3-2-2 grouping rather than 3-3-3
}

// localeTable is the set of locales goant formats natively. Anything else
// resolves to defaultLocale. Locales on a non-Gregorian calendar or non-Latin
// digits are intentionally absent so they fall back rather than render wrong.
var localeTable = map[string]localeInfo{
""")
    for tag in sorted(locales):
        e = locales[tag]
        w("\t%s: {date: %s, time: %s, glue: %s, hourCycle: %s, am: %s, pm: %s, "
          "group: %s, decimal: %s, minus: %s, nan: %s, inf: %s, "
          "minGroup: %d, indian: %s},\n" % (
              go_str(tag), go_str(e["date"]), go_str(e["time"]), go_str(e["glue"]),
              go_str(e["hourCycle"]), go_str(e["am"]), go_str(e["pm"]),
              go_str(e["group"]), go_str(e["decimal"]), go_str(e["minus"]),
              go_str(e["nan"]), go_str(e["inf"]),
              e["minGroup"], "true" if e["indian"] else "false"))
    w("}\n")

    w("""
// languageDefaults maps a bare language tag to the locale it resolves to, which
// is not derivable from the tag: ICU sends "de" to Germany and "pt" to Portugal.
var languageDefaults = map[string]string{
""")
    for lang in sorted(langs):
        w("\t%s: %s,\n" % (go_str(lang), go_str(langs[lang])))
    w("}\n")

    if skipped:
        w("\n// Deliberately not in localeTable (they fall back to en-US):\n")
        for tag, why in sorted(skipped):
            w("//\t%s: %s\n" % (tag, why))


def gen_language_defaults(locales):
    """Which shipped locale a bare language tag should resolve to.

    ICU resolves 'de' to Germany's formats and 'pt' to Portugal's, and those
    choices are not derivable from the tag, so ask the oracle: format with the
    bare language, then find the shipped entry that renders identically.
    """
    langs = sorted({t.split("-")[0] for t in locales})
    probe = ["var out=[];", "var L=%s;" % json.dumps(langs),
             "var D=new Date(Date.UTC(2026,0,2,9,4,5));",
             "var O={timeZone:'UTC'};",
             "for(var i=0;i<L.length;i++){var r;",
             "try{r=[L[i],D.toLocaleDateString(L[i],O),D.toLocaleTimeString(L[i],O),",
             "D.toLocaleString(L[i],O),(1234567.891).toLocaleString(L[i])].join('\\x1d')}",
             "catch(e){r=L[i]+'\\x1dERR'}out.push(r)}"]
    raw = run_oracle("\n".join(probe) + "\nout.join('\\n')")

    want = {}
    for line in raw.strip().split("\n"):
        f = line.split("\x1d")
        if len(f) == 5:
            want[f[0]] = tuple(f[1:])

    # Several regions of a language render identically (en-US and en-PH agree on
    # all four probes), so ties are broken towards the canonical region rather
    # than alphabetically, which would pick en-PH.
    CANONICAL = {"en": "en-US", "pt": "pt-PT", "zh": "zh-CN", "ja": "ja-JP",
                 "ko": "ko-KR", "sw": "sw-KE", "fil": "fil-PH", "he": "he-IL",
                 "hi": "hi-IN", "ur": "ur-PK", "ms": "ms-MY", "id": "id-ID",
                 "vi": "vi-VN", "am": "am-ET", "af": "af-ZA"}

    def rank(lang, tag):
        if tag == CANONICAL.get(lang):
            return 0
        if tag == "%s-%s" % (lang, lang.upper()):
            return 1
        return 2

    out = {}
    for lang, rendered in want.items():
        cands = sorted((t for t in locales if t.split("-")[0] == lang),
                       key=lambda t: (rank(lang, t), t))
        for tag in cands:
            e = locales[tag]["expect"]
            if (e["date_am"], e["time_am"], e["both_am"], e["num"]) == rendered:
                out[lang] = tag
                break
    return out


if __name__ == "__main__":
    loc, skipped = gen_locales()
    zones = gen_zones()
    langs = gen_language_defaults(loc)
    if "--expectations" in sys.argv:
        json.dump({t: loc[t]["expect"] for t in loc}, sys.stdout,
                  ensure_ascii=False, indent=1)
    else:
        emit(loc, zones, skipped, langs)
    print("// %d locales, %d zones, %d languages" %
          (len(loc), len(zones), len(langs)), file=sys.stderr)
