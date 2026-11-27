package store

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A report's stated times follow one rule (component-feedback-reports
// invariants 4, 6, and 7; req-production-feedback AC-4.1, AC-4.3, AC-4.4).
// Every decision compares the exact stated value: the default and the clamp
// to the arrival, the order check, the merge choice of the earlier first-seen
// and the later last-seen, and the comparison with the closing time. A value
// is cut to the stored microsecond only when it is written, and every stored
// time from 0000-01-01T00:00:00Z to 9999-12-31T23:59:59.999999Z reads back
// equal on every backend.

// FeedbackReportTime is one stated time with every digit the request gave.
// At is the UTC instant to the nanosecond. Beyond holds the fractional digits
// after the ninth, with trailing zeros removed. A leap second, second 60, is
// held as second 59 of the same minute with Leap set, so it orders after
// every time of second 59 and before the next minute.
type FeedbackReportTime struct {
	At     time.Time
	Leap   bool
	Beyond string
}

// StatedFeedbackReportTime is a Go time as a stated time.
func StatedFeedbackReportTime(t time.Time) *FeedbackReportTime {
	return &FeedbackReportTime{At: t.UTC()}
}

// feedbackReportEarliest is the earliest time a report can state: the stored
// range starts at 0000-01-01T00:00:00Z.
var feedbackReportEarliest = time.Date(0, time.January, 1, 0, 0, 0, 0, time.UTC)

// Compare orders two stated times exactly: by the whole second, then a leap
// second after its second 59, then by nanoseconds, then by the digits beyond.
func (t FeedbackReportTime) Compare(other FeedbackReportTime) int {
	if order := t.At.Truncate(time.Second).Compare(other.At.Truncate(time.Second)); order != 0 {
		return order
	}
	if t.Leap != other.Leap {
		if t.Leap {
			return 1
		}
		return -1
	}
	if order := t.At.Compare(other.At); order != 0 {
		return order
	}
	a, b := t.Beyond, other.Beyond
	for len(a) < len(b) {
		a += "0"
	}
	for len(b) < len(a) {
		b += "0"
	}
	return strings.Compare(a, b)
}

// Stored is the value a backend writes: the instant cut to the microsecond.
// A leap second is stored as the last microsecond of its second 59, the
// latest stored value it can follow.
func (t FeedbackReportTime) Stored() time.Time {
	if t.Leap {
		return t.At.Truncate(time.Second).Add(time.Second - time.Microsecond)
	}
	return EnvironmentTime(t.At)
}

// feedbackReportStoredTime is a stored time as a stated time, so a decision
// compares it with an entry's exact time.
func feedbackReportStoredTime(t time.Time) FeedbackReportTime {
	return FeedbackReportTime{At: t.UTC()}
}

var feedbackReportTimePattern = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})[Tt]([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]+))?(?:([Zz])|([+-])([0-9]{2}):([0-9]{2}))$`)

// ParseFeedbackReportTime reads one RFC 3339 date-time (RFC 3339 section
// 5.6) and refuses everything else, including what Go's time.Parse lets
// through: an offset hour above 23 or minute above 59, a day the month does
// not hold, and lowercase or missing separators other than "t" and "z". It
// keeps every fractional digit. A leap second is accepted where UTC 23:59
// begins it (section 5.7). The time can lie outside the stored range; the
// normalizer decides that.
func ParseFeedbackReportTime(text string) (FeedbackReportTime, bool) {
	match := feedbackReportTimePattern.FindStringSubmatch(text)
	if match == nil {
		return FeedbackReportTime{}, false
	}
	number := func(i int) int {
		value, _ := strconv.Atoi(match[i])
		return value
	}
	year, month, day := number(1), number(2), number(3)
	hour, minute, second := number(4), number(5), number(6)
	if month < 1 || month > 12 || day < 1 || day > daysIn(year, time.Month(month)) || hour > 23 || minute > 59 || second > 60 {
		return FeedbackReportTime{}, false
	}
	offset := 0
	if match[8] == "" {
		offsetHour, offsetMinute := number(10), number(11)
		if offsetHour > 23 || offsetMinute > 59 {
			return FeedbackReportTime{}, false
		}
		offset = offsetHour*60 + offsetMinute
		if match[9] == "-" {
			offset = -offset
		}
	}
	leap := second == 60
	if leap {
		// UTC minute of the local minute: a leap second ends UTC 23:59.
		if utc := ((hour*60+minute-offset)%1440 + 1440) % 1440; utc != 23*60+59 {
			return FeedbackReportTime{}, false
		}
		second = 59
	}
	fraction := match[7]
	nanos := 0
	if fraction != "" {
		digits := fraction
		if len(digits) > 9 {
			digits = digits[:9]
		}
		nanos, _ = strconv.Atoi(digits + strings.Repeat("0", 9-len(digits)))
	}
	beyond := ""
	if len(fraction) > 9 {
		beyond = strings.TrimRight(fraction[9:], "0")
	}
	at := time.Date(year, time.Month(month), day, hour, minute, second, nanos, time.UTC).Add(-time.Duration(offset) * time.Minute)
	return FeedbackReportTime{At: at, Leap: leap, Beyond: beyond}, true
}

// daysIn is the number of days of a month in the proleptic Gregorian
// calendar that RFC 3339 uses, where year 0000 is a leap year.
func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
