package engine

// The `timeZone` option, for Intl.DateTimeFormat and Date.prototype.toLocale*.
//
// Until this file existed the option was accepted and ignored: a flow asking
// for America/New_York got the robot's own wall clock, silently, and an
// unknown zone got it too rather than the RangeError the spec requires. That
// is the worst of the three ways to be incomplete, because the caller has no
// way to find out.
//
// The set of identifiers is zoneDisplayNames' key set. It is not a second
// table: it was read out of ICU zone by zone (see tools/intlgen) and its keys
// agree exactly, name for name, with the IANA database Go bundles — which is
// what makes it usable as the answer to "is this a zone", the question
// ECMA-402 §6.4 asks.

import (
	"sync"
	"time"

	// Go looks for the zone database in the operating system's usual places
	// and, failing that, in $GOROOT/lib/time/zoneinfo.zip. On Windows there is
	// no system copy at all, and a deployed robot has no GOROOT, so
	// LoadLocation("America/New_York") fails there on exactly the machines this
	// engine runs on. Embedding the database costs ~450 KB and is the
	// difference between a timestamp in the zone that was asked for and a
	// silent fall back to the host's.
	_ "time/tzdata"
)

// tzIndex maps an ASCII-lowercased identifier to its canonical spelling.
// Built on first use: a host that formats nothing never pays for it.
var tzIndex = sync.OnceValue(func() map[string]string {
	m := make(map[string]string, len(zoneDisplayNames))
	for id := range zoneDisplayNames {
		m[asciiLower(id)] = id
	}
	return m
})

// asciiLower lowercases A-Z and leaves every other byte alone. Matching is
// ASCII-case-insensitive per GetAvailableNamedTimeZoneIdentifier, which is not
// the same as Unicode case folding: "Europe/İstanbul" and "asıa/baku" are
// invalid identifiers, and folding them the Unicode way would spell valid ones.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// zoneCache holds the *time.Location for each identifier that has been asked
// for. time.LoadLocation re-reads and re-parses the database on every call —
// it caches nothing — and format() would otherwise pay that per message.
var (
	zoneCacheMu sync.Mutex
	zoneCache   = map[string]*time.Location{}
)

func loadZone(id string) (*time.Location, bool) {
	zoneCacheMu.Lock()
	defer zoneCacheMu.Unlock()
	if loc, ok := zoneCache[id]; ok {
		return loc, loc != nil
	}
	loc, err := time.LoadLocation(id)
	if err != nil {
		// Cached as a miss so a script in a loop does not re-read the database
		// to be told the same thing. Only reachable if the embedded copy above
		// is somehow not linked in.
		zoneCache[id] = nil
		return nil, false
	}
	zoneCache[id] = loc
	return loc, true
}

// resolveTimeZone implements the identifier half of CreateDateTimeFormat steps
// 29-31: an offset string, or a named zone matched case-insensitively. It
// reports the identifier to store, which is the one that was ASKED for, not the
// zone it links to — Asia/Calcutta and Asia/Kolkata are the same instant and
// two different answers from resolvedOptions().
func resolveTimeZone(name string) (string, *time.Location, bool) {
	if off, ok := parseOffsetTimeZone(name); ok {
		return formatOffsetTimeZone(off), time.FixedZone(formatOffsetTimeZone(off), off*60), true
	}
	// A non-ASCII byte cannot appear in an IANA identifier, and the lookup
	// below would only fail more slowly.
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			return "", nil, false
		}
	}
	id, ok := tzIndex()[asciiLower(name)]
	if !ok {
		return "", nil, false
	}
	loc, ok := loadZone(id)
	if !ok {
		return "", nil, false
	}
	return id, loc, true
}

// parseOffsetTimeZone implements IsTimeZoneOffsetString, returning the offset in
// minutes. The grammar is ASCIISign Hour (TimeSeparator MinuteSecond)? with no
// sub-minute precision: "+05", "+05:30" and "+0530" are identifiers, "+5",
// "+15:59:00" and a U+2212 MINUS SIGN in place of the hyphen are not.
func parseOffsetTimeZone(s string) (int, bool) {
	if len(s) < 3 || (s[0] != '+' && s[0] != '-') {
		return 0, false
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	rest := s[1:]
	two := func(p string) (int, bool) {
		if len(p) != 2 || p[0] < '0' || p[0] > '9' || p[1] < '0' || p[1] > '9' {
			return 0, false
		}
		return int(p[0]-'0')*10 + int(p[1]-'0'), true
	}
	var hh, mm string
	switch len(rest) {
	case 2:
		hh = rest
	case 4:
		hh, mm = rest[:2], rest[2:]
	case 5:
		if rest[2] != ':' {
			return 0, false
		}
		hh, mm = rest[:2], rest[3:]
	default:
		return 0, false
	}
	h, ok := two(hh)
	if !ok || h > 23 {
		return 0, false
	}
	m := 0
	if mm != "" {
		if m, ok = two(mm); !ok || m > 59 {
			return 0, false
		}
	}
	return sign * (h*60 + m), true
}

// formatOffsetTimeZone is FormatOffsetTimeZoneIdentifier: the canonical
// ±HH:MM spelling that resolvedOptions() reports for an offset zone, whichever
// of the three accepted spellings was passed in.
func formatOffsetTimeZone(minutes int) string {
	sign := "+"
	if minutes < 0 {
		sign, minutes = "-", -minutes
	}
	return sign + twoDigits(minutes/60) + ":" + twoDigits(minutes%60)
}

// optionTimeZone reads options.timeZone and resolves it, throwing the way
// CreateDateTimeFormat does: TypeError for an options argument that is not
// coercible to an object, RangeError for an identifier that is not a zone —
// both at construction, so a bad zone is reported where it was written rather
// than on the first format() somewhere else.
func (rt *Runtime) optionTimeZone(options Value) (string, *ThrowError) {
	if options.IsUndefined() {
		return localZoneID(), nil
	}
	if options.IsNull() {
		return "", rt.typeError("Options must be an object")
	}
	v, e := rt.getField(options, "timeZone")
	if e != nil {
		return "", e
	}
	if v.IsUndefined() {
		return localZoneID(), nil
	}
	s, e := rt.toStringValue(v)
	if e != nil {
		return "", e
	}
	name := rt.strGo(s)
	id, _, ok := resolveTimeZone(name)
	if !ok {
		return "", rt.rangeError("Invalid time zone specified: " + name)
	}
	return id, nil
}

// zoneFor is the *time.Location an already-validated identifier denotes. The
// host zone stands in for one that cannot be loaded, which the validation above
// has already ruled out for anything reaching here.
func zoneFor(id string) *time.Location {
	if off, ok := parseOffsetTimeZone(id); ok {
		return time.FixedZone(id, off*60)
	}
	if loc, ok := loadZone(id); ok {
		return loc
	}
	return localLoc()
}

// intlZoneOf reads back the zone a DateTimeFormat was constructed with. As with
// intlLocaleOf, `format` pulled off the prototype rather than an instance
// formats against the host zone.
func (rt *Runtime) intlZoneID(this Value) string {
	if o := rt.objPtr(this); o != nil {
		if v := o.getSlot(slotIntlTimeZone); v.IsString() {
			return rt.strGo(v)
		}
	}
	return localZoneID()
}
