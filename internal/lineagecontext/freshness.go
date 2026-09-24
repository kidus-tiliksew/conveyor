package lineagecontext

import (
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func itemDescriptor(item Item) core.ContextDescriptor {
	return core.ContextDescriptor{Node: item.Node, ArtifactID: item.ArtifactID, ContentDigest: core.ContextBytesDigest([]byte(item.Content)), SourceEventID: item.SourceEventID, EdgePath: item.EdgePath, SelectionReason: item.SelectionReason, Origin: item.Origin}
}
func artifactDescriptor(a core.Artifact, graph core.LineageTraversal, task string) core.ContextDescriptor {
	node := artifactNode(a)
	path := graph.Paths[node]
	reason := "adjacent_evidence"
	if a.TaskID == task {
		reason = "task_local_artifact"
		if a.EligibleVerificationEvidence() {
			reason = "direct_task_verification_evidence"
		}
	}
	d := core.ContextDescriptor{Attached: true, Available: true, Node: node, ArtifactID: a.ID, ContentDigest: a.ID, Role: a.Role, ContentType: a.ContentType, SizeBytes: a.SizeBytes, EdgePath: path, SelectionReason: reason, Origin: "source"}
	if len(path) > 0 {
		d.SourceEventID = path[len(path)-1].CreatedByEventID
	}
	return d
}
func selectionSnapshot(workspace string, roots []core.LineageNode, task string, evidence bool, r Result, omissions []core.ContextDescriptor) core.ContextSnapshot {
	ordered := append([]core.LineageNode{}, roots...)
	sort.Slice(ordered, func(i, j int) bool {
		return string(ordered[i].Type)+"/"+ordered[i].ID < string(ordered[j].Type)+"/"+ordered[j].ID
	})
	var items, artifacts []core.ContextDescriptor
	for _, i := range r.Items {
		items = append(items, itemDescriptor(i))
	}
	for _, a := range r.Artifacts {
		artifacts = append(artifacts, artifactDescriptor(a, r.Traversal, task))
	}
	s := core.ContextSnapshot{Schema: 1, Workspace: workspace, TaskID: task, Roots: ordered, IncludeLocalEvidence: evidence, Budget: r.Budget, ItemsDigest: core.ContextDigest(items), Artifacts: core.DescribeContext(artifacts, r.Budget.ArtifactRefs), Omissions: core.DescribeContext(omissions, 64), IncompleteCoverage: r.Traversal.Truncated, OmittedCount: r.OmittedCount, ExhaustionReasons: r.ExhaustionReasons}
	// Digests cover every selected/known omitted descriptor, including overflow.
	s.Seal()
	return s
}
