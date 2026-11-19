package postgres

// Crate briefing storage on PostgreSQL (feature-crate-insights CI-2;
// component-crates invariant 29). The commands touch only crate_briefings
// and append no event (DEC-142(2)).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func crateBriefingFromDB(row db.CrateBriefing) (store.CrateBriefing, error) {
	var entries []store.CrateBriefingEntry
	if err := json.Unmarshal(row.LookFirst, &entries); err != nil {
		return store.CrateBriefing{}, fmt.Errorf("decode crate briefing %s look_first: %w", row.CrateID, err)
	}
	return store.CrateBriefing{
		WorkspaceID: row.WorkspaceID, CrateID: row.CrateID, Fingerprint: row.Fingerprint,
		Headline: row.Headline, Account: row.Account, LookFirst: store.CloneCrateBriefingEntries(entries),
		Model: row.Model, TokensIn: row.TokensIn, TokensOut: row.TokensOut,
		GeneratedBy: row.GeneratedBy, GeneratedAt: row.GeneratedAt.Time.UTC(),
	}, nil
}

// GetCrateBriefing implements store.CrateStore: the bound workspace's stored
// briefing of the crate, or ErrNotFound.
func (s *Store) GetCrateBriefing(ctx context.Context, crateID string) (store.CrateBriefing, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetCrateBriefing")
	row, err := s.queries.GetCrateBriefing(ctx, db.GetCrateBriefingParams{WorkspaceID: workspace(ctx), CrateID: crateID})
	if err != nil {
		return store.CrateBriefing{}, notFound(err, "crate briefing %s", crateID)
	}
	return crateBriefingFromDB(row)
}

// RecordCrateBriefing implements store.CrateStore with one upsert whose
// fingerprint predicate is the compare-and-write. Concurrent inserts of one
// key wait on the uncommitted row and then evaluate the predicate against
// it, so concurrent writers of one fingerprint leave one winner. A stored row
// with an equal fingerprint is left unchanged and the statement returns no
// row, which is ErrCrateBriefingCurrent. A failed statement rolls back and
// leaves the previous row (component-crates invariant 29).
func (s *Store) RecordCrateBriefing(ctx context.Context, briefing store.CrateBriefing) (store.CrateBriefing, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordCrateBriefing")
	if err := store.ValidateCrateBriefing(briefing); err != nil {
		return store.CrateBriefing{}, err
	}
	workspaceID := workspace(ctx)
	if workspaceID == "" {
		return store.CrateBriefing{}, store.ErrWorkspaceRequired
	}
	entries, err := json.Marshal(store.CloneCrateBriefingEntries(briefing.LookFirst))
	if err != nil {
		return store.CrateBriefing{}, fmt.Errorf("encode crate briefing %s look_first: %w", briefing.CrateID, err)
	}
	var stored store.CrateBriefing
	err = s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		row, err := q.UpsertCrateBriefing(ctx, db.UpsertCrateBriefingParams{
			WorkspaceID: workspaceID, CrateID: briefing.CrateID, Fingerprint: briefing.Fingerprint,
			Headline: briefing.Headline, Account: briefing.Account, LookFirst: entries, Model: briefing.Model,
			TokensIn: briefing.TokensIn, TokensOut: briefing.TokensOut, GeneratedBy: briefing.GeneratedBy,
			GeneratedAt: timestamp(time.Now().UTC().Truncate(time.Microsecond)),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrCrateBriefingCurrent
		}
		if err != nil {
			return err
		}
		stored, err = crateBriefingFromDB(row)
		return err
	})
	if err != nil {
		return store.CrateBriefing{}, err
	}
	return stored, nil
}
