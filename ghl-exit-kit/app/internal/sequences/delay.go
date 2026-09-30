package sequences

import (
	"fmt"
	"strconv"
	"strings"
)

// delayUnits are the units of the delay notation, largest first.
var delayUnits = []struct {
	suffix  string
	seconds int64
}{{"d", 86400}, {"h", 3600}, {"m", 60}, {"s", 1}}

// ParseDelay parses the delay notation of the YAML format: "0", or a whole
// number followed by one unit: s (seconds), m (minutes), h (hours) or d
// (days), e.g. "90m", "36h", "2d". It returns seconds, at most MaxDelay.
func ParseDelay(s string) (int64, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return 0, fmt.Errorf("delay is required (e.g. 0, 90m, 36h, 2d)")
	}
	if v == "0" {
		return 0, nil
	}
	for _, u := range delayUnits {
		num, ok := strings.CutSuffix(v, u.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil || n < 0 || num == "" || num[0] == '+' {
			break
		}
		if n > MaxDelay/u.seconds {
			return 0, fmt.Errorf("delay %q is longer than 365 days", s)
		}
		return n * u.seconds, nil
	}
	return 0, fmt.Errorf("delay %q must be 0 or a whole number with a unit s, m, h or d (e.g. 90m, 36h, 2d)", s)
}

// FormatDelay writes seconds in the delay notation with the largest unit
// that divides it exactly: 0, "2d", "36h", "90m" or "45s".
func FormatDelay(seconds int64) string {
	if seconds <= 0 {
		return "0"
	}
	for _, u := range delayUnits {
		if seconds%u.seconds == 0 {
			return strconv.FormatInt(seconds/u.seconds, 10) + u.suffix
		}
	}
	return strconv.FormatInt(seconds, 10) + "s"
}
