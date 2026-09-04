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
		{TaskStream("260903-ebf5ec"), "task", "260903-ebf5ec", true},
		{WorkOrderStream("wo-1"), "work_order", "wo-1", true},
		{RequirementStream("req-1"), "requirement", "req-1", true},
		{DesignStream("design-database"), "design", "design-database", true},
		{DecisionStream("DEC-6"), "decision", "DEC-6", true},
		{ReferenceDocumentStream("ref-1"), "reference_document", "ref-1", true},
		{PlanningSessionStream("ps-1"), "planning_session", "ps-1", true},
		{PlanningBundleStream("pb-1"), "planning_bundle", "pb-1", true},
		{WorkspaceStream("demo"), "workspace", "demo", true},
		{UserStream("usr_1"), "user", "usr_1", true},
		{WorkerStream("worker-1"), "worker", "worker-1", true},
		{GenesisStream, "log", "genesis", true},
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
	if err := ValidateAppend("ws", TaskStream("t"), ok); err != nil {
		t.Fatalf("valid append rejected: %v", err)
	}
	if err := ValidateAppend(" ", TaskStream("t"), ok); err != ErrEmptyWorkspace {
		t.Fatalf("blank workspace err=%v", err)
	}
	if err := ValidateAppend("ws", StreamID("nope"), ok); err == nil {
		t.Fatal("invalid stream accepted")
	}
	if err := ValidateAppend("ws", TaskStream("t"), nil); err != ErrEmptyAppend {
		t.Fatalf("empty append err=%v", err)
	}
	if err := ValidateAppend("ws", TaskStream("t"), []NewEvent{{Kind: "a"}, {Kind: ""}}); err == nil {
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

func TestRouterBindsWorkspacesToStores(t *testing.T) {
	fallback, singlestore := &stubStore{}, &stubStore{}
	r := NewRouter(fallback)
	if r.For("demo") != fallback {
		t.Fatal("unbound workspace did not use the fallback")
	}
	r.Bind("ff-demo-2", singlestore)
	if r.For("ff-demo-2") != singlestore || r.For("demo") != fallback {
		t.Fatal("binding not honoured")
	}
	stores := r.Stores()
	if len(stores) != 2 || stores[0] != fallback {
		t.Fatalf("stores=%v", stores)
	}
	r.Bind("ff-demo-2", fallback)
	if len(r.Stores()) != 1 {
		t.Fatal("rebinding to the fallback still lists two stores")
	}
}

func TestVersionConflictError(t *testing.T) {
	err := &VersionConflictError{Workspace: "ws", Stream: TaskStream("t"), Expected: 2, Actual: 3}
	if !IsVersionConflict(err) || IsVersionConflict(ErrEmptyAppend) {
		t.Fatal("IsVersionConflict misclassified")
	}
	if err.Error() != "eventlog: ws task/t expected version 2, head is 3" {
		t.Fatalf("message=%q", err.Error())
	}
}
