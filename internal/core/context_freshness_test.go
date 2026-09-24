package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestContextDeltaAndEnvelopeBounds(t *testing.T) {
	var descriptors []ContextDescriptor
	for i := 0; i < 150; i++ {
		descriptors = append(descriptors, ContextDescriptor{ArtifactID: fmt.Sprint(i), Node: LineageNode{Type: LineageTask, ID: "task"}, SelectionReason: "task_local_artifact", Origin: "source", EdgePath: []LineageLink{{SrcID: strings.Repeat("p", 2048), DstID: strings.Repeat("q", 2048)}}})
	}
	list := DescribeContext(descriptors, 64)
	if list.Count != 150 || len(list.Items) != 64 || !list.Truncated {
		t.Fatal(list)
	}
	snapshot := ContextSnapshot{Schema: 1, Artifacts: list, Omissions: DescribeContext(descriptors, 64)}
	snapshot.Seal()
	revision := snapshot.Revision
	f := CompareContext(snapshot, &snapshot)
	f.Additions = list
	f.Removals = list
	f.Changes = list
	f = BoundContextFreshness(f)
	b, _ := json.Marshal(f)
	if len(b) > ContextEnvelopeBytes || !f.Truncated || f.SelectionRevision != revision || f.Snapshot.Artifacts.Count != 150 || f.Snapshot.Artifacts.Digest != list.Digest {
		t.Fatalf("invalid bound: %d", len(b))
	}
	if f.ComparisonStatus != "unchanged" {
		t.Fatal(f.ComparisonStatus)
	}
	unknown := CompareContext(snapshot, nil)
	if unknown.ComparisonStatus != "unavailable" || unknown.Additions.Count != 0 {
		t.Fatal("invented unknown-baseline additions")
	}
	// Replacing overflow identities cannot be hidden by a stable returned prefix.
	descriptors[149].ArtifactID = "replacement"
	changed := DescribeContext(descriptors, 64)
	if changed.Digest == list.Digest {
		t.Fatal("overflow identity not hashed")
	}
}
func TestContextDeltaTracksProvenanceAndOmission(t *testing.T) {
	d := ContextDescriptor{ArtifactID: "a", Node: LineageNode{Type: LineageTask, ID: "task"}, SelectionReason: "task_local_artifact", Origin: "source"}
	old := ContextSnapshot{Schema: 1, Artifacts: DescribeContext([]ContextDescriptor{d}, 64)}
	old.Seal()
	next := old
	d.SourceEventID = 7
	next.Artifacts = DescribeContext([]ContextDescriptor{d}, 64)
	next.Seal()
	f := CompareContext(next, &old)
	if f.Changes.Count != 1 || f.Additions.Count != 0 || f.Removals.Count != 0 {
		t.Fatal(f)
	}
	next.Artifacts = DescribeContext(nil, 64)
	d.OmissionReason = "renderable_bytes"
	next.Omissions = DescribeContext([]ContextDescriptor{d}, 64)
	next.Seal()
	f = CompareContext(next, &old)
	if f.Changes.Count != 1 {
		t.Fatal("omission classification not changed")
	}
}
