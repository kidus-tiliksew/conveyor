package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Context freshness is observational, never authority (req-260802-72fc68
// REQ-2/REQ-5; component-lineage CF-L1; component-work-orders CF-W1–W3).
const ContextDescriptorLimit = 64
const ContextEnvelopeBytes = 64 * 1024

type ContextBudget struct {
	Depth           int `json:"depth"`
	Nodes           int `json:"nodes"`
	Links           int `json:"links"`
	RenderableBytes int `json:"renderable_bytes"`
	ArtifactRefs    int `json:"artifact_refs"`
	AuthorityNodes  int `json:"authority_nodes"`
}

type ContextDescriptor struct {
	Attached        bool          `json:"attached"`
	Available       bool          `json:"available"`
	Node            LineageNode   `json:"node"`
	ArtifactID      string        `json:"artifact_id,omitempty"`
	ContentDigest   string        `json:"content_digest,omitempty"`
	Role            ArtifactRole  `json:"role,omitempty"`
	ContentType     string        `json:"content_type,omitempty"`
	SizeBytes       int64         `json:"size_bytes,omitempty"`
	SourceEventID   int64         `json:"source_event_id,omitempty"`
	EdgePath        []LineageLink `json:"edge_path,omitempty"`
	SelectionReason string        `json:"selection_reason"`
	OmissionReason  string        `json:"omission_reason,omitempty"`
	Origin          string        `json:"origin"`
}

// A list carries a digest of ALL known entries even when its returned prefix
// is truncated. Unknown graph nodes are reported separately as incomplete.
type ContextDescriptors struct {
	Items     []ContextDescriptor `json:"items"`
	Count     int                 `json:"count"`
	Digest    string              `json:"digest"`
	Truncated bool                `json:"truncated"`
}

func DescribeContext(items []ContextDescriptor, limit int) ContextDescriptors {
	if limit < 0 {
		limit = 0
	}
	if limit > ContextDescriptorLimit {
		limit = ContextDescriptorLimit
	}
	n := min(len(items), limit)
	return ContextDescriptors{Items: append([]ContextDescriptor{}, items[:n]...), Count: len(items), Digest: ContextDigest(items), Truncated: n < len(items)}
}
func ContextDigest(v any) string         { b, _ := json.Marshal(v); return ContextBytesDigest(b) }
func ContextBytesDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func ValidContextRevision(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, e := hex.DecodeString(s)
	return e == nil && hex.EncodeToString(b) == s
}

type ContextSnapshot struct {
	Schema               int                `json:"schema"`
	Revision             string             `json:"revision"`
	Workspace            string             `json:"workspace"`
	TaskID               string             `json:"task_id"`
	Roots                []LineageNode      `json:"roots"`
	IncludeLocalEvidence bool               `json:"include_local_evidence"`
	Budget               ContextBudget      `json:"budget"`
	ItemsDigest          string             `json:"items_digest"`
	Artifacts            ContextDescriptors `json:"artifacts"`
	Omissions            ContextDescriptors `json:"omissions"`
	IncompleteCoverage   bool               `json:"incomplete_coverage"`
	OmittedCount         int                `json:"omitted_count"`
	ExhaustionReasons    []string           `json:"exhaustion_reasons"`
	AuthoritySource      string             `json:"authority_source,omitempty"`
	AuthorityReferences  []string           `json:"authority_references,omitempty"`
	AuthorityDigest      string             `json:"authority_digest,omitempty"`
}

func (s *ContextSnapshot) Seal() { s.Revision = ""; s.Revision = ContextDigest(s) }

type ContextDelivery struct {
	ArtifactID      string `json:"artifact_id"`
	State           string `json:"state"`
	Fetched         bool   `json:"fetched"`
	Failed          bool   `json:"failed"`
	Omitted         bool   `json:"omitted"`
	Truncated       bool   `json:"truncated"`
	Acknowledgement string `json:"acknowledgement"`
}
type ContextFreshness struct {
	ObservationID             string             `json:"observation_id,omitempty"`
	SelectionSchema           int                `json:"selection_schema"`
	SelectionRevision         string             `json:"selection_revision,omitempty"`
	ObservationRevision       string             `json:"observation_revision,omitempty"`
	ObservedAt                time.Time          `json:"observed_at"`
	Snapshot                  ContextSnapshot    `json:"snapshot"`
	BaselineRevision          string             `json:"baseline_revision,omitempty"`
	ComparisonStatus          string             `json:"comparison_status"`
	Additions                 ContextDescriptors `json:"additions"`
	Removals                  ContextDescriptors `json:"removals"`
	Changes                   ContextDescriptors `json:"changes"`
	Deliveries                []ContextDelivery  `json:"deliveries"`
	UnfetchedAdditions        int                `json:"unfetched_additions"`
	ObservationRecorded       bool               `json:"observation_recorded"`
	AcknowledgementSupported  bool               `json:"acknowledgement_supported"`
	AuthorityReferenceChanged bool               `json:"authority_reference_changed"`
	PriorAuthoritySource      string             `json:"prior_authority_source,omitempty"`
	PriorAuthorityReferences  []string           `json:"prior_authority_references,omitempty"`
	PriorAuthorityDigest      string             `json:"prior_authority_digest,omitempty"`
	Diagnostic                string             `json:"diagnostic,omitempty"`
	Truncated                 bool               `json:"truncated"`
}

func CompareContext(current ContextSnapshot, prior *ContextSnapshot) ContextFreshness {
	f := ContextFreshness{SelectionSchema: 1, SelectionRevision: current.Revision, Snapshot: current, ComparisonStatus: "unavailable"}
	var added, removed, changed []ContextDescriptor
	if prior != nil {
		f.BaselineRevision = prior.Revision
		f.AuthorityReferenceChanged = current.AuthoritySource != prior.AuthoritySource || current.AuthorityDigest != prior.AuthorityDigest
		if f.AuthorityReferenceChanged {
			f.PriorAuthoritySource = prior.AuthoritySource
			f.PriorAuthorityReferences = append([]string{}, prior.AuthorityReferences...)
			f.PriorAuthorityDigest = prior.AuthorityDigest
		}
		if current.Revision == prior.Revision {
			f.ComparisonStatus = "unchanged"
		} else {
			f.ComparisonStatus = "changed"
			old := map[string]ContextDescriptor{}
			next := map[string]ContextDescriptor{}
			key := func(d ContextDescriptor) string { return string(d.Node.Type) + "/" + d.Node.ID + "/" + d.ArtifactID }
			for _, d := range append(append([]ContextDescriptor{}, prior.Artifacts.Items...), prior.Omissions.Items...) {
				old[key(d)] = d
			}
			for _, d := range append(append([]ContextDescriptor{}, current.Artifacts.Items...), current.Omissions.Items...) {
				next[key(d)] = d
				if p, ok := old[key(d)]; ok {
					if ContextDigest(p) != ContextDigest(d) {
						changed = append(changed, d)
					}
				} else {
					added = append(added, d)
				}
			}
			for _, d := range append(append([]ContextDescriptor{}, prior.Artifacts.Items...), prior.Omissions.Items...) {
				if _, ok := next[key(d)]; !ok {
					removed = append(removed, d)
				}
			}
			// A truncated baseline cannot prove that an unlisted identity is new.
			if prior.Artifacts.Truncated || prior.Omissions.Truncated || current.Artifacts.Truncated || current.Omissions.Truncated {
				f.ComparisonStatus = "unavailable"
				added = nil
				removed = nil
				changed = nil
				f.Truncated = true
			}
		}
	}
	f.Additions = DescribeContext(added, 64)
	f.Removals = DescribeContext(removed, 64)
	f.Changes = DescribeContext(changed, 64)
	return f
}

// BoundContextFreshness preserves complete list digests/counts and shortens
// prefixes deterministically. No body or URL is ever part of this envelope.
func BoundContextFreshness(f ContextFreshness) ContextFreshness {
	for {
		b, _ := json.Marshal(f)
		if len(b) <= ContextEnvelopeBytes {
			return f
		}
		f.Truncated = true
		lists := []*ContextDescriptors{&f.Changes, &f.Removals, &f.Additions, &f.Snapshot.Omissions, &f.Snapshot.Artifacts}
		reduced := false
		for _, l := range lists {
			if len(l.Items) > 0 {
				l.Items = l.Items[:len(l.Items)-1]
				l.Truncated = true
				reduced = true
				break
			}
		}
		if reduced {
			continue
		}
		if len(f.Deliveries) > 0 {
			f.Deliveries = f.Deliveries[:len(f.Deliveries)-1]
			continue
		}
		if len(f.PriorAuthorityReferences) > 0 {
			f.PriorAuthorityReferences = f.PriorAuthorityReferences[:len(f.PriorAuthorityReferences)-1]
			continue
		}
		if len(f.Snapshot.AuthorityReferences) > 0 {
			f.Snapshot.AuthorityReferences = f.Snapshot.AuthorityReferences[:len(f.Snapshot.AuthorityReferences)-1]
			continue
		}
		if len(f.Snapshot.Roots) > 0 {
			f.Snapshot.Roots = f.Snapshot.Roots[:len(f.Snapshot.Roots)-1]
			continue
		}
		f.Diagnostic = "refresh_unavailable"
		return f
	}
}

type ContextObservation struct {
	ComparisonStatus   string           `json:"comparison_status,omitempty"`
	UnfetchedAdditions int              `json:"unfetched_additions,omitempty"`
	Truncated          bool             `json:"truncated,omitempty"`
	Diagnostic         string           `json:"diagnostic,omitempty"`
	JobID              string           `json:"job_id,omitempty"`
	DeliveryBoundary   string           `json:"delivery_boundary"`
	Schema             int              `json:"schema"`
	ID                 string           `json:"id"`
	Workspace          string           `json:"workspace"`
	TaskID             string           `json:"task_id"`
	WorkOrderID        string           `json:"work_order_id"`
	AttemptID          string           `json:"attempt_id"`
	SessionID          string           `json:"session_id"`
	ActorID            string           `json:"actor_id"`
	ActorRole          ActorRole        `json:"actor_role"`
	Kind               string           `json:"kind"`
	PriorRevision      string           `json:"prior_revision,omitempty"`
	SelectionRevision  string           `json:"selection_revision"`
	Snapshot           *ContextSnapshot `json:"snapshot,omitempty"`
	ArtifactID         string           `json:"artifact_id,omitempty"`
	Outcome            string           `json:"outcome"`
	ContentDigest      string           `json:"content_digest,omitempty"`
	ReturnedBytes      int64            `json:"returned_bytes,omitempty"`
	ObservedAt         time.Time        `json:"observed_at"`
}

func (o ContextObservation) ReplayKey() string {
	o.ID = ""
	o.ObservedAt = time.Time{}
	return ContextDigest(o)
}

func WithContextAuthority(snapshot ContextSnapshot, source string, requirements []ServedRequirementContext, governance *GovernanceSnapshot) ContextSnapshot {
	snapshot.AuthoritySource = source
	for _, r := range requirements {
		snapshot.AuthorityReferences = append(snapshot.AuthorityReferences, fmt.Sprintf("requirement:%s:v%d", r.ID, r.Version))
	}
	if governance != nil {
		for _, d := range governance.Designs {
			snapshot.AuthorityReferences = append(snapshot.AuthorityReferences, fmt.Sprintf("system_design:%s:v%d", d.ID, d.Version))
		}
	}
	sort.Strings(snapshot.AuthorityReferences)
	snapshot.AuthorityDigest = ContextDigest(snapshot.AuthorityReferences)
	snapshot.Seal()
	return snapshot
}
