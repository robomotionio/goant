package engine

// The arithmetic underneath Temporal: an ISO date, a time of day, the two
// together, and the conversions between those and a count of nanoseconds since
// the epoch.
//
// None of this is JavaScript. Everything here is a plain Go value with no
// runtime attached, which is what makes it testable on its own and what keeps
// the object files above it about objects.

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// ---- the records ----

// isoDateRec is a date in the ISO 8601 calendar. Every Temporal object stores
// its date this way whatever calendar it displays in: the calendar is a lens,
// the ISO date is the thing.
type isoDateRec struct{ year, month, day int }

// isoTimeRec is a time of day, to the nanosecond.
type isoTimeRec struct{ hour, minute, second, ms, us, ns int }

// isoDateTimeRec is both.
type isoDateTimeRec struct {
	date isoDateRec
	time isoTimeRec
}

const (
	nsPerMicro  = 1000
	nsPerMilli  = 1000 * nsPerMicro
	nsPerSecond = 1000 * nsPerMilli
	nsPerMinute = 60 * nsPerSecond
	nsPerHour   = 60 * nsPerMinute
	nsPerDay    = 24 * nsPerHour
	secsPerDay  = 24 * 60 * 60
)

// The outer edge of representable time: a hundred million days either side of
// the epoch, which is what an Instant may hold. A date-time may sit one day
// beyond that, so that every instant has a local time in every zone.
var (
	bigNsPerDay  = big.NewInt(nsPerDay)
	nsMaxInstant = new(big.Int).Mul(big.NewInt(secsPerDay*100000000), big.NewInt(nsPerSecond))
	nsMinInstant = new(big.Int).Neg(nsMaxInstant)
)

func bigInt(i int64) *big.Int { return big.NewInt(i) }

// ---- days ----

// isoDateToEpochDays is the day number of an ISO date, with out-of-range months
// and days rolling over -- which is what lets month arithmetic be written as
// "add to the month, then convert".
func isoDateToEpochDays(y, m, d int) int { return isoDay(y, m, d) }

func epochDaysToISODate(day int) isoDateRec {
	y, m, d := isoDate(day)
	return isoDateRec{y, m, d}
}

func (d isoDateRec) epochDays() int { return isoDay(d.year, d.month, d.day) }

// balanceISODate normalises a date whose month or day is out of range.
func balanceISODate(y, m, d int) isoDateRec {
	y, m = balanceISOYearMonth(y, m)
	return epochDaysToISODate(isoDay(y, m, d))
}

func balanceISOYearMonth(y, m int) (int, int) {
	y += floorDiv(m-1, 12)
	return y, floorMod(m-1, 12) + 1
}

// ---- the time of day ----

// nanosecond is the time as a count of nanoseconds since midnight.
func (t isoTimeRec) nanosecond() int64 {
	return int64(t.hour)*nsPerHour + int64(t.minute)*nsPerMinute +
		int64(t.second)*nsPerSecond + int64(t.ms)*nsPerMilli +
		int64(t.us)*nsPerMicro + int64(t.ns)
}

// timeFromNanoseconds is the reverse, for a value already inside a day.
func timeFromNanoseconds(n int64) isoTimeRec {
	var t isoTimeRec
	t.ns = int(n % 1000)
	n /= 1000
	t.us = int(n % 1000)
	n /= 1000
	t.ms = int(n % 1000)
	n /= 1000
	t.second = int(n % 60)
	n /= 60
	t.minute = int(n % 60)
	n /= 60
	t.hour = int(n)
	return t
}

func midnightTime() isoTimeRec { return isoTimeRec{} }
func noonTime() isoTimeRec     { return isoTimeRec{hour: 12} }

// balanceTime carries a time that has overflowed its day into a day count and
// a time inside it. The count is what the caller adds to its date.
func balanceTime(n *big.Int) (days *big.Int, t isoTimeRec) {
	days = new(big.Int)
	rest := new(big.Int)
	days.DivMod(n, bigNsPerDay, rest) // Euclidean: rest is never negative
	return days, timeFromNanoseconds(rest.Int64())
}

func isValidTime(h, mi, s, ms, us, ns int) bool {
	return h >= 0 && h <= 23 && mi >= 0 && mi <= 59 && s >= 0 && s <= 59 &&
		ms >= 0 && ms <= 999 && us >= 0 && us <= 999 && ns >= 0 && ns <= 999
}

// constrainTime clamps every field into range, which is what overflow:
// "constrain" asks for.
func constrainTime(h, mi, s, ms, us, ns int) isoTimeRec {
	return isoTimeRec{
		hour: clampInt(h, 0, 23), minute: clampInt(mi, 0, 59),
		second: clampInt(s, 0, 59), ms: clampInt(ms, 0, 999),
		us: clampInt(us, 0, 999), ns: clampInt(ns, 0, 999),
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---- validity ----

func isValidISODate(y, m, d int) bool {
	if m < 1 || m > 12 {
		return false
	}
	return d >= 1 && d <= isoDaysInMonth(y, m)
}

// isoDateWithinLimits asks whether a date can be represented at all. The test
// is made at noon, so that a date is in range exactly when some instant in it
// is.
func isoDateWithinLimits(d isoDateRec) bool {
	return isoDateTimeWithinLimits(isoDateTimeRec{d, noonTime()})
}

func isoDateTimeWithinLimits(dt isoDateTimeRec) bool {
	days := dt.date.epochDays()
	if days > 100000001 || days < -100000001 {
		return false
	}
	ns := isoDateTimeToEpochNanoseconds(dt, 0)
	lo := new(big.Int).Sub(nsMinInstant, bigNsPerDay)
	hi := new(big.Int).Add(nsMaxInstant, bigNsPerDay)
	return ns.Cmp(lo) > 0 && ns.Cmp(hi) < 0
}

func epochNsWithinLimits(ns *big.Int) bool {
	return ns.Cmp(nsMinInstant) >= 0 && ns.Cmp(nsMaxInstant) <= 0
}

// ---- instants ----

// isoDateTimeToEpochNanoseconds is the instant a local date-time names, given
// the offset that local time is at.
func isoDateTimeToEpochNanoseconds(dt isoDateTimeRec, offsetNs int64) *big.Int {
	ns := new(big.Int).Mul(bigInt(int64(dt.date.epochDays())), bigNsPerDay)
	ns.Add(ns, bigInt(dt.time.nanosecond()))
	ns.Sub(ns, bigInt(offsetNs))
	return ns
}

// getISODateTimeFor is the local date-time an instant reads as at an offset.
func getISODateTimeFor(offsetNs int64, epochNs *big.Int) isoDateTimeRec {
	local := new(big.Int).Add(epochNs, bigInt(offsetNs))
	days, t := balanceTime(local)
	return isoDateTimeRec{epochDaysToISODate(int(days.Int64())), t}
}

// ---- comparison ----

func compareISODate(a, b isoDateRec) int {
	if a.year != b.year {
		return sign64(int64(a.year - b.year))
	}
	if a.month != b.month {
		return sign64(int64(a.month - b.month))
	}
	return sign64(int64(a.day - b.day))
}

func compareTimeRec(a, b isoTimeRec) int {
	return sign64(a.nanosecond() - b.nanosecond())
}

func compareISODateTime(a, b isoDateTimeRec) int {
	if c := compareISODate(a.date, b.date); c != 0 {
		return c
	}
	return compareTimeRec(a.time, b.time)
}

func sign64(i int64) int {
	switch {
	case i < 0:
		return -1
	case i > 0:
		return 1
	}
	return 0
}

// ---- rounding a quantity ----

// roundNumberToIncrement rounds x to a multiple of increment under one of the
// nine rounding modes. It is the one rounding routine Temporal has: every
// smallestUnit, every roundingIncrement and every total goes through it.
func roundNumberToIncrement(x, increment *big.Int, mode string) *big.Int {
	// Euclidean division puts the remainder in [0, increment), so the quotient
	// is the multiple below and one more is the multiple above.
	q, r := new(big.Int), new(big.Int)
	q.DivMod(x, increment, r)
	if r.Sign() == 0 {
		return new(big.Int).Set(x)
	}
	r1 := new(big.Int).Mul(q, increment)
	r2 := new(big.Int).Add(r1, increment)
	positive := x.Sign() > 0

	var up bool // take the multiple above?
	switch mode {
	case "ceil":
		up = true
	case "floor":
		up = false
	case "expand":
		up = positive
	case "trunc":
		up = !positive
	default:
		// The half-modes turn on how the remainder compares with half the
		// increment, which is a comparison of twice it against the whole.
		twice := new(big.Int).Lsh(r, 1)
		switch c := twice.Cmp(increment); {
		case c < 0:
			up = false
		case c > 0:
			up = true
		default:
			switch mode {
			case "halfCeil":
				up = true
			case "halfFloor":
				up = false
			case "halfExpand":
				up = positive
			case "halfTrunc":
				up = !positive
			default: // halfEven: the neighbour whose quotient is even
				up = q.Bit(0) == 1
			}
		}
	}
	if up {
		return r2
	}
	return r1
}

// ---- writing them out ----

func pad(n, width int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		s = strconv.Itoa(-n)
	}
	for len(s) < width {
		s = "0" + s
	}
	if n < 0 {
		return "-" + s
	}
	return s
}

// formatISOYear writes a year, which needs a sign and six digits once it is
// outside the four-digit range -- an expanded year, in ISO 8601's words.
func formatISOYear(y int) string {
	if y >= 0 && y <= 9999 {
		return pad(y, 4)
	}
	sign := "+"
	if y < 0 {
		sign = "-"
		y = -y
	}
	return sign + pad(y, 6)
}

func formatISODate(d isoDateRec) string {
	return formatISOYear(d.year) + "-" + pad(d.month, 2) + "-" + pad(d.day, 2)
}

// formatFractionalSeconds writes the sub-second part, including its leading
// dot, to a fixed number of digits or to as few as say everything.
func formatFractionalSeconds(ms, us, ns, precision int) string {
	if precision == 0 {
		return ""
	}
	frac := pad(ms, 3) + pad(us, 3) + pad(ns, 3)
	if precision < 0 { // "auto": as many as are needed, in groups of three
		frac = strings.TrimRight(frac, "0")
		if frac == "" {
			return ""
		}
		return "." + frac
	}
	return "." + frac[:precision]
}

// formatTimeString writes a time to the requested precision, where precision is
// the number of fractional digits, -1 for "auto", and -2 for "minute" -- which
// omits the seconds entirely.
func formatTimeString(t isoTimeRec, precision int) string {
	s := pad(t.hour, 2) + ":" + pad(t.minute, 2)
	if precision == -2 {
		return s
	}
	return s + ":" + pad(t.second, 2) + formatFractionalSeconds(t.ms, t.us, t.ns, precision)
}

func formatISODateTime(dt isoDateTimeRec, precision int) string {
	return formatISODate(dt.date) + "T" + formatTimeString(dt.time, precision)
}

// formatOffsetNanoseconds writes a UTC offset. Whole minutes are written as
// ±HH:MM; anything finer keeps the seconds it needs, which the older zones do
// have.
func formatOffsetNanoseconds(offsetNs int64) string {
	sign := "+"
	if offsetNs < 0 {
		sign = "-"
		offsetNs = -offsetNs
	}
	h := offsetNs / nsPerHour
	m := (offsetNs / nsPerMinute) % 60
	s := (offsetNs / nsPerSecond) % 60
	sub := offsetNs % nsPerSecond
	out := sign + pad(int(h), 2) + ":" + pad(int(m), 2)
	if s != 0 || sub != 0 {
		out += ":" + pad(int(s), 2)
		if sub != 0 {
			out += formatFractionalSeconds(int(sub/nsPerMilli), int(sub/nsPerMicro)%1000, int(sub%1000), -1)
		}
	}
	return out
}

// ---- numbers that must be integers ----

// isIntegralNumber is the test the Temporal constructors apply to every
// argument: a finite value with nothing after the point.
func isIntegralNumber(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f
}
