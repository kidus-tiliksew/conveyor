// Package eventlog is the engine-neutral persistence contract for the log core:
// an append-only, per-workspace event log with expected-version appends, plus
// a snapshot store for projector state.
//
// Every entity Conveyor persists is one stream. A write is an append that
// names the stream version it expects; two writers racing on the same stream
// have exactly one lose with VersionConflictError. Read models are folds over
// streams and can be rebuilt from the log by any replica.
//
// Drivers implement Store. Nothing above a driver may use engine-specific
// features; the conformance suite in logtest is the contract's definition.
// Traceability: log-core migration plan (2026-09-04), phase 1. Supersedes the
// engine binding in DEC-6 once DEC-35 is confirmed.
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

// Event kinds the log core itself emits, as opposed to kinds mirrored from
// or shared with the legacy schema.
const (
	// SnapshotImportedKind carries an entity's legacy projection rows
	// verbatim. A projector folding a stream replaces its state with the
	// snapshot when it meets one, then folds later events on top.
	SnapshotImportedKind = "log.snapshot_imported"
	// GenesisCompletedKind marks a workspace stream after an import run that
	// wrote at least one history entry or snapshot.
	GenesisCompletedKind = "log.genesis_completed"
)

// StreamID identifies one entity's stream inside a workspace. The form is
// "<type>/<id>"; the constructors below are the only sanctioned shapes.
type StreamID string

// GenesisStream is the per-workspace bookkeeping stream for import runs. It
// is separate from entity streams so a marker never sits between an
// entity's snapshot and its head.
const GenesisStream StreamID = "log/genesis"

func TaskStream(id string) StreamID              { return StreamID("task/" + id) }
func WorkOrderStream(id string) StreamID         { return StreamID("work_order/" + id) }
func RequirementStream(id string) StreamID       { return StreamID("requirement/" + id) }
func DesignStream(id string) StreamID            { return StreamID("design/" + id) }
func DecisionStream(id string) StreamID          { return StreamID("decision/" + id) }
func ReferenceDocumentStream(id string) StreamID { return StreamID("reference_document/" + id) }
func PlanningSessionStream(id string) StreamID   { return StreamID("planning_session/" + id) }
func PlanningBundleStream(id string) StreamID    { return StreamID("planning_bundle/" + id) }
func WorkspaceStream(id string) StreamID         { return StreamID("workspace/" + id) }
func UserStream(id string) StreamID              { return StreamID("user/" + id) }
func WorkerStream(id string) StreamID            { return StreamID("worker/" + id) }

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
	// ExpectAny skips the version check. Reserved for mirroring legacy writes
	// that are serialized by other means; new code names the version.
	ExpectAny Version = ^Version(0)
)

// NewEvent is what a writer appends. Drivers stamp At when it is zero and
// normalise a nil Payload to an empty JSON object.
type NewEvent struct {
	Kind      string
	ActorID   string
	ActorRole string
	Payload   json.RawMessage
	At        time.Time
	// LegacyID carries the legacy events.id when a write is mirrored from the
	// pre-log schema. Zero for native appends.
	LegacyID int64
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
	LegacyID  int64
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
	ErrEmptyAppend     = errors.New("eventlog: append requires at least one event")
	ErrInvalidStream   = errors.New("eventlog: stream id must have the form <type>/<id>")
	ErrEmptyWorkspace  = errors.New("eventlog: workspace is required")
	ErrEmptyKind       = errors.New("eventlog: event kind is required")
	ErrSnapshotMissing = errors.New("eventlog: snapshot not found")
)

// Log is the append and read contract.
type Log interface {
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

// Snapshot is a projector's persisted state at a log position.
type Snapshot struct {
	Key      string
	Version  Version
	Position Position
	Blob     []byte
	At       time.Time
}

// Snapshots stores projector state so replay resumes from a position rather
// than from the first event.
type Snapshots interface {
	PutSnapshot(ctx context.Context, workspace string, snapshot Snapshot) error
	// GetSnapshot returns ErrSnapshotMissing when no snapshot exists for key.
	GetSnapshot(ctx context.Context, workspace, key string) (Snapshot, error)
}

// Store is what a driver provides.
type Store interface {
	Log
	Snapshots
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

// Normalize fills the defaults drivers apply before storing an event.
func Normalize(event NewEvent, now time.Time) NewEvent {
	if event.At.IsZero() {
		event.At = now
	}
	event.At = event.At.UTC()
	if len(event.Payload) == 0 {
		event.Payload = json.RawMessage(`{}`)
	}
	return event
}

// Router selects the driver that owns a workspace. Workspaces without an
// explicit binding use the default store, so a deployment can move one
// workspace to another engine while the rest stay put.
type Router struct {
	fallback    Store
	byWorkspace map[string]Store
}

func NewRouter(fallback Store) *Router {
	return &Router{fallback: fallback, byWorkspace: map[string]Store{}}
}

// Bind routes workspace to store. Rebinding replaces the previous store.
func (r *Router) Bind(workspace string, store Store) {
	r.byWorkspace[workspace] = store
}

// For returns the store owning workspace.
func (r *Router) For(workspace string) Store {
	if s, ok := r.byWorkspace[workspace]; ok {
		return s
	}
	return r.fallback
}

// Stores returns every distinct store the router can reach, fallback first.
func (r *Router) Stores() []Store {
	out := []Store{r.fallback}
	seen := map[Store]bool{r.fallback: true}
	for _, s := range r.byWorkspace {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
