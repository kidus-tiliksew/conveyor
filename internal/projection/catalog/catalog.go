// Package catalog is the first read model over the log: for every stream it
// keeps the latest imported snapshot (the entity's legacy projection rows)
// and the events that arrived after it.
//
// During the shadow phases this is the discovery engine for fold rules. An
// entity whose live rows differ from its last snapshot has, by definition,
// been changed by the events listed in Since; those kinds are the ones a
// real projector must learn to fold. The parity checker reports exactly
// that. Log-core migration plan, phase 2.
package catalog

import (
	"encoding/json"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
)

// Snapshot is the decoded payload of a log.snapshot_imported event.
type Snapshot struct {
	Family      string                      `json:"family"`
	Table       string                      `json:"table"`
	ID          string                      `json:"id"`
	Row         map[string]any              `json:"row"`
	Children    map[string][]map[string]any `json:"children,omitempty"`
	ContentHash string                      `json:"content_hash"`
	Version     eventlog.Version            `json:"version"`
	Position    eventlog.Position           `json:"position"`
}

// EventRef is what the catalog remembers about an event after a snapshot.
type EventRef struct {
	Kind     string            `json:"kind"`
	Version  eventlog.Version  `json:"version"`
	Position eventlog.Position `json:"position"`
	LegacyID int64             `json:"legacy_id,omitempty"`
}

// Entity is one stream's catalog entry.
type Entity struct {
	Stream   eventlog.StreamID `json:"stream"`
	Head     eventlog.Version  `json:"head"`
	Snapshot *Snapshot         `json:"snapshot,omitempty"`
	// Since lists events after the snapshot, or every event when the stream
	// has no snapshot yet.
	Since []EventRef `json:"since,omitempty"`
}

// Catalog is the projector.
type Catalog struct {
	entities map[eventlog.StreamID]*Entity
}

func New() *Catalog {
	return &Catalog{entities: map[eventlog.StreamID]*Entity{}}
}

func (c *Catalog) Name() string { return "catalog" }
func (c *Catalog) Version() int { return 1 }
func (c *Catalog) Reset()       { c.entities = map[eventlog.StreamID]*Entity{} }

func (c *Catalog) Apply(event eventlog.Event) error {
	entity, ok := c.entities[event.Stream]
	if !ok {
		entity = &Entity{Stream: event.Stream}
		c.entities[event.Stream] = entity
	}
	entity.Head = event.Version
	if event.Kind == eventlog.SnapshotImportedKind {
		var snapshot Snapshot
		if err := json.Unmarshal(event.Payload, &snapshot); err != nil {
			return err
		}
		snapshot.Version = event.Version
		snapshot.Position = event.Position
		entity.Snapshot = &snapshot
		entity.Since = nil
		return nil
	}
	entity.Since = append(entity.Since, EventRef{Kind: event.Kind, Version: event.Version, Position: event.Position, LegacyID: event.LegacyID})
	return nil
}

func (c *Catalog) MarshalState() ([]byte, error) {
	return json.Marshal(c.entities)
}

func (c *Catalog) UnmarshalState(data []byte) error {
	entities := map[eventlog.StreamID]*Entity{}
	if err := json.Unmarshal(data, &entities); err != nil {
		return err
	}
	c.entities = entities
	return nil
}

// Entity returns one stream's entry, or nil.
func (c *Catalog) Entity(stream eventlog.StreamID) *Entity {
	return c.entities[stream]
}

// Len is the number of streams seen.
func (c *Catalog) Len() int { return len(c.entities) }

// Streams returns the streams of one type, sorted.
func (c *Catalog) Streams(streamType string) []eventlog.StreamID {
	var out []eventlog.StreamID
	for stream := range c.entities {
		if streamType == "" || stream.Type() == streamType {
			out = append(out, stream)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Stale returns entities with events after their snapshot, or with no
// snapshot at all, sorted by stream.
func (c *Catalog) Stale() []*Entity {
	var out []*Entity
	for _, entity := range c.entities {
		if entity.Snapshot == nil || len(entity.Since) > 0 {
			out = append(out, entity)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stream < out[j].Stream })
	return out
}
