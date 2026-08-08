package engine

// What the calendars are called, in English.
//
// The names are the only part of a calendar that is data rather than
// arithmetic, and this engine carries no per-locale CLDR, so these are the
// English ones. That is a limit worth stating plainly: a Hebrew date formatted
// for a French locale will name its month in English. It is still a great deal
// better than naming it in Gregorian, which is what happened before the
// calendars existed.

import "strconv"

// eraNames is each era's name in the three widths, keyed by the identifier the
// calendars use.
var eraNames = map[string][3]string{
	// long, short, narrow
	"gregory":         {"Anno Domini", "AD", "A"},
	"gregory-inverse": {"Before Christ", "BC", "B"},
	"be":              {"Buddhist Era", "BE", "BE"},
	"roc":             {"Minguo", "Minguo", "M"},
	"broc":            {"Before R.O.C.", "Before R.O.C.", "BM"},
	"meiji":           {"Meiji", "Meiji", "M"},
	"taisho":          {"Taishō", "Taishō", "T"},
	"showa":           {"Shōwa", "Shōwa", "S"},
	"heisei":          {"Heisei", "Heisei", "H"},
	"reiwa":           {"Reiwa", "Reiwa", "R"},
	"ce":              {"Anno Domini", "AD", "A"},
	"bce":             {"Before Christ", "BC", "B"},
	"am":              {"Anno Mundi", "AM", "AM"},
	"coptic":          {"Era of Martyrs", "ERA0", "ERA0"},
	"coptic-inverse":  {"Before Era of Martyrs", "ERA1", "ERA1"},
	"ethiopic":        {"Incarnation Era", "ERA1", "ERA1"},
	"ethioaa":         {"Amete Alem", "ERA0", "ERA0"},
	"saka":            {"Saka", "Saka", "S"},
	"ap":              {"Anno Persico", "AP", "AP"},
	"ah":              {"Anno Hegirae", "AH", "AH"},
	"ah-inverse":      {"Before Hijrah", "BH", "BH"},
}

// eraName is what to print for an era, falling back to the identifier so that
// an era with no entry is named rather than blank.
func eraName(era, style string) string {
	names, ok := eraNames[era]
	if !ok {
		return era
	}
	switch style {
	case "long":
		return names[0]
	case "narrow":
		return names[2]
	}
	return names[1]
}

// monthNames are the months of the calendars whose months have names of their
// own. A calendar not listed writes its months as numbers, which is what the
// lunisolar ones do anyway.
var monthNames = map[string][]string{
	"islamic": {"Muharram", "Safar", "Rabiʻ I", "Rabiʻ II", "Jumada I",
		"Jumada II", "Rajab", "Shaʻban", "Ramadan", "Shawwal",
		"Dhuʻl-Qiʻdah", "Dhuʻl-Hijjah"},
	"hebrew": {"Tishri", "Heshvan", "Kislev", "Tevet", "Shevat", "Adar I",
		"Adar", "Nisan", "Iyar", "Sivan", "Tamuz", "Av", "Elul"},
	"coptic": {"Tout", "Baba", "Hator", "Kiahk", "Toba", "Amshir", "Baramhat",
		"Baramouda", "Bashans", "Paona", "Epep", "Mesra", "Nasie"},
	"ethiopic": {"Meskerem", "Tekemt", "Hedar", "Tahsas", "Ter", "Yekatit",
		"Megabit", "Miazia", "Genbot", "Sene", "Hamle", "Nehasse", "Pagumen"},
	"persian": {"Farvardin", "Ordibehesht", "Khordad", "Tir", "Mordad",
		"Shahrivar", "Mehr", "Aban", "Azar", "Dey", "Bahman", "Esfand"},
	"indian": {"Chaitra", "Vaisakha", "Jyaistha", "Asadha", "Sravana",
		"Bhadra", "Asvina", "Kartika", "Agrahayana", "Pausa", "Magha",
		"Phalguna"},
}

// monthNameFor is the month's name in a calendar, or "" where the calendar
// writes its months as numbers. A Hebrew common year has no Adar I, so its
// months after Shevat shift back one place in the table.
func monthNameFor(calendarID string, cal calendar, year, month int) string {
	key := calendarID
	switch calendarID {
	case "islamic-civil", "islamic-tbla", "islamic-umalqura":
		key = "islamic"
	case "ethioaa":
		key = "ethiopic"
	case "hebrew":
		if !cal.inLeapYear(year) && month >= 6 {
			month++
		}
	}
	names, ok := monthNames[key]
	if !ok || month < 1 || month > len(names) {
		return ""
	}
	return names[month-1]
}

// The sexagenary cycle: ten stems and twelve branches, paired off, which names
// every year in a sixty-year round. It is how a Chinese year is named when it
// is named at all -- 2024 is the year of jiǎ-chén -- and it is what
// Intl.DateTimeFormat reports as the yearName.
var (
	sexagenaryStems    = []string{"jia", "yi", "bing", "ding", "wu", "ji", "geng", "xin", "ren", "gui"}
	sexagenaryBranches = []string{"zi", "chou", "yin", "mao", "chen", "si", "wu", "wei", "shen", "you", "xu", "hai"}
	// The characters the names are actually written in, which a Chinese
	// locale uses and a romanisation stands in for everywhere else.
	sexagenaryStemChars   = []rune("甲乙丙丁戊己庚辛壬癸")
	sexagenaryBranchChars = []rune("子丑寅卯辰巳午未申酉戌亥")
)

// sexagenaryName is the year's name, romanised. lang is the language it is
// being written for: Chinese writes the two characters with nothing between
// them, and everything else gets the pinyin joined by a hyphen.
func sexagenaryName(year int, lang string) string {
	n := floorMod(year-1, 60)
	if lang == "zh" || lang == "ja" || lang == "ko" {
		return string(sexagenaryStemChars[n%10]) + string(sexagenaryBranchChars[n%12])
	}
	return sexagenaryStems[n%10] + "-" + sexagenaryBranches[n%12]
}

// relatedGregorianYear is the Gregorian year a lunisolar year began in, which
// is what a formatter writes beside the year's name: "2024(jia-chen)".
func relatedGregorianYear(cal calendar, id string, year int) string {
	c, ok := cal.(lunisolarCalendar)
	if !ok {
		return strconv.Itoa(year)
	}
	y, _, _ := isoDate(c.year(year).starts[0])
	return strconv.Itoa(y)
}
