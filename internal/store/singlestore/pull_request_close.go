package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
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
	return s.taskTx(ctx, p.TaskID, func(tx *sql.Tx) error {
		var raw []byte
		err := documentRow(ctx, tx, `SELECT payload_json FROM pull_request_closes WHERE workspace_id=? AND task_id=? FOR UPDATE`, documentWorkspace(ctx), p.TaskID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err = documentExec(ctx, tx, `INSERT INTO pull_request_closes(workspace_id,task_id,payload_json) VALUES(?,?,?)`, documentWorkspace(ctx), p.TaskID, core.JSONPayload(p)); err != nil {
				return err
			}
			if err = taskEvent(ctx, tx, store.PullRequestCloseEvent(p)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if err = json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if p.Terminal() {
			return nil
		}
		_, err = logqueue.Enqueue(s2log.WithTx(ctx, tx), s.log, documentWorkspace(ctx), queue.PullRequestCloseArgs{}.Kind(), p.TaskID, queue.PullRequestCloseArgs{WorkspaceID: documentWorkspace(ctx), TaskID: p.TaskID}, core.PullRequestCloseMaxAttempts, time.Now().UTC())
		return err
	})
}
func (s *Store) GetPullRequestClose(ctx context.Context, id string) (core.PullRequestClose, bool, error) {
	var raw []byte
	var p core.PullRequestClose
	err := documentRow(ctx, s.db, `SELECT payload_json FROM pull_request_closes WHERE workspace_id=? AND task_id=?`, documentWorkspace(ctx), id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
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
	return s.taskTx(ctx, p.TaskID, func(tx *sql.Tx) error {
		var raw []byte
		var old core.PullRequestClose
		if err := documentRow(ctx, tx, `SELECT payload_json FROM pull_request_closes WHERE workspace_id=? AND task_id=? FOR UPDATE`, documentWorkspace(ctx), p.TaskID).Scan(&raw); err != nil {
			return notFound(err, "pull request close %s", p.TaskID)
		}
		if err := json.Unmarshal(raw, &old); err != nil {
			return err
		}
		if err := store.ValidatePullRequestCloseUpdate(old, p); err != nil {
			return err
		}
		if _, err := documentExec(ctx, tx, `UPDATE pull_request_closes SET payload_json=? WHERE workspace_id=? AND task_id=?`, core.JSONPayload(p), documentWorkspace(ctx), p.TaskID); err != nil {
			return err
		}
		return taskEvent(ctx, tx, store.PullRequestCloseEvent(p))
	})
}
