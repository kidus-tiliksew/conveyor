package store

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A report's stated times follow component-feedback-reports v2 invariant 4
// (req-production-feedback AC-4.1, AC-4.3, AC-4.4). A time is an RFC 3339
// date-time. It is refused when it states a second of 60, when it states more
// than nine digits of a fraction of a second, or when its instant is earlier
// than 1970-01-01T00:00:00Z. Every accepted time is exact to the nanosecond,
// and every decision (the default and the clamp to the arrival, the order
// check, the merge choice of the earlier first-seen and the later last-seen,
// and the comparison with the closing time) compares those exact values. A
// time is cut to the stored microsecond only when it is written.

// The fixed refusal messages of a stated time. None repeats a submitted
// value.
const (
	FeedbackReportTimeInvalid     = "must be an RFC 3339 time"
	FeedbackReportTimeLeapSecond  = "must not state a second of 60"
	FeedbackReportTimeTooPrecise  = "must not state more than nine digits of a fraction of a second"
	FeedbackReportTimeBeforeEpoch = "must not be earlier than 1970-01-01T00:00:00Z"
)

// feedbackReportEarliest is the earliest instant a report can state
// (invariant 4).
var feedbackReportEarliest = time.Unix(0, 0).UTC()

// StatedFeedbackReportTime is a stated time for an entry, in UTC.
func StatedFeedbackReportTime(t time.Time) *time.Time {
	at := t.UTC()
	return &at
}

var feedbackReportTimePattern = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})[Tt]([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]+))?(?:([Zz])|([+-])([0-9]{2}):([0-9]{2}))$`)

// ParseFeedbackReportTime reads one date-time of RFC 3339 section 5.6 and
// answers its exact instant, or a fixed refusal message. It refuses what Go's
// time.Parse lets through, such as an offset hour above 23, an offset minute
// above 59, or a day the month does not hold. The section 5.6 grammar is
// ABNF, whose literals do not depend on case, so "T" and "Z" are accepted in
// either case, as its note on lowercase "t" and "z" states. A second of 60 and
// more than nine fraction digits are refused (invariant 4). The 1970 floor is
// checked by NormalizeFeedbackReports on the instant, so every form of one
// instant is treated the same.
func ParseFeedbackReportTime(text string) (time.Time, string) {
	match := feedbackReportTimePattern.FindStringSubmatch(text)
	if match == nil {
		return time.Time{}, FeedbackReportTimeInvalid
	}
	number := func(i int) int {
		value, _ := strconv.Atoi(match[i])
		return value
	}
	year, month, day := number(1), number(2), number(3)
	hour, minute, second := number(4), number(5), number(6)
	if month < 1 || month > 12 || day < 1 || day > daysIn(year, time.Month(month)) || hour > 23 || minute > 59 || second > 60 {
		return time.Time{}, FeedbackReportTimeInvalid
	}
	offset := 0
	if match[8] == "" {
		offsetHour, offsetMinute := number(10), number(11)
		if offsetHour > 23 || offsetMinute > 59 {
			return time.Time{}, FeedbackReportTimeInvalid
		}
		offset = offsetHour*60 + offsetMinute
		if match[9] == "-" {
			offset = -offset
		}
	}
	if second == 60 {
		return time.Time{}, FeedbackReportTimeLeapSecond
	}
	fraction := match[7]
	if len(fraction) > 9 {
		return time.Time{}, FeedbackReportTimeTooPrecise
	}
	nanos := 0
	if fraction != "" {
		nanos, _ = strconv.Atoi(fraction + strings.Repeat("0", 9-len(fraction)))
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, nanos, time.UTC).Add(-time.Duration(offset) * time.Minute), ""
}

// daysIn is the number of days of a month in the proleptic Gregorian
// calendar that RFC 3339 uses.
func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
