package engine

// Time zones as Temporal needs them: the offset a zone was at an instant, and
// the instants a local wall-clock reading could have been.
//
// The second question is the one a formatter never has to ask. Twice a year a
// local time either happens twice or does not happen at all, and a
// ZonedDateTime built from a date and a time has to say which of those it is
// looking at.

import (
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"
)

// temporalZone is a resolved time zone: either a fixed offset or a named zone
// out of the IANA database.
type temporalZone struct {
	id     string
	loc    *time.Location
	offset int64 // nanoseconds, for the fixed kind
	fixed  bool
}

// temporalZoneFor resolves an identifier. Offsets are accepted only at minute
// precision, which is what a time zone identifier may be.
func temporalZoneFor(id string) (temporalZone, bool) {
	if off, ok := parseOffsetTimeZone(id); ok {
		return temporalZone{id: formatOffsetTimeZone(off),
			offset: int64(off) * nsPerMinute, fixed: true}, true
	}
	name, loc, ok := resolveTimeZone(id)
	if !ok {
		return temporalZone{}, false
	}
	return temporalZone{id: name, loc: loc}, true
}

// offsetNs is the offset the zone was at, at an instant.
func (z temporalZone) offsetNs(epochNs *big.Int) int64 {
	if z.fixed {
		return z.offset
	}
	sec := new(big.Int).Div(epochNs, bigInt(nsPerSecond))
	if !sec.IsInt64() {
		return 0
	}
	_, off := time.Unix(sec.Int64(), 0).In(z.loc).Zone()
	return int64(off) * nsPerSecond
}

// possibleInstants is every instant that reads as this local time in this zone:
// two of them where a clock went back, none where it went forward, one the rest
// of the year.
func (z temporalZone) possibleInstants(dt isoDateTimeRec) []*big.Int {
	local := isoDateTimeToEpochNanoseconds(dt, 0)
	if z.fixed {
		return []*big.Int{new(big.Int).Sub(local, bigInt(z.offset))}
	}
	// The offsets a day either side bracket any transition near this time.
	before := z.offsetNs(new(big.Int).Sub(local, bigNsPerDay))
	after := z.offsetNs(new(big.Int).Add(local, bigNsPerDay))
	var out []*big.Int
	for _, off := range []int64{before, after} {
		cand := new(big.Int).Sub(local, bigInt(off))
		if z.offsetNs(cand) != off {
			continue
		}
		dup := false
		for _, have := range out {
			if have.Cmp(cand) == 0 {
				dup = true
			}
		}
		if !dup {
			out = append(out, cand)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cmp(out[j]) < 0 })
	return out
}

// disambiguate picks one instant out of what a local time could have meant.
// The interesting case is the third: a time that never happened, where the
// answer is on the far side of the gap.
func (z temporalZone) disambiguate(dt isoDateTimeRec, mode string) (*big.Int, bool) {
	possible := z.possibleInstants(dt)
	switch {
	case len(possible) == 1:
		return possible[0], true
	case len(possible) > 1:
		switch mode {
		case "earlier", "compatible":
			return possible[0], true
		case "later":
			return possible[len(possible)-1], true
		}
		return nil, false // "reject"
	}
	if mode == "reject" {
		return nil, false
	}
	// A gap. Its width is the difference between the offsets either side, and
	// stepping the local time by that width lands on the other side of it.
	local := isoDateTimeToEpochNanoseconds(dt, 0)
	before := z.offsetNs(new(big.Int).Sub(local, bigNsPerDay))
	after := z.offsetNs(new(big.Int).Add(local, bigNsPerDay))
	gap := after - before
	var shifted isoDateTimeRec
	if mode == "earlier" {
		shifted = addNsToDateTime(dt, -gap)
	} else {
		shifted = addNsToDateTime(dt, gap)
	}
	possible = z.possibleInstants(shifted)
	if len(possible) == 0 {
		return nil, false
	}
	if mode == "earlier" {
		return possible[len(possible)-1], true
	}
	return possible[0], true
}

// addNsToDateTime moves a local date-time by a count of nanoseconds without
// consulting any zone.
func addNsToDateTime(dt isoDateTimeRec, ns int64) isoDateTimeRec {
	total := isoDateTimeToEpochNanoseconds(dt, 0)
	total.Add(total, bigInt(ns))
	days, t := balanceTime(total)
	return isoDateTimeRec{epochDaysToISODate(int(days.Int64())), t}
}

// startOfDay is the first instant of a calendar day in a zone, which is
// midnight except on the days a zone starts its clocks somewhere else.
func (z temporalZone) startOfDay(d isoDateRec) (*big.Int, bool) {
	dt := isoDateTimeRec{d, midnightTime()}
	if p := z.possibleInstants(dt); len(p) > 0 {
		// A day that begins after the last instant does not begin: the day
		// after the last one is not a day this engine can hand back the start
		// of, which is what makes hoursInDay unanswerable there.
		if !epochNsWithinLimits(p[0]) {
			return nil, false
		}
		return p[0], true
	}
	// Midnight never happened, so the day begins at the moment the clocks
	// moved -- which is not midnight and is not the round hour after it
	// either. America/Toronto skipped 1919-03-30T23:30 to 00:30, so the
	// thirty-first began at half past twelve.
	local := isoDateTimeToEpochNanoseconds(dt, 0)
	if at, ok := z.transition(new(big.Int).Sub(local, bigNsPerDay), true); ok {
		return at, true
	}
	return z.disambiguate(dt, "compatible")
}

// getISODateTimeForZone is the local reading of an instant.
func (z temporalZone) dateTimeFor(epochNs *big.Int) isoDateTimeRec {
	return getISODateTimeFor(z.offsetNs(epochNs), epochNs)
}

// primaryTimeZone resolves a zone identifier to the one IANA calls primary, so
// that Asia/Calcutta and Asia/Kolkata compare equal. The links come from CLDR's
// bcp47 timezone data, where every identifier for one zone is listed together.
func primaryTimeZone(id string) (string, bool) {
	if _, ok := parseOffsetTimeZone(id); ok {
		return id, true
	}
	lower := asciiLower(id)
	if p, ok := tzPrimary()[lower]; ok {
		return p, true
	}
	if _, ok := tzIndex()[lower]; ok {
		return lower, true
	}
	return "", false
}

// tzPrimary maps every spelling to the primary one, built once from the table
// the generator wrote.
var tzPrimary = sync.OnceValue(func() map[string]string {
	m := make(map[string]string, 1024)
	for _, group := range cldrTimeZoneAliases {
		names := strings.Fields(group)
		if len(names) == 0 {
			continue
		}
		primary := asciiLower(names[0])
		for _, n := range names {
			m[asciiLower(n)] = primary
		}
	}
	return m
})
