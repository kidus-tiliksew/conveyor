package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The turn-on time is server-owned: only a turn-on records the write time,
// and a client-sent value is never kept (req-delivery-and-forge AC-8.1,
// AC-8.3; component-git-delivery).
func TestStampIssuePublication(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 30, 0, 123456789, time.UTC)
	turnedOn := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	forged := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		previous IssuePublication
		next     IssuePublication
		want     IssuePublication
	}{
		{name: "absent stays off", want: IssuePublication{}},
		{name: "creation turned on", next: IssuePublication{Enabled: true, EnabledAt: forged}, want: IssuePublication{Enabled: true, EnabledAt: now.Truncate(time.Microsecond)}},
		{name: "turn-on", previous: IssuePublication{EnabledAt: turnedOn}, next: IssuePublication{Enabled: true, EnabledAt: forged}, want: IssuePublication{Enabled: true, EnabledAt: now.Truncate(time.Microsecond)}},
		{name: "kept on", previous: IssuePublication{Enabled: true, EnabledAt: turnedOn}, next: IssuePublication{Enabled: true, EnabledAt: forged}, want: IssuePublication{Enabled: true, EnabledAt: turnedOn}},
		{name: "on without a recorded time", previous: IssuePublication{Enabled: true}, next: IssuePublication{Enabled: true}, want: IssuePublication{Enabled: true, EnabledAt: now.Truncate(time.Microsecond)}},
		{name: "turn-off keeps the last turn-on", previous: IssuePublication{Enabled: true, EnabledAt: turnedOn}, next: IssuePublication{EnabledAt: forged}, want: IssuePublication{EnabledAt: turnedOn}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := tc.next
			StampIssuePublication(tc.previous, &next, now)
			if next.Enabled != tc.want.Enabled || !next.EnabledAt.Equal(tc.want.EnabledAt) {
				t.Fatalf("stamped %+v, want %+v", next, tc.want)
			}
		})
	}
	if _, on := (IssuePublication{Enabled: true}).ApprovedSince(); on {
		t.Fatal("a setting without a turn-on time admitted approvals")
	}
	if _, on := (IssuePublication{EnabledAt: turnedOn}).ApprovedSince(); on {
		t.Fatal("a turned-off setting admitted approvals")
	}
	if since, on := (IssuePublication{Enabled: true, EnabledAt: turnedOn}).ApprovedSince(); !on || !since.Equal(turnedOn) {
		t.Fatalf("approved since=%s on=%t", since, on)
	}
}

// A document that never set the setting parses as off, and the policy
// projection serves the setting (AC-8.1, AC-8.5).
func TestIssuePublicationPolicyDocumentRoundTrip(t *testing.T) {
	deployment := &Config{Workspace: "demo", Repos: []Repo{{Name: "app", URL: "https://github.com/acme/app.git"}}}
	parsed, err := ParseWorkspaceDocument([]byte("workspace: demo\nmax_bounces: 2\nrepos:\n  - name: app\n    url: https://github.com/acme/app.git\n"), deployment, "test")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.IssuePublication.Enabled || !parsed.IssuePublication.EnabledAt.IsZero() {
		t.Fatalf("absent setting parsed as %+v", parsed.IssuePublication)
	}
	stored, err := MarshalPolicyDocument(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "issue_publication") {
		t.Fatalf("an absent setting was written: %s", stored)
	}
	turnedOn := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	parsed.IssuePublication = IssuePublication{Enabled: true, EnabledAt: turnedOn}
	stored, err = MarshalPolicyDocument(parsed)
	if err != nil {
		t.Fatal(err)
	}
	reparsed, err := ParseWorkspaceDocument(stored, deployment, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !reparsed.IssuePublication.Enabled || !reparsed.IssuePublication.EnabledAt.Equal(turnedOn) {
		t.Fatalf("round trip=%+v", reparsed.IssuePublication)
	}
	encoded, err := json.Marshal(reparsed.PolicyDocument())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"issue_publication":{"enabled":true,"enabled_at":"2026-10-29T09:00:00Z"}`) {
		t.Fatalf("policy projection=%s", encoded)
	}
}
