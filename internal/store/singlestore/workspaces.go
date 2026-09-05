package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"gopkg.in/yaml.v3"
)

func (s *Store) ListWorkspaces(ctx context.Context) ([]core.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,config_version,created_at FROM workspaces ORDER BY LOWER(name),id`)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	defer rows.Close()
	var result []core.Workspace
	for rows.Next() {
		var w core.Workspace
		if err = rows.Scan(&w.ID, &w.Name, &w.ConfigVersion, &w.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, w)
	}
	return result, rows.Err()
}
func (s *Store) GetWorkspace(ctx context.Context, id string) (core.Workspace, error) {
	var w core.Workspace
	err := s.db.QueryRowContext(ctx, `SELECT id,name,config_version,created_at FROM workspaces WHERE id=?`, id).Scan(&w.ID, &w.Name, &w.ConfigVersion, &w.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = store.ErrNotFound
	}
	return w, translateBackendConflict(err)
}
func upsertRepos(ctx context.Context, tx *sql.Tx, ws string, repos []config.Repo) error {
	for _, r := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO repos(workspace_id,name,url,github_slug,default_base) VALUES(?,?,?,?,?) ON DUPLICATE KEY UPDATE url=VALUES(url),github_slug=VALUES(github_slug),default_base=VALUES(default_base)`, ws, r.Name, r.URL, r.GitHub, r.Base); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) BootstrapWorkspaceConfig(ctx context.Context, cfg *config.Config) (bool, error) {
	if cfg == nil || cfg.Workspace == "" {
		return false, store.ErrWorkspaceRequired
	}
	data, err := config.MarshalPolicyDocument(cfg)
	if err != nil {
		return false, err
	}
	seeded := false
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockKey(ctx, tx, "workspace-registry"); err != nil {
			return err
		}
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT config_yaml FROM workspaces WHERE id=? FOR UPDATE`, cfg.Workspace).Scan(&existing)
		if err == nil {
			_, _, err = config.ParseStoredWorkspaceDocument([]byte(existing), cfg, "stored workspace config")
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err = checkWorkspaceName(ctx, tx, cfg.Workspace); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO workspaces(id,name,config_yaml) VALUES(?,?,?)`, cfg.Workspace, cfg.Workspace, string(data)); err != nil {
			return err
		}
		if err = upsertRepos(ctx, tx, cfg.Workspace, cfg.Repos); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) SELECT ?,id,'operator' FROM users ORDER BY created_at,id LIMIT 1`, cfg.Workspace)
		seeded = err == nil
		return err
	})
	return seeded && err == nil, err
}
func checkWorkspaceName(ctx context.Context, tx *sql.Tx, name string) error {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspaces WHERE LOWER(TRIM(name))=LOWER(TRIM(?))`, name).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return store.ErrWorkspaceConflict
	}
	return nil
}
func (s *Store) CreateWorkspace(ctx context.Context, id, name string, cfg *config.Config) (core.Workspace, error) {
	if id == "" || strings.TrimSpace(name) == "" || cfg == nil {
		return core.Workspace{}, fmt.Errorf("workspace id, name and configuration are required")
	}
	data, err := config.MarshalPolicyDocument(cfg)
	if err != nil {
		return core.Workspace{}, err
	}
	var w core.Workspace
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := lockKey(ctx, tx, "workspace-registry"); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspaces WHERE id=?`, id).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return store.ErrWorkspaceConflict
		}
		if err := checkWorkspaceName(ctx, tx, name); err != nil {
			return err
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces(id,name,config_yaml,created_at) VALUES(?,?,?,?)`, id, name, string(data), now); err != nil {
			return err
		}
		if err := upsertRepos(ctx, tx, id, cfg.Repos); err != nil {
			return err
		}
		if credential, ok := store.CredentialFromContext(ctx); ok {
			if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES(?,?,'operator')`, id, credential.OwnerUserID); err != nil {
				return err
			}
		}
		if _, err := appendWorkspaceEvent(ctx, tx, id, "workspace.created", map[string]any{"id": id, "name": name, "config_version": 1}); err != nil {
			return err
		}
		w = core.Workspace{ID: id, Name: name, ConfigVersion: 1, CreatedAt: now}
		return nil
	})
	return w, err
}
func (s *Store) configRow(ctx context.Context) (string, int64, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return "", 0, err
	}
	var data string
	var version int64
	err = s.db.QueryRowContext(ctx, `SELECT config_yaml,config_version FROM workspaces WHERE id=?`, ws).Scan(&data, &version)
	if errors.Is(err, sql.ErrNoRows) {
		err = store.ErrNotFound
	}
	return data, version, translateBackendConflict(err)
}
func (s *Store) WorkspaceConfig(ctx context.Context) (config.VersionedDocument, error) {
	data, version, err := s.configRow(ctx)
	if err != nil {
		return config.VersionedDocument{}, err
	}
	var doc config.WorkspaceDocument
	dec := yaml.NewDecoder(strings.NewReader(data))
	dec.KnownFields(true)
	if err = dec.Decode(&doc); err != nil {
		return config.VersionedDocument{}, err
	}
	if doc.Harnesses == nil {
		doc.Harnesses = []config.Harness{}
	}
	if doc.Repos == nil {
		doc.Repos = []config.Repo{}
	}
	return config.VersionedDocument{Document: doc, Version: version}, nil
}
func (s *Store) RuntimeConfig(ctx context.Context, deployment *config.Config) (*config.Config, error) {
	data, _, err := s.configRow(ctx)
	if err != nil {
		return nil, err
	}
	if deployment == nil {
		return nil, fmt.Errorf("deployment config is required")
	}
	base := *deployment
	base.Workspace, _ = workspace(ctx)
	cfg, _, err := config.ParseStoredWorkspaceDocument([]byte(data), &base, "stored workspace config")
	return cfg, err
}
func (s *Store) UpdateWorkspaceConfig(ctx context.Context, expected int64, next *config.Config) (config.UpdateReceipt, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return config.UpdateReceipt{}, err
	}
	if next == nil {
		return config.UpdateReceipt{}, fmt.Errorf("configuration is required")
	}
	data, err := config.MarshalPolicyDocument(next)
	if err != nil {
		return config.UpdateReceipt{}, err
	}
	var receipt config.UpdateReceipt
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var previous string
		var version int64
		err := tx.QueryRowContext(ctx, `SELECT config_yaml,config_version FROM workspaces WHERE id=? FOR UPDATE`, ws).Scan(&previous, &version)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		if version != expected {
			return config.ErrVersionConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE workspaces SET config_yaml=?,config_version=config_version+1 WHERE id=?`, string(data), ws); err != nil {
			return err
		}
		if err = upsertRepos(ctx, tx, ws, next.Repos); err != nil {
			return err
		}
		var before config.WorkspaceDocument
		if err = yaml.Unmarshal([]byte(previous), &before); err != nil {
			return err
		}
		sections := configDiff(before, next.PolicyDocument())
		eventID, err := appendWorkspaceEvent(ctx, tx, ws, "config.updated", map[string]any{"from_version": version, "to_version": version + 1, "sections": sections})
		if err != nil {
			return err
		}
		receipt = config.UpdateReceipt{VersionedDocument: config.VersionedDocument{Document: next.PolicyDocument(), Version: version + 1}, EventID: eventID, ActorID: store.ActorFromContext(ctx).ID, Sections: sections}
		return nil
	})
	return receipt, err
}
func appendWorkspaceEvent(ctx context.Context, tx *sql.Tx, ws, kind string, payload map[string]any) (int64, error) {
	actor := store.ActorFromContext(ctx)
	result, err := writeRow(ctx, tx, rowWrite{table: "events", operation: "INSERT", values: map[string]any{"workspace_id": ws, "kind": kind, "actor_id": actor.ID, "actor_role": string(actor.Role), "payload_json": []byte(core.JSONPayload(payload)), "at": time.Now().UTC().Truncate(time.Microsecond)}})
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func configDiff(before, after config.WorkspaceDocument) []string {
	sections := make([]string, 0, 9)
	if before.Workspace != after.Workspace || before.MaxBounces != after.MaxBounces ||
		before.WorkOrderQueueTimeoutText != after.WorkOrderQueueTimeoutText {
		sections = append(sections, "workspace")
	}
	if !reflect.DeepEqual(before.Routing, after.Routing) {
		sections = append(sections, "routing")
	}
	if !reflect.DeepEqual(before.ExecutionSettings, after.ExecutionSettings) {
		sections = append(sections, "execution_settings")
	}
	if !reflect.DeepEqual(before.Repos, after.Repos) {
		sections = append(sections, "repos")
	}
	if !reflect.DeepEqual(before.Monitor, after.Monitor) {
		sections = append(sections, "monitor")
	}
	if !reflect.DeepEqual(before.Harnesses, after.Harnesses) {
		sections = append(sections, "harnesses")
	}
	if !reflect.DeepEqual(before.Review, after.Review) {
		sections = append(sections, "review")
	}
	if !reflect.DeepEqual(before.Setups, after.Setups) || before.DefaultSetup != after.DefaultSetup {
		sections = append(sections, "setups")
	}
	if !reflect.DeepEqual(before.Execution, after.Execution) {
		sections = append(sections, "execution")
	}
	return sections
}
