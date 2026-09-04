// Package eventlog is the engine-neutral contract for the log: an
// append-only, per-workspace event log with expected-version appends.
//
// A stream is a sequence of events inside a workspace. A write is an append
// that names the stream version it expects; two writers racing on the same
// stream have exactly one lose with VersionConflictError. Readers fold
// streams and can rebuild their state from the log.
//
// Drivers implement Store. Nothing above a driver may use engine-specific
// features; the conformance suite in logtest is the contract's definition.
// Traceability: docs/log-core.md.
package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DeploymentWorkspace is the reserved workspace id for streams that belong to
// the deployment rather than to any workspace: users and the organization.
const DeploymentWorkspace = "_deployment"

// StreamID identifies one stream inside a workspace. The form is
// "<type>/<id>"; the queue builds its ids with logqueue.StreamFor.
type StreamID string

// Type returns the stream's entity type, the part before the slash.
func (s StreamID) Type() string {
	if i := strings.IndexByte(string(s), '/'); i >= 0 {
		return string(s[:i])
	}
	return string(s)
}

// EntityID returns the stream's entity id, the part after the slash.
func (s StreamID) EntityID() string {
	if i := strings.IndexByte(string(s), '/'); i >= 0 {
		return string(s[i+1:])
	}
	return ""
}

// Valid reports whether the stream id has the "<type>/<id>" shape with both
// parts non-empty.
func (s StreamID) Valid() bool {
	return s.Type() != "" && s.EntityID() != "" && !strings.ContainsAny(string(s), " \t\n")
}

// Version is a per-stream, 1-based sequence number. Zero means "no events".
type Version uint64

// Position is the driver-wide append order inside one workspace. Positions
// are strictly increasing per workspace but need not be contiguous.
type Position int64

const (
	// ExpectNew requires the stream to have no events yet.
	ExpectNew Version = 0
	// ExpectAny skips the version check, for writers serialized by other
	// means; new code names the version.
	ExpectAny Version = ^Version(0)
)

// NewEvent is what a writer appends. Drivers stamp At when it is zero,
// keep it to microseconds, and normalise a nil Payload to an empty JSON
// object.
type NewEvent struct {
	Kind      string
	ActorID   string
	ActorRole string
	Payload   json.RawMessage
	At        time.Time
}

// Event is one stored entry.
type Event struct {
	Workspace string
	Stream    StreamID
	Version   Version
	Position  Position
	Kind      string
	ActorID   string
	ActorRole string
	Payload   json.RawMessage
	At        time.Time
}

// VersionConflictError reports an append whose expected version did not match
// the stream head. Actual is the head the driver observed.
type VersionConflictError struct {
	Workspace string
	Stream    StreamID
	Expected  Version
	Actual    Version
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("eventlog: %s %s expected version %d, head is %d", e.Workspace, e.Stream, e.Expected, e.Actual)
}

// IsVersionConflict reports whether err is, or wraps, a VersionConflictError.
func IsVersionConflict(err error) bool {
	var conflict *VersionConflictError
	return errors.As(err, &conflict)
}

var (
	ErrEmptyAppend    = errors.New("eventlog: append requires at least one event")
	ErrInvalidStream  = errors.New("eventlog: stream id must have the form <type>/<id>")
	ErrEmptyWorkspace = errors.New("eventlog: workspace is required")
	ErrEmptyKind      = errors.New("eventlog: event kind is required")
)

// Store is what a driver provides: the append and read contract.
type Store interface {
	// Append writes events to one stream atomically and returns the new head.
	// expected is ExpectNew, ExpectAny, or the exact head the writer observed.
	Append(ctx context.Context, workspace string, stream StreamID, expected Version, events []NewEvent) (Version, error)
	// Read returns a stream's events with version > after, ascending. A limit
	// of zero means no limit.
	Read(ctx context.Context, workspace string, stream StreamID, after Version, limit int) ([]Event, error)
	// Head returns the stream's current version, zero when the stream is empty.
	Head(ctx context.Context, workspace string, stream StreamID) (Version, error)
	// Tail returns a workspace's events with position > after, ascending by
	// position. A limit of zero means no limit.
	Tail(ctx context.Context, workspace string, after Position, limit int) ([]Event, error)
}

// ValidateAppend checks the arguments every driver must reject identically.
func ValidateAppend(workspace string, stream StreamID, events []NewEvent) error {
	if strings.TrimSpace(workspace) == "" {
		return ErrEmptyWorkspace
	}
	if !stream.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidStream, stream)
	}
	if len(events) == 0 {
		return ErrEmptyAppend
	}
	for i := range events {
		if strings.TrimSpace(events[i].Kind) == "" {
			return fmt.Errorf("%w (event %d)", ErrEmptyKind, i)
		}
	}
	return nil
}

// Normalize fills the defaults drivers apply before storing an event. At is
// kept to microseconds, the precision every supported engine stores, so a
// reader sees the same instant the writer compared against.
func Normalize(event NewEvent, now time.Time) NewEvent {
	if event.At.IsZero() {
		event.At = now
	}
	event.At = event.At.UTC().Truncate(time.Microsecond)
	if len(event.Payload) == 0 {
		event.Payload = json.RawMessage(`{}`)
	}
	return event
}
