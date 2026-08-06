package store

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// LineageEventProjection is the single replay contract shared by both stores.
// Unknown or malformed events project no edges (spec §16).
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

	switch event.Kind {
	case "task.created":
		parent, version := text("parent_task_id"), number("origin_spec_version")
		if parent != "" && version > 0 && event.TaskID != "" {
			return emit(link(core.LineageBlueprintVersion, core.BlueprintVersionLineageID(parent, version), core.LineageTask, event.TaskID, "materializes"))
		}
	case "task.dependency_added":
		taskID, dependencyID := text("task_id"), text("depends_on_task_id")
		if taskID == "" {
			taskID = event.TaskID
		}
		return emit(link(core.LineageTask, taskID, core.LineageTask, dependencyID, "depends_on"))
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
	case TaskContextRequirementAdded:
		return emit(link(core.LineageRequirement, text("id"), core.LineageTask, event.TaskID, "serves"))
	case TaskContextRequirementRemoved:
		result.Suppresses = valid(link(core.LineageRequirement, text("id"), core.LineageTask, event.TaskID, "serves"))
		return result
	case TaskContextDesignAdded:
		return emit(link(core.LineageSystemDesignVersion, core.SystemDesignVersionLineageID(text("id"), number("version")), core.LineageTask, event.TaskID, "governs"))
	case TaskContextDesignRemoved:
		result.Suppresses = valid(link(core.LineageSystemDesignVersion, core.SystemDesignVersionLineageID(text("id"), number("version")), core.LineageTask, event.TaskID, "governs"))
		return result
	case "requirement.version_confirmed":
		var links []core.LineageLink
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
			if predecessor <= 0 {
				return emit(links...)
			}
			links = append(links, link(core.LineageRequirementVersion, core.RequirementVersionLineageID(id, version), core.LineageRequirementVersion, core.RequirementVersionLineageID(id, predecessor), "supersedes"))
		}
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
		return emit(links...)
	}
	return result
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
}

// Direction is part of the vocabulary contract even though ownership is keyed
// only by kind: planning_session -consulted-> reference_document_version and
// requirement_version -derived_from-> reference_document_version,
// system_design_version -governs-> repository_path/task,
// requirement -serves-> blueprint/task,
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
