package singlestore

// Crate briefing storage on SingleStore (feature-crate-insights CI-2;
// component-crates invariant 29). The commands touch only crate_briefings
// and append no event (DEC-142(2)).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

const crateBriefingColumns = `workspace_id,crate_id,fingerprint,headline,account,look_first,model,tokens_in,tokens_out,generated_by,generated_at`

func scanCrateBriefing(row interface{ Scan(...any) error }) (store.CrateBriefing, error) {
	var briefing store.CrateBriefing
	var entries string
	if err := row.Scan(&briefing.WorkspaceID, &briefing.CrateID, &briefing.Fingerprint, &briefing.Headline, &briefing.Account, &entries, &briefing.Model, &briefing.TokensIn, &briefing.TokensOut, &briefing.GeneratedBy, &briefing.GeneratedAt); err != nil {
		return store.CrateBriefing{}, err
	}
	var lookFirst []store.CrateBriefingEntry
	if err := json.Unmarshal([]byte(entries), &lookFirst); err != nil {
		return store.CrateBriefing{}, fmt.Errorf("decode crate briefing %s look_first: %w", briefing.CrateID, err)
	}
	briefing.LookFirst = store.CloneCrateBriefingEntries(lookFirst)
	briefing.GeneratedAt = briefing.GeneratedAt.UTC()
	return briefing, nil
}

// GetCrateBriefing implements store.CrateStore: the bound workspace's stored
// briefing of the crate, or ErrNotFound.
func (s *Store) GetCrateBriefing(ctx context.Context, crateID string) (store.CrateBriefing, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetCrateBriefing")
	ws, err := workspace(ctx)
	if err != nil {
		return store.CrateBriefing{}, err
	}
	briefing, err := scanCrateBriefing(documentRow(ctx, s.db, `SELECT `+crateBriefingColumns+` FROM crate_briefings WHERE workspace_id=? AND crate_id=?`, ws, crateID))
	if err != nil {
		return store.CrateBriefing{}, notFound(err, "crate briefing %s", crateID)
	}
	return briefing, nil
}

// RecordCrateBriefing implements store.CrateStore. It locks the briefing's
// own key rather than a conveyor_locks row: a lock row on another partition
// can be released before a predecessor's briefing row is visible
// (component-requirement-insights invariant 14, through component-crates
// invariant 29).
//
// One transaction first inserts the row. Its primary key decides the race
// for an absent row: a concurrent insert of the same key waits on the
// uncommitted row, so success means this caller created the row and holds
// its lock. A duplicate key rolls back only that statement and proves the
// row exists. The same transaction then selects it FOR UPDATE, compares
// fingerprints on the locked row, and replaces it or refuses with
// ErrCrateBriefingCurrent, rolling back with nothing written. Affected-row
// counts are never read.
func (s *Store) RecordCrateBriefing(ctx context.Context, briefing store.CrateBriefing) (store.CrateBriefing, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordCrateBriefing")
	if err := store.ValidateCrateBriefing(briefing); err != nil {
		return store.CrateBriefing{}, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.CrateBriefing{}, err
	}
	briefing.WorkspaceID = ws
	briefing.LookFirst = store.CloneCrateBriefingEntries(briefing.LookFirst)
	briefing.GeneratedAt = time.Now().UTC().Truncate(time.Microsecond)
	entries, err := json.Marshal(briefing.LookFirst)
	if err != nil {
		return store.CrateBriefing{}, fmt.Errorf("encode crate briefing %s look_first: %w", briefing.CrateID, err)
	}
	values := []any{briefing.Fingerprint, briefing.Headline, briefing.Account, string(entries), briefing.Model, briefing.TokensIn, briefing.TokensOut, briefing.GeneratedBy, briefing.GeneratedAt}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO crate_briefings (`+crateBriefingColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, append([]any{ws, briefing.CrateID}, values...)...)
		var driver *mysql.MySQLError
		if err == nil || !errors.As(err, &driver) || driver.Number != 1062 {
			return err
		}
		current, err := scanCrateBriefing(tx.QueryRowContext(ctx, `SELECT `+crateBriefingColumns+` FROM crate_briefings WHERE workspace_id=? AND crate_id=? FOR UPDATE`, ws, briefing.CrateID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("crate briefing %s: duplicate key without a visible row", briefing.CrateID)
		}
		if err != nil {
			return err
		}
		if current.Fingerprint == briefing.Fingerprint {
			return store.ErrCrateBriefingCurrent
		}
		_, err = tx.ExecContext(ctx, `UPDATE crate_briefings SET fingerprint=?, headline=?, account=?, look_first=?, model=?, tokens_in=?, tokens_out=?, generated_by=?, generated_at=? WHERE workspace_id=? AND crate_id=?`, append(values, ws, briefing.CrateID)...)
		return err
	})
	if err != nil {
		return store.CrateBriefing{}, err
	}
	briefing.LookFirst = store.CloneCrateBriefingEntries(briefing.LookFirst)
	return briefing, nil
}
