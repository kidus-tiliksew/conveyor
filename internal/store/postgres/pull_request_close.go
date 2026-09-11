package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) QueuePullRequestClose(ctx context.Context, p core.PullRequestClose) error {
	if err := store.ValidatePullRequestCloseActor(ctx); err != nil {
		return err
	}
	task, err := s.GetTask(ctx, p.TaskID)
	if err != nil {
		return err
	}
	if err = store.ValidatePullRequestCloseIntent(p, task); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		tag, err := tx.Exec(ctx, `INSERT INTO pull_request_closes(workspace_id,task_id,payload_json) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, workspace(ctx), p.TaskID, core.JSONPayload(p))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if err = insertEvent(ctx, q, store.PullRequestCloseEvent(p)); err != nil {
				return err
			}
		}
		var raw []byte
		if err = tx.QueryRow(ctx, `SELECT payload_json FROM pull_request_closes WHERE workspace_id=$1 AND task_id=$2 FOR UPDATE`, workspace(ctx), p.TaskID).Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if p.Terminal() {
			return nil
		}
		_, err = logqueue.Enqueue(pglog.WithTx(ctx, tx), s.queue.log, workspace(ctx), queue.PullRequestCloseArgs{}.Kind(), p.TaskID, queue.PullRequestCloseArgs{WorkspaceID: workspace(ctx), TaskID: p.TaskID}, core.PullRequestCloseMaxAttempts, time.Now().UTC())
		return err
	})
}
func (s *Store) GetPullRequestClose(ctx context.Context, id string) (core.PullRequestClose, bool, error) {
	var raw []byte
	var p core.PullRequestClose
	err := s.pool.QueryRow(ctx, `SELECT payload_json FROM pull_request_closes WHERE workspace_id=$1 AND task_id=$2`, workspace(ctx), id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	err = json.Unmarshal(raw, &p)
	return p, err == nil, err
}
func (s *Store) UpdatePullRequestClose(ctx context.Context, p core.PullRequestClose) error {
	if err := store.ValidatePullRequestCloseActor(ctx); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var raw []byte
		var old core.PullRequestClose
		if err := tx.QueryRow(ctx, `SELECT payload_json FROM pull_request_closes WHERE workspace_id=$1 AND task_id=$2 FOR UPDATE`, workspace(ctx), p.TaskID).Scan(&raw); err != nil {
			return notFound(err, "pull request close %s", p.TaskID)
		}
		if err := json.Unmarshal(raw, &old); err != nil {
			return err
		}
		if err := store.ValidatePullRequestCloseUpdate(old, p); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE pull_request_closes SET payload_json=$3 WHERE workspace_id=$1 AND task_id=$2`, workspace(ctx), p.TaskID, core.JSONPayload(p)); err != nil {
			return err
		}
		return insertEvent(ctx, q, store.PullRequestCloseEvent(p))
	})
}
func (s *Store) hydratePullRequestCloses(ctx context.Context, tasks []core.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
	}
	rows, err := s.pool.Query(ctx, `SELECT payload_json FROM pull_request_closes WHERE workspace_id=$1 AND task_id=ANY($2::text[])`, workspace(ctx), ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := map[string]core.PullRequestClose{}
	for rows.Next() {
		var raw []byte
		var p core.PullRequestClose
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &p); err != nil {
			return err
		}
		byID[p.TaskID] = p
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range tasks {
		if p, ok := byID[tasks[i].ID]; ok {
			tasks[i].PullRequestClose = &p
			tasks[i].PullRequestCloseState = p.State
		}
	}
	return nil
}
