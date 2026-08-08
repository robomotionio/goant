package engine

// The calendars.
//
// Sixteen of them, fixed by ECMA-402 and by Temporal, and all sixteen are one
// question asked twice: which year, month and day is this DAY, and which day is
// that year, month and day. Everything else -- the length of a month, whether a
// year is a leap year, what era it falls in -- follows from those two.
//
// The day is the unit throughout: an integer count of days since 1970-01-01 in
// the proleptic Gregorian calendar, which is what a JavaScript Date's Day(t)
// already is. Nothing here is expressed as a timestamp, because a calendar
// knows nothing about time of day and dividing by 86,400,000 in the middle of a
// conversion is how a date lands on the wrong side of midnight.
//
// The arithmetic is the standard one from Calendrical Calculations: each
// calendar names a day-number epoch and a rule for counting years from it. The
// two lunisolar ones -- chinese and dangi -- are not arithmetic at all, and are
// in calendar_lunisolar.go.

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// calendarDate is a date as a calendar sees it. The month is the ordinal
// position in the year, counting leap months where a calendar has them, and
// monthCode is the name that survives a leap month: in a Chinese year with a
// leap fourth month, month 5 is "M04L" and month 6 is "M05".
type calendarDate struct {
	era     string // "" for a calendar with no eras
	eraYear int
	year    int // the arithmetic year, which eras count from differently
	month   int // 1-based ordinal
	day     int
	code    string // monthCode
}

// calendar is what every one of the sixteen provides.
type calendar interface {
	// dateFromDay and dayFromDate are the two directions of the one question.
	dateFromDay(day int) calendarDate
	dayFromDate(year, month, day int) int
	monthsInYear(year int) int
	daysInMonth(year, month int) int
	daysInYear(year int) int
	inLeapYear(year int) bool
	// monthCode names a month, and monthFromCode reverses it. They differ only
	// for the lunisolar calendars, where a leap month shares its neighbour's
	// name with an L on the end.
	monthCode(year, month int) string
	monthFromCode(year, code string) (int, bool)
	// eraOf splits an arithmetic year into an era and a year within it.
	eraOf(year int) (string, int)
	yearFromEra(era string, eraYear int) (int, bool)
	// eras lists them, most recent first, for the fields a caller may pass.
	eras() []string
}

// calendarFor returns the calendar with a given identifier.
func calendarFor(id string) calendar {
	switch id {
	case "iso8601":
		return gregorianCalendar{iso: true}
	case "gregory":
		return gregorianCalendar{}
	case "buddhist":
		return offsetCalendar{shift: 543, era: "be"}
	case "roc":
		return offsetCalendar{shift: -1911, era: "roc", before: "broc"}
	case "japanese":
		return japaneseCalendar{}
	case "coptic":
		return copticCalendar{epoch: copticEpoch, era: "am"}
	case "ethiopic":
		return ethiopicCalendar{}
	case "ethioaa":
		return copticCalendar{epoch: ethiopicEpoch, era: "aa", shift: 5500}
	case "indian":
		return indianCalendar{}
	case "persian":
		return persianCalendar{}
	case "islamic-civil":
		return islamicCalendar{epoch: islamicCivilEpoch, id: "islamic-civil"}
	case "islamic-tbla":
		return islamicCalendar{epoch: islamicCivilEpoch - 1, id: "islamic-tbla"}
	case "islamic-umalqura":
		return umalquraCalendar{}
	case "hebrew":
		return hebrewCalendar{}
	case "chinese":
		return lunisolarCalendar{zone: "Asia/Shanghai", id: "chinese"}
	case "dangi":
		return lunisolarCalendar{zone: "Asia/Seoul", id: "dangi"}
	}
	return gregorianCalendar{}
}

// ---- day numbers ----

// isoDay is the day number of an ISO year-month-day. Months and days outside
// their usual ranges roll over, which is what makes month arithmetic writable
// as "add to the month and convert".
func isoDay(year, month, day int) int {
	// Howard Hinnant's days_from_civil, which is exact for the whole range and
	// has no loops in it.
	y := year
	if month <= 2 {
		y--
	}
	era := y / 400
	if y < 0 {
		era = (y - 399) / 400
	}
	yoe := y - era*400
	mp := (month + 9) % 12
	doy := (153*mp+2)/5 + day - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// isoDate is days_from_civil run backwards.
func isoDate(day int) (year, month, dayOfMonth int) {
	z := day + 719468
	era := z / 146097
	if z < 0 {
		era = (z - 146096) / 146097
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

func isoLeapYear(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

var isoMonthLengths = [12]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

func isoDaysInMonth(y, m int) int {
	if m == 2 && isoLeapYear(y) {
		return 29
	}
	if m < 1 || m > 12 {
		return 30
	}
	return isoMonthLengths[m-1]
}

// floorDiv and floorMod divide the way a calendar needs: towards minus
// infinity, so that year -1 is the year before year 0 rather than after it.
func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func floorMod(a, b int) int { return a - floorDiv(a, b)*b }

// ---- monthCode ----

// simpleMonthCode is "M01" through "M12" (or M13), which is every calendar that
// does not insert months.
func simpleMonthCode(month int) string {
	return fmt.Sprintf("M%02d", month)
}

// parseMonthCode reads "M05" or "M05L" into a month number and whether it was
// marked as a leap month.
func parseMonthCode(code string) (month int, leap bool, ok bool) {
	if len(code) < 3 || code[0] != 'M' {
		return 0, false, false
	}
	body := code[1:]
	if strings.HasSuffix(body, "L") {
		leap, body = true, body[:len(body)-1]
	}
	if len(body) != 2 || body[0] < '0' || body[0] > '9' || body[1] < '0' || body[1] > '9' {
		return 0, false, false
	}
	n, err := strconv.Atoi(body)
	if err != nil || n < 1 || n > 13 {
		return 0, false, false
	}
	return n, leap, true
}

// ---- gregory and iso8601 ----

type gregorianCalendar struct{ iso bool }

func (c gregorianCalendar) dateFromDay(day int) calendarDate {
	y, m, d := isoDate(day)
	out := calendarDate{year: y, month: m, day: d, code: simpleMonthCode(m)}
	if !c.iso {
		out.era, out.eraYear = c.eraOf(y)
	}
	return out
}

func (c gregorianCalendar) dayFromDate(y, m, d int) int { return isoDay(y, m, d) }
func (c gregorianCalendar) monthsInYear(int) int        { return 12 }
func (c gregorianCalendar) daysInMonth(y, m int) int    { return isoDaysInMonth(y, m) }
func (c gregorianCalendar) inLeapYear(y int) bool       { return isoLeapYear(y) }
func (c gregorianCalendar) monthCode(_, m int) string   { return simpleMonthCode(m) }
func (c gregorianCalendar) daysInYear(y int) int {
	if isoLeapYear(y) {
		return 366
	}
	return 365
}

func (c gregorianCalendar) monthFromCode(_ string, code string) (int, bool) {
	m, leap, ok := parseMonthCode(code)
	if !ok || leap || m > 12 {
		return 0, false
	}
	return m, true
}

func (c gregorianCalendar) eraOf(y int) (string, int) {
	if c.iso {
		return "", y
	}
	// The proleptic Gregorian calendar has no year zero: ISO year 0 is 1 BC.
	if y <= 0 {
		return "bce", 1 - y
	}
	return "ce", y
}

func (c gregorianCalendar) yearFromEra(era string, eraYear int) (int, bool) {
	switch era {
	case "ce", "ad":
		return eraYear, true
	case "bce", "bc":
		return 1 - eraYear, true
	}
	return 0, false
}

func (c gregorianCalendar) eras() []string {
	if c.iso {
		return nil
	}
	return []string{"bce", "ce"}
}

// ---- buddhist and roc: Gregorian months, a different year number ----

type offsetCalendar struct {
	shift  int    // added to the Gregorian year
	era    string // the era for a positive year
	before string // the era for year <= 0, "" where the calendar has only one
}

func (c offsetCalendar) gregorian(y int) int { return y - c.shift }

func (c offsetCalendar) dateFromDay(day int) calendarDate {
	y, m, d := isoDate(day)
	y += c.shift
	era, ey := c.eraOf(y)
	return calendarDate{era: era, eraYear: ey, year: y, month: m, day: d,
		code: simpleMonthCode(m)}
}

func (c offsetCalendar) dayFromDate(y, m, d int) int { return isoDay(c.gregorian(y), m, d) }
func (c offsetCalendar) monthsInYear(int) int        { return 12 }
func (c offsetCalendar) daysInMonth(y, m int) int    { return isoDaysInMonth(c.gregorian(y), m) }
func (c offsetCalendar) inLeapYear(y int) bool       { return isoLeapYear(c.gregorian(y)) }
func (c offsetCalendar) monthCode(_, m int) string   { return simpleMonthCode(m) }

func (c offsetCalendar) daysInYear(y int) int {
	if isoLeapYear(c.gregorian(y)) {
		return 366
	}
	return 365
}

func (c offsetCalendar) monthFromCode(_ string, code string) (int, bool) {
	m, leap, ok := parseMonthCode(code)
	if !ok || leap || m > 12 {
		return 0, false
	}
	return m, true
}

func (c offsetCalendar) eraOf(y int) (string, int) {
	if c.before != "" && y <= 0 {
		return c.before, 1 - y
	}
	return c.era, y
}

func (c offsetCalendar) yearFromEra(era string, eraYear int) (int, bool) {
	switch era {
	case c.era:
		return eraYear, true
	case c.before:
		return 1 - eraYear, true
	}
	return 0, false
}

func (c offsetCalendar) eras() []string {
	if c.before == "" {
		return []string{c.era}
	}
	return []string{c.era, c.before}
}

// ---- japanese: Gregorian, with an era per emperor ----

// japaneseEras are the modern eras and the day each began. Before Meiji the
// calendar reports the Gregorian year in the "ce"/"bce" eras, which is what
// ICU does: the pre-Meiji Japanese eras are a different calendar (lunisolar)
// and naming them here would claim a conversion this does not do.
var japaneseEras = []struct {
	name  string
	start int // day number of the first day of the era
	year  int // the Gregorian year that is year 1 of the era
}{
	{"reiwa", 0, 2019},  // 2019-05-01
	{"heisei", 0, 1989}, // 1989-01-08
	{"showa", 0, 1926},  // 1926-12-25
	{"taisho", 0, 1912}, // 1912-07-30
	{"meiji", 0, 1868},  // 1868-09-08
}

func init() {
	starts := [][3]int{{2019, 5, 1}, {1989, 1, 8}, {1926, 12, 25}, {1912, 7, 30}, {1868, 9, 8}}
	for i := range japaneseEras {
		japaneseEras[i].start = isoDay(starts[i][0], starts[i][1], starts[i][2])
	}
}

type japaneseCalendar struct{}

func (c japaneseCalendar) dateFromDay(day int) calendarDate {
	y, m, d := isoDate(day)
	era, ey := "ce", y
	if y <= 0 {
		era, ey = "bce", 1-y
	}
	for _, e := range japaneseEras {
		if day >= e.start {
			era, ey = e.name, y-e.year+1
			break
		}
	}
	return calendarDate{era: era, eraYear: ey, year: y, month: m, day: d,
		code: simpleMonthCode(m)}
}

func (c japaneseCalendar) dayFromDate(y, m, d int) int { return isoDay(y, m, d) }
func (c japaneseCalendar) monthsInYear(int) int        { return 12 }
func (c japaneseCalendar) daysInMonth(y, m int) int    { return isoDaysInMonth(y, m) }
func (c japaneseCalendar) inLeapYear(y int) bool       { return isoLeapYear(y) }
func (c japaneseCalendar) monthCode(_, m int) string   { return simpleMonthCode(m) }

func (c japaneseCalendar) daysInYear(y int) int {
	if isoLeapYear(y) {
		return 366
	}
	return 365
}

func (c japaneseCalendar) monthFromCode(_ string, code string) (int, bool) {
	m, leap, ok := parseMonthCode(code)
	if !ok || leap || m > 12 {
		return 0, false
	}
	return m, true
}

func (c japaneseCalendar) eraOf(y int) (string, int) {
	// Without a day there is no telling which era a year is in when it spans
	// two, so the Gregorian era is the answer: a year is only ambiguous
	// against a day, and every caller that has one uses dateFromDay.
	if y <= 0 {
		return "bce", 1 - y
	}
	return "ce", y
}

func (c japaneseCalendar) yearFromEra(era string, eraYear int) (int, bool) {
	switch era {
	case "ce", "ad":
		return eraYear, true
	case "bce", "bc":
		return 1 - eraYear, true
	}
	for _, e := range japaneseEras {
		if e.name == era {
			return e.year + eraYear - 1, true
		}
	}
	return 0, false
}

func (c japaneseCalendar) eras() []string {
	out := make([]string, 0, len(japaneseEras)+2)
	for _, e := range japaneseEras {
		out = append(out, e.name)
	}
	return append(out, "ce", "bce")
}

// ---- coptic, ethiopic and ethioaa: twelve months of thirty and a short one ----

const (
	// The first days of Coptic year 1 and Ethiopic year 1, as day numbers.
	// They are 29 August 284 and 29 August 8 in the Julian calendar; the
	// numbers here are anchored on a conversion anyone can check, that
	// 2024-09-11 is 1 Tout 1741 and 1 Meskerem 2017.
	copticEpoch   = -615558
	ethiopicEpoch = -716367
)

type copticCalendar struct {
	epoch int
	era   string
	shift int // added to the year, for ethioaa's Amete Alem count
}

func (c copticCalendar) dateFromDay(day int) calendarDate {
	n := day - c.epoch
	year := floorDiv(4*n+1463, 1461)
	dayOfYear := n - (365*(year-1) + floorDiv(year, 4))
	month := dayOfYear/30 + 1
	d := dayOfYear%30 + 1
	year += c.shift
	era, ey := c.eraOf(year)
	return calendarDate{era: era, eraYear: ey, year: year, month: month, day: d,
		code: simpleMonthCode(month)}
}

func (c copticCalendar) dayFromDate(y, m, d int) int {
	// The sixth epagomenal day falls in the year BEFORE a Julian leap year,
	// which is why the quarter is counted from y rather than from y-1.
	y -= c.shift
	return c.epoch + 365*(y-1) + floorDiv(y, 4) + 30*(m-1) + d - 1
}

func (c copticCalendar) monthsInYear(int) int { return 13 }

func (c copticCalendar) daysInMonth(y, m int) int {
	if m < 13 {
		return 30
	}
	if c.inLeapYear(y) {
		return 6
	}
	return 5
}

func (c copticCalendar) daysInYear(y int) int {
	if c.inLeapYear(y) {
		return 366
	}
	return 365
}

func (c copticCalendar) inLeapYear(y int) bool { return floorMod(y-c.shift, 4) == 3 }

func (c copticCalendar) monthCode(_, m int) string { return simpleMonthCode(m) }

func (c copticCalendar) monthFromCode(_ string, code string) (int, bool) {
	m, leap, ok := parseMonthCode(code)
	if !ok || leap || m > 13 {
		return 0, false
	}
	return m, true
}

// The Coptic era runs backwards through zero rather than turning into a second
// era: a year before the Era of Martyrs is a negative year in it, which is what
// every calendar with only one era does.
func (c copticCalendar) eraOf(y int) (string, int) { return c.era, y }

func (c copticCalendar) yearFromEra(era string, eraYear int) (int, bool) {
	if era == c.era {
		return eraYear, true
	}
	return 0, false
}

func (c copticCalendar) eras() []string { return []string{c.era} }

// ethiopicCalendar is the Coptic arithmetic with the Ethiopic epoch, and with
// the Amete Alem years before year 1 of the Incarnation counted in their own
// era rather than as negative years.
type ethiopicCalendar struct{}

func (c ethiopicCalendar) base() copticCalendar {
	return copticCalendar{epoch: ethiopicEpoch, era: "am"}
}

func (c ethiopicCalendar) dateFromDay(day int) calendarDate {
	d := c.base().dateFromDay(day)
	if d.year <= 0 {
		d.era, d.eraYear = "aa", d.year+5500
	} else {
		d.era, d.eraYear = "am", d.year
	}
	return d
}

func (c ethiopicCalendar) dayFromDate(y, m, d int) int { return c.base().dayFromDate(y, m, d) }
func (c ethiopicCalendar) monthsInYear(y int) int      { return 13 }
func (c ethiopicCalendar) daysInMonth(y, m int) int    { return c.base().daysInMonth(y, m) }
func (c ethiopicCalendar) daysInYear(y int) int        { return c.base().daysInYear(y) }
func (c ethiopicCalendar) inLeapYear(y int) bool       { return c.base().inLeapYear(y) }
func (c ethiopicCalendar) monthCode(_, m int) string   { return simpleMonthCode(m) }

func (c ethiopicCalendar) monthFromCode(y string, code string) (int, bool) {
	return c.base().monthFromCode(y, code)
}

func (c ethiopicCalendar) eraOf(y int) (string, int) {
	if y <= 0 {
		return "aa", y + 5500
	}
	return "am", y
}

func (c ethiopicCalendar) yearFromEra(era string, eraYear int) (int, bool) {
	switch era {
	case "am":
		return eraYear, true
	case "aa":
		return eraYear - 5500, true
	}
	return 0, false
}

func (c ethiopicCalendar) eras() []string { return []string{"aa", "am"} }

// ---- indian: the Saka era, on a Gregorian leap rule ----

type indianCalendar struct{}

// indianMonthLengths after the first, which is 30 or 31 with the leap year.
var indianMonthLengths = [12]int{30, 31, 31, 31, 31, 31, 30, 30, 30, 30, 30, 30}

func (c indianCalendar) daysInMonth(y, m int) int {
	if m == 1 {
		if c.inLeapYear(y) {
			return 31
		}
		return 30
	}
	if m < 1 || m > 12 {
		return 30
	}
	return indianMonthLengths[m-1]
}

func (c indianCalendar) inLeapYear(y int) bool { return isoLeapYear(y + 78) }

func (c indianCalendar) daysInYear(y int) int {
	if c.inLeapYear(y) {
		return 366
	}
	return 365
}

func (c indianCalendar) dayFromDate(y, m, d int) int {
	// Year 1 of the Saka era began on 79-03-22 Gregorian; a leap year starts a
	// day earlier because Chaitra gains its day at the front.
	start := isoDay(y+78, 3, 22)
	if c.inLeapYear(y) {
		start--
	}
	n := 0
	for i := 1; i < m; i++ {
		n += c.daysInMonth(y, i)
	}
	return start + n + d - 1
}

func (c indianCalendar) dateFromDay(day int) calendarDate {
	gy, _, _ := isoDate(day)
	y := gy - 78
	if day < c.dayFromDate(y, 1, 1) {
		y--
	}
	rest := day - c.dayFromDate(y, 1, 1)
	m := 1
	for rest >= c.daysInMonth(y, m) {
		rest -= c.daysInMonth(y, m)
		m++
	}
	return calendarDate{era: "shaka", eraYear: y, year: y, month: m, day: rest + 1,
		code: simpleMonthCode(m)}
}

func (c indianCalendar) monthsInYear(int) int      { return 12 }
func (c indianCalendar) monthCode(_, m int) string { return simpleMonthCode(m) }
func (c indianCalendar) eraOf(y int) (string, int) { return "shaka", y }
func (c indianCalendar) eras() []string            { return []string{"shaka"} }

func (c indianCalendar) monthFromCode(_ string, code string) (int, bool) {
	m, leap, ok := parseMonthCode(code)
	if !ok || leap || m > 12 {
		return 0, false
	}
	return m, true
}

func (c indianCalendar) yearFromEra(era string, eraYear int) (int, bool) {
	if era == "shaka" {
		return eraYear, true
	}
	return 0, false
}

// ---- persian: the 33-year cycle ----

type persianCalendar struct{}

// persianEpoch is the first day of year 1, 19 March 622 in the Julian
// calendar, anchored on Nowruz 1403 falling on 2024-03-20.
const persianEpoch = -492267

// cycleYear is the year's place in the 2820-year cycle the arithmetic Persian
// calendar repeats on. There is no year zero, so a year before the epoch
// counts from one further back.
func persianCycleYear(y int) (y0, year1 int) {
	y0 = y - 474
	if y <= 0 {
		y0 = y - 473
	}
	return y0, floorMod(y0, 2820) + 474
}

// inLeapYear uses the arithmetic 2820-year cycle, which is the rule ICU and
// Temporal use: it agrees with the astronomical calendar for every year in the
// range a script can reach.
func (c persianCalendar) inLeapYear(y int) bool {
	_, year1 := persianCycleYear(y)
	return floorMod((year1+38)*31, 128) < 31
}

func (c persianCalendar) daysInMonth(y, m int) int {
	switch {
	case m <= 6:
		return 31
	case m <= 11:
		return 30
	case m == 12:
		if c.inLeapYear(y) {
			return 30
		}
		return 29
	}
	return 30
}

func (c persianCalendar) daysInYear(y int) int {
	if c.inLeapYear(y) {
		return 366
	}
	return 365
}

func (c persianCalendar) dayFromDate(y, m, d int) int {
	y0, year1 := persianCycleYear(y)
	days := persianEpoch + 1029983*floorDiv(y0, 2820) + 365*(year1-1) +
		floorDiv(31*year1-5, 128)
	for i := 1; i < m; i++ {
		days += c.daysInMonth(y, i)
	}
	return days + d - 1
}

func (c persianCalendar) dateFromDay(day int) calendarDate {
	// A first guess from the average year length, then walked into place. The
	// walk is at most a step or two.
	y := 474 + floorDiv(day-persianEpoch, 366)
	for c.dayFromDate(y+1, 1, 1) <= day {
		y++
	}
	for c.dayFromDate(y, 1, 1) > day {
		y--
	}
	rest := day - c.dayFromDate(y, 1, 1)
	m := 1
	for rest >= c.daysInMonth(y, m) {
		rest -= c.daysInMonth(y, m)
		m++
	}
	return calendarDate{era: "ap", eraYear: y, year: y, month: m, day: rest + 1,
		code: simpleMonthCode(m)}
}

func (c persianCalendar) monthsInYear(int) int      { return 12 }
func (c persianCalendar) monthCode(_, m int) string { return simpleMonthCode(m) }
func (c persianCalendar) eraOf(y int) (string, int) { return "ap", y }
func (c persianCalendar) eras() []string            { return []string{"ap"} }

func (c persianCalendar) monthFromCode(_ string, code string) (int, bool) {
	m, leap, ok := parseMonthCode(code)
	if !ok || leap || m > 12 {
		return 0, false
	}
	return m, true
}

func (c persianCalendar) yearFromEra(era string, eraYear int) (int, bool) {
	if era == "ap" {
		return eraYear, true
	}
	return 0, false
}

// ---- the tabular Islamic calendars ----

// islamicCivilEpoch is the first day of year 1, 16 July 622 in the Julian
// calendar, anchored on 1 Muharram 1446 falling on 2024-07-07. The
// astronomical variant (tbla) starts a day earlier.
const islamicCivilEpoch = -492149

type islamicCalendar struct {
	epoch int
	id    string
}

// A thirty-year cycle with eleven leap years in it, in the places the tabular
// calendar puts them.
func (c islamicCalendar) inLeapYear(y int) bool {
	switch floorMod(y, 30) {
	case 2, 5, 7, 10, 13, 16, 18, 21, 24, 26, 29:
		return true
	}
	return false
}

func (c islamicCalendar) daysInMonth(y, m int) int {
	if m%2 == 1 {
		return 30
	}
	if m == 12 && c.inLeapYear(y) {
		return 30
	}
	return 29
}

func (c islamicCalendar) daysInYear(y int) int {
	if c.inLeapYear(y) {
		return 355
	}
	return 354
}

func (c islamicCalendar) dayFromDate(y, m, d int) int {
	return c.epoch + (y-1)*354 + floorDiv(3+11*y, 30) + (m-1)*29 + floorDiv(m, 2) + d - 1
}

func (c islamicCalendar) dateFromDay(day int) calendarDate {
	y := floorDiv(30*(day-c.epoch)+10646, 10631)
	for c.dayFromDate(y+1, 1, 1) <= day {
		y++
	}
	for c.dayFromDate(y, 1, 1) > day {
		y--
	}
	rest := day - c.dayFromDate(y, 1, 1)
	m := 1
	for m < 12 && rest >= c.daysInMonth(y, m) {
		rest -= c.daysInMonth(y, m)
		m++
	}
	era, ey := c.eraOf(y)
	return calendarDate{era: era, eraYear: ey, year: y, month: m, day: rest + 1,
		code: simpleMonthCode(m)}
}

func (c islamicCalendar) monthsInYear(int) int      { return 12 }
func (c islamicCalendar) monthCode(_, m int) string { return simpleMonthCode(m) }
func (c islamicCalendar) eraOf(y int) (string, int) {
	// There is no year zero: the year before 1 AH is 1 BH, on the other side
	// of the Hijra.
	if y <= 0 {
		return "bh", 1 - y
	}
	return "ah", y
}

func (c islamicCalendar) eras() []string { return []string{"bh", "ah"} }

func (c islamicCalendar) monthFromCode(_ string, code string) (int, bool) {
	m, leap, ok := parseMonthCode(code)
	if !ok || leap || m > 12 {
		return 0, false
	}
	return m, true
}

func (c islamicCalendar) yearFromEra(era string, eraYear int) (int, bool) {
	switch era {
	case "ah":
		return eraYear, true
	case "bh":
		return 1 - eraYear, true
	}
	return 0, false
}

// ---- the Umm al-Qura calendar ----

// umalquraMonthLengths is a bit per month of each year from 1300 AH: 1 for a
// thirty-day month, 0 for twenty-nine. The Umm al-Qura calendar is a published
// table rather than a rule, because it follows the sighting of the moon in
// Mecca; outside the tabulated range it falls back to the civil arithmetic,
// which is what ICU does.
type umalquraCalendar struct{}

func (c umalquraCalendar) civil() islamicCalendar {
	return islamicCalendar{epoch: islamicCivilEpoch, id: "islamic-civil"}
}

func (c umalquraCalendar) inRange(y int) bool {
	return y >= umalquraFirstYear && y < umalquraFirstYear+len(umalquraMonthLengths)
}

func (c umalquraCalendar) daysInMonth(y, m int) int {
	if !c.inRange(y) {
		return c.civil().daysInMonth(y, m)
	}
	if m < 1 || m > 12 {
		return 29
	}
	// The high bit of the twelve is the first month.
	if umalquraMonthLengths[y-umalquraFirstYear]&(1<<(12-m)) != 0 {
		return 30
	}
	return 29
}

func (c umalquraCalendar) daysInYear(y int) int {
	if !c.inRange(y) {
		return c.civil().daysInYear(y)
	}
	n := 0
	for m := 1; m <= 12; m++ {
		n += c.daysInMonth(y, m)
	}
	return n
}

func (c umalquraCalendar) inLeapYear(y int) bool { return c.daysInYear(y) > 354 }

func (c umalquraCalendar) dayFromDate(y, m, d int) int {
	if !c.inRange(y) {
		return c.civil().dayFromDate(y, m, d)
	}
	day := umalquraYearStarts()[y-umalquraFirstYear]
	for mm := 1; mm < m; mm++ {
		day += c.daysInMonth(y, mm)
	}
	return day + d - 1
}

func (c umalquraCalendar) dateFromDay(day int) calendarDate {
	starts := umalquraYearStarts()
	if day < starts[0] || day >= starts[len(starts)-1]+c.daysInYear(umalquraFirstYear+len(starts)-1) {
		return c.civil().dateFromDay(day)
	}
	y := umalquraFirstYear
	for i, s := range starts {
		if s > day {
			break
		}
		y = umalquraFirstYear + i
	}
	rest := day - starts[y-umalquraFirstYear]
	m := 1
	for m < 12 && rest >= c.daysInMonth(y, m) {
		rest -= c.daysInMonth(y, m)
		m++
	}
	era, ey := c.eraOf(y)
	return calendarDate{era: era, eraYear: ey, year: y, month: m, day: rest + 1,
		code: simpleMonthCode(m)}
}

func (c umalquraCalendar) monthsInYear(int) int      { return 12 }
func (c umalquraCalendar) monthCode(_, m int) string { return simpleMonthCode(m) }
func (c umalquraCalendar) eras() []string            { return c.civil().eras() }

func (c umalquraCalendar) eraOf(y int) (string, int) { return c.civil().eraOf(y) }

func (c umalquraCalendar) monthFromCode(y string, code string) (int, bool) {
	return c.civil().monthFromCode(y, code)
}

func (c umalquraCalendar) yearFromEra(era string, eraYear int) (int, bool) {
	return c.civil().yearFromEra(era, eraYear)
}

// ---- hebrew ----

type hebrewCalendar struct{}

// hebrewElapsedDays is the day the Hebrew year begins, counted from the epoch,
// with the four postponements applied: the year may not begin on a Sunday,
// Wednesday or Friday, and two rules stop a year being too long or too short.
func hebrewElapsedDays(y int) int {
	monthsElapsed := 235*floorDiv(y-1, 19) + 12*floorMod(y-1, 19) +
		floorDiv(7*floorMod(y-1, 19)+1, 19)
	partsElapsed := 204 + 793*floorMod(monthsElapsed, 1080)
	hoursElapsed := 11 + 12*monthsElapsed + 793*floorDiv(monthsElapsed, 1080) +
		floorDiv(partsElapsed, 1080)
	day := 29*monthsElapsed + floorDiv(hoursElapsed, 24)
	parts := 1080*floorMod(hoursElapsed, 24) + floorMod(partsElapsed, 1080)
	switch {
	case parts >= 19440,
		floorMod(day, 7) == 2 && parts >= 9924 && !hebrewLeapYear(y),
		floorMod(day, 7) == 1 && parts >= 16789 && hebrewLeapYear(y-1):
		day++
	}
	if d := floorMod(day, 7); d == 0 || d == 3 || d == 5 {
		day++
	}
	return day
}

func hebrewLeapYear(y int) bool { return floorMod(7*y+1, 19) < 7 }

// hebrewEpoch is the day number of 1 Tishri, year 1 (7 October 3761 BC
// Julian), anchored on Rosh Hashanah 5785 falling on 2024-10-03.
var hebrewEpoch = isoDay(-3760, 9, 7) - 1

func (c hebrewCalendar) newYear(y int) int { return hebrewEpoch + hebrewElapsedDays(y) }

func (c hebrewCalendar) daysInYear(y int) int { return c.newYear(y+1) - c.newYear(y) }

func (c hebrewCalendar) monthsInYear(y int) int {
	if hebrewLeapYear(y) {
		return 13
	}
	return 12
}

func (c hebrewCalendar) inLeapYear(y int) bool { return hebrewLeapYear(y) }

// The months in Temporal's ordinal order, which starts at Tishri. Adar I is
// inserted as month 6 in a leap year, so Adar is month 6 in a common year and
// month 7 in a leap year.
func (c hebrewCalendar) daysInMonth(y, m int) int {
	length := c.daysInYear(y)
	leap := hebrewLeapYear(y)
	switch m {
	case 1: // Tishri
		return 30
	case 2: // Heshvan: long in a complete year
		if length%10 == 5 {
			return 30
		}
		return 29
	case 3: // Kislev: short in a deficient year
		if length%10 == 3 {
			return 29
		}
		return 30
	case 4: // Tevet
		return 29
	case 5: // Shevat
		return 30
	case 6: // Adar, or Adar I in a leap year
		if leap {
			return 30
		}
		return 29
	}
	if leap {
		m-- // past Adar I, the months line up with a common year again
	}
	switch m {
	case 6: // Adar (II)
		return 29
	case 7: // Nisan
		return 30
	case 8: // Iyar
		return 29
	case 9: // Sivan
		return 30
	case 10: // Tammuz
		return 29
	case 11: // Av
		return 30
	case 12: // Elul
		return 29
	}
	return 29
}

func (c hebrewCalendar) dayFromDate(y, m, d int) int {
	day := c.newYear(y)
	for i := 1; i < m; i++ {
		day += c.daysInMonth(y, i)
	}
	return day + d - 1
}

func (c hebrewCalendar) dateFromDay(day int) calendarDate {
	y := 1 + floorDiv(day-hebrewEpoch, 366)
	for c.newYear(y+1) <= day {
		y++
	}
	for c.newYear(y) > day {
		y--
	}
	rest := day - c.newYear(y)
	m := 1
	for rest >= c.daysInMonth(y, m) {
		rest -= c.daysInMonth(y, m)
		m++
	}
	return calendarDate{era: "am", eraYear: y, year: y, month: m, day: rest + 1,
		code: c.monthCode(y, m)}
}

// A leap year's Adar I is the leap month: it is M05L, sitting between Shevat
// (M05) and Adar (M06).
func (c hebrewCalendar) monthCode(y, m int) string {
	if hebrewLeapYear(y) {
		switch {
		case m == 6:
			return "M05L"
		case m > 6:
			return simpleMonthCode(m - 1)
		}
	}
	return simpleMonthCode(m)
}

func (c hebrewCalendar) monthFromCode(year string, code string) (int, bool) {
	y, err := strconv.Atoi(year)
	if err != nil {
		return 0, false
	}
	n, leap, ok := parseMonthCode(code)
	if !ok {
		return 0, false
	}
	if leap {
		if n != 5 || !hebrewLeapYear(y) {
			return 0, false
		}
		return 6, true
	}
	if n > 12 {
		return 0, false
	}
	if hebrewLeapYear(y) && n >= 6 {
		return n + 1, true
	}
	return n, true
}

func (c hebrewCalendar) eraOf(y int) (string, int) { return "am", y }
func (c hebrewCalendar) eras() []string            { return []string{"am"} }

func (c hebrewCalendar) yearFromEra(era string, eraYear int) (int, bool) {
	if era == "am" {
		return eraYear, true
	}
	return 0, false
}

// umalquraYearStarts is the day each tabulated year begins, accumulated once.
// Walking three hundred years of month lengths per conversion would be the
// only other way, and this calendar is a table: the table is the arithmetic.
var umalquraYearStarts = sync.OnceValue(func() []int {
	c := umalquraCalendar{}
	out := make([]int, len(umalquraMonthLengths))
	day := umalquraEpoch
	for i := range out {
		out[i] = day
		y := umalquraFirstYear + i
		for m := 1; m <= 12; m++ {
			day += c.daysInMonth(y, m)
		}
	}
	return out
})

// atoiErr keeps the calendar code free of the strconv error dance, which it
// does not need: a year is a small integer or it is not one.
func atoiErr(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err != nil
}
