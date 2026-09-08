package schedule

import (
	"testing"
	"time"
)

var stockholm = mustLoad("Europe/Stockholm")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func at(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, stockholm)
}

func TestParseRejectsBadSpecs(t *testing.T) {
	for _, bad := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *",
		"* * * 13 *", "* * * * 8", "a * * * *", "*/0 * * * *", "5-3 * * * *", "1,, * * * *", "@never",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestParseAcceptsNamesAliasesAndSteps(t *testing.T) {
	for _, good := range []string{
		"0 7 * * 1", "*/15 * * * *", "0 9-17/2 * * mon-fri", "30 6 1,15 * *", "0 0 * jan,jul *",
		"0 0 * * 7", "@hourly", "@daily", "@weekly", "@monthly", "  0   7 * * 1  ",
	} {
		if _, err := Parse(good); err != nil {
			t.Errorf("Parse(%q): %v", good, err)
		}
	}
}

func TestNextWeeklyMondaySeven(t *testing.T) {
	sp, _ := Parse("0 7 * * 1")
	// Wednesday 9 Sep 2026 → Monday 14 Sep 07:00.
	got, ok := sp.Next(at(2026, time.September, 9, 12, 0))
	if !ok || !got.Equal(at(2026, time.September, 14, 7, 0)) {
		t.Fatalf("next = %v, %v", got, ok)
	}
	// Exactly at the fire time: the NEXT one, a week later, not now.
	got, _ = sp.Next(at(2026, time.September, 14, 7, 0))
	if !got.Equal(at(2026, time.September, 21, 7, 0)) {
		t.Fatalf("from the minute itself = %v", got)
	}
}

func TestNextEveryFifteenMinutesAndHourly(t *testing.T) {
	sp, _ := Parse("*/15 * * * *")
	got, _ := sp.Next(at(2026, time.September, 9, 12, 7))
	if !got.Equal(at(2026, time.September, 9, 12, 15)) {
		t.Fatalf("quarter = %v", got)
	}
	sp, _ = Parse("@hourly")
	got, _ = sp.Next(at(2026, time.September, 9, 23, 30))
	if !got.Equal(at(2026, time.September, 10, 0, 0)) {
		t.Fatalf("hourly over midnight = %v", got)
	}
}

func TestNextDayOfMonthSkipsShortMonths(t *testing.T) {
	sp, _ := Parse("0 9 31 * *")
	got, _ := sp.Next(at(2026, time.September, 1, 0, 0))
	if !got.Equal(at(2026, time.October, 31, 9, 0)) {
		t.Fatalf("31st = %v", got)
	}
}

func TestNextDayOfMonthOrDayOfWeekWhenBothGiven(t *testing.T) {
	// Vixie cron: both restricted means EITHER matches.
	sp, _ := Parse("0 8 15 * fri")
	got, _ := sp.Next(at(2026, time.September, 9, 0, 0)) // Wed 9 Sep
	if !got.Equal(at(2026, time.September, 11, 8, 0)) {  // Fri 11 Sep, before the 15th
		t.Fatalf("either = %v", got)
	}
}

func TestNextAcrossDaylightSaving(t *testing.T) {
	sp, _ := Parse("30 2 * * *")
	// 29 Mar 2026: 02:00 becomes 03:00 in Stockholm; 02:30 does not exist,
	// so that day is skipped rather than fired at a made-up time.
	got, _ := sp.Next(at(2026, time.March, 28, 3, 0))
	if !got.Equal(at(2026, time.March, 30, 2, 30)) {
		t.Fatalf("spring forward = %v", got)
	}
	// 25 Oct 2026: 02:30 happens twice; the first one fires, once.
	got, _ = sp.Next(at(2026, time.October, 25, 0, 0))
	if got.Hour() != 2 || got.Minute() != 30 || got.Day() != 25 {
		t.Fatalf("fall back = %v", got)
	}
	after, _ := sp.Next(got)
	if after.Day() != 26 {
		t.Fatalf("after the repeated hour = %v", after)
	}
}

func TestNextGivesUpOnImpossibleDates(t *testing.T) {
	sp, _ := Parse("0 0 31 feb *")
	if _, ok := sp.Next(at(2026, time.January, 1, 0, 0)); ok {
		t.Fatal("31 Feb should never fire")
	}
}

func TestNextInHalfHourZone(t *testing.T) {
	kolkata := mustLoad("Asia/Kolkata")
	sp, _ := Parse("0 7 * * 1")
	got, ok := sp.Next(time.Date(2026, time.September, 8, 10, 15, 0, 0, kolkata))
	if !ok || !got.Equal(time.Date(2026, time.September, 14, 7, 0, 0, 0, kolkata)) {
		t.Fatalf("Kolkata next = %v, %v", got, ok)
	}
}
