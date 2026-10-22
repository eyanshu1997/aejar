// Package limits parses subscription usage-limit messages into a reset time.
package limits

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FallbackWait is used when a limit was hit but no reset time could be parsed;
// aejard probes again after it.
const FallbackWait = 15 * time.Minute

// Claude Code phrases resets like "resets 6pm (Asia/Kolkata)",
// "resets at 18:30" or "resets 5:30pm".
var resetRe = regexp.MustCompile(`(?i)resets?\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([A-Za-z_]+/[A-Za-z_]+|UTC)\))?`)

// ResetTime returns the next reset after now described in msg. ok is false
// when msg names no time; the caller then waits FallbackWait.
func ResetTime(msg string, now time.Time, defaultLoc *time.Location) (reset time.Time, ok bool) {
	m := resetRe.FindStringSubmatch(msg)
	if m == nil {
		return now.Add(FallbackWait), false
	}
	hour, _ := strconv.Atoi(m[1])
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	switch strings.ToLower(m[3]) {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}
	if hour > 23 || minute > 59 {
		return now.Add(FallbackWait), false
	}
	loc := defaultLoc
	if m[4] != "" {
		if l, err := time.LoadLocation(m[4]); err == nil {
			loc = l
		}
	}
	local := now.In(loc)
	reset = time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !reset.After(now) {
		reset = reset.AddDate(0, 0, 1)
	}
	return reset, true
}
