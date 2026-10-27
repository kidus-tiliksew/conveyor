package store

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// LineageEventProjection is the single replay contract shared by both stores.
// Unknown or malformed events project no edges (component-lineage).
type LineageEventProjection struct {
	Links       []core.LineageLink
	Suppresses  []core.LineageLink
	Unsupported int
}

func projectLineageEvent(workspace string, event core.Event) LineageEventProjection {
	var result LineageEventProjection
	link := func(srcType core.LineageNodeType, srcID string, dstType core.LineageNodeType, dstID, kind string) core.LineageLink {
		return core.LineageLink{Workspace: workspace, SrcType: srcType, SrcID: srcID,
			DstType: dstType, DstID: dstID, Kind: kind,
			CreatedByEventID: event.ID, CreatedAt: event.At}
	}
	var payload map[string]any
	if json.Unmarshal(event.Payload, &payload) != nil {
		return result
	}
	text := func(key string) string {
		value, _ := payload[key].(string)
		return strings.TrimSpace(value)
	}
	number := func(key string) int {
		switch value := payload[key].(type) {
		case float64:
			return int(value)
		case string:
			result, _ := strconv.Atoi(value)
			return result
		default:
			return 0
		}
	}
	boolean := func(key string) bool {
		value, _ := payload[key].(bool)
		return value
	}
	valid := func(items ...core.LineageLink) []core.LineageLink {
		out := make([]core.LineageLink, 0, len(items))
		for _, item := range items {
			if item.Validate() == nil {
				out = append(out, item)
			} else {
				result.Unsupported++
			}
		}
		return out
	}
	emit := func(items ...core.LineageLink) LineageEventProjection {
		result.Links = append(result.Links, valid(items...)...)
		return result
	}
	// A confirmation records the decisions its content names; the projector
	// reads that field, never the content, and a confirmation recorded before
	// the field projects no citation (component-lineage; DEC-90(2);
	// req-260802-72fc68 AC-6.3).
	versionDecisionCitations := func(versionType core.LineageNodeType, versionID string) []core.LineageLink {
		ids := recordedDecisionIDs(payload["cited_decision_ids"])
		links := make([]core.LineageLink, 0, len(ids))
		for _, id := range ids {
			links = append(links, link(versionType, versionID, core.LineageDecision, id, "cites"))
		}
		return links
	}

	switch event.Kind {
	case "task.created":
		if event.TaskID == "" {
			return result
		}
		// The plan side (blueprint:<id>) and the delivery side (task:<id>) of
		// one task share its ID; this join lets a walk from either reach the
		// other (component-lineage; req-260802-72fc68 AC-5.1, AC-5.3).
		links := []core.LineageLink{link(core.LineageBlueprint, event.TaskID, core.LineageTask, event.TaskID, "plans")}
		if parent, version := text("parent_task_id"), number("origin_spec_version"); parent != "" && version > 0 {
			links = append(links, link(core.LineageBlueprintVersion, core.BlueprintVersionLineageID(parent, version), core.LineageTask, event.TaskID, "materializes"))
		}
		return emit(links...)
	case "task.started_over":
		return emit(link(core.LineageTask, event.TaskID, core.LineageTask, text("successor_id"), "succeeded_by"))
	case "task.dependency_added":
		taskID, dependencyID := text("task_id"), text("depends_on_task_id")
		if taskID == "" {
			taskID = event.TaskID
		}
		return emit(link(core.LineageTask, taskID, core.LineageTask, dependencyID, "depends_on"))
	case "task.dependency_removed":
		// Removal suppresses the edge its addition projected, so a removed
		// dependency stops feeding context and rebuild drops its stale row
		// (component-lineage; req-260802-72fc68 AC-1.1, AC-2.3).
		taskID, dependencyID := text("task_id"), text("depends_on_task_id")
		if taskID == "" {
			taskID = event.TaskID
		}
		result.Suppresses = valid(link(core.LineageTask, taskID, core.LineageTask, dependencyID, "depends_on"))
		return result
	case "work_order.created":
		id := text("id")
		if id == "" {
			id = text("work_order_id")
		}
		return emit(link(core.LineageTask, event.TaskID, core.LineageWorkOrder, id, "dispatches"))
	case "planning_session.finalized":
		sessionID := text("session_id")
		var links []core.LineageLink
		if requirementID := text("produced_requirement_id"); requirementID != "" {
			links = append(links, link(core.LineagePlanningSession, sessionID, core.LineageRequirement, requirementID, "produced_requirement"))
		}
		if taskID := text("produced_task_id"); taskID != "" {
			links = append(links, link(core.LineagePlanningSession, sessionID, core.LineageBlueprint, taskID, "produced_blueprint"))
		}
		if designID := text("produced_system_design_id"); designID != "" {
			links = append(links, link(core.LineagePlanningSession, sessionID, core.LineageSystemDesign, designID, "produced_design"))
		}
		if bundleID := text("produced_bundle_id"); bundleID != "" {
			links = append(links, link(core.LineagePlanningSession, sessionID, core.LineagePlanningBundle, bundleID, "produced_bundle"))
		}
		return emit(links...)
	case PlanningBundleFinalized:
		var payload struct {
			BundleID  string                        `json:"bundle_id"`
			Documents []core.PlanningBundleDocument `json:"documents"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			return result
		}
		var links []core.LineageLink
		for _, document := range payload.Documents {
			switch document.Kind {
			case core.PlanningBundleRequirement:
				links = append(links, link(core.LineagePlanningBundle, payload.BundleID, core.LineageRequirementVersion, core.RequirementVersionLineageID(document.ID, document.Version), "proposes"))
			case core.PlanningBundleSystemDesign:
				links = append(links, link(core.LineagePlanningBundle, payload.BundleID, core.LineageSystemDesignVersion, core.SystemDesignVersionLineageID(document.ID, document.Version), "proposes"))
			case core.PlanningBundleDecision:
				links = append(links, link(core.LineagePlanningBundle, payload.BundleID, core.LineageDecision, document.ID, "proposes"))
			}
		}
		return emit(links...)
	case PlanningBundleRevised:
		var payload struct {
			BundleID          string                        `json:"bundle_id"`
			PreviousDocuments []core.PlanningBundleDocument `json:"previous_documents"`
			Documents         []core.PlanningBundleDocument `json:"documents"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			return result
		}
		documentLinks := func(documents []core.PlanningBundleDocument) []core.LineageLink {
			links := make([]core.LineageLink, 0, len(documents))
			for _, document := range documents {
				switch document.Kind {
				case core.PlanningBundleRequirement:
					links = append(links, link(core.LineagePlanningBundle, payload.BundleID, core.LineageRequirementVersion, core.RequirementVersionLineageID(document.ID, document.Version), "proposes"))
				case core.PlanningBundleSystemDesign:
					links = append(links, link(core.LineagePlanningBundle, payload.BundleID, core.LineageSystemDesignVersion, core.SystemDesignVersionLineageID(document.ID, document.Version), "proposes"))
				case core.PlanningBundleDecision:
					links = append(links, link(core.LineagePlanningBundle, payload.BundleID, core.LineageDecision, document.ID, "proposes"))
				}
			}
			return links
		}
		result.Suppresses = valid(documentLinks(payload.PreviousDocuments)...)
		result.Links = valid(documentLinks(payload.Documents)...)
		return result
	case PlanningBundleApproved:
		var payload struct {
			BundleID       string   `json:"bundle_id"`
			CreatedTaskIDs []string `json:"created_task_ids"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			return result
		}
		var links []core.LineageLink
		for _, taskID := range payload.CreatedTaskIDs {
			links = append(links, link(core.LineagePlanningBundle, payload.BundleID, core.LineageTask, taskID, "creates"))
		}
		return emit(links...)
	case "requirement.serves_confirmed":
		return emit(link(core.LineageRequirement, text("requirement_id"), core.LineageBlueprint, event.TaskID, "serves"))
	case "requirement.serves_dismissed":
		result.Suppresses = valid(link(core.LineageRequirement, text("requirement_id"), core.LineageBlueprint, event.TaskID, "serves"))
		return result
	case TaskContextRequirementAdded, TaskContextRequirementActive:
		if event.Kind == TaskContextRequirementAdded && boolean("unconfirmed") {
			return result
		}
		// The requirement edge stays for delivery lineage and staleness; the
		// version edge names the confirmed version the attachment recorded, so
		// a requirement version reaches the tasks that served it
		// (component-lineage; req-260802-72fc68 AC-5.2).
		id := text("id")
		links := []core.LineageLink{link(core.LineageRequirement, id, core.LineageTask, event.TaskID, "serves")}
		if version := number("version"); id != "" && version > 0 {
			links = append(links, link(core.LineageRequirementVersion, core.RequirementVersionLineageID(id, version), core.LineageTask, event.TaskID, "serves"))
		}
		return emit(links...)
	case TaskContextRequirementRemoved:
		id := text("id")
		suppresses := []core.LineageLink{link(core.LineageRequirement, id, core.LineageTask, event.TaskID, "serves")}
		for _, version := range recordedRequirementVersions(payload) {
			if id == "" {
				break
			}
			suppresses = append(suppresses, link(core.LineageRequirementVersion, core.RequirementVersionLineageID(id, version), core.LineageTask, event.TaskID, "serves"))
		}
		result.Suppresses = valid(suppresses...)
		return result
	case TaskContextDesignAdded:
		if boolean("unconfirmed") {
			return result
		}
		return emit(link(core.LineageSystemDesignVersion, core.SystemDesignVersionLineageID(text("id"), number("version")), core.LineageTask, event.TaskID, "governs"))
	case TaskContextDesignActive:
		return emit(link(core.LineageSystemDesignVersion, core.SystemDesignVersionLineageID(text("id"), number("version")), core.LineageTask, event.TaskID, "governs"))
	case TaskContextDesignRemoved:
		result.Suppresses = valid(link(core.LineageSystemDesignVersion, core.SystemDesignVersionLineageID(text("id"), number("version")), core.LineageTask, event.TaskID, "governs"))
		return result
	case "requirement.version_confirmed":
		var links []core.LineageLink
		if id, version := text("requirement_id"), number("version"); id != "" && version > 0 {
			links = append(links, link(core.LineageRequirement, id, core.LineageRequirementVersion, core.RequirementVersionLineageID(id, version), "versions"))
		}
		if documentID, documentVersion := text("derived_document_id"), number("derived_document_version"); documentID != "" && documentVersion > 0 {
			links = append(links, link(core.LineageRequirementVersion, core.RequirementVersionLineageID(text("requirement_id"), number("version")), core.LineageReferenceDocumentVersion, core.ReferenceDocumentVersionLineageID(documentID, documentVersion), "derived_from"))
		}
		if version := number("version"); version > 1 {
			id := text("requirement_id")
			_, predecessorRecorded := payload["supersedes_version"]
			predecessor := number("supersedes_version")
			if !predecessorRecorded {
				predecessor = version - 1
			}
			if predecessor > 0 {
				links = append(links, link(core.LineageRequirementVersion, core.RequirementVersionLineageID(id, version), core.LineageRequirementVersion, core.RequirementVersionLineageID(id, predecessor), "supersedes"))
			}
		}
		links = append(links, versionDecisionCitations(core.LineageRequirementVersion, core.RequirementVersionLineageID(text("requirement_id"), number("version")))...)
		return emit(links...)
	case "reference_document.superseded":
		id, version, predecessor := text("document_id"), number("version"), number("supersedes_version")
		return emit(
			link(core.LineageReferenceDocument, id, core.LineageReferenceDocumentVersion, core.ReferenceDocumentVersionLineageID(id, version), "versions"),
			link(core.LineageReferenceDocumentVersion, core.ReferenceDocumentVersionLineageID(id, version), core.LineageReferenceDocumentVersion, core.ReferenceDocumentVersionLineageID(id, predecessor), "supersedes"),
		)
	case "reference_document.created":
		id, version := text("document_id"), number("version")
		return emit(link(core.LineageReferenceDocument, id, core.LineageReferenceDocumentVersion, core.ReferenceDocumentVersionLineageID(id, version), "versions"))
	case "reference_document.consulted":
		id, version, sessionID := text("document_id"), number("version"), text("session_id")
		return emit(link(core.LineagePlanningSession, sessionID, core.LineageReferenceDocumentVersion, core.ReferenceDocumentVersionLineageID(id, version), "consulted"))
	case "system_design.consulted":
		id, version := text("document_id"), number("version")
		destination := core.SystemDesignVersionLineageID(id, version)
		if workOrderID := text("work_order_id"); workOrderID != "" {
			return emit(link(core.LineageWorkOrder, workOrderID, core.LineageSystemDesignVersion, destination, "consulted"))
		}
		if taskID := text("delivery_task_id"); taskID != "" {
			return emit(link(core.LineageTask, taskID, core.LineageSystemDesignVersion, destination, "consulted"))
		}
		return emit(link(core.LineagePlanningSession, text("session_id"), core.LineageSystemDesignVersion, destination, "consulted"))
	case "system_design.version_confirmed":
		id, version, predecessor := text("document_id"), number("version"), number("supersedes_version")
		versionID := core.SystemDesignVersionLineageID(id, version)
		links := []core.LineageLink{link(core.LineageSystemDesign, id, core.LineageSystemDesignVersion, versionID, "versions")}
		if predecessor > 0 {
			links = append(links, link(core.LineageSystemDesignVersion, versionID, core.LineageSystemDesignVersion, core.SystemDesignVersionLineageID(id, predecessor), "supersedes"))
		}
		if raw, ok := payload["governs"].([]any); ok {
			for _, rawScope := range raw {
				scope, _ := rawScope.(map[string]any)
				repository, _ := scope["repository"].(string)
				if repository == "" {
					repository, _ = scope["repo"].(string)
				}
				paths, _ := scope["paths"].([]any)
				for _, rawPath := range paths {
					glob, _ := rawPath.(string)
					links = append(links, link(core.LineageSystemDesignVersion, versionID, core.LineageRepositoryPath, core.RepoPathComponentLineageID(repository, glob), "governs"))
				}
			}
		}
		if taskID := text("origin_task_id"); taskID != "" {
			links = append(links, link(core.LineageSystemDesignVersion, versionID, core.LineageTask, taskID, "proposed_by"))
		}
		if sessionID := text("origin_session_id"); sessionID != "" {
			links = append(links, link(core.LineageSystemDesignVersion, versionID, core.LineagePlanningSession, sessionID, "proposed_by"))
		}
		links = append(links, versionDecisionCitations(core.LineageSystemDesignVersion, versionID)...)
		return emit(links...)
	case "decision.proposed":
		id := text("decision_id")
		if sessionID := text("origin_session_id"); sessionID != "" {
			return emit(link(core.LineageDecision, id, core.LineagePlanningSession, sessionID, "proposed_by"))
		}
		if taskID := text("origin_task_id"); taskID != "" {
			return emit(link(core.LineageDecision, id, core.LineageTask, taskID, "proposed_by"))
		}
	case "decision.confirmed":
		if predecessor := text("supersedes"); predecessor != "" {
			return emit(link(core.LineageDecision, text("decision_id"), core.LineageDecision, predecessor, "supersedes"))
		}
	case "decision.dismissed":
		return result
	case "spec.version_created":
		if version := number("version"); version > 0 {
			links := []core.LineageLink{link(core.LineageBlueprint, event.TaskID, core.LineageBlueprintVersion, core.BlueprintVersionLineageID(event.TaskID, version), "versions")}
			if version > 1 {
				links = append(links, link(core.LineageBlueprintVersion, core.BlueprintVersionLineageID(event.TaskID, version), core.LineageBlueprintVersion, core.BlueprintVersionLineageID(event.TaskID, version-1), "supersedes"))
			}
			return emit(links...)
		}
	case "pull_request.opened":
		repository, prNumber := text("repository"), number("number")
		var links []core.LineageLink
		if repository != "" && prNumber > 0 {
			links = append(links, link(core.LineageTask, event.TaskID, core.LineagePullRequest, core.PullRequestLineageID(repository, prNumber), "submitted_as"))
		} else if repository != "" || payload["number"] != nil {
			result.Unsupported++
		}
		if baseSHA, headSHA := text("base_sha"), text("head_sha"); repository != "" && baseSHA != "" && headSHA != "" {
			links = append(links, link(core.LineageTask, event.TaskID, core.LineageCommitRange, core.CommitRangeLineageID(repository, baseSHA, headSHA), "submitted_range"))
		} else if baseSHA != "" || headSHA != "" {
			result.Unsupported++
		}
		return emit(links...)
	case "merge.confirmed", "merge.reconciled":
		if repository, baseSHA, headSHA := text("repository"), text("base_sha"), text("head_sha"); repository != "" && baseSHA != "" && headSHA != "" {
			return emit(link(core.LineageTask, event.TaskID, core.LineageCommitRange, core.CommitRangeLineageID(repository, baseSHA, headSHA), "merged_range"))
		}
	case "review.completed":
		workOrderID := text("review_work_order_id")
		verdictID := core.VerdictLineageID(workOrderID)
		links := []core.LineageLink{link(core.LineageWorkOrder, workOrderID, core.LineageVerdict, verdictID, "produced_verdict")}
		if evidence, ok := payload["evidence_ids"].([]any); ok {
			for _, raw := range evidence {
				if id, ok := raw.(string); ok {
					links = append(links, link(core.LineageEvidence, id, core.LineageVerdict, verdictID, "supports"))
				}
			}
		}
		// The verdict's validated governance assessment is the one record of
		// which decisions a delivery relied on; the pinned snapshot names every
		// confirmed decision and projects nothing (component-lineage; DEC-90(1);
		// req-260802-72fc68 AC-6.1, AC-6.2). An event without a review work
		// order already counts its produced_verdict link as unsupported.
		if assessment, ok := payload["governance_assessment"].(map[string]any); ok && workOrderID != "" {
			for _, id := range recordedDecisionIDs(assessment["cited_ids"]) {
				links = append(links, link(core.LineageVerdict, verdictID, core.LineageDecision, id, "cites"))
			}
			for _, id := range recordedDecisionIDs(assessment["superseded_ids"]) {
				links = append(links, link(core.LineageVerdict, verdictID, core.LineageDecision, id, "flags_superseded"))
			}
		}
		return emit(links...)
	}
	return result
}

// recordedDecisionIDs reads a recorded ID list and keeps its DEC-n entries,
// trimmed, deduplicated, and in numeric order, so one verdict or version
// yields one key per decision. Other entries, such as System Design IDs in a
// verdict's cited_ids, project nothing.
func recordedDecisionIDs(raw any) []string {
	values, _ := raw.([]any)
	seen := map[string]bool{}
	ids := make([]string, 0, len(values))
	for _, value := range values {
		id, _ := value.(string)
		if id = strings.TrimSpace(id); core.IsDecisionID(id) && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	core.SortDecisionIDs(ids)
	return ids
}

// recordedRequirementVersions reads the versions a requirement removal
// records: every version in `versions` plus a scalar `version`, deduplicated
// and ascending. A historical removal records none.
func recordedRequirementVersions(payload map[string]any) []int {
	seen := map[int]bool{}
	add := func(raw any) {
		switch value := raw.(type) {
		case float64:
			if version := int(value); version > 0 {
				seen[version] = true
			}
		case string:
			if version, err := strconv.Atoi(value); err == nil && version > 0 {
				seen[version] = true
			}
		}
	}
	if values, ok := payload["versions"].([]any); ok {
		for _, raw := range values {
			add(raw)
		}
	}
	add(payload["version"])
	versions := make([]int, 0, len(seen))
	for version := range seen {
		versions = append(versions, version)
	}
	sort.Ints(versions)
	return versions
}

// LineageReplay carries the one piece of state a rebuild needs that the
// per-event projector cannot hold: the requirement versions replay has
// projected as served by each task since that attachment's last removal. A
// historical task.context_requirement_removed without recorded versions then
// suppresses those version edges too (component-lineage; req-260802-72fc68
// AC-1.3). Live writers record the versions on the removal itself, so only
// rebuild needs this.
type LineageReplay struct {
	served map[string]map[string]core.LineageLink
}

func NewLineageReplay() *LineageReplay {
	return &LineageReplay{served: map[string]map[string]core.LineageLink{}}
}

// Project applies the canonical projector and then the replay-only removal
// completion described on LineageReplay.
func (r *LineageReplay) Project(workspace string, event core.Event) LineageEventProjection {
	projection := projectLineageEvent(workspace, event)
	switch event.Kind {
	case TaskContextRequirementAdded, TaskContextRequirementActive:
		for _, link := range projection.Links {
			if link.Kind != "serves" || link.SrcType != core.LineageRequirementVersion || link.DstType != core.LineageTask {
				continue
			}
			attachment := r.attachmentKey(event)
			if r.served[attachment] == nil {
				r.served[attachment] = map[string]core.LineageLink{}
			}
			r.served[attachment][lineageLinkKey(link)] = link
		}
	case TaskContextRequirementRemoved:
		attachment := r.attachmentKey(event)
		suppressed := map[string]bool{}
		for _, link := range projection.Suppresses {
			suppressed[lineageLinkKey(link)] = true
		}
		keys := make([]string, 0, len(r.served[attachment]))
		for key := range r.served[attachment] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !suppressed[key] {
				projection.Suppresses = append(projection.Suppresses, r.served[attachment][key])
			}
		}
		delete(r.served, attachment)
	}
	return projection
}

func (r *LineageReplay) attachmentKey(event core.Event) string {
	var payload struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(event.Payload, &payload)
	return event.TaskID + "\x00" + strings.TrimSpace(payload.ID)
}

func lineageLinksForEvent(workspace string, event core.Event) []core.LineageLink {
	return projectLineageEvent(workspace, event).Links
}

// LineageLinksForEvent exposes the canonical projector to durable store
// adapters without duplicating event interpretation.
func LineageLinksForEvent(workspace string, event core.Event) []core.LineageLink {
	return lineageLinksForEvent(workspace, event)
}

// ProjectLineageEvent exposes replay-only suppression and rejection metadata
// alongside the canonical event-derived links.
func ProjectLineageEvent(workspace string, event core.Event) LineageEventProjection {
	return projectLineageEvent(workspace, event)
}

// HistoricalRequirementConfirmation identifies confirmation events written
// before supersedes_version was recorded explicitly.
func HistoricalRequirementConfirmation(event core.Event) (string, int, bool) {
	if event.Kind != "requirement.version_confirmed" {
		return "", 0, false
	}
	var payload map[string]any
	if json.Unmarshal(event.Payload, &payload) != nil {
		return "", 0, false
	}
	if _, recorded := payload["supersedes_version"]; recorded {
		return "", 0, false
	}
	requirementID, _ := payload["requirement_id"].(string)
	version, _ := payload["version"].(float64)
	return strings.TrimSpace(requirementID), int(version), requirementID != "" && version > 1
}

// LineageLinksForHistoricalConfirmation supplies the durable confirmed
// predecessor for an event that predates supersedes_version.
func LineageLinksForHistoricalConfirmation(workspace string, event core.Event, predecessor int) []core.LineageLink {
	var payload map[string]any
	if json.Unmarshal(event.Payload, &payload) != nil {
		return nil
	}
	payload["supersedes_version"] = predecessor
	event.Payload = core.JSONPayload(payload)
	return lineageLinksForEvent(workspace, event)
}

func lineageLinkKey(link core.LineageLink) string {
	return strings.Join([]string{link.Workspace, string(link.SrcType), link.SrcID, string(link.DstType), link.DstID, link.Kind}, "\x00")
}

var projectorOwnedLineageKinds = map[string]struct{}{
	"dispatches": {}, "produced_requirement": {}, "produced_blueprint": {}, "serves": {},
	"versions": {}, "supersedes": {}, "submitted_as": {}, "submitted_range": {},
	"merged_range": {}, "produced_verdict": {}, "supports": {}, "depends_on": {}, "materializes": {},
	"consulted": {}, "derived_from": {},
	"governs": {}, "proposed_by": {}, "produced_design": {},
	"produced_bundle": {}, "proposes": {}, "creates": {},
	"plans": {}, "succeeded_by": {},
	"cites": {}, "flags_superseded": {},
}

// Direction is part of the vocabulary contract even though ownership is keyed
// only by kind: planning_session -consulted-> reference_document_version,
// task/work_order -consulted-> system_design_version, and
// requirement_version -derived_from-> reference_document_version,
// system_design_version -governs-> repository_path/task,
// requirement/requirement_version -serves-> blueprint/task,
// requirement -versions-> requirement_version, blueprint -plans-> task,
// task -succeeded_by-> task,
// verdict/requirement_version/system_design_version -cites-> decision,
// verdict -flags_superseded-> decision,
// system_design_version -proposed_by-> task/planning_session, and
// planning_session -produced_design-> system_design,
// planning_session -produced_bundle-> planning_bundle,
// planning_bundle -proposes-> requirement_version/system_design_version/decision,
// and planning_bundle -creates-> task. Keep this mirror aligned
// with the Postgres delete vocabularies below.

func projectorOwnsLineageKind(kind string) bool { _, ok := projectorOwnedLineageKinds[kind]; return ok }

func CanonicalLineageKinds() map[string]struct{} {
	out := make(map[string]struct{}, len(projectorOwnedLineageKinds))
	for kind := range projectorOwnedLineageKinds {
		out[kind] = struct{}{}
	}
	return out
}
