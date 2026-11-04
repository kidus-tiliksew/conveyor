package core

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// DecisionSummaryMaxRunes and DecisionSummaryMaxBytes bound one decision
	// summary, visible ellipsis included, so multibyte statements cannot
	// multiply the list's size (req-design-delivery AC-1.3; DEC-103).
	DecisionSummaryMaxRunes = 240
	DecisionSummaryMaxBytes = 240

	decisionSummaryEllipsis = "…"
)

// GovernanceDesignGroup names why a design is listed on a work order. The
// groups render in declaration order (req-design-delivery AC-1.2).
type GovernanceDesignGroup string

const (
	GovernanceDesignAttached   GovernanceDesignGroup = "attached"
	GovernanceDesignDiffScoped GovernanceDesignGroup = "diff_scoped"
	GovernanceDesignRepository GovernanceDesignGroup = "repository"
)

// GovernanceDesignReference names one design version without its body.
type GovernanceDesignReference struct {
	ID                 string                `json:"id"`
	Title              string                `json:"title"`
	Version            int                   `json:"version"`
	Group              GovernanceDesignGroup `json:"group,omitempty"`
	PinnedAtAttachment bool                  `json:"pinned_at_attachment,omitempty"`
	Archived           bool                  `json:"archived,omitempty"`
	SupersededBy       []string              `json:"superseded_by,omitempty"`
}

// GovernanceDecisionReference names one decision without its record. Only a
// confirmed decision carries a summary; a superseded one keeps the identity
// and status its citation diagnostics need and is never active authority.
type GovernanceDecisionReference struct {
	ID           string         `json:"id"`
	Status       DecisionStatus `json:"status"`
	Summary      string         `json:"summary,omitempty"`
	SupersededBy string         `json:"superseded_by,omitempty"`
}

// GovernanceReferences is the outbound list a work order delivers in place of
// GovernanceSnapshot, which stays complete on the server for verdict
// validation and freshness comparison (req-design-delivery REQ-1; DEC-103).
type GovernanceReferences struct {
	Designs                []GovernanceDesignReference   `json:"designs"`
	Decisions              []GovernanceDecisionReference `json:"decisions"`
	PendingDesignProposals []PendingSystemDesignProposal `json:"pending_design_proposals,omitempty"`
	ResolutionNotes        []string                      `json:"resolution_notes,omitempty"`
}

// GovernanceDesignProvenance is what a task's attachment history says about
// its designs: operator or intake attachments in attachment order, and the
// designs a submission attached because its diff reached them.
type GovernanceDesignProvenance struct {
	Attached   []string
	DiffScoped []string
}

// DecisionSummary derives the deterministic one-line summary of a decision
// statement: whitespace is normalized, and a statement over the bound is cut
// at a code point with a visible ellipsis counted within both bounds
// (req-design-delivery AC-1.3).
func DecisionSummary(statement string) string {
	line := strings.Join(strings.Fields(strings.ToValidUTF8(statement, "\uFFFD")), " ")
	if len(line) <= DecisionSummaryMaxBytes && utf8.RuneCountInString(line) <= DecisionSummaryMaxRunes {
		return line
	}
	keepBytes := DecisionSummaryMaxBytes - len(decisionSummaryEllipsis)
	keepRunes := DecisionSummaryMaxRunes - utf8.RuneCountInString(decisionSummaryEllipsis)
	cut, runes := 0, 0
	for index := range line {
		if index > keepBytes || runes > keepRunes {
			break
		}
		cut = index
		runes++
	}
	return strings.TrimRight(line[:cut], " ") + decisionSummaryEllipsis
}

// ProjectGovernanceReferences builds the delivered list from a snapshot and
// never writes to it. Versions come from the snapshot, so a pinned order names
// its pins. Attached designs keep attachment order and win an overlap; the
// other groups sort by ID. A design in diffScoped or covered by changedPaths
// is diff-scoped; with neither, that group is absent
// (req-design-delivery AC-1.2, AC-1.4).
func ProjectGovernanceReferences(snapshot GovernanceSnapshot, repository string, provenance GovernanceDesignProvenance, changedPaths []string) GovernanceReferences {
	inDiff := make(map[string]bool, len(provenance.DiffScoped))
	for _, id := range provenance.DiffScoped {
		inDiff[id] = true
	}
	for _, match := range ResolveGovernedDesigns(snapshot.Designs, repository, changedPaths) {
		inDiff[match.Design.ID] = true
	}
	attachedRank := make(map[string]int, len(provenance.Attached))
	for _, id := range provenance.Attached {
		if _, seen := attachedRank[id]; !seen {
			attachedRank[id] = len(attachedRank)
		}
	}
	designs := make([]GovernanceDesignReference, 0, len(snapshot.Designs))
	seen := make(map[string]bool, len(snapshot.Designs))
	for _, design := range snapshot.Designs {
		if seen[design.ID] {
			continue
		}
		seen[design.ID] = true
		group := GovernanceDesignRepository
		if _, attached := attachedRank[design.ID]; attached {
			group = GovernanceDesignAttached
		} else if inDiff[design.ID] {
			group = GovernanceDesignDiffScoped
		}
		designs = append(designs, GovernanceDesignReference{ID: design.ID, Title: design.Title, Version: design.Version, Group: group,
			PinnedAtAttachment: design.PinnedAtAttachment, Archived: design.Archived, SupersededBy: append([]string(nil), design.SupersededBy...)})
	}
	groupRank := map[GovernanceDesignGroup]int{GovernanceDesignAttached: 0, GovernanceDesignDiffScoped: 1, GovernanceDesignRepository: 2}
	sort.SliceStable(designs, func(i, j int) bool {
		left, right := designs[i], designs[j]
		if left.Group != right.Group {
			return groupRank[left.Group] < groupRank[right.Group]
		}
		if left.Group == GovernanceDesignAttached {
			return attachedRank[left.ID] < attachedRank[right.ID]
		}
		return left.ID < right.ID
	})
	decisions := make([]GovernanceDecisionReference, 0, len(snapshot.Decisions))
	for _, decision := range sortedGovernanceDecisions(snapshot.Decisions) {
		reference := GovernanceDecisionReference{ID: decision.ID, Status: decision.Status, SupersededBy: decision.SupersededBy}
		if decision.Status == DecisionConfirmed {
			reference.Summary = DecisionSummary(decision.Statement)
		}
		decisions = append(decisions, reference)
	}
	pending := append([]PendingSystemDesignProposal(nil), snapshot.PendingDesignProposals...)
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].DocumentID != pending[j].DocumentID {
			return pending[i].DocumentID < pending[j].DocumentID
		}
		return pending[i].Version < pending[j].Version
	})
	return GovernanceReferences{Designs: designs, Decisions: decisions, PendingDesignProposals: pending, ResolutionNotes: append([]string(nil), snapshot.ResolutionNotes...)}
}

// GovernanceEventReferences is the identity-only list a new work_order.claimed
// or work_order.updated event carries: IDs, titles, versions, and statuses,
// with no summary, so a pinned order's events stay small. Events hold no
// attachment history, so designs sort by ID without groups. The persisted
// snapshot, named by its digest, stays the authority source.
func GovernanceEventReferences(snapshot GovernanceSnapshot) GovernanceReferences {
	designs := make([]GovernanceDesignReference, 0, len(snapshot.Designs))
	for _, design := range snapshot.Designs {
		designs = append(designs, GovernanceDesignReference{ID: design.ID, Title: design.Title, Version: design.Version})
	}
	sort.Slice(designs, func(i, j int) bool { return designs[i].ID < designs[j].ID })
	decisions := make([]GovernanceDecisionReference, 0, len(snapshot.Decisions))
	for _, decision := range sortedGovernanceDecisions(snapshot.Decisions) {
		decisions = append(decisions, GovernanceDecisionReference{ID: decision.ID, Status: decision.Status, SupersededBy: decision.SupersededBy})
	}
	return GovernanceReferences{Designs: designs, Decisions: decisions}
}

func sortedGovernanceDecisions(decisions []Decision) []Decision {
	sorted := append([]Decision(nil), decisions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return sorted
}

// OutboundWorkOrder is the order as a response carries it: the pinned snapshot
// bodies stay on the server, and the digest still names them. The copy shares
// nothing it changes, so the caller's order keeps its pins (DEC-103).
func OutboundWorkOrder(order WorkOrder) WorkOrder {
	order.GovernanceSnapshot = nil
	order.ServedRequirementSnapshot = nil
	return order
}

// OutboundContextFreshness keeps the digests, revisions, and comparison a
// client needs and drops the current authority references, which would list
// every design a second time. Prior references stay only where they differ
// from the current ones, so a detected change still names what moved.
func OutboundContextFreshness(freshness ContextFreshness) ContextFreshness {
	current := make(map[string]bool, len(freshness.Snapshot.AuthorityReferences))
	for _, reference := range freshness.Snapshot.AuthorityReferences {
		current[reference] = true
	}
	var prior []string
	for _, reference := range freshness.PriorAuthorityReferences {
		if !current[reference] {
			prior = append(prior, reference)
		}
	}
	freshness.PriorAuthorityReferences = prior
	freshness.Snapshot.AuthorityReferences = nil
	return freshness
}

// AppendJSONObjectField adds one rendered `"key":value` to a JSON object. A
// payload that is not an object is returned unchanged.
func AppendJSONObjectField(payload []byte, field string) []byte {
	trimmed := bytes.TrimRight(payload, " \n")
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return payload
	}
	separator := ","
	if len(bytes.TrimSpace(trimmed[1:len(trimmed)-1])) == 0 {
		separator = ""
	}
	extended := append([]byte(nil), trimmed[:len(trimmed)-1]...)
	extended = append(extended, separator...)
	extended = append(extended, field...)
	return append(extended, '}')
}

// DiffChangedPaths reads the changed paths from a unified diff's file headers,
// so a review order's list can apply the governed-scope resolver to the diff
// it already holds. Hunk bodies are skipped, because a changed line can look
// like a header. Git C-quotes a path holding a tab, quote, backslash, or
// non-ASCII byte; quoted names are decoded.
func DiffChangedPaths(diff string) []string {
	seen := map[string]bool{}
	add := func(raw, prefix string) {
		path, ok := decodeDiffPath(raw, prefix)
		if ok && path != "" {
			seen[path] = true
		}
	}
	inHeader := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHeader = true
			rest := strings.TrimPrefix(line, "diff --git ")
			if strings.HasPrefix(rest, `"`) {
				if first, err := strconv.QuotedPrefix(rest); err == nil {
					add(first, "a/")
					add(strings.TrimPrefix(rest[len(first):], " "), "b/")
				}
			} else if plain := strings.TrimPrefix(rest, "a/"); plain != rest {
				if half := (len(plain) - len(" b/")) / 2; half > 0 && plain[half:] == " b/"+plain[:half] {
					add(plain[:half], "")
				}
			}
		case strings.HasPrefix(line, "@@"):
			inHeader = false
		case !inHeader:
		case strings.HasPrefix(line, "--- "):
			add(strings.TrimPrefix(line, "--- "), "a/")
		case strings.HasPrefix(line, "+++ "):
			add(strings.TrimPrefix(line, "+++ "), "b/")
		case strings.HasPrefix(line, "rename from "):
			add(strings.TrimPrefix(line, "rename from "), "")
		case strings.HasPrefix(line, "rename to "):
			add(strings.TrimPrefix(line, "rename to "), "")
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// decodeDiffPath unquotes one header name and removes its side prefix. It
// refuses /dev/null, a name without the prefix, and a malformed quoted name.
func decodeDiffPath(raw, prefix string) (string, bool) {
	path := raw
	if strings.HasPrefix(raw, `"`) {
		decoded, err := strconv.Unquote(raw)
		if err != nil {
			return "", false
		}
		path = decoded
	}
	if path == "/dev/null" || !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(path, prefix), true
}
