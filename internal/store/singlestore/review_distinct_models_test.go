package singlestore

// The rule that tells a seat claim's order row image from the image it replaced
// (req-review-gates-evidence AC-2.10; component-persistence, SingleStore
// visibility).

import (
	"testing"
	"time"
)

func TestReviewSeatClaimNoteRoundTrip(t *testing.T) {
	note := reviewSeatClaimNote{OrderID: "task:with:colons-review-1-seat-2", AttemptID: "attempt-abc", ReplacedAt: time.UnixMicro(1_793_896_955_123_456)}
	parsed, err := parseReviewSeatClaimNote(note.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.OrderID != note.OrderID || parsed.AttemptID != note.AttemptID || !parsed.ReplacedAt.Equal(note.ReplacedAt) {
		t.Fatalf("parsed=%+v, want %+v", parsed, note)
	}
	for _, bad := range []string{"", "1", "1:attempt", "x:attempt:order", "1::order", "1:attempt:"} {
		if _, err := parseReviewSeatClaimNote(bad); err == nil {
			t.Errorf("note %q parsed", bad)
		}
	}
}

func TestReviewSeatClaimNoteRowImages(t *testing.T) {
	replaced := time.UnixMicro(1_000_000)
	note := reviewSeatClaimNote{OrderID: "seat", AttemptID: "attempt-a", ReplacedAt: replaced}
	for _, tc := range []struct {
		name      string
		attempt   string
		updatedAt time.Time
		want      bool
	}{
		{"the image the claim replaced", "", replaced, false},
		{"an earlier attempt's image the claim replaced", "attempt-old", replaced, false},
		{"the claim's own image", "attempt-a", replaced.Add(time.Second), true},
		{"the claim's image renewed", "attempt-a", replaced.Add(time.Minute), true},
		{"a release after the claim", "", replaced.Add(time.Minute), true},
		{"a release stamped by a slower clock", "", replaced.Add(-time.Minute), true},
		{"a later attempt", "attempt-b", replaced.Add(time.Minute), true},
		{"the same stamp in another location", "", replaced.UTC(), false},
	} {
		if got := note.showsOrFollowsClaim(tc.attempt, tc.updatedAt); got != tc.want {
			t.Errorf("%s: got %t, want %t", tc.name, got, tc.want)
		}
	}
}
