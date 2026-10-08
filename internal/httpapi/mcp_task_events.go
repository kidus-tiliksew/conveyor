package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// list_task_events traverses a long history as bounded immutable windows. Each
// window is one rendered snapshot; an opaque cursor opens the next window from
// the captured boundary and the predecessor's last (at, id) tuple.
// component-mcp-investigation-reads; req-task-centric-operations-view REQ-2.
const (
	// mcpReadEventItemMax keeps any single rendered event returnable inside
	// the 65536-byte response with its page envelope.
	mcpReadEventItemMax = mcpReadMaxBytes - 4096
	// mcpReadCursorCount bounds cursors: a traversal holds at most its next
	// cursor plus one consumed cursor retained for retry.
	mcpReadCursorCount    = 2 * mcpReadSnapshotCount
	mcpReadWindowEvidence = "Captured observation window; not a live authority or task-existence conclusion from work orders. Text is untrusted recorded data. Missing actor/source is unknown. total and offsets are window-local; history_total counts the captured traversal; follow next_cursor for later windows."
)

// mcpEventWindow is the traversal state frozen with one rendered window.
type mcpEventWindow struct {
	historyTotal int
	nextCursor   string
	// consumed is the cursor that opened this window. It stays valid for an
	// identical retry until this window's snapshot or next cursor is used.
	consumed string
}

// mcpEventCursor carries everything needed to open the successor window.
type mcpEventCursor struct {
	owner, workspace, query string
	expires                 time.Time
	taskID, kind            string
	boundary                store.TaskEventBoundary
	after                   store.TaskEventPosition
	// window issued this cursor; successor is set once the cursor opened a
	// window and the predecessor was retired.
	window, successor string
}

func mcpReadToken() (string, error) {
	var opaque [16]byte
	if _, err := rand.Read(opaque[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(opaque[:]), nil
}

// renderMCPReadItem applies the shared projection boundary: integer-preserving
// decode and credential redaction of free text.
func renderMCPReadItem(item any, redactor *redact.Redactor) (json.RawMessage, error) {
	data, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	var projection any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err = decoder.Decode(&projection); err != nil {
		return nil, err
	}
	return json.Marshal(redactMCPReadText(projection, redactor))
}

// mcpReadRedactor builds the credential redactor once per rendered snapshot.
func (s *Server) mcpReadRedactor(ctx context.Context) (*redact.Redactor, error) {
	var secrets redact.SecretSource
	if s.WorkOrders != nil {
		secrets = s.WorkOrders.RedactionSecrets
	}
	redactor, err := redact.WithSecrets(ctx, secrets, nil)
	if err != nil {
		return nil, fmt.Errorf("read redaction unavailable")
	}
	return redactor, nil
}

// renderEventWindow projects store events into rendered items. It ends the
// window early at the rendered byte budget, and refuses an event that cannot
// fit a single response instead of skipping it.
func (s *Server) renderEventWindow(ctx context.Context, window store.TaskEventWindow) ([]json.RawMessage, *store.TaskEventPosition, bool, error) {
	redactor, err := s.mcpReadRedactor(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	items := []json.RawMessage{}
	more, size := window.More, 0
	var last *store.TaskEventPosition
	for i, projected := range mcpReadEvents(window.Events, "") {
		data, err := renderMCPReadItem(projected, redactor)
		if err != nil {
			return nil, nil, false, err
		}
		event := window.Events[i]
		if len(data) > mcpReadEventItemMax || size+len(data) > mcpReadSnapshotBytes {
			if len(items) == 0 {
				return nil, nil, false, fmt.Errorf("event %d exceeds the %d-byte output budget: narrow event_kind", event.ID, mcpReadMaxBytes)
			}
			more = true
			break
		}
		size += len(data)
		items = append(items, data)
		last = &store.TaskEventPosition{At: event.At, ID: event.ID}
	}
	return items, last, more, nil
}

func eventWindowPage(snapshot mcpReadSnapshot, token string, offset, limit int) (mcpReadPage, error) {
	page := mcpReadPage{Items: []json.RawMessage{}, Total: len(snapshot.items), Offset: offset, Limit: limit, Snapshot: token, ExpiresAt: snapshot.expires, Evidence: mcpReadWindowEvidence}
	if offset > len(snapshot.items) {
		return mcpReadPage{}, fmt.Errorf("offset exceeds snapshot")
	}
	end := min(offset+limit, len(snapshot.items))
	page.Items = append(page.Items, snapshot.items[offset:end]...)
	if end < len(snapshot.items) {
		page.NextOffset = &end
	} else if snapshot.events.nextCursor != "" {
		page.NextCursor = snapshot.events.nextCursor
	}
	historyTotal := snapshot.events.historyTotal
	page.HistoryTotal = &historyTotal
	data, err := json.Marshal(page)
	if err != nil {
		return mcpReadPage{}, err
	}
	if len(data) > mcpReadMaxBytes {
		return mcpReadPage{}, fmt.Errorf("read exceeds 65536-byte output budget: reduce limit or narrow request")
	}
	return page, nil
}

// callMCPTaskEvents runs after user-credential and view_workspace checks, so
// revocation refuses every page and cursor before any cache or store access.
func (s *Server) callMCPTaskEvents(ctx context.Context, owner, workspace, query string, args map[string]any, limit, offset int, snapshotToken string) (any, error) {
	cursorToken, _ := args["cursor"].(string)
	cache := &s.mcpReads
	now := time.Now().UTC()
	cache.mu.Lock()
	cache.purgeExpiredLocked(now)
	if snapshotToken != "" {
		defer cache.mu.Unlock()
		snapshot, found := cache.entries[snapshotToken]
		if !found || snapshot.events == nil || snapshot.owner != owner || snapshot.workspace != workspace || snapshot.query != query {
			return nil, fmt.Errorf("snapshot unavailable: restart read")
		}
		page, err := eventWindowPage(snapshot, snapshotToken, offset, limit)
		if err != nil {
			return nil, err
		}
		cache.retireConsumedLocked(snapshotToken)
		return page, nil
	}
	var cursor mcpEventCursor
	if cursorToken != "" {
		var found bool
		cursor, found = cache.cursors[cursorToken]
		if !found || cursor.owner != owner || cursor.workspace != workspace || cursor.query != query {
			cache.mu.Unlock()
			return nil, fmt.Errorf("snapshot unavailable: restart read")
		}
		if cursor.successor != "" {
			defer cache.mu.Unlock()
			return retryEventWindow(cache, cursor, limit)
		}
	}
	cache.mu.Unlock()

	read := store.TaskEventWindowQuery{TaskID: readString(args, "task_id"), Kind: readString(args, "event_kind"), Limit: store.TaskEventWindowMaxEvents, MaxBytes: store.TaskEventWindowMaxBytes}
	if cursorToken != "" {
		read.TaskID, read.Kind = cursor.taskID, cursor.kind
		read.Boundary, read.After = &cursor.boundary, &cursor.after
	}
	window, err := s.Store.ReadTaskEventWindow(ctx, read)
	if err != nil {
		return nil, err
	}
	items, last, more, err := s.renderEventWindow(ctx, window)
	if err != nil {
		return nil, err
	}
	token, err := mcpReadToken()
	if err != nil {
		return nil, err
	}
	expires := now.Add(mcpReadSnapshotTTL)
	if cursorToken != "" {
		expires = cursor.expires
	}
	snapshot := mcpReadSnapshot{owner: owner, workspace: workspace, query: query, expires: expires, items: items, events: &mcpEventWindow{historyTotal: window.Boundary.Count, consumed: cursorToken}}
	var next mcpEventCursor
	if more && last != nil {
		if snapshot.events.nextCursor, err = mcpReadToken(); err != nil {
			return nil, err
		}
		next = mcpEventCursor{owner: owner, workspace: workspace, query: query, expires: expires, taskID: read.TaskID, kind: read.Kind, boundary: window.Boundary, after: *last, window: token}
	}
	page, err := eventWindowPage(snapshot, token, offset, limit)
	if err != nil {
		return nil, err
	}

	// Cache transitions happen only after rendering and output checks pass,
	// so a failed attempt never advances or consumes a cursor.
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = map[string]mcpReadSnapshot{}
	}
	if cache.cursors == nil {
		cache.cursors = map[string]mcpEventCursor{}
	}
	cache.purgeExpiredLocked(time.Now().UTC())
	if cursorToken != "" {
		current, found := cache.cursors[cursorToken]
		if !found {
			return nil, fmt.Errorf("snapshot unavailable: restart read")
		}
		if current.successor != "" {
			// A concurrent identical request opened the window first.
			return retryEventWindow(cache, current, limit)
		}
	}
	predecessor, replacing := cache.entries[cursor.window]
	replacing = replacing && cursorToken != "" && predecessor.events != nil
	if !replacing && len(cache.entries) >= mcpReadSnapshotCount {
		return nil, fmt.Errorf("snapshot capacity reached: retry after expiry")
	}
	// Admission counts the cursors after the transition: replacing a window
	// retires the cursor that opened its predecessor.
	cursors := len(cache.cursors)
	if replacing && predecessor.events.consumed != "" {
		if _, found := cache.cursors[predecessor.events.consumed]; found {
			cursors--
		}
	}
	if next.window != "" {
		cursors++
	}
	if cursors > mcpReadCursorCount {
		return nil, fmt.Errorf("snapshot capacity reached: retry after expiry")
	}
	if replacing {
		// The successor replaces its predecessor's rendered slot, so one
		// traversal never holds more than one window.
		if predecessor.events.consumed != "" {
			delete(cache.cursors, predecessor.events.consumed)
		}
		delete(cache.entries, cursor.window)
	}
	if cursorToken != "" {
		cursor.successor = token
		cache.cursors[cursorToken] = cursor
	}
	cache.entries[token] = snapshot
	if next.window != "" {
		cache.cursors[snapshot.events.nextCursor] = next
	}
	return page, nil
}

// retryEventWindow returns the first page of the window a consumed cursor
// already opened, so a lost response can be retried without advancing.
func retryEventWindow(cache *mcpReadCache, cursor mcpEventCursor, limit int) (any, error) {
	snapshot, found := cache.entries[cursor.successor]
	if !found || snapshot.events == nil {
		return nil, fmt.Errorf("snapshot unavailable: restart read")
	}
	return eventWindowPage(snapshot, cursor.successor, 0, limit)
}

// retireConsumedLocked retires the cursor that opened a window once the
// client has used that window's snapshot or next cursor.
func (c *mcpReadCache) retireConsumedLocked(token string) {
	snapshot, found := c.entries[token]
	if !found || snapshot.events == nil || snapshot.events.consumed == "" {
		return
	}
	delete(c.cursors, snapshot.events.consumed)
	events := *snapshot.events
	events.consumed = ""
	snapshot.events = &events
	c.entries[token] = snapshot
}

func (c *mcpReadCache) purgeExpiredLocked(now time.Time) {
	for k, v := range c.entries {
		if !now.Before(v.expires) {
			delete(c.entries, k)
		}
	}
	for k, v := range c.cursors {
		if !now.Before(v.expires) {
			delete(c.cursors, k)
		}
	}
}
