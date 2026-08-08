package engine

import "testing"

// Every calendar, on dates whose conversion is published rather than derived
// from this code. A derivation would share whatever the converter believes.
func TestCalendarConversions(t *testing.T) {
	for _, c := range []struct {
		id               string
		iso              [3]int // the ISO date
		year, month, day int    // what the calendar calls it
		era              string
		eraYear          int
	}{
		// The Gregorian family: the year number moves, the months do not.
		{"gregory", [3]int{2024, 2, 29}, 2024, 2, 29, "gregory", 2024},
		{"gregory", [3]int{0, 1, 1}, 0, 1, 1, "gregory-inverse", 1},
		{"buddhist", [3]int{2024, 5, 22}, 2567, 5, 22, "be", 2567},
		{"roc", [3]int{2024, 10, 10}, 113, 10, 10, "roc", 113},
		{"roc", [3]int{1911, 10, 10}, 0, 10, 10, "broc", 1},
		{"japanese", [3]int{2024, 5, 1}, 2024, 5, 1, "reiwa", 6},
		{"japanese", [3]int{2019, 4, 30}, 2019, 4, 30, "heisei", 31},
		{"japanese", [3]int{2019, 5, 1}, 2019, 5, 1, "reiwa", 1},
		{"japanese", [3]int{1926, 12, 25}, 1926, 12, 25, "showa", 1},

		// The Coptic and Ethiopic families share their arithmetic.
		// 2024-09-11 is 1 Tout 1741 in the Coptic calendar and 1 Meskerem
		// 2017 in the Ethiopic.
		{"coptic", [3]int{2024, 9, 11}, 1741, 1, 1, "am", 1741},
		{"ethiopic", [3]int{2024, 9, 11}, 2017, 1, 1, "ethiopic", 2017},
		{"ethioaa", [3]int{2024, 9, 11}, 7517, 1, 1, "ethioaa", 7517},

		// Saka: year 1946 began on 2024-03-21, a leap year, so Chaitra has
		// thirty-one days and starts a day early.
		{"indian", [3]int{2024, 3, 21}, 1946, 1, 1, "saka", 1946},
		{"indian", [3]int{2023, 3, 22}, 1945, 1, 1, "saka", 1945},

		// Persian: Nowruz 1403 was 2024-03-20.
		{"persian", [3]int{2024, 3, 20}, 1403, 1, 1, "ap", 1403},
		{"persian", [3]int{2023, 3, 21}, 1402, 1, 1, "ap", 1402},

		// The tabular Islamic calendars differ by exactly one day.
		{"islamic-civil", [3]int{2024, 7, 8}, 1446, 1, 2, "ah", 1446},
		{"islamic-tbla", [3]int{2024, 7, 8}, 1446, 1, 3, "ah", 1446},
		// Umm al-Qura put 1 Muharram 1446 on 2024-07-07.
		{"islamic-umalqura", [3]int{2024, 7, 7}, 1446, 1, 1, "ah", 1446},

		// Hebrew: Rosh Hashanah 5785 was 2024-10-03, and 5784 was a leap year
		// with an Adar I.
		{"hebrew", [3]int{2024, 10, 3}, 5785, 1, 1, "am", 5785},
		{"hebrew", [3]int{2024, 3, 11}, 5784, 7, 1, "am", 5784},
	} {
		cal := calendarFor(c.id)
		day := isoDay(c.iso[0], c.iso[1], c.iso[2])
		got := cal.dateFromDay(day)
		if got.year != c.year || got.month != c.month || got.day != c.day {
			t.Errorf("%s: %v is %d-%d-%d, want %d-%d-%d", c.id, c.iso,
				got.year, got.month, got.day, c.year, c.month, c.day)
		}
		if got.era != c.era || got.eraYear != c.eraYear {
			t.Errorf("%s: %v is era %q year %d, want %q %d", c.id, c.iso,
				got.era, got.eraYear, c.era, c.eraYear)
		}
		// And back again, which is the half a one-directional table cannot fake.
		if back := cal.dayFromDate(c.year, c.month, c.day); back != day {
			gy, gm, gd := isoDate(back)
			t.Errorf("%s: %d-%d-%d is %04d-%02d-%02d, want %v", c.id,
				c.year, c.month, c.day, gy, gm, gd, c.iso)
		}
	}
}

// The lunisolar calendars, on the one date a year everyone knows.
func TestChineseNewYear(t *testing.T) {
	for iso, want := range map[[3]int]int{
		{2024, 2, 10}: 4661, // the year of the dragon
		{2023, 1, 22}: 4660,
		{2025, 1, 29}: 4662,
		{2000, 2, 5}:  4637,
		{1900, 1, 31}: 4537,
	} {
		c := calendarFor("chinese")
		day := isoDay(iso[0], iso[1], iso[2])
		got := c.dateFromDay(day)
		if got.month != 1 || got.day != 1 {
			t.Errorf("chinese: %v is month %d day %d, want the first of the first",
				iso, got.month, got.day)
		}
		if got.year != want {
			t.Errorf("chinese: %v is year %d, want %d", iso, got.year, want)
		}
		// The day before is the last day of the previous year.
		if prev := c.dateFromDay(day - 1); prev.year != want-1 {
			t.Errorf("chinese: the day before %v is year %d, want %d", iso, prev.year, want-1)
		}
	}
}

// A leap month takes the name of the month before it, and the months after it
// keep the names they would have had.
func TestChineseLeapMonths(t *testing.T) {
	c := calendarFor("chinese")
	// 2023 had a leap second month: it ran from 2023-03-22 to 2023-04-19.
	for _, tc := range []struct {
		iso  [3]int
		code string
	}{
		{[3]int{2023, 2, 20}, "M02"},
		{[3]int{2023, 3, 22}, "M02L"},
		{[3]int{2023, 4, 20}, "M03"},
	} {
		got := c.dateFromDay(isoDay(tc.iso[0], tc.iso[1], tc.iso[2]))
		if got.code != tc.code {
			t.Errorf("chinese: %v is in month %s, want %s", tc.iso, got.code, tc.code)
		}
	}
}

// Round-tripping every day over a long stretch is the check that catches an
// off-by-one in a month length: a calendar that loses a day somewhere will
// disagree with itself the next time round.
func TestCalendarsRoundTrip(t *testing.T) {
	for _, id := range supportedCalendars {
		cal := calendarFor(id)
		for day := isoDay(1890, 1, 1); day < isoDay(2110, 1, 1); day += 7 {
			d := cal.dateFromDay(day)
			if back := cal.dayFromDate(d.year, d.month, d.day); back != day {
				y, m, dd := isoDate(day)
				t.Fatalf("%s: %04d-%02d-%02d became %d-%d-%d and came back as day %d",
					id, y, m, dd, d.year, d.month, d.day, back)
			}
			if d.day < 1 || d.day > cal.daysInMonth(d.year, d.month) {
				y, m, dd := isoDate(day)
				t.Fatalf("%s: %04d-%02d-%02d is day %d of a month with %d days",
					id, y, m, dd, d.day, cal.daysInMonth(d.year, d.month))
			}
			if d.month < 1 || d.month > cal.monthsInYear(d.year) {
				y, m, dd := isoDate(day)
				t.Fatalf("%s: %04d-%02d-%02d is month %d of a year with %d months",
					id, y, m, dd, d.month, cal.monthsInYear(d.year))
			}
		}
	}
}
