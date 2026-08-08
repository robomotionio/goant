package engine

// The Chinese and Korean lunisolar calendars.
//
// These two are not arithmetic. A month begins on the day of a new moon, and
// which month it IS depends on where the sun is: the month containing the
// winter solstice is the eleventh, and a year that needs a thirteenth month
// puts it wherever a month falls with no major solar term in it. So the
// calendar cannot be computed without computing the moon and the sun.
//
// The formulae are the standard ones -- Meeus' Astronomical Algorithms, as
// arranged in Calendrical Calculations -- truncated to the terms that matter at
// the precision a calendar needs. A month boundary is a midnight, so an error
// of a few minutes is invisible; the terms kept here are good to well under an
// hour over the range a script can reach.
//
// Everything is computed in the calendar's own zone, because the day a new moon
// falls on is a local question: the same instant is the 1st in Shanghai and the
// 30th in Honolulu. China has kept UTC+8 since 1929 and Korea UTC+9 since 1961,
// and before those dates the older offsets are used, which is what ICU does.

import (
	"math"
	"strconv"
	"sync"
)

// ---- moments ----
//
// A moment is a day number with a fraction: 0.0 is 1970-01-01T00:00Z and 0.5
// is noon that day. It is the same unit as a calendar day number, which is what
// makes "which day did this new moon fall on" a floor.

// julianDay converts a moment to a Julian Day number, which is what the
// astronomical formulae are written against.
func julianDay(t float64) float64 { return t + 2440587.5 }

// julianCentury is the time in Julian centuries from J2000.0, in Terrestrial
// Time.
func julianCentury(t float64) float64 {
	return (julianDay(t+deltaT(t)/86400) - 2451545.0) / 36525.0
}

// deltaT is TT - UT in seconds. The polynomial series is Espenak and Meeus's,
// and only the segments a JavaScript date can reach are kept.
func deltaT(t float64) float64 {
	year := 1970 + t/365.2425
	switch {
	case year >= 2150:
		u := (year - 1820) / 100
		return -20 + 32*u*u
	case year >= 2050:
		return -20 + 32*math.Pow((year-1820)/100, 2) - 0.5628*(2150-year)
	case year >= 2005:
		u := year - 2000
		return 62.92 + 0.32217*u + 0.005589*u*u
	case year >= 1986:
		u := year - 2000
		return 63.86 + 0.3345*u - 0.060374*u*u + 0.0017275*u*u*u +
			0.000651814*u*u*u*u + 0.00002373599*u*u*u*u*u
	case year >= 1961:
		u := year - 1975
		return 45.45 + 1.067*u - u*u/260 - u*u*u/718
	case year >= 1941:
		u := year - 1950
		return 29.07 + 0.407*u - u*u/233 + u*u*u/2547
	case year >= 1920:
		u := year - 1920
		return 21.20 + 0.84493*u - 0.076100*u*u + 0.0020936*u*u*u
	case year >= 1900:
		u := year - 1900
		return -2.79 + 1.494119*u - 0.0598939*u*u + 0.0061966*u*u*u - 0.000197*u*u*u*u
	case year >= 1860:
		u := year - 1860
		return 7.62 + 0.5737*u - 0.251754*u*u + 0.01680668*u*u*u -
			0.0004473624*u*u*u*u + u*u*u*u*u/233174
	case year >= 1800:
		u := year - 1800
		return 13.72 - 0.332447*u + 0.0068612*u*u + 0.0041116*u*u*u -
			0.00037436*u*u*u*u + 0.0000121272*u*u*u*u*u -
			0.0000001699*u*u*u*u*u*u + 0.000000000875*u*u*u*u*u*u*u
	case year >= 1700:
		u := year - 1700
		return 8.83 + 0.1603*u - 0.0059285*u*u + 0.00013336*u*u*u - u*u*u*u/1174000
	case year >= 1600:
		u := year - 1600
		return 120 - 0.9808*u - 0.01532*u*u + u*u*u/7129
	default:
		u := (year - 1820) / 100
		return -20 + 32*u*u
	}
}

func degToRad(d float64) float64 { return d * math.Pi / 180 }

// normDegrees folds an angle into [0, 360).
func normDegrees(d float64) float64 {
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	return d
}

// ---- the sun ----

// solarLongitude is the sun's apparent longitude in degrees at a moment. The
// series is Meeus chapter 25 with the periodic terms that reach a hundredth of
// a degree, which is a quarter of a minute of solar-term time.
func solarLongitude(t float64) float64 {
	c := julianCentury(t)
	// Geometric mean longitude and the mean anomaly.
	l0 := 280.46646 + 36000.76983*c + 0.0003032*c*c
	m := 357.52911 + 35999.05029*c - 0.0001537*c*c
	mr := degToRad(m)
	// The equation of the centre.
	eq := (1.914602-0.004817*c-0.000014*c*c)*math.Sin(mr) +
		(0.019993-0.000101*c)*math.Sin(2*mr) +
		0.000289*math.Sin(3*mr)
	trueLong := l0 + eq
	// Nutation and aberration, as the apparent longitude needs both.
	omega := 125.04 - 1934.136*c
	return normDegrees(trueLong - 0.00569 - 0.00478*math.Sin(degToRad(omega)))
}

// solarLongitudeAfter is the first moment at or after t when the sun's
// longitude is exactly lambda degrees, found by bisection on the difference.
// The sun moves about a degree a day, so the bracket is a fortnight either way.
func solarLongitudeAfter(lambda, t float64) float64 {
	rate := 365.2425 / 360
	// A first guess from where the sun is now, then bisected.
	delta := normDegrees(lambda - solarLongitude(t))
	lo := t + rate*(delta-2)
	hi := t + rate*(delta+2)
	if lo < t {
		lo = t
	}
	for i := 0; i < 60 && hi-lo > 1e-7; i++ {
		mid := (lo + hi) / 2
		if normDegrees(solarLongitude(mid)-lambda) < 180 {
			hi = mid
		} else {
			lo = mid
		}
	}
	return (lo + hi) / 2
}

// ---- the moon ----

// meanNewMoon is the mean time of the k'th new moon after the one of January
// 2000, as a moment. k need not be an integer.
func meanNewMoon(k float64) float64 {
	c := k / 1236.85
	jde := 2451550.09766 + 29.530588861*k +
		0.00015437*c*c - 0.000000150*c*c*c + 0.00000000073*c*c*c*c
	return jde - 2440587.5
}

// nthNewMoon is the k'th new moon, with the periodic corrections of Meeus
// chapter 49 applied: the moon's orbit is eccentric enough that the mean time
// is out by up to fourteen hours.
func nthNewMoon(k float64) float64 {
	c := k / 1236.85
	// The four arguments the corrections are written in.
	e := 1 - 0.002516*c - 0.0000074*c*c
	m := degToRad(normDegrees(2.5534 + 29.10535670*k - 0.0000014*c*c - 0.00000011*c*c*c))
	mp := degToRad(normDegrees(201.5643 + 385.81693528*k + 0.0107582*c*c +
		0.00001238*c*c*c - 0.000000058*c*c*c*c))
	f := degToRad(normDegrees(160.7108 + 390.67050284*k - 0.0016118*c*c -
		0.00000227*c*c*c + 0.000000011*c*c*c*c))
	omega := degToRad(normDegrees(124.7746 - 1.56375588*k + 0.0020672*c*c + 0.00000215*c*c*c))

	corr := -0.40720*math.Sin(mp) +
		0.17241*e*math.Sin(m) +
		0.01608*math.Sin(2*mp) +
		0.01039*math.Sin(2*f) +
		0.00739*e*math.Sin(mp-m) +
		-0.00514*e*math.Sin(mp+m) +
		0.00208*e*e*math.Sin(2*m) +
		-0.00111*math.Sin(mp-2*f) +
		-0.00057*math.Sin(mp+2*f) +
		0.00056*e*math.Sin(2*mp+m) +
		-0.00042*math.Sin(3*mp) +
		0.00042*e*math.Sin(m+2*f) +
		0.00038*e*math.Sin(m-2*f) +
		-0.00024*e*math.Sin(2*mp-m) +
		-0.00017*math.Sin(omega) +
		-0.00007*math.Sin(mp+2*m) +
		0.00004*math.Sin(2*mp-2*f) +
		0.00004*math.Sin(3*m) +
		0.00003*math.Sin(mp+m-2*f) +
		0.00003*math.Sin(2*mp+2*f) +
		-0.00003*math.Sin(mp+m+2*f) +
		0.00003*math.Sin(mp-m+2*f) +
		-0.00002*math.Sin(mp-m-2*f) +
		-0.00002*math.Sin(3*mp+m) +
		0.00002*math.Sin(4*mp)

	// The additional planetary corrections, which reach a quarter of a minute.
	a := []struct{ amp, deg, rate float64 }{
		{0.000325, 299.77, 0.107408}, {0.000165, 251.88, 0.016321},
		{0.000164, 251.83, 26.651886}, {0.000126, 349.42, 36.412478},
		{0.000110, 84.66, 18.206239}, {0.000062, 141.74, 53.303771},
		{0.000060, 207.14, 2.453732}, {0.000056, 154.84, 7.306860},
		{0.000047, 34.52, 27.261239}, {0.000042, 207.19, 0.121824},
		{0.000040, 291.34, 1.844379}, {0.000037, 161.72, 24.198154},
		{0.000035, 239.56, 25.513099}, {0.000023, 331.55, 3.592518},
	}
	extra := 0.0
	for _, term := range a {
		arg := term.deg + term.rate*k
		if term.amp == 0.000325 {
			arg -= 0.009173 * c * c
		}
		extra += term.amp * math.Sin(degToRad(normDegrees(arg)))
	}
	// The result is Terrestrial Time; a calendar wants Universal.
	tt := meanNewMoon(k) + corr + extra
	return tt - deltaT(tt)/86400
}

// newMoonBefore is the last new moon strictly before the moment t, and
// newMoonAtOrAfter the first at or after it.
func newMoonBefore(t float64) float64 {
	k := math.Floor((t - meanNewMoon(0)) / 29.530588861)
	for nthNewMoon(k) >= t {
		k--
	}
	for nthNewMoon(k+1) < t {
		k++
	}
	return nthNewMoon(k)
}

func newMoonAtOrAfter(t float64) float64 {
	k := math.Floor((t-meanNewMoon(0))/29.530588861) - 1
	for nthNewMoon(k) < t {
		k++
	}
	return nthNewMoon(k)
}

// ---- the lunisolar calendars ----

type lunisolarCalendar struct {
	zone string
	id   string
}

// offsetDays is the zone's offset from UTC as a fraction of a day, at a moment.
// The two zones have each used one offset for the whole modern period and a
// different one before it; the switch dates are the ones ICU uses.
func (c lunisolarCalendar) offsetDays(t float64) float64 {
	if c.id == "dangi" {
		// Korea: UTC+8:30 until 1912, then a period on Japanese time, then
		// UTC+9 from 1961. The intermediate years are within an hour of the
		// modern offset, which is under the precision this needs.
		if t < float64(isoDay(1912, 1, 1)) {
			return 8.5 / 24
		}
		return 9.0 / 24
	}
	// China: Beijing local mean time until 1929, then UTC+8.
	if t < float64(isoDay(1929, 1, 1)) {
		return (7 + 45.0/60 + 40.0/3600) / 24
	}
	return 8.0 / 24
}

// localDay is the day number a moment falls on in the calendar's zone.
func (c lunisolarCalendar) localDay(t float64) int {
	return int(math.Floor(t + c.offsetDays(t)))
}

// winterSolstice is the December solstice on or before the given day, as the
// day it falls on locally. The sun is at 270 degrees there.
func (c lunisolarCalendar) winterSolsticeOnOrBefore(day int) int {
	// Start from the solstice of the Gregorian year the day falls in, then
	// step back a year if that one is later.
	y, _, _ := isoDate(day)
	for {
		t := solarLongitudeAfter(270, float64(isoDay(y, 12, 1)))
		if d := c.localDay(t); d <= day {
			return d
		}
		y--
	}
}

// hasMajorTerm reports whether the month beginning at newMoonDay contains one
// of the twelve major solar terms -- the sun crossing a multiple of thirty
// degrees. A month without one is the leap month.
func (c lunisolarCalendar) hasMajorTerm(start, next int) bool {
	// The major term after the month's first midnight, if it lands before the
	// next month begins.
	t := float64(start) - c.offsetDays(float64(start))
	lambda := solarLongitude(t)
	target := math.Ceil(lambda/30) * 30
	when := solarLongitudeAfter(math.Mod(target, 360), t)
	return c.localDay(when) < next
}

// lunisolarYear is one calendar year: the day each of its months begins, and
// which of them (if any) is the leap month.
type lunisolarYear struct {
	starts []int // month starts, first is the first day of the year
	leap   int   // 1-based index into starts of the leap month, 0 for none
	year   int   // the calendar's own year number
}

// yearContaining computes the lunisolar year that the given day falls in.
//
// The rule: the month containing the December solstice is month 11. Count the
// new moons from one solstice-month to the next; if there are thirteen, one of
// them is a leap month, and it is the first that contains no major solar term.
func (c lunisolarCalendar) yearContaining(day int) lunisolarYear {
	for {
		y := c.suiFrom(c.winterSolsticeOnOrBefore(day))
		if len(y.starts) > 0 && day >= y.starts[0] {
			if day < y.starts[len(y.starts)-1]+40 {
				return y
			}
		}
		// The day is before this year's first month, so it belongs to the
		// previous one.
		day = y.starts[0] - 1
	}
}

// suiFrom builds the calendar year whose eleventh month contains the solstice
// on the given day.
func (c lunisolarCalendar) suiFrom(solstice int) lunisolarYear {
	m11 := c.localDay(newMoonBefore(float64(solstice) + 1 - c.offsetDays(float64(solstice))))
	// The next year's eleventh month, to count the months between.
	nextSolstice := c.localDay(solarLongitudeAfter(270, float64(solstice)+10))
	m11next := c.localDay(newMoonBefore(float64(nextSolstice) + 1 - c.offsetDays(float64(nextSolstice))))

	var months []int
	for d := m11; d < m11next; {
		months = append(months, d)
		d = c.localDay(newMoonAtOrAfter(float64(d) + 1 - c.offsetDays(float64(d))))
	}
	leapSui := len(months) == 13
	// The leap month is the first without a major term, counting from the
	// month after the eleventh.
	leapAt := 0
	if leapSui {
		for i := 0; i < len(months); i++ {
			next := m11next
			if i+1 < len(months) {
				next = months[i+1]
			}
			if !c.hasMajorTerm(months[i], next) {
				leapAt = i
				break
			}
		}
	}
	// Month 1 is two months after month 11, or three when the leap falls in
	// between. Its index in `months`:
	first := 2
	if leapSui && leapAt <= 2 && leapAt != 0 {
		first = 3
	}
	if first >= len(months) {
		first = len(months) - 1
	}
	// The year runs from month 1 to the month before the NEXT year's month 1,
	// which is the next sui's. Its months are the tail of this sui plus the
	// head of the next, so it is built by walking new moons from month 1.
	starts := []int{months[first]}
	for len(starts) < 13 {
		d := c.localDay(newMoonAtOrAfter(float64(starts[len(starts)-1]) + 1 -
			c.offsetDays(float64(starts[len(starts)-1]))))
		starts = append(starts, d)
	}
	// Where the leap month falls inside the calendar year, if it does.
	leapIndex := 0
	if leapSui && leapAt >= first {
		leapIndex = leapAt - first + 1
	}
	// A leap month before month 1 belongs to the previous calendar year, and
	// this year then has twelve months.
	count := 12
	if leapIndex > 0 {
		count = 13
	}
	starts = starts[:count]
	gy, _, _ := isoDate(starts[0])
	return lunisolarYear{starts: starts, leap: leapIndex, year: c.yearNumber(gy)}
}

// yearNumber is what the year is called. The Chinese and Korean calendars have
// epochs of their own -- 2637 BC and 2333 BC -- but a year is named by the
// Gregorian year it begins in, which is what a caller writing
// {year: 2020, monthCode: "M04L"} means and what the sexagenary cycle is
// derived from rather than reported as.
func (c lunisolarCalendar) yearNumber(gregorianYear int) int { return gregorianYear }

func (c lunisolarCalendar) gregorianYear(year int) int { return year }

// cyclicYear is the year in the calendar's own count, from which the stem and
// branch that name it are taken.
func (c lunisolarCalendar) cyclicYear(year int) int {
	if c.id == "dangi" {
		return year + 2333
	}
	return year + 2637
}

// lunisolarCache memoises the years, which are expensive: each one is a dozen
// new moons and a solstice, and every field access would otherwise recompute
// them.
var (
	lunisolarMu    sync.Mutex
	lunisolarYears = map[string]lunisolarYear{}
)

func (c lunisolarCalendar) year(n int) lunisolarYear {
	key := c.id + ":" + strconv.Itoa(n)
	lunisolarMu.Lock()
	if y, ok := lunisolarYears[key]; ok {
		lunisolarMu.Unlock()
		return y
	}
	lunisolarMu.Unlock()
	// A day safely inside the calendar year: it begins between late January
	// and late February, so mid-March is always in it.
	y := c.yearContaining(isoDay(c.gregorianYear(n), 3, 15))
	lunisolarMu.Lock()
	lunisolarYears[key] = y
	lunisolarMu.Unlock()
	return y
}

func (c lunisolarCalendar) dateFromDay(day int) calendarDate {
	// Through the cache rather than through yearContaining: a lunisolar year
	// costs a solstice and thirteen new moons to compute, and a loop over days
	// would pay that for every one of them.
	gy, _, _ := isoDate(day)
	y := c.year(c.yearNumber(gy))
	if day < y.starts[0] {
		y = c.year(c.yearNumber(gy) - 1)
	} else if next := c.year(c.yearNumber(gy) + 1); day >= next.starts[0] {
		y = next
	}
	m := 1
	for m < len(y.starts) && y.starts[m] <= day {
		m++
	}
	return calendarDate{year: y.year, eraYear: y.year, month: m,
		day: day - y.starts[m-1] + 1, code: c.codeFor(y, m)}
}

func (c lunisolarCalendar) dayFromDate(year, month, day int) int {
	y := c.year(year)
	if month < 1 {
		month = 1
	}
	if month > len(y.starts) {
		month = len(y.starts)
	}
	return y.starts[month-1] + day - 1
}

func (c lunisolarCalendar) monthsInYear(year int) int { return len(c.year(year).starts) }

func (c lunisolarCalendar) daysInMonth(year, month int) int {
	y := c.year(year)
	if month < 1 || month > len(y.starts) {
		return 29
	}
	if month == len(y.starts) {
		return c.year(year + 1).starts[0] - y.starts[month-1]
	}
	return y.starts[month] - y.starts[month-1]
}

func (c lunisolarCalendar) daysInYear(year int) int {
	return c.year(year + 1).starts[0] - c.year(year).starts[0]
}

func (c lunisolarCalendar) inLeapYear(year int) bool { return c.year(year).leap != 0 }

// codeFor names a month. A leap month takes the name of the month before it
// with an L on the end, and the months after it shift back by one.
func (c lunisolarCalendar) codeFor(y lunisolarYear, month int) string {
	switch {
	case y.leap == 0 || month < y.leap:
		return simpleMonthCode(month)
	case month == y.leap:
		return simpleMonthCode(month-1) + "L"
	default:
		return simpleMonthCode(month - 1)
	}
}

func (c lunisolarCalendar) monthCode(year, month int) string {
	return c.codeFor(c.year(year), month)
}

func (c lunisolarCalendar) monthFromCode(year string, code string) (int, bool) {
	n, err := atoiErr(year)
	if err {
		return 0, false
	}
	m, leap, ok := parseMonthCode(code)
	if !ok || m > 12 {
		return 0, false
	}
	y := c.year(n)
	if leap {
		if y.leap == 0 || y.leap != m+1 {
			return 0, false
		}
		return y.leap, true
	}
	if y.leap != 0 && m >= y.leap {
		return m + 1, true
	}
	return m, true
}

func (c lunisolarCalendar) eraOf(year int) (string, int) { return "", year }
func (c lunisolarCalendar) eras() []string               { return nil }

func (c lunisolarCalendar) yearFromEra(string, int) (int, bool) { return 0, false }
