package store

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// lineageLinksForEvent is the single event-to-edge contract shared by both
// stores. Unknown or malformed events project no edges (spec §16).
func lineageLinksForEvent(workspace string, event core.Event) []core.LineageLink {
	link := func(srcType core.LineageNodeType, srcID string, dstType core.LineageNodeType, dstID, kind string) core.LineageLink {
		return core.LineageLink{Workspace: workspace, SrcType: srcType, SrcID: srcID,
			DstType: dstType, DstID: dstID, Kind: kind,
			CreatedByEventID: event.ID, CreatedAt: event.At}
	}
	var payload map[string]any
	if json.Unmarshal(event.Payload, &payload) != nil {
		return nil
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
			}
		}
		return out
	}

	switch event.Kind {
	case "task.created":
		parent, version := text("parent_task_id"), number("origin_spec_version")
		if parent != "" && version > 0 && event.TaskID != "" {
			return valid(link(core.LineageBlueprintVersion, core.BlueprintVersionLineageID(parent, version), core.LineageTask, event.TaskID, "materializes"))
		}
	case "task.dependency_added":
		taskID, dependencyID := text("task_id"), text("depends_on_task_id")
		if taskID == "" {
			taskID = event.TaskID
		}
		return valid(link(core.LineageTask, taskID, core.LineageTask, dependencyID, "depends_on"))
	case "work_order.created":
		id := text("id")
		if id == "" {
			id = text("work_order_id")
		}
		return valid(link(core.LineageTask, event.TaskID, core.LineageWorkOrder, id, "dispatches"))
	case "planning_session.finalized":
		sessionID := text("session_id")
		var links []core.LineageLink
		if requirementID := text("produced_requirement_id"); requirementID != "" {
			links = append(links, link(core.LineagePlanningSession, sessionID, core.LineageRequirement, requirementID, "produced_requirement"))
		}
		if taskID := text("produced_task_id"); taskID != "" {
			links = append(links, link(core.LineagePlanningSession, sessionID, core.LineageBlueprint, taskID, "produced_blueprint"))
		}
		return valid(links...)
	case "requirement.serves_confirmed":
		return valid(link(core.LineageRequirement, text("requirement_id"), core.LineageBlueprint, event.TaskID, "serves"))
	case "requirement.version_confirmed":
		if version := number("version"); version > 1 {
			id := text("requirement_id")
			_, predecessorRecorded := payload["supersedes_version"]
			predecessor := number("supersedes_version")
			if !predecessorRecorded {
				predecessor = version - 1
			}
			if predecessor <= 0 {
				return nil
			}
			return valid(link(core.LineageRequirementVersion, core.RequirementVersionLineageID(id, version), core.LineageRequirementVersion, core.RequirementVersionLineageID(id, predecessor), "supersedes"))
		}
	case "spec.version_created":
		if version := number("version"); version > 0 {
			links := []core.LineageLink{link(core.LineageBlueprint, event.TaskID, core.LineageBlueprintVersion, core.BlueprintVersionLineageID(event.TaskID, version), "versions")}
			if version > 1 {
				links = append(links, link(core.LineageBlueprintVersion, core.BlueprintVersionLineageID(event.TaskID, version), core.LineageBlueprintVersion, core.BlueprintVersionLineageID(event.TaskID, version-1), "supersedes"))
			}
			return valid(links...)
		}
	case "pull_request.opened":
		repository, prNumber := text("repository"), number("number")
		var links []core.LineageLink
		if repository != "" && prNumber > 0 {
			links = append(links, link(core.LineageTask, event.TaskID, core.LineagePullRequest, core.PullRequestLineageID(repository, prNumber), "submitted_as"))
		}
		if baseSHA, headSHA := text("base_sha"), text("head_sha"); repository != "" && baseSHA != "" && headSHA != "" {
			links = append(links, link(core.LineageTask, event.TaskID, core.LineageCommitRange, core.CommitRangeLineageID(repository, baseSHA, headSHA), "submitted_range"))
		}
		return valid(links...)
	case "merge.confirmed", "merge.reconciled":
		if repository, baseSHA, headSHA := text("repository"), text("base_sha"), text("head_sha"); repository != "" && baseSHA != "" && headSHA != "" {
			return valid(link(core.LineageTask, event.TaskID, core.LineageCommitRange, core.CommitRangeLineageID(repository, baseSHA, headSHA), "merged_range"))
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
		return valid(links...)
	}
	return nil
}

// LineageLinksForEvent exposes the canonical projector to durable store
// adapters without duplicating event interpretation.
func LineageLinksForEvent(workspace string, event core.Event) []core.LineageLink {
	return lineageLinksForEvent(workspace, event)
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
}

func projectorOwnsLineageKind(kind string) bool { _, ok := projectorOwnedLineageKinds[kind]; return ok }

func CanonicalLineageKinds() map[string]struct{} {
	out := make(map[string]struct{}, len(projectorOwnedLineageKinds))
	for kind := range projectorOwnedLineageKinds {
		out[kind] = struct{}{}
	}
	return out
}
