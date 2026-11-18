package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// artifactHighWaterSQL reads the newest artifact key that fences a listing.
const artifactHighWaterSQL = `SELECT created_at,id FROM artifacts WHERE workspace_id=? ORDER BY created_at DESC,id DESC LIMIT 1`

// ListArtifacts is two statements for a nonempty workspace, however many
// artifacts or associations exist. The first fixes (created_at,id) of the
// newest artifact as a high-water key; the second is one complete
// metadata/association listing bounded by that inclusive key. This is not a
// database snapshot: SingleStore offers READ COMMITTED only and no
// transaction spans the two statements. Artifacts created after the fence
// carry a later key and are excluded even if they commit before the listing.
// A row below the fence that commits before the listing statement reads can
// appear. Association rows (role and owners) are as of that single listing
// statement, so a link change is never partially incorporated across
// artificial pages. Production writers stamp created_at from the server
// clock, never from client input.
func (s *Store) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListArtifacts")
	ws := documentWorkspace(ctx)
	var snapAt time.Time
	var snapID string
	err := documentRow(ctx, s.db, artifactHighWaterSQL, ws).Scan(&snapAt, &snapID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	store.RunArtifactListPageHook()
	query, args := s.artifactFullListQuery(ws, snapAt, snapID)
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	return scanArtifactListRows(rows)
}

// artifactFullListQuery builds the single complete listing: ten metadata and
// association columns, every physical link as a row, artifacts bounded by the
// inclusive (created_at,id) fence. Args follow placeholder order: artifact
// workspace, fence time twice, fence ID.
//
// The link join uses the ordinary equality l.artifact_id=a.id and the exact
// BINARY equality, and deliberately omits the generated digest equality H
// (artifact_lookup_key=SHA2(a.id,256)) that bounded reads add. H cannot change
// this result. Let P be the exact-ID predicate l.artifact_id=BINARY a.id.
// One binary operand makes both sides bytewise. When P is true the two IDs are
// byte-identical, so H is true: the generated column is a deterministic SHA2
// of the same bytes, and a ready
// layout verifies that expression. When P is false H is irrelevant, because
// P already rejects the pair, so a hash collision between different IDs still
// fails P. A NULL generated key makes H not true, but P is then also not true
// for that pair, so the LEFT JOIN gives the same default row and the same
// multiplicity. A legacy layout has no generated column and H would not be
// valid there, so it is omitted in every layout. The result is therefore
// identical to P alone and no SHA256 collision fixture is needed.
func (s *Store) artifactFullListQuery(workspace string, snapAt time.Time, snapID string) (string, []any) {
	return `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id AND l.artifact_id=BINARY a.id WHERE a.workspace_id=? AND (a.created_at<? OR (a.created_at=? AND a.id<=?)) ORDER BY a.created_at,a.id,l.role`, []any{workspace, snapAt, snapAt, snapID}
}

func (s *Store) hydrateArtifactAssociations(ctx context.Context, ids []string) ([]core.Artifact, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	query, args := s.artifactHydrationQuery(documentWorkspace(ctx), ids)
	rows, err := documentRows(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	return scanArtifactListRows(rows)
}

// artifactHydrationQuery builds the page association read. The link side is a
// derived selector restricted to the page IDs (digest IN-list when the lookup
// is prepared, plus the raw BINARY OR set always), joined on workspace and the
// raw BINARY artifact ID. Args follow placeholder order: link workspace,
// [digest IDs], raw link IDs, artifact workspace, raw artifact IDs.
func (s *Store) artifactHydrationQuery(workspace string, ids []string) (string, []any) {
	raw := make([]string, 0, len(ids))
	digests := make([]string, 0, len(ids))
	for range ids {
		raw = append(raw, "BINARY artifact_id=BINARY ?")
		digests = append(digests, "SHA2(?,256)")
	}
	args := []any{workspace}
	selector := "workspace_id=?"
	if s.hotReadIndexReady("artifact_links_artifact_lookup_idx") {
		selector += " AND artifact_lookup_key IN (" + strings.Join(digests, ",") + ")"
		for _, id := range ids {
			args = append(args, id)
		}
	}
	selector += " AND (" + strings.Join(raw, " OR ") + ")"
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, workspace)
	clauses := make([]string, 0, len(ids))
	for _, id := range ids {
		clauses = append(clauses, "BINARY a.id=BINARY ?")
		args = append(args, id)
	}
	return `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN (SELECT workspace_id,artifact_id,role,task_id,requirement_id,planning_session_id FROM artifact_links WHERE ` + selector + `) l ON l.workspace_id=a.workspace_id AND BINARY l.artifact_id=BINARY a.id WHERE a.workspace_id=? AND (` + strings.Join(clauses, " OR ") + `) ORDER BY a.created_at,a.id,l.role`, args
}

// artifactEquality renders the original value comparison. exact keeps the
// byte-wise BINARY guard the listing readers always used; the single-artifact
// readers keep the plain comparison they had, so a digest never changes what
// either family of readers accepts.
func artifactEquality(left, right string, exact bool) string {
	if exact {
		return "BINARY " + left + "=BINARY " + right
	}
	return left + "=" + right
}

// artifactLinkIDMatch matches an artifact link to an artifact ID expression.
// A prepared layout adds the generated digest equality so the lookup index
// applies, and the original equality always stays. A legacy layout
// references no generated column.
func (s *Store) artifactLinkIDMatch(link, idExpr string, exact bool) string {
	original := artifactEquality(link+".artifact_id", idExpr, exact)
	if s.hotReadIndexReady("artifact_links_artifact_lookup_idx") {
		return link + ".artifact_lookup_key=SHA2(" + idExpr + ",256) AND " + original
	}
	return original
}

// artifactOwnerKeys maps a link owner column to its generated digest column
// and the index whose readiness gates it. Each lookup is gated independently.
var artifactOwnerKeys = map[string]struct{ key, index string }{
	"task_id":             {"task_lookup_key", "artifact_links_task_lookup_idx"},
	"requirement_id":      {"requirement_lookup_key", "artifact_links_requirement_lookup_idx"},
	"planning_session_id": {"session_lookup_key", "artifact_links_session_lookup_idx"},
}

// artifactLinkOwnerMatch matches an owner column to a value expression. The
// expression appears twice when the digest is used, so a placeholder needs
// its argument once per occurrence (see strings.Count of "?").
func (s *Store) artifactLinkOwnerMatch(link, column, valueExpr string, exact bool) string {
	original := artifactEquality(link+"."+column, valueExpr, exact)
	if k := artifactOwnerKeys[column]; s.hotReadIndexReady(k.index) {
		return link + "." + k.key + "=SHA2(" + valueExpr + ",256) AND " + original
	}
	return original
}

func (s *Store) ListArtifactsForOwners(ctx context.Context, owners store.ArtifactOwners) ([]core.Artifact, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListArtifactsForOwners")
	var preds []ownerPredicate
	for _, id := range store.ArtifactOwnerIDs(owners.TaskIDs) {
		preds = append(preds, ownerPredicate{column: "task_id", id: id})
	}
	for _, id := range store.ArtifactOwnerIDs(owners.RequirementIDs) {
		preds = append(preds, ownerPredicate{column: "requirement_id", id: id})
	}
	for _, id := range store.ArtifactOwnerIDs(owners.SessionIDs) {
		preds = append(preds, ownerPredicate{column: "planning_session_id", id: id})
	}
	if len(preds) == 0 {
		return []core.Artifact{}, nil
	}
	var result []core.Artifact
	// A link has exactly one owner (ValidateAttachmentTarget), so it matches
	// a predicate in at most one OR chunk and no row repeats across chunks.
	for start := 0; start < len(preds); start += store.ArtifactMetadataPageSize {
		chunk := preds[start:min(start+store.ArtifactMetadataPageSize, len(preds))]
		query, args := s.artifactOwnerChunkQuery(documentWorkspace(ctx), chunk)
		rows, err := documentRows(ctx, s.db, query, args...)
		if err != nil {
			return nil, err
		}
		page, err := scanArtifactListRows(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
	}
	if result == nil {
		return []core.Artifact{}, nil
	}
	sort.SliceStable(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		if result[i].ID != result[j].ID {
			return result[i].ID < result[j].ID
		}
		return result[i].Role < result[j].Role
	})
	return result, nil
}

// artifactOwnerChunkQuery builds one OR chunk of owner predicates. Each
// predicate pairs its digest equality with the original value equality when
// its own lookup is prepared, and uses the original equality alone otherwise.
func (s *Store) artifactOwnerChunkQuery(workspace string, chunk []ownerPredicate) (string, []any) {
	clauses := make([]string, 0, len(chunk))
	args := []any{workspace}
	for _, pred := range chunk {
		clause := s.artifactLinkOwnerMatch("l", pred.column, "?", true)
		clauses = append(clauses, "("+clause+")")
		for range strings.Count(clause, "?") {
			args = append(args, pred.id)
		}
	}
	return `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifact_links l JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id WHERE l.workspace_id=? AND (` + strings.Join(clauses, " OR ") + `) ORDER BY a.created_at,a.id,l.role`, args
}

type ownerPredicate struct {
	column, id string
}

func scanArtifactListRows(rows *documentResultSet) ([]core.Artifact, error) {
	defer rows.Close()
	var result []core.Artifact
	for rows.Next() {
		var artifact core.Artifact
		if err := rows.Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &artifact.CreatedAt, &artifact.Role, &artifact.TaskID, &artifact.RequirementID, &artifact.PlanningSessionID); err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, rows.Err()
}
