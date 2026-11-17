package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

type boardDiffTransition struct {
	Kind            string    `json:"kind"`
	From            string    `json:"from"`
	To              string    `json:"to"`
	At              time.Time `json:"at"`
	PreviousSeconds *int64    `json:"previous_seconds"`
}
type boardDiffDecision struct {
	Opened  bool   `json:"opened,omitempty"`
	Cleared bool   `json:"cleared,omitempty"`
	Label   string `json:"label"`
}
type boardDiffPullRequest struct {
	Number int `json:"number"`
}
type boardDiffChange struct {
	ID          string                `json:"id"`
	Title       string                `json:"title"`
	Created     bool                  `json:"created,omitempty"`
	Terminal    bool                  `json:"terminal,omitempty"`
	Transitions []boardDiffTransition `json:"transitions,omitempty"`
	Decision    *boardDiffDecision    `json:"decision,omitempty"`
	PullRequest *boardDiffPullRequest `json:"pull_request,omitempty"`
}
type boardDiffHistoryEvent struct {
	ID           string                     `json:"id"`
	Kind         string                     `json:"kind"`
	At           time.Time                  `json:"at"`
	Payload      map[string]json.RawMessage `json:"payload"`
	PayloadIndex map[string]json.RawMessage `json:"payload_index"`
}
type boardDiffHistoryPage struct {
	Events      []boardDiffHistoryEvent `json:"events"`
	HasOlder    bool                    `json:"has_older"`
	OlderCursor string                  `json:"older_cursor"`
}
type boardDiffHistory struct {
	Events     []boardDiffHistoryEvent
	Incomplete bool
	NotVisible bool
}

type boardDiffIncomplete struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

func (e boardDiffHistoryEvent) field(name string, out any) bool {
	raw := e.Payload[name]
	if len(raw) == 0 {
		raw = e.PayloadIndex[name]
	}
	return len(raw) > 0 && json.Unmarshal(raw, out) == nil
}
func (c *client) boardDiffHistory(id string, cutoff time.Time, needStart bool) (boardDiffHistory, error) {
	for range 2 {
		result, restart, err := c.walkBoardDiffHistory(id, cutoff, needStart)
		if err != nil {
			return result, err
		}
		if !restart {
			return result, nil
		}
	}
	return boardDiffHistory{Incomplete: true}, nil
}
func (c *client) walkBoardDiffHistory(id string, cutoff time.Time, needStart bool) (boardDiffHistory, bool, error) {
	var result boardDiffHistory
	cursor := ""
	crossed := false
	extra := 0
	for range 20 {
		q := url.Values{"limit": {"50"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		body, _, err := c.getRawWithHeader("/v1/tasks/" + url.PathEscape(id) + "/history?" + q.Encode())
		if err != nil {
			var status *apiStatusError
			if errors.As(err, &status) && status.Status == http.StatusConflict {
				return boardDiffHistory{}, true, nil
			}
			if errors.As(err, &status) && status.Status == http.StatusNotFound {
				return boardDiffHistory{NotVisible: true}, false, nil
			}
			return result, false, err
		}
		var page boardDiffHistoryPage
		if err := json.Unmarshal(body, &page); err != nil {
			return result, false, fmt.Errorf("decode task history: %w", err)
		}
		result.Events = append(result.Events, page.Events...)
		foundStart := false
		for _, event := range page.Events {
			if !event.At.After(cutoff) {
				crossed = true
				if event.Kind == "work_order.created" || event.Kind == "work_order.claimed" {
					var stage string
					if event.field("stage", &stage) && stage != "" {
						foundStart = true
					}
				}
			}
		}
		if !page.HasOlder {
			return result, false, nil
		}
		if page.OlderCursor == "" || page.OlderCursor == cursor {
			return result, false, fmt.Errorf("invalid task history cursor")
		}
		if crossed {
			if !needStart || foundStart || extra >= 2 {
				return result, false, nil
			}
			extra++
		}
		cursor = page.OlderCursor
	}
	result.Incomplete = true
	return result, false, nil
}

func boardDiffEventLess(a, b boardDiffHistoryEvent) bool {
	if !a.At.Equal(b.At) {
		return a.At.Before(b.At)
	}
	ai, ae := strconv.ParseUint(a.ID, 10, 64)
	bi, be := strconv.ParseUint(b.ID, 10, 64)
	if ae == nil && be == nil {
		return ai < bi
	}
	return a.ID < b.ID
}

// reduceBoardDiff merges one activity row with the saved task. Events at or
// before cutoff seed an unknown stage start but never produce duplicate changes.
func reduceBoardDiff(row boardDiffActivity, previous boardDiffTask, exists bool, cutoff time.Time, history boardDiffHistory) (boardDiffChange, boardDiffTask, bool) {
	change := boardDiffChange{ID: row.Task.ID, Title: row.Task.Title}
	next := previous
	next.Title = row.Task.Title
	if title := []rune(next.Title); len(title) > 80 {
		next.Title = string(title[:80])
	}
	sort.Slice(history.Events, func(i, j int) bool { return boardDiffEventLess(history.Events[i], history.Events[j]) })
	if next.Stage == "" {
		next.Stage = "unknown"
	}
	if next.EnteredAt == nil {
		for _, e := range history.Events {
			if e.At.After(cutoff) {
				break
			}
			if e.Kind == "work_order.created" || e.Kind == "work_order.claimed" {
				var stage string
				if e.field("stage", &stage) && stage != "" {
					if next.Stage == "unknown" {
						next.Stage = stage
					}
					if next.Stage != stage {
						next.EnteredAt = nil
					} else if next.EnteredAt == nil {
						at := e.At
						next.EnteredAt = &at
					}
				}
			}
		}
	}
	incomplete := history.Incomplete
	for _, e := range history.Events {
		if !e.At.After(cutoff) {
			continue
		}
		switch e.Kind {
		case "task.created":
			if !exists {
				change.Created = true
			}
		case "task.state_changed":
			var from, to string
			if !e.field("to", &to) || to == "" {
				to = "unknown"
				incomplete = true
			}
			if !e.field("from", &from) || from == "" {
				from = "unknown"
				incomplete = true
			}
			if from != to {
				change.Transitions = append(change.Transitions, boardDiffTransition{Kind: "state", From: from, To: to, At: e.At})
			}
			next.State = to
		case "work_order.created", "work_order.claimed":
			var stage string
			if !e.field("stage", &stage) || stage == "" {
				stage = "unknown"
				incomplete = true
			}
			if next.Stage != stage {
				var seconds *int64
				if next.EnteredAt != nil && !e.At.Before(*next.EnteredAt) {
					n := int64(e.At.Sub(*next.EnteredAt).Seconds())
					seconds = &n
				}
				change.Transitions = append(change.Transitions, boardDiffTransition{Kind: "stage", From: next.Stage, To: stage, At: e.At, PreviousSeconds: seconds})
				next.Stage = stage
				at := e.At
				next.EnteredAt = &at
			}
		case "pull_request.opened":
			var number int
			if !e.field("number", &number) {
				incomplete = true
				continue
			}
			if number != next.PullRequest {
				next.PullRequest = number
				change.PullRequest = &boardDiffPullRequest{Number: number}
			}
		}
	}
	next.State = string(row.Task.State)
	decision := ""
	if row.NextAction != nil && row.NextAction.Judgment {
		decision = row.NextAction.Label
	}
	if decision != previous.Decision {
		if decision != "" {
			change.Decision = &boardDiffDecision{Opened: true, Label: decision}
		} else {
			change.Decision = &boardDiffDecision{Cleared: true, Label: previous.Decision}
		}
	}
	next.Decision = decision
	change.Terminal = next.State == "merged" || next.State == "closed"
	return change, next, incomplete
}

// diffBoardActivities reads history only for changed overlap rows. Incomplete
// tasks are returned separately so the caller can avoid advancing their marker.
func (c *client) diffBoardActivities(state boardDiffState, feed boardDiffFeed) ([]boardDiffChange, boardDiffState, []boardDiffIncomplete, error) {
	changes := []boardDiffChange{}
	incomplete := []boardDiffIncomplete{}
	next := state
	next.Tasks = make(map[string]boardDiffTask, len(state.Tasks))
	for id, task := range state.Tasks {
		next.Tasks[id] = task
	}
	next.Recent = make(map[string]time.Time)
	for _, row := range feed.Items {
		id := row.Task.ID
		old, exists := state.Tasks[id]
		seen, wasRecent := state.Recent[id]
		decision := ""
		if row.NextAction != nil && row.NextAction.Judgment {
			decision = row.NextAction.Label
		}
		terminal := row.Task.State == "merged" || row.Task.State == "closed"
		if wasRecent && seen.Equal(row.LastEventAt) && ((!exists && terminal) || (old.State == string(row.Task.State) && old.Decision == decision)) {
			next.Recent[id] = seen
			continue
		}
		cutoff := state.Position.Add(-2 * time.Minute)
		if wasRecent {
			cutoff = seen
		}
		history, err := c.boardDiffHistory(id, cutoff, old.EnteredAt == nil)
		if err != nil {
			return nil, state, nil, err
		}
		if history.NotVisible {
			incomplete = append(incomplete, boardDiffIncomplete{ID: id, Reason: "task no longer visible"})
			if wasRecent {
				next.Recent[id] = seen
			}
			continue
		}
		change, task, partial := reduceBoardDiff(row, old, exists, cutoff, history)
		if change.Created || change.Terminal || len(change.Transitions) > 0 || change.Decision != nil || change.PullRequest != nil {
			changes = append(changes, change)
		}
		if partial {
			incomplete = append(incomplete, boardDiffIncomplete{ID: id, Reason: "history incomplete"})
			if wasRecent {
				next.Recent[id] = seen
			}
			continue
		}
		next.Recent[id] = row.LastEventAt
		if change.Terminal {
			delete(next.Tasks, id)
		} else {
			next.Tasks[id] = task
		}
	}
	for id, at := range next.Recent {
		if at.Before(feed.Position.Add(-2 * time.Minute)) {
			delete(next.Recent, id)
		}
	}
	next.Position = feed.Position
	return changes, next, incomplete, nil
}
