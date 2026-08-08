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

// calendarMonthDayFromFields is a month and a day with no year. The result
// still needs an ISO date, so it takes the latest year at or before 1972 in
// which that month and day both exist.
func calendarMonthDayFromFields(id string, f calFieldSet, overflow string) (isoDateRec, error) {
	cal := calendarFor(id)
	if !f.got(fDay) {
		return isoDateRec{}, errCalendarFields
	}
	// With a year in hand there is nothing to search for.
	if f.got(fYear) || (f.got(fEra) && f.got(fEraYear)) {
		iso, err := calendarDateFromFields(id, f, overflow)
		if err != nil {
			return iso, err
		}
		cd := cal.dateFromDay(iso.epochDays())
		f = calFieldSet{monthCode: cd.code, day: cd.day, has: fMonthCode | fDay}
		return calendarMonthDayFromFields(id, f, overflow)
	}
	// A numeric month with no year does not name a month in a calendar whose
	// months move; only a month code does.
	code := f.monthCode
	if !f.got(fMonthCode) {
		if id != "iso8601" && id != "gregory" && id != "japanese" && id != "roc" &&
			id != "buddhist" {
			return isoDateRec{}, errCalendarFields
		}
		if f.month < 1 || f.month > 12 {
			if overflow == "reject" {
				return isoDateRec{}, errCalendarOverflow
			}
			f.month = clampInt(f.month, 1, 12)
		}
		code = simpleMonthCode(f.month)
	}
	// 1972-12-31 is where the search starts, the last day of the reference year.
	start := cal.dateFromDay(isoDay(1972, 12, 31))
	for y := start.year; y > start.year-200; y-- {
		m, ok := cal.monthFromCode(strconv.Itoa(y), code)
		if !ok {
			continue
		}
		day := f.day
		if n := cal.daysInMonth(y, m); day > n {
			// The last year in which the day itself fits is the one wanted, so
			// keep looking rather than clamping here.
			if y == start.year && overflow == "reject" && day > maxDayOfMonthCode(cal, code) {
				return isoDateRec{}, errCalendarOverflow
			}
			continue
		}
		if day < 1 {
			return isoDateRec{}, errCalendarOverflow
		}
		d := cal.dayFromDate(y, m, day)
		if d > isoDay(1972, 12, 31) {
			continue
		}
		return epochDaysToISODate(d), nil
	}
	if overflow == "reject" {
		return isoDateRec{}, errCalendarOverflow
	}
	// Nothing fits: constrain the day to the longest that month ever is.
	for y := start.year; y > start.year-200; y-- {
		m, ok := cal.monthFromCode(strconv.Itoa(y), code)
		if !ok {
			continue
		}
		d := cal.dayFromDate(y, m, cal.daysInMonth(y, m))
		if d <= isoDay(1972, 12, 31) {
			return epochDaysToISODate(d), nil
		}
	}
	return isoDateRec{}, errCalendarFields
}

// maxDayOfMonthCode is the most days a month with this code ever has, over the
// years near the reference year.
func maxDayOfMonthCode(cal calendar, code string) int {
	best := 0
	base := cal.dateFromDay(isoDay(1972, 12, 31)).year
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

	years := int64(c2.year - c1.year)
	// Overshooting by a year is normal when the months are the other way round.
	for years != 0 {
		mid, err := calendarDateAdd(id, one, dateDuration{years: years}, "constrain")
		if err != nil || sign*compareISODate(mid, two) > 0 {
			years -= int64(sign)
			continue
		}
		break
	}
	// Then months, one at a time: after the year step there are never many.
	var months int64
	for {
		mid, err := calendarDateAdd(id, one,
			dateDuration{years: years, months: months + int64(sign)}, "constrain")
		if err != nil || sign*compareISODate(mid, two) > 0 {
			break
		}
		months += int64(sign)
		if months > 1000 || months < -1000 {
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
		step := int64(sign)
		for y := c1.year; y != c1.year+int(years); y += int(step) {
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
