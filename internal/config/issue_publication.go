package config

import "time"

// IssuePublication is the workspace switch that lets plan approval publish a
// GitHub issue (req-delivery-and-forge REQ-8; DEC-91). The zero value is off,
// so a document that never set it, including every workspace created before
// the setting existed, publishes nothing (AC-8.1).
//
// EnabledAt is server-owned: StampIssuePublication sets it from the stored
// document on every workspace write, and a client value is never kept
// (component-git-delivery).
type IssuePublication struct {
	Enabled   bool      `yaml:"enabled" json:"enabled"`
	EnabledAt time.Time `yaml:"enabled_at,omitempty" json:"enabled_at,omitzero"`
}

// StampIssuePublication sets next.EnabledAt from the stored previous value. A
// turn-on records now, a write that keeps the setting on keeps the stored
// time, and a turn-off keeps the last turn-on time. A creation or seed has no
// previous document and passes the zero value.
func StampIssuePublication(previous IssuePublication, next *IssuePublication, now time.Time) {
	switch {
	case next.Enabled && previous.Enabled && !previous.EnabledAt.IsZero():
		next.EnabledAt = previous.EnabledAt
	case next.Enabled:
		// Both durable backends hold microseconds, the precision plan
		// approval times are compared at.
		next.EnabledAt = now.UTC().Truncate(time.Microsecond)
	default:
		next.EnabledAt = previous.EnabledAt
	}
}

// ApprovedSince returns the earliest plan approval the lifecycle reconcile
// may repair: the last turn-on time while the setting is on. It reports false
// while the setting is off or carries no turn-on time, so the reconcile
// repairs no approval then (AC-8.3).
func (p IssuePublication) ApprovedSince() (time.Time, bool) {
	if !p.Enabled || p.EnabledAt.IsZero() {
		return time.Time{}, false
	}
	return p.EnabledAt, true
}
