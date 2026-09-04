package eventlog

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStreamIDShape(t *testing.T) {
	cases := []struct {
		stream  StreamID
		typ, id string
		valid   bool
	}{
		{StreamID("task/260903-ebf5ec"), "task", "260903-ebf5ec", true},
		{StreamID("job/dispatch_task:x"), "job", "dispatch_task:x", true},
		{StreamID("task"), "task", "", false},
		{StreamID("task/"), "task", "", false},
		{StreamID("/x"), "", "x", false},
		{StreamID("task/has space"), "task", "has space", false},
		{StreamID(""), "", "", false},
	}
	for _, tc := range cases {
		if tc.stream.Type() != tc.typ || tc.stream.EntityID() != tc.id || tc.stream.Valid() != tc.valid {
			t.Errorf("%q: type=%q id=%q valid=%t", tc.stream, tc.stream.Type(), tc.stream.EntityID(), tc.stream.Valid())
		}
	}
}

func TestValidateAppend(t *testing.T) {
	ok := []NewEvent{{Kind: "k"}}
	if err := ValidateAppend("ws", StreamID("task/t"), ok); err != nil {
		t.Fatalf("valid append rejected: %v", err)
	}
	if err := ValidateAppend(" ", StreamID("task/t"), ok); err != ErrEmptyWorkspace {
		t.Fatalf("blank workspace err=%v", err)
	}
	if err := ValidateAppend("ws", StreamID("nope"), ok); err == nil {
		t.Fatal("invalid stream accepted")
	}
	if err := ValidateAppend("ws", StreamID("task/t"), nil); err != ErrEmptyAppend {
		t.Fatalf("empty append err=%v", err)
	}
	if err := ValidateAppend("ws", StreamID("task/t"), []NewEvent{{Kind: "a"}, {Kind: ""}}); err == nil {
		t.Fatal("blank kind accepted")
	}
}

func TestNormalize(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	got := Normalize(NewEvent{Kind: "k"}, now)
	if !got.At.Equal(now) || string(got.Payload) != "{}" {
		t.Fatalf("normalised=%+v", got)
	}
	local := time.Date(2026, 9, 4, 12, 0, 0, 0, time.FixedZone("plus2", 2*3600))
	got = Normalize(NewEvent{Kind: "k", At: local, Payload: json.RawMessage(`{"a":1}`)}, now)
	if got.At.Location() != time.UTC || !got.At.Equal(local) || string(got.Payload) != `{"a":1}` {
		t.Fatalf("normalised=%+v", got)
	}
}

type stubStore struct{ Store }

func TestVersionConflictError(t *testing.T) {
	err := &VersionConflictError{Workspace: "ws", Stream: StreamID("task/t"), Expected: 2, Actual: 3}
	if !IsVersionConflict(err) || IsVersionConflict(ErrEmptyAppend) {
		t.Fatal("IsVersionConflict misclassified")
	}
	if err.Error() != "eventlog: ws task/t expected version 2, head is 3" {
		t.Fatalf("message=%q", err.Error())
	}
}
