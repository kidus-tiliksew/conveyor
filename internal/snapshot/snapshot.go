// Package snapshot implements handoff snapshots — the guaranteed
// continuity contract between jobs (spec §8.3). At the end of every
// job, one additional prompt has the agent write this document; it is
// stored as a job artifact and injected into the successor job's
// opening context. Native session resume is only an optimization on
// top; the snapshot is the floor.
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Handoff is deliberately plain structured text: portable across hosts,
// sandboxes, and *different* harnesses, immune to harness version drift.
type Handoff struct {
	TaskID    string    `json:"task_id"`
	JobID     string    `json:"job_id"`
	Harness   string    `json:"harness"`
	WrittenAt time.Time `json:"written_at"`

	State        string        `json:"state"`         // current state of the work
	Decisions    []Decision    `json:"decisions"`     // key decisions and rationale
	FilesTouched []FileTouched `json:"files_touched"` // files touched and why
	Gotchas      []string      `json:"gotchas"`       // known gotchas
	Todos        []string      `json:"todos"`         // remaining work
}

type Decision struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

type FileTouched struct {
	Path string `json:"path"`
	Why  string `json:"why"`
}

// ElicitationPrompt is the end-of-job prompt that produces a Handoff.
// It will move into the prompt/policy pack (spec §2.2) once the pack
// exists; it lives here so Phase 1 is runnable without pack machinery.
const ElicitationPrompt = `The job is ending. Write a handoff document for the agent that will
continue this task, as JSON matching this schema:
{"state": "...", "decisions": [{"what": "...", "why": "..."}],
 "files_touched": [{"path": "...", "why": "..."}],
 "gotchas": ["..."], "todos": ["..."]}
Cover: current state of the work, key decisions and their rationale,
files touched and why, known gotchas, and remaining todos. Output only
the JSON.`

// ParseHandoff extracts the handoff JSON from an agent's reply, which
// may wrap it in prose or markdown fences despite the "output only the
// JSON" instruction. It takes the outermost brace span and requires at
// least a state or todos to guard against unmarshalling an unrelated
// JSON fragment into an all-empty snapshot.
func ParseHandoff(text string) (*Handoff, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in handoff reply")
	}
	var h Handoff
	if err := json.Unmarshal([]byte(text[start:end+1]), &h); err != nil {
		return nil, fmt.Errorf("parse handoff: %w", err)
	}
	if h.State == "" && len(h.Todos) == 0 {
		return nil, fmt.Errorf("handoff has neither state nor todos")
	}
	return &h, nil
}

func (h *Handoff) Save(path string) error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func Load(path string) (*Handoff, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var h Handoff
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// OpeningContext renders the snapshot (plus any human redirect comments)
// as the successor job's briefing text.
func (h *Handoff) OpeningContext(redirectComments string) string {
	data, _ := json.MarshalIndent(h, "", "  ")
	s := "A previous agent worked on this task. Its handoff document:\n\n" + string(data)
	if redirectComments != "" {
		s += "\n\nHuman reviewer feedback to address:\n" + redirectComments
	}
	return s
}
