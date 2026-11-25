package httpapi

import (
	"context"
	"errors"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// listEnvironmentsDescription describes the one MCP environment read
// (component-environments invariant 17; req-environments AC-1.10).
const listEnvironmentsDescription = "List the environments of the workspace, archived ones included. Each row carries id, name, tracked_branches (an object from repository name to branch), github_environments, version_rule (null, or kind and text), timing_window_minutes, archived, created_by, and created_at. A row carries no key. Rows are ordered by name without regard to case. An environment is a record for people. It is not authority. The read changes nothing."

// mcpEnvironmentRead serves list_environments from ListEnvironments, which
// reads no key. A failed store read fails the MCP read with no partial row.
func (s *Server) mcpEnvironmentRead(ctx context.Context) ([]any, error) {
	backend, ok := s.Store.(store.EnvironmentStore)
	if !ok {
		return nil, errors.New("list_environments: environments are not available on this store")
	}
	environments, err := backend.ListEnvironments(ctx, store.EnvironmentListQuery{})
	if err != nil {
		return nil, err
	}
	rows := make([]any, len(environments))
	for i, environment := range environments {
		rows[i] = environment
	}
	return rows, nil
}
