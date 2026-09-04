package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The state-carrying bridge.
//
// Legacy events do not carry enough to fold: requirement.version_proposed
// names the requirement but not the content, which lives in a version table.
// So during the transition every store transaction records, at commit, the
// final rows of each entity it emitted events for, as one
// log.state_recorded event per touched stream. The log is then authoritative
// for state from the first deploy, parity stays clean on live traffic by
// construction, and real fold rules are written for native events after
// cutover rather than for 138 legacy kinds before it.
//
// The mechanism is a pgx.Tx wrapper. The mirror registers each touched
// stream on the wrapper; Commit reads the rows exactly as the genesis import
// does, skips streams whose content hash is unchanged, appends the state
// events, and only then commits. A failure anywhere rolls the whole
// transaction back, rows and log together.
//
// Log-core migration plan, phase 2 (bridge), decided by the operator on
// 2026-09-04.

// stateTx is a pgx.Tx that flushes state events before committing.
type stateTx struct {
	pgx.Tx
	store   *Store
	mu      sync.Mutex
	touched map[touchedStream][]string // stream -> kinds that touched it
}

type touchedStream struct {
	workspace string
	stream    eventlog.StreamID
}

// begin opens a wrapped transaction on the pool.
func (s *Store) begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return s.wrapTx(tx), nil
}

// beginOn opens a wrapped transaction on a pinned connection.
func (s *Store) beginOn(ctx context.Context, conn *pgxpool.Conn) (pgx.Tx, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return s.wrapTx(tx), nil
}

func (s *Store) wrapTx(tx pgx.Tx) pgx.Tx {
	return &stateTx{Tx: tx, store: s, touched: map[touchedStream][]string{}}
}

// touch records that kind was mirrored onto stream inside this transaction.
func (t *stateTx) touch(workspace string, stream eventlog.StreamID, kind string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := touchedStream{workspace, stream}
	t.touched[key] = append(t.touched[key], kind)
}

// noteStateChange registers a stream whose rows a transaction changes
// without emitting a legacy event, so its state is still recorded at
// commit. reason names the write path and lands in trigger_kinds. It is a
// no-op on a transaction the store did not open.
func noteStateChange(tx pgx.Tx, workspace string, stream eventlog.StreamID, reason string) {
	if wrapped, ok := tx.(*stateTx); ok {
		wrapped.touch(workspace, stream, reason)
	}
}

// Commit flushes state events for every touched stream, then commits.
func (t *stateTx) Commit(ctx context.Context) error {
	if err := t.flush(ctx); err != nil {
		return err
	}
	return t.Tx.Commit(ctx)
}

func (t *stateTx) flush(ctx context.Context) error {
	t.mu.Lock()
	keys := make([]touchedStream, 0, len(t.touched))
	for key := range t.touched {
		keys = append(keys, key)
	}
	t.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].workspace != keys[j].workspace {
			return keys[i].workspace < keys[j].workspace
		}
		return keys[i].stream < keys[j].stream
	})
	for _, key := range keys {
		if err := t.store.recordState(ctx, t, key.workspace, key.stream, t.touched[key]); err != nil {
			return fmt.Errorf("record state for %s %s: %w", key.workspace, key.stream, err)
		}
	}
	return nil
}

// recordState appends one log.state_recorded event for stream unless the
// entity's rows hash to the same value as the stream's last state.
func (s *Store) recordState(ctx context.Context, tx pgx.Tx, workspace string, stream eventlog.StreamID, kinds []string) error {
	family, ok := genesisFamilyForStream(stream)
	if !ok {
		return nil
	}
	snapshot, err := s.readGenesisSnapshot(ctx, tx, workspace, family, stream.EntityID())
	if err != nil {
		return err
	}
	if snapshot == nil {
		// Deleted inside this transaction; the legacy events say so.
		return nil
	}
	head, err := s.logDriver().LockStream(ctx, tx, workspace, stream)
	if err != nil {
		return err
	}
	if head > 0 {
		last, err := s.logDriver().Read(pglogCtx(ctx, tx), workspace, stream, head-1, 1)
		if err != nil {
			return err
		}
		if len(last) == 1 && isStateKind(last[0].Kind) {
			var previous genesisSnapshot
			if json.Unmarshal(last[0].Payload, &previous) == nil && previous.ContentHash == snapshot.ContentHash {
				return nil
			}
		}
	}
	snapshot.TriggerKinds = dedupeKinds(kinds)
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	actor := store.ActorFromContext(ctx)
	_, err = s.logDriver().AppendWith(ctx, tx, workspace, stream, head, []eventlog.NewEvent{{
		Kind: eventlog.StateRecordedKind, ActorID: actor.ID, ActorRole: string(actor.Role), Payload: payload,
	}})
	return err
}

// isStateKind reports whether kind carries a full entity state.
func isStateKind(kind string) bool {
	return kind == eventlog.SnapshotImportedKind || kind == eventlog.StateRecordedKind
}

func dedupeKinds(kinds []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		if !seen[kind] {
			seen[kind] = true
			out = append(out, kind)
		}
	}
	sort.Strings(out)
	return out
}

// genesisFamilyForStream maps a stream type to the family whose rows
// describe it. Streams with no family (log/genesis) carry no state.
func genesisFamilyForStream(stream eventlog.StreamID) (genesisFamily, bool) {
	typ := stream.Type()
	for _, family := range genesisFamilies {
		if family.stream("x").Type() == typ {
			return family, true
		}
	}
	for _, family := range genesisDeploymentFamilies {
		if family.stream("x").Type() == typ {
			return family, true
		}
	}
	return genesisFamily{}, false
}
