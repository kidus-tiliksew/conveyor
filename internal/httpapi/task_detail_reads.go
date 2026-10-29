package httpapi

import (
	"context"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Request-scoped task-detail reads (component-http-api, Request-scoped detail
// reads): each helper consumes records the detail handler already read, or
// one set read, instead of re-reading per work order or document.

// taskDetailDocumentIDs is the one metadata selection of a detail read: every
// active context reference and every checkpoint citation's document ID, in
// first-seen order without duplicates.
func taskDetailDocumentIDs(requirements map[string]bool, designs map[string]int, orders []core.WorkOrder) []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, id := range sortedKeys(requirements) {
		add(id)
	}
	for _, id := range sortedKeys(designs) {
		add(id)
	}
	for _, order := range orders {
		if order.Checkpoint == nil {
			continue
		}
		for _, citation := range order.Checkpoint.Citations {
			add(citation.DocumentID)
		}
	}
	return ids
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// taskDetailContext reproduces store.TaskContextFromEvents from the request's
// metadata set: a requirement needs a row with a confirmed version, a design
// needs a row and carries its pinned version, and a reference without a row
// is omitted.
func taskDetailContext(requirements map[string]bool, designs map[string]int, metadata store.DocumentMetadataSet) core.TaskContext {
	result := core.TaskContext{Requirements: []core.TaskRequirementContext{}, Designs: []core.TaskDesignContext{}}
	for id := range requirements {
		document, ok := metadata.Requirements[id]
		if !ok || document.CurrentVersion <= 0 {
			continue
		}
		result.Requirements = append(result.Requirements, core.TaskRequirementContext{ID: id, Title: document.Title, Version: document.CurrentVersion, Archived: document.Archived, SupersededBy: append([]string(nil), document.SupersededBy...)})
	}
	for id, version := range designs {
		document, ok := metadata.Designs[id]
		if !ok {
			continue
		}
		result.Designs = append(result.Designs, core.TaskDesignContext{ID: id, Title: document.Title, Version: version, Archived: document.Archived, SupersededBy: append([]string(nil), document.SupersededBy...)})
	}
	sort.Slice(result.Requirements, func(i, j int) bool { return result.Requirements[i].ID < result.Requirements[j].ID })
	sort.Slice(result.Designs, func(i, j int) bool { return result.Designs[i].ID < result.Designs[j].ID })
	return result
}

// taskDetailArtifacts partitions one task-node artifact read with the
// previous ListArtifacts predicates: task-context attachments and eligible
// verification evidence, each with its download URL and never nil.
func taskDetailArtifacts(taskID string, artifacts []core.Artifact) ([]core.Artifact, []core.Artifact) {
	attachments, evidence := make([]core.Artifact, 0), make([]core.Artifact, 0)
	for _, artifact := range artifacts {
		if artifact.TaskID != taskID {
			continue
		}
		artifact.DownloadURL = "/v1/artifacts/" + artifact.ID
		if artifact.Role == core.ArtifactRoleTaskContext {
			attachments = append(attachments, artifact)
		}
		if artifact.EligibleVerificationEvidence() {
			evidence = append(evidence, artifact)
		}
	}
	return attachments, evidence
}

// detailNextActionOptions grants the four next-action capabilities exactly as
// nextActionOptionsFor does, with one membership decision instead of one per
// capability. Nothing is inferred from a claim.
func (s *Server) detailNextActionOptions(ctx context.Context) (store.NextActionOptions, error) {
	opts := store.NextActionOptions{Now: time.Now().UTC(), Capabilities: map[core.Capability]bool{}, MergeReadiness: s.taskRunReadiness.lastKnown}
	credential, ok := store.CredentialFromContext(ctx)
	if !ok || credential.Kind != core.CredentialUser {
		return opts, nil
	}
	workspaceID, scoped := store.WorkspaceFromContext(ctx)
	if scoped && s.Memberships != nil {
		decisions, err := s.Memberships.AuthorizeWorkspaceCapabilities(ctx, credential.OwnerUserID, workspaceID, nextActionCapabilities)
		if err != nil {
			return store.NextActionOptions{}, err
		}
		for _, capability := range nextActionCapabilities {
			if decisions[capability] {
				opts.Capabilities[capability] = true
			}
		}
		return opts, nil
	}
	if credential.Scope == core.CredentialScopeOperator {
		for _, capability := range nextActionCapabilities {
			opts.Capabilities[capability] = true
		}
	}
	return opts, nil
}

// nextActionOrders returns the orders as ListWorkOrdersForTasks returns them:
// without the two pin snapshots and before any detail decoration.
func nextActionOrders(orders []core.WorkOrder) []core.WorkOrder {
	out := make([]core.WorkOrder, len(orders))
	for i, order := range orders {
		order.ServedRequirementSnapshot = nil
		order.GovernanceSnapshot = nil
		out[i] = order
	}
	return out
}
