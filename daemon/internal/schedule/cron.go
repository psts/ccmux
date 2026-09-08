// Package schedule parses five-field cron lines and finds their next run.
//
// Hand-rolled on purpose (decided 2026-09-08): the syntax an agent will ever
// produce is the classic one — minute hour day-of-month month day-of-week,
// with *, lists, ranges, steps and month/weekday names — and a hundred lines
// beats a dependency for that. Vixie semantics where they matter: when both
// day fields are restricted, a day matches if EITHER does.
package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec is a parsed cron line. Each field is a bitset over its range.
type Spec struct {
	min, hour, dom, mon, dow uint64
	domStar, dowStar         bool
}

var aliases = map[string]string{
	"@hourly": "0 * * * *", "@daily": "0 0 * * *", "@midnight": "0 0 * * *",
	"@weekly": "0 0 * * 0", "@monthly": "0 0 1 * *", "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *",
}

var monthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

type field struct {
	lo, hi int
	names  []string
}

var fields = [5]field{{0, 59, nil}, {0, 23, nil}, {1, 31, nil}, {1, 12, monthNames}, {0, 7, dayNames}}

// Parse reads a cron line ("0 7 * * 1") or an alias ("@daily").
func Parse(line string) (Spec, error) {
	line = strings.TrimSpace(strings.ToLower(line))
	if alias, ok := aliases[line]; ok {
		line = alias
	}
	parts := strings.Fields(line)
	if len(parts) != 5 {
		return Spec{}, errors.New("a cron line has five fields: minute hour day-of-month month day-of-week")
	}
	var sets [5]uint64
	var stars [5]bool
	for i, p := range parts {
		set, star, err := parseField(p, fields[i])
		if err != nil {
			return Spec{}, fmt.Errorf("field %d (%q): %w", i+1, p, err)
		}
		sets[i], stars[i] = set, star
	}
	dow := sets[4]
	if dow&(1<<7) != 0 { // 7 is Sunday too
		dow |= 1
	}
	return Spec{min: sets[0], hour: sets[1], dom: sets[2], mon: sets[3], dow: dow & 0x7f, domStar: stars[2], dowStar: stars[4]}, nil
}

// parseField reads one comma list; star reports a bare "*" (which is what
// makes a day field unrestricted for the either-matches rule).
func parseField(s string, f field) (set uint64, star bool, err error) {
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			return 0, false, errors.New("empty list item")
		}
		bits, isStar, err := parsePart(part, f)
		if err != nil {
			return 0, false, err
		}
		set |= bits
		star = star || isStar
	}
	return set, star, nil
}

// parsePart reads "*", "*/n", "a", "a-b" or "a-b/n".
func parsePart(part string, f field) (uint64, bool, error) {
	rng, stepText, hasStep := strings.Cut(part, "/")
	lo, hi := f.lo, f.hi
	star := rng == "*"
	if !star {
		var err error
		if lo, hi, err = parseRange(rng, f); err != nil {
			return 0, false, err
		}
	}
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepText)
		if err != nil || n < 1 {
			return 0, false, errors.New("step must be a positive number")
		}
		step = n
	}
	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v)
	}
	return bits, star && !hasStep, nil
}

func parseRange(rng string, f field) (int, int, error) {
	a, b, isRange := strings.Cut(rng, "-")
	lo, err := parseBound(a, f)
	if err != nil {
		return 0, 0, err
	}
	hi := lo
	if isRange {
		if hi, err = parseBound(b, f); err != nil {
			return 0, 0, err
		}
	}
	if lo > hi {
		return 0, 0, fmt.Errorf("range %s runs backwards", rng)
	}
	return lo, hi, nil
}

func parseBound(s string, f field) (int, error) {
	for i, name := range f.names {
		if s == name {
			return f.lo + i, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < f.lo || n > f.hi {
		return 0, fmt.Errorf("%q is not between %d and %d", s, f.lo, f.hi)
	}
	return n, nil
}

// horizon bounds the search: a spec that cannot fire within five years
// (31 Feb) reports false rather than spinning.
const horizon = 5 * 366 * 24 * time.Hour

// Next is the first minute strictly after t that matches, in t's location.
func (sp Spec) Next(after time.Time) (time.Time, bool) {
	t := after.Truncate(time.Minute).Add(time.Minute)
	end := after.Add(horizon)
	loc := after.Location()
	for t.Before(end) {
		y, m, d := t.Date()
		switch {
		case sp.mon&(1<<uint(m)) == 0:
			t = time.Date(y, m+1, 1, 0, 0, 0, 0, loc)
		case !sp.dayMatches(t):
			t = time.Date(y, m, d+1, 0, 0, 0, 0, loc)
		case sp.hour&(1<<uint(t.Hour())) == 0:
			// Wall-clock hour, not Truncate: that rounds against UTC and lands
			// on :30 forever in a half-hour zone (Kolkata, Tehran, Adelaide).
			t = time.Date(y, m, d, t.Hour()+1, 0, 0, 0, loc)
		case sp.min&(1<<uint(t.Minute())) == 0:
			t = t.Add(time.Minute)
		case sameWallClock(t, after):
			// Autumn fall-back: the wall clock reads the previous run's time
			// again an hour later. That is the same slot, not a second one.
			t = t.Add(time.Minute)
		default:
			return t, true
		}
	}
	return time.Time{}, false
}

func sameWallClock(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay() && a.Hour() == b.Hour() && a.Minute() == b.Minute()
}

func (sp Spec) dayMatches(t time.Time) bool {
	dom := sp.dom&(1<<uint(t.Day())) != 0
	dow := sp.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case sp.domStar && sp.dowStar:
		return true
	case sp.domStar:
		return dow
	case sp.dowStar:
		return dom
	}
	return dom || dow
}
