package snapshot

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseHandoff(t *testing.T) {
	reply := "Here is the handoff document:\n```json\n" +
		`{"state":"README updated, tests green","decisions":[{"what":"kept old API","why":"consumers"}],` +
		`"files_touched":[{"path":"README.md","why":"the task"}],"gotchas":["CI needs node 22"],"todos":["await review"]}` +
		"\n```\nGood luck!"
	h, err := ParseHandoff(reply)
	if err != nil {
		t.Fatal(err)
	}
	if h.State != "README updated, tests green" {
		t.Fatalf("state = %q", h.State)
	}
	if len(h.Decisions) != 1 || h.Decisions[0].Why != "consumers" {
		t.Fatalf("decisions = %+v", h.Decisions)
	}
	if len(h.Todos) != 1 {
		t.Fatalf("todos = %+v", h.Todos)
	}
}

func TestParseHandoffRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"I could not produce a handoff.", // no JSON at all
		`{"unrelated": true}`,            // JSON but no handoff content
		"{broken json",                   // unparseable
	} {
		if _, err := ParseHandoff(bad); err == nil {
			t.Errorf("ParseHandoff(%q): want error", bad)
		}
	}
}

func TestHandoffRoundTripAndBriefing(t *testing.T) {
	dir := t.TempDir()
	h := &Handoff{State: "half done", Todos: []string{"finish the parser"}}
	path := dir + "/handoff.json"
	if err := h.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "half done" {
		t.Fatalf("state = %q", got.State)
	}
	brief := got.OpeningContext("please also fix the typo")
	if !strings.Contains(brief, "half done") || !strings.Contains(brief, "please also fix the typo") {
		t.Fatalf("briefing missing content: %s", brief)
	}
}

func TestPathIsJobScopedAndRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	path, err := Path(dir, "task-1-implement-2")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := path, filepath.Join(dir, "handoff-task-1-implement-2.json"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	for _, bad := range []string{"", "../escape", `..\escape`} {
		if _, err := Path(dir, bad); err == nil {
			t.Errorf("Path(%q): want error", bad)
		}
	}
}

func TestSaveAtomicallyReplacesSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.json")
	if err := (&Handoff{State: "old"}).Save(path); err != nil {
		t.Fatal(err)
	}
	if err := (&Handoff{State: "new"}).Save(path); err != nil {
		t.Fatal(err)
	}
	h, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if h.State != "new" {
		t.Fatalf("state = %q, want new", h.State)
	}
}
