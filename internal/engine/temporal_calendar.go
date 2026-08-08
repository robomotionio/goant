package engine

// The bridge between Temporal and the sixteen calendars.
//
// Temporal stores every date as an ISO date and names a calendar beside it.
// Everything a calendar is asked -- what year is this, what is the month
// called, how many days has it, what does adding a year mean -- goes through
// here, which turns the ISO date into that calendar's own and back again.

import (
	"errors"
	"strconv"
)

var (
	errCalendarOverflow = errors.New("date is outside the range of its month")
	errCalendarFields   = errors.New("the fields do not name a date")
	errCalendarRange    = errors.New("date is outside the representable range")
	errCalendarValue    = errors.New("the fields contradict one another")
)

// calFieldSet is the set of calendar fields a caller supplied. Which of them
// are present matters as much as their values: month and monthCode say the same
// thing two ways, and era and eraYear are a pair.
type calFieldSet struct {
	era       string
	eraYear   int
	year      int
	month     int
	monthCode string
	day       int
	has       uint16
}

const (
	fEra = 1 << iota
	fEraYear
	fYear
	fMonth
	fMonthCode
	fDay
)

func (f calFieldSet) got(bit uint16) bool { return f.has&bit != 0 }

// calendarHasEras reports whether a calendar counts its years from named eras,
// which decides whether era and eraYear are fields it accepts at all.
func calendarHasEras(id string) bool { return len(calendarFor(id).eras()) > 0 }

// ---- reading a date out of fields ----

// resolveCalendarYear turns either a year or an era and a year within it into
// the arithmetic year the calendar counts in.
func resolveCalendarYear(id string, cal calendar, f calFieldSet) (int, error) {
	hasEraPair := f.got(fEra) && f.got(fEraYear)
	if f.got(fEra) != f.got(fEraYear) {
		// One without the other says nothing.
		return 0, errCalendarFields
	}
	if !f.got(fYear) && !hasEraPair {
		return 0, errCalendarFields
	}
	if hasEraPair {
		y, ok := cal.yearFromEra(f.era, f.eraYear)
		if !ok {
			return 0, errCalendarValue
		}
		if f.got(fYear) && f.year != y {
			return 0, errCalendarValue
		}
		return y, nil
	}
	return f.year, nil
}

// resolveCalendarMonth turns a month code, or an ordinal month, into the
// ordinal the calendar uses -- checking that the two agree where both are given.
func resolveCalendarMonth(cal calendar, year int, f calFieldSet, overflow string) (int, error) {
	if !f.got(fMonth) && !f.got(fMonthCode) {
		return 0, errCalendarFields
	}
	ys := strconv.Itoa(year)
	if f.got(fMonthCode) {
		m, ok := cal.monthFromCode(ys, f.monthCode)
		if !ok {
			// A month code the year does not have: the leap month of a year
			// that has none. Constraining drops to the month it follows.
			if overflow == "reject" {
				return 0, errCalendarOverflow
			}
			base, leap, good := parseMonthCode(f.monthCode)
			if !good || !leap {
				return 0, errCalendarValue
			}
			m, ok = cal.monthFromCode(ys, simpleMonthCode(base))
			if !ok {
				return 0, errCalendarValue
			}
		}
		if f.got(fMonth) && f.month != m {
			return 0, errCalendarValue
		}
		return m, nil
	}
	m := f.month
	if m < 1 {
		return 0, errCalendarOverflow
	}
	if n := cal.monthsInYear(year); m > n {
		if overflow == "reject" {
			return 0, errCalendarOverflow
		}
		m = n
	}
	return m, nil
}

// calendarDateFromFields is CalendarDateFromFields: a whole date out of the
// fields a caller gave, under the overflow rule they asked for.
func calendarDateFromFields(id string, f calFieldSet, overflow string) (isoDateRec, error) {
	cal := calendarFor(id)
	if !f.got(fDay) {
		return isoDateRec{}, errCalendarFields
	}
	year, err := resolveCalendarYear(id, cal, f)
	if err != nil {
		return isoDateRec{}, err
	}
	month, err := resolveCalendarMonth(cal, year, f, overflow)
	if err != nil {
		return isoDateRec{}, err
	}
	day := f.day
	if day < 1 {
		return isoDateRec{}, errCalendarOverflow
	}
	if n := cal.daysInMonth(year, month); day > n {
		if overflow == "reject" {
			return isoDateRec{}, errCalendarOverflow
		}
		day = n
	}
	out := epochDaysToISODate(cal.dayFromDate(year, month, day))
	if !isoDateWithinLimits(out) {
		return out, errCalendarRange
	}
	return out, nil
}

// calendarYearMonthFromFields is the same without a day: the result is the
// first of the month, which is the reference day a PlainYearMonth carries.
func calendarYearMonthFromFields(id string, f calFieldSet, overflow string) (isoDateRec, error) {
	f.has |= fDay
	f.day = 1
	return calendarDateFromFields(id, f, overflow)
}

// monthNumberNeedsYear is true wherever the number of a month says nothing
// without a year. Outside the ISO calendar that is everywhere: a leap month
// shifts every month after it, so "month 6" is Adar in one year and Adar I in
// the next, and even where the months are fixed the spec asks for the year.
func monthNumberNeedsYear(id string) bool { return id != "iso8601" }

// lunisolarID is true of the two calendars whose months are moons.
func lunisolarID(id string) bool { return id == "chinese" || id == "dangi" }

// The years a month-day's reference date is looked for in. A month code that
// names a leap month may not occur for decades, so the search runs backwards
// from the reference year first and then forwards -- which is how M09L in the
// Chinese calendar comes to be dated 2014 when every other month is dated 1972.
const (
	monthDayRefDay   = 1972
	monthDaySearchLo = 1900
	monthDaySearchHi = 2100
)

// calendarMonthDayFromFields is a month and a day with no year. The result
// still needs an ISO date, so it takes the year in which that month and day
// last occurred at or before 1972 -- or, for a month that has not happened
// since, the year it next occurs in.
func calendarMonthDayFromFields(id string, f calFieldSet, overflow string) (isoDateRec, error) {
	cal := calendarFor(id)
	// The fields are checked for presence before any of them is checked for
	// sense: a missing field is a different kind of mistake from a wrong one.
	if !f.got(fMonth) && !f.got(fMonthCode) {
		return isoDateRec{}, errCalendarFields
	}
	if !f.got(fDay) {
		return isoDateRec{}, errCalendarFields
	}
	hasYear := f.got(fYear) || (f.got(fEra) && f.got(fEraYear))
	if f.got(fMonth) && !hasYear && monthNumberNeedsYear(id) {
		return isoDateRec{}, errCalendarFields
	}
	// With a year in hand the date is fully determined, and the month-day is
	// read back off it.
	if hasYear {
		iso, err := calendarDateFromFields(id, f, overflow)
		if err != nil {
			return iso, err
		}
		cd := cal.dateFromDay(iso.epochDays())
		return calendarMonthDayFromFields(id,
			calFieldSet{monthCode: cd.code, day: cd.day, has: fMonthCode | fDay}, overflow)
	}
	code := f.monthCode
	if !f.got(fMonthCode) {
		if f.month < 1 || f.month > cal.monthsInYear(1972) {
			if overflow == "reject" {
				return isoDateRec{}, errCalendarOverflow
			}
			f.month = clampInt(f.month, 1, cal.monthsInYear(1972))
		}
		code = simpleMonthCode(f.month)
	} else if f.got(fMonth) {
		// Both were given and there is no year to resolve the number against,
		// so the two can only be compared where the number means one thing.
		if n, _, ok := parseMonthCode(code); !ok || n != f.month {
			return isoDateRec{}, errCalendarValue
		}
	}
	if _, _, ok := parseMonthCode(code); !ok {
		return isoDateRec{}, errCalendarValue
	}
	day := f.day
	if day < 1 {
		return isoDateRec{}, errCalendarOverflow
	}
	// How long that month ever is. The lunisolar calendars answer thirty for
	// every month, because the rule there is about which years the month
	// happens in rather than how long it is.
	maxDays := 0
	if lunisolarID(id) {
		maxDays = 30
	} else {
		maxDays = maxDaysOfMonthCode(cal, code)
		if maxDays == 0 {
			return isoDateRec{}, errCalendarValue // no such month, ever
		}
	}
	if day > maxDays {
		if overflow == "reject" {
			return isoDateRec{}, errCalendarOverflow
		}
		day = maxDays
	}
	if iso, ok := searchMonthDayReference(cal, code, day); ok {
		return iso, nil
	}
	// The month code never occurs with that many days. A leap month then
	// constrains to the month it follows; anything else has nowhere to go.
	if overflow == "reject" {
		return isoDateRec{}, errCalendarOverflow
	}
	base, leap, ok := parseMonthCode(code)
	if !ok || !leap {
		return isoDateRec{}, errCalendarValue
	}
	if iso, ok := searchMonthDayReference(cal, simpleMonthCode(base), day); ok {
		return iso, nil
	}
	return isoDateRec{}, errCalendarValue
}

// searchMonthDayReference finds the year whose month names this code and is
// long enough to hold the day, looking backwards from 1972 and then forwards.
func searchMonthDayReference(cal calendar, code string, day int) (isoDateRec, bool) {
	try := func(y int) (isoDateRec, bool) {
		m, ok := cal.monthFromCode(strconv.Itoa(y), code)
		if !ok || day > cal.daysInMonth(y, m) {
			return isoDateRec{}, false
		}
		return epochDaysToISODate(cal.dayFromDate(y, m, day)), true
	}
	last := cal.dateFromDay(isoDay(monthDayRefDay, 12, 31)).year
	first := cal.dateFromDay(isoDay(monthDaySearchLo, 1, 1)).year
	end := cal.dateFromDay(isoDay(monthDaySearchHi, 12, 31)).year
	for y := last; y >= first; y-- {
		if iso, ok := try(y); ok && iso.year <= monthDayRefDay {
			return iso, true
		}
	}
	for y := last + 1; y <= end; y++ {
		if iso, ok := try(y); ok {
			return iso, true
		}
	}
	return isoDateRec{}, false
}

// maxDaysOfMonthCode is the most days a month with this code ever has.
func maxDaysOfMonthCode(cal calendar, code string) int {
	best := 0
	base := cal.dateFromDay(isoDay(monthDayRefDay, 12, 31)).year
	for y := base; y > base-40; y-- {
		if m, ok := cal.monthFromCode(strconv.Itoa(y), code); ok {
			if n := cal.daysInMonth(y, m); n > best {
				best = n
			}
		}
	}
	return best
}

// ---- reading fields out of a date ----

// calendarFieldsOf is everything a calendar can say about one date.
type calendarView struct {
	era        string
	eraYear    int
	hasEra     bool
	year       int
	month      int
	monthCode  string
	day        int
	dayOfWeek  int
	dayOfYear  int
	daysInWeek int
	daysInMonth,
	daysInYear,
	monthsInYear int
	inLeapYear bool
}

func calendarViewOf(id string, iso isoDateRec) calendarView {
	cal := calendarFor(id)
	day := iso.epochDays()
	cd := cal.dateFromDay(day)
	v := calendarView{
		era: cd.era, eraYear: cd.eraYear, hasEra: cd.era != "",
		year: cd.year, month: cd.month, monthCode: cd.code, day: cd.day,
		dayOfWeek:   floorMod(day+4, 7) + 1, // 1970-01-01 was a Thursday
		daysInWeek:  7,
		daysInMonth: cal.daysInMonth(cd.year, cd.month),
		daysInYear:  cal.daysInYear(cd.year),
		monthsInYear: cal.monthsInYear(cd.year),
		inLeapYear:  cal.inLeapYear(cd.year),
	}
	v.dayOfYear = day - cal.dayFromDate(cd.year, 1, 1) + 1
	return v
}

// isoWeekOfYear is the ISO 8601 week number and the year that week belongs to.
// Only the ISO calendar has one; every other calendar answers undefined.
func isoWeekOfYear(d isoDateRec) (week, year int) {
	day := d.epochDays()
	dow := floorMod(day+3, 7) // 0 = Monday
	// The Thursday of this week decides which year the week is in.
	thursday := day - dow + 3
	y, _, _ := isoDate(thursday)
	jan1 := isoDay(y, 1, 1)
	return (thursday-jan1)/7 + 1, y
}

// ---- arithmetic ----

// calendarDateAdd is CalendarDateAdd: years and months move through the
// calendar's own months, then weeks and days are counted off in days.
func calendarDateAdd(id string, iso isoDateRec, dur dateDuration, overflow string) (isoDateRec, error) {
	cal := calendarFor(id)
	epoch := iso.epochDays()
	if dur.years != 0 || dur.months != 0 {
		cd := cal.dateFromDay(epoch)
		y := cd.year + int(dur.years)
		ys := strconv.Itoa(y)
		m, ok := cal.monthFromCode(ys, cd.code)
		if !ok {
			// A leap month in a year that has none constrains to the month it
			// would have followed.
			base, leap, good := parseMonthCode(cd.code)
			if !good || !leap {
				return isoDateRec{}, errCalendarFields
			}
			if overflow == "reject" {
				return isoDateRec{}, errCalendarOverflow
			}
			m, ok = cal.monthFromCode(ys, simpleMonthCode(base))
			if !ok {
				return isoDateRec{}, errCalendarFields
			}
		}
		m += int(dur.months)
		for {
			n := cal.monthsInYear(y)
			if m <= n {
				break
			}
			m -= n
			y++
		}
		for m < 1 {
			y--
			m += cal.monthsInYear(y)
		}
		day := cd.day
		if n := cal.daysInMonth(y, m); day > n {
			if overflow == "reject" {
				return isoDateRec{}, errCalendarOverflow
			}
			day = n
		}
		epoch = cal.dayFromDate(y, m, day)
	}
	epoch += int(dur.weeks)*7 + int(dur.days)
	out := epochDaysToISODate(epoch)
	if !isoDateWithinLimits(out) {
		return out, errCalendarRange
	}
	return out, nil
}

// calendarDateUntil is CalendarDateUntil: the difference between two dates,
// expressed in the largest unit asked for and everything under it.
//
// The ISO calendar and the other fifteen answer this differently, and the
// difference is where the day gets constrained. In the ISO calendar, adding a
// year to the 29th of February and landing on the 28th counts as a whole year.
// Everywhere else the day is compared before it is constrained, so the same
// step counts as one short of a year -- which is why the last day of a Coptic
// leap year is twelve months and twenty-nine days from the last day of the
// next.
func calendarDateUntil(id string, one, two isoDateRec, largestUnit int) dateDuration {
	sign := -compareISODate(one, two)
	if sign == 0 {
		return dateDuration{}
	}
	if largestUnit == unitDay || largestUnit == unitWeek {
		days := int64(two.epochDays() - one.epochDays())
		if largestUnit == unitWeek {
			return dateDuration{weeks: days / 7, days: days % 7}
		}
		return dateDuration{days: days}
	}
	cal := calendarFor(id)
	c1 := cal.dateFromDay(one.epochDays())
	c2 := cal.dateFromDay(two.epochDays())

	// candidate is where the start lands after so many years and months, with
	// the day left as it was however long the month turns out to be.
	candidate := func(years, months int64) (int, int, int) {
		y := c1.year + int(years)
		m, ok := cal.monthFromCode(strconv.Itoa(y), c1.code)
		if !ok {
			base, leap, good := parseMonthCode(c1.code)
			if !good {
				return y, c1.month, c1.day
			}
			if leap {
				m, ok = cal.monthFromCode(strconv.Itoa(y), simpleMonthCode(base))
			}
			if !ok {
				m = c1.month
			}
		}
		m += int(months)
		for {
			n := cal.monthsInYear(y)
			if m <= n {
				break
			}
			m -= n
			y++
		}
		for m < 1 {
			y--
			m += cal.monthsInYear(y)
		}
		return y, m, c1.day
	}
	// past reports whether a candidate has gone beyond the far end.
	past := func(years, months int64) bool {
		y, m, d := candidate(years, months)
		c := 0
		switch {
		case y != c2.year:
			c = sign64(int64(y - c2.year))
		case m != c2.month:
			c = sign64(int64(m - c2.month))
		default:
			c = sign64(int64(d - c2.day))
		}
		return sign*c > 0
	}
	isoLike := id == "iso8601"
	pastISO := func(years, months int64) bool {
		mid, err := calendarDateAdd(id, one, dateDuration{years: years, months: months}, "constrain")
		return err != nil || sign*compareISODate(mid, two) > 0
	}
	overshot := past
	if isoLike {
		overshot = pastISO
	}

	years := int64(c2.year - c1.year)
	for years != 0 && overshot(years, 0) {
		years -= int64(sign)
	}
	var months int64
	for !overshot(years, months+int64(sign)) {
		months += int64(sign)
		if months > 10000 || months < -10000 {
			break
		}
	}
	mid, err := calendarDateAdd(id, one, dateDuration{years: years, months: months}, "constrain")
	if err != nil {
		return dateDuration{}
	}
	days := int64(two.epochDays() - mid.epochDays())
	if largestUnit == unitMonth && years != 0 {
		// Count the months the years covered, which is not twelve everywhere.
		step := int(sign)
		for y := c1.year; y != c1.year+int(years); y += step {
			if step > 0 {
				months += int64(cal.monthsInYear(y))
			} else {
				months -= int64(cal.monthsInYear(y - 1))
			}
		}
		years = 0
	}
	return dateDuration{years: years, months: months, days: days}
}
