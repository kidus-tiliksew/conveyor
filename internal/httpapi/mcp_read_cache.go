package httpapi

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Investigation-read capacity is isolated per authenticated credential and
// bounded per process. component-mcp-investigation-reads (Snapshots; Window
// responses and cursors); req-accounts-and-membership REQ-4/REQ-5.
//
// Quota and token ownership use the non-secret credential ID, never the
// bearer and never the owning user: two credentials of one user are isolated,
// and one credential's quota spans every workspace it reads.
const (
	// mcpReadCredentialSnapshots bounds one credential's retained snapshots,
	// including list_task_events windows.
	mcpReadCredentialSnapshots = 8
	// mcpReadCredentialCursors bounds one credential's event cursors,
	// including consumed cursors retained for retry.
	mcpReadCredentialCursors = 16
)

// mcpReadSnapshot freezes one rendered projection. Snapshots are observations
// at capture time, never current corpus authority.
type mcpReadSnapshot struct {
	// owner is the authenticated credential ID that captured the snapshot.
	owner, workspace, query string
	expires                 time.Time
	items                   []json.RawMessage
	// events is set only for list_task_events windows (component-mcp-investigation-reads).
	events *mcpEventWindow
	// access is the LRU sequence. It never extends expires.
	access uint64
}

// mcpReadCache is the process-local snapshot and cursor cache. Its zero value
// is ready for use.
type mcpReadCache struct {
	mu      sync.Mutex
	entries map[string]mcpReadSnapshot
	cursors map[string]mcpEventCursor
	// sequence is the monotonic access counter behind LRU eviction.
	sequence uint64
	// advancing protects windows whose next cursor is being advanced from
	// eviction by concurrent reads of the same credential.
	advancing map[string]int
	// clock and beforeAdmit are deterministic test seams. beforeAdmit runs
	// after rendering and output checks, immediately before the admission lock.
	clock       func() time.Time
	beforeAdmit func()
}

// mcpReadAdmission is one cache transition, applied entirely or not at all.
type mcpReadAdmission struct {
	owner    string
	token    string
	snapshot mcpReadSnapshot
	// replace and retire name the predecessor window and its consumed cursor
	// that an advancing traversal removes.
	replace, retire string
	// cursorToken/cursor record the advancing cursor with its successor.
	cursorToken string
	cursor      mcpEventCursor
	// nextToken/next add the new window's next cursor.
	nextToken string
	next      mcpEventCursor
}

func (c *mcpReadCache) now() time.Time {
	if c.clock != nil {
		return c.clock().UTC()
	}
	return time.Now().UTC()
}

func (c *mcpReadCache) ensureLocked() {
	if c.entries == nil {
		c.entries = map[string]mcpReadSnapshot{}
	}
	if c.cursors == nil {
		c.cursors = map[string]mcpEventCursor{}
	}
	if c.advancing == nil {
		c.advancing = map[string]int{}
	}
}

// lookup returns a live snapshot bound to the credential, workspace and query.
func (c *mcpReadCache) lookup(owner, workspace, query, token string) (mcpReadSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeExpiredLocked(c.now())
	snapshot, found := c.entries[token]
	if !found || snapshot.owner != owner || snapshot.workspace != workspace || snapshot.query != query {
		return mcpReadSnapshot{}, false
	}
	return snapshot, true
}

// touch records a successful page read of a retained snapshot.
func (c *mcpReadCache) touch(owner, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if snapshot, found := c.entries[token]; found && snapshot.owner == owner {
		c.touchLocked(token)
	}
}

func (c *mcpReadCache) touchLocked(token string) {
	snapshot, found := c.entries[token]
	if !found {
		return
	}
	c.sequence++
	snapshot.access = c.sequence
	c.entries[token] = snapshot
}

func (c *mcpReadCache) protectLocked(window string) {
	c.ensureLocked()
	c.advancing[window]++
}

func (c *mcpReadCache) unprotect(window string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.advancing[window] <= 1 {
		delete(c.advancing, window)
		return
	}
	c.advancing[window]--
}

// admit applies a non-traversal snapshot admission.
func (c *mcpReadCache) admit(a mcpReadAdmission) error {
	if c.beforeAdmit != nil {
		c.beforeAdmit()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLocked()
	c.purgeExpiredLocked(c.now())
	return c.admitLocked(a)
}

// admitLocked is the single admission/eviction transition. It projects the
// post-transition occupancy, selects the caller's own least-recently-used
// snapshots/traversals while the caller exceeds its quota, checks the process
// ceilings, and only then mutates the cache. A refusal leaves every snapshot
// and cursor unchanged. Another credential's state is never evicted.
func (c *mcpReadCache) admitLocked(a mcpReadAdmission) error {
	c.ensureLocked()
	removedEntries, removedCursors := map[string]bool{}, map[string]bool{}
	if _, found := c.entries[a.replace]; found && a.replace != "" {
		removedEntries[a.replace] = true
	}
	if _, found := c.cursors[a.retire]; found && a.retire != "" {
		removedCursors[a.retire] = true
	}
	// A traversal's cursors are linked to the window that issued them and the
	// window they opened; evicting the window removes both.
	linked := map[string][]string{}
	for token, cursor := range c.cursors {
		if cursor.owner != a.owner {
			continue
		}
		if cursor.window != "" {
			linked[cursor.window] = append(linked[cursor.window], token)
		}
		if cursor.successor != "" && cursor.successor != cursor.window {
			linked[cursor.successor] = append(linked[cursor.successor], token)
		}
	}
	liveLinked := func(window string) int {
		n := 0
		for _, token := range linked[window] {
			if !removedCursors[token] && token != a.cursorToken {
				n++
			}
		}
		return n
	}
	for {
		entries, cursors := 1, 0
		for token, snapshot := range c.entries {
			if snapshot.owner == a.owner && !removedEntries[token] {
				entries++
			}
		}
		for token, cursor := range c.cursors {
			if cursor.owner == a.owner && !removedCursors[token] {
				cursors++
			}
		}
		if a.nextToken != "" {
			cursors++
		}
		overEntries, overCursors := entries > mcpReadCredentialSnapshots, cursors > mcpReadCredentialCursors
		if !overEntries && !overCursors {
			break
		}
		victim, oldest := "", uint64(0)
		for token, snapshot := range c.entries {
			if snapshot.owner != a.owner || removedEntries[token] || c.advancing[token] > 0 {
				continue
			}
			if !overEntries && liveLinked(token) == 0 {
				continue
			}
			// Lowest access sequence first; the token breaks ties deterministically.
			if victim == "" || snapshot.access < oldest || snapshot.access == oldest && token < victim {
				victim, oldest = token, snapshot.access
			}
		}
		if victim == "" {
			if overEntries {
				return fmt.Errorf("snapshot capacity reached: credential limit %d retained snapshots are held by reads in progress; retry after they complete", mcpReadCredentialSnapshots)
			}
			return fmt.Errorf("event cursor capacity reached: credential limit %d retained cursors are held by reads in progress; retry after they complete", mcpReadCredentialCursors)
		}
		removedEntries[victim] = true
		for _, token := range linked[victim] {
			if token != a.cursorToken {
				removedCursors[token] = true
			}
		}
	}
	if len(c.entries)-len(removedEntries)+1 > mcpReadSnapshotCount {
		var earliest time.Time
		for _, snapshot := range c.entries {
			if earliest.IsZero() || snapshot.expires.Before(earliest) {
				earliest = snapshot.expires
			}
		}
		return fmt.Errorf("snapshot capacity reached: process limit %d retained snapshots; retry at %s after expiry (within 5 minutes)", mcpReadSnapshotCount, mcpReadRetryAt(earliest))
	}
	cursors := len(c.cursors) - len(removedCursors)
	if a.nextToken != "" {
		cursors++
	}
	if cursors > mcpReadCursorCount {
		var earliest time.Time
		for _, cursor := range c.cursors {
			if earliest.IsZero() || cursor.expires.Before(earliest) {
				earliest = cursor.expires
			}
		}
		return fmt.Errorf("event cursor capacity reached: process limit %d retained cursors; retry at %s after expiry (within 5 minutes)", mcpReadCursorCount, mcpReadRetryAt(earliest))
	}
	for token := range removedEntries {
		delete(c.entries, token)
	}
	for token := range removedCursors {
		delete(c.cursors, token)
	}
	if a.cursorToken != "" {
		c.cursors[a.cursorToken] = a.cursor
	}
	c.sequence++
	a.snapshot.access = c.sequence
	c.entries[a.token] = a.snapshot
	if a.nextToken != "" {
		c.cursors[a.nextToken] = a.next
	}
	return nil
}

// mcpReadRetryAt rounds an expiry up to the whole second so a retry at the
// stated RFC3339 time always follows the expiry.
func mcpReadRetryAt(expires time.Time) string {
	expires = expires.UTC()
	if expires.Nanosecond() != 0 {
		expires = expires.Truncate(time.Second).Add(time.Second)
	}
	return expires.Format(time.RFC3339)
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
