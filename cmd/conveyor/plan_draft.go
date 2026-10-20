package main

// Local planning draft reader and request builder (feature-local-planning-drafts
// LP-2, LP-4, LP-5; req-local-planning REQ-1, REQ-2; DEC-69). The reader opens
// no network connection and changes no file. BuildPlanItemRequest is the only
// source of the payload that both the approval digest and `conveyor plan push`
// use, so an approval binds exactly what push would send.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"gopkg.in/yaml.v3"
)

// Draft layers in push and citation order (LP-2).
const (
	PlanLayerRequirements  = "requirements"
	PlanLayerDecisions     = "decisions"
	PlanLayerSystemDesigns = "system-designs"
	PlanLayerTasks         = "tasks"
)

// PlanLayers lists the layers in the order push sends them and citations may
// point (requirements, decisions, System Design documents, tasks).
var PlanLayers = []string{PlanLayerRequirements, PlanLayerDecisions, PlanLayerSystemDesigns, PlanLayerTasks}

// PlanItemKind names an item's corpus or task kind in the digest envelope.
type PlanItemKind string

const (
	PlanKindRequirement  PlanItemKind = "requirement"
	PlanKindDecision     PlanItemKind = "decision"
	PlanKindSystemDesign PlanItemKind = "system_design"
	PlanKindTask         PlanItemKind = "task"
)

const (
	PlanModeCreate = "create"
	PlanModeRevise = "revise"

	planDecisionIDPrefix = "draft:dec/"
	planTaskIDPrefix     = "draft:task/"
	planStateDir         = ".plan"
	planStateFile        = "state.json"
	planBaseSuffix       = ".base.md"

	// Rule labels for diagnostics that come from the draft layout and the
	// factory content rules rather than one acceptance criterion.
	planRuleLayout  = "LP-2"
	planRuleContent = "LP-3"
	// planDraftItemID names diagnostics about the draft directory itself.
	planDraftItemID = "draft"
)

var planLocalNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// PlanDiagnostic is one check result. Warning results never fail a check.
type PlanDiagnostic struct {
	ItemID  string
	Rule    string
	Reason  string
	Warning bool
}

// String renders the diagnostic as one line: `<item-id>: <rule>: <reason>`,
// prefixed with `warning: ` for a warning. Line breaks inside a factory
// reason are escaped so each diagnostic stays on one line (AC-1.1).
func (d PlanDiagnostic) String() string {
	reason := strings.NewReplacer("\r\n", `\n`, "\n", `\n`, "\r", `\r`).Replace(d.Reason)
	line := fmt.Sprintf("%s: %s: %s", d.ItemID, d.Rule, reason)
	if d.Warning {
		return "warning: " + line
	}
	return line
}

// PlanDecisionFields holds a decision item's YAML fields. Supersedes keeps
// the raw node so check can refuse a list or a draft-local ID (AC-1.12).
type PlanDecisionFields struct {
	Statement            string
	Context              string
	AlternativesRejected string
	Supersedes           *yaml.Node
}

// PlanTaskFields holds a task item's front matter. Gates stay pointers so an
// omitted gate differs from an explicit false (AC-3.7).
type PlanTaskFields struct {
	Repo            string
	BaseBranch      string
	DependsOn       []string
	RequirementIDs  []string
	SystemDesignIDs []string
	PlanApproval    *bool
	MergeApproval   *bool
}

// PlanItem is one draft item read from its LP-2 path.
type PlanItem struct {
	ID    string
	Kind  PlanItemKind
	Layer string
	// Path is the item file relative to the draft root, slash-separated.
	Path string
	// Name is the file stem: a document ID or a draft-local name.
	Name        string
	Mode        string
	BaseVersion int
	// HasBase reports that the `.base.md` companion exists; Base holds its
	// exact bytes.
	HasBase bool
	Base    []byte
	// Content is the text after the front matter with CRLF normalized to LF:
	// a document body or a task body.
	Content  string
	Title    string
	Category string
	Decision PlanDecisionFields
	Task     PlanTaskFields
	// Invalid holds the first layout problem; an invalid item keeps its ID so
	// citations to it resolve, but it has no request or digest.
	Invalid error
}

// PlanDraft is a whole draft directory with its local state.
type PlanDraft struct {
	Root  string
	Items []PlanItem
	State PlanState
}

// Item returns the items carrying id; more than one means the ID is ambiguous.
func (d *PlanDraft) Item(id string) []PlanItem {
	var found []PlanItem
	for _, item := range d.Items {
		if item.ID == id {
			found = append(found, item)
		}
	}
	return found
}

// PlanItemRequest describes the request push would send for an item. It
// sends nothing. For a task, Multipart is true and Body is the JSON `task`
// part; push adds the `idempotency_key` form part. A decision's `id` holds
// its draft-local ID until push replaces it with the chosen DEC-n.
type PlanItemRequest struct {
	Method    string
	Path      string
	Body      map[string]any
	Multipart bool
}

type planDocumentFrontMatter struct {
	Mode        string `yaml:"mode"`
	BaseVersion *int   `yaml:"base_version"`
	Title       string `yaml:"title"`
	Category    string `yaml:"category"`
}

type planTaskFrontMatter struct {
	Repo            string   `yaml:"repo"`
	BaseBranch      string   `yaml:"base_branch"`
	DependsOn       []string `yaml:"depends_on"`
	RequirementIDs  []string `yaml:"requirement_ids"`
	SystemDesignIDs []string `yaml:"system_design_ids"`
	PlanApproval    *bool    `yaml:"plan_approval"`
	MergeApproval   *bool    `yaml:"merge_approval"`
}

type planDecisionFile struct {
	Statement            string    `yaml:"statement"`
	Context              string    `yaml:"context"`
	AlternativesRejected string    `yaml:"alternatives_rejected"`
	Supersedes           yaml.Node `yaml:"supersedes"`
}

// ReadPlanDraft reads the LP-2 layout under dir. Item-level problems become
// diagnostics and reading continues; the error reports an unreadable draft
// root, an unreadable item file, or an unreadable state file. Every path
// outside the layout, including notes/**, is ignored (AC-2.4).
func ReadPlanDraft(ctx context.Context, dir string) (*PlanDraft, []PlanDiagnostic, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, fmt.Errorf("read draft: %w", err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("read draft: %s is not a directory", root)
	}
	draft := &PlanDraft{Root: root}
	var diagnostics []PlanDiagnostic
	for _, layer := range PlanLayers {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		items, layerDiagnostics, err := readPlanLayer(root, layer)
		if err != nil {
			return nil, nil, err
		}
		draft.Items = append(draft.Items, items...)
		diagnostics = append(diagnostics, layerDiagnostics...)
	}
	seen := map[string]int{}
	for _, item := range draft.Items {
		seen[item.ID]++
	}
	for index := range draft.Items {
		if count := seen[draft.Items[index].ID]; count > 1 {
			err := fmt.Errorf("item ID is defined by %d files; rename one of them", count)
			if draft.Items[index].Invalid == nil {
				draft.Items[index].Invalid = err
			}
			diagnostics = append(diagnostics, PlanDiagnostic{ItemID: draft.Items[index].ID, Rule: planRuleLayout, Reason: fmt.Sprintf("%s: %v", draft.Items[index].Path, err)})
		}
	}
	state, err := LoadPlanState(root)
	if err != nil {
		return nil, nil, err
	}
	draft.State = state
	return draft, diagnostics, nil
}

func readPlanLayer(root, layer string) ([]PlanItem, []PlanDiagnostic, error) {
	entries, err := os.ReadDir(filepath.Join(root, layer))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read draft layer %s: %w", layer, err)
	}
	extension := ".md"
	if layer == PlanLayerDecisions {
		extension = ".yaml"
	}
	var items []PlanItem
	var diagnostics []PlanDiagnostic
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, extension) || strings.HasSuffix(name, planBaseSuffix) {
			continue
		}
		stem := strings.TrimSuffix(name, extension)
		data, err := os.ReadFile(filepath.Join(root, layer, name))
		if err != nil {
			return nil, nil, fmt.Errorf("read draft item %s/%s: %w", layer, name, err)
		}
		item := PlanItem{Layer: layer, Path: layer + "/" + name, Name: stem}
		switch layer {
		case PlanLayerRequirements, PlanLayerSystemDesigns:
			item.ID, item.Kind = stem, PlanKindRequirement
			if layer == PlanLayerSystemDesigns {
				item.Kind = PlanKindSystemDesign
			}
			item.Invalid = parsePlanDocumentItem(&item, data)
			base, err := os.ReadFile(filepath.Join(root, layer, stem+planBaseSuffix))
			switch {
			case err == nil:
				item.HasBase, item.Base = true, base
			case !errors.Is(err, os.ErrNotExist):
				return nil, nil, fmt.Errorf("read draft base copy %s/%s%s: %w", layer, stem, planBaseSuffix, err)
			}
			if item.Invalid == nil && item.Mode == PlanModeCreate && item.HasBase {
				item.Invalid = fmt.Errorf("a created document has no base copy; remove %s%s or set mode: revise", stem, planBaseSuffix)
			}
		case PlanLayerDecisions:
			item.ID, item.Kind = planDecisionIDPrefix+stem, PlanKindDecision
			item.Invalid = parsePlanDecisionItem(&item, data)
		case PlanLayerTasks:
			item.ID, item.Kind = planTaskIDPrefix+stem, PlanKindTask
			item.Invalid = parsePlanTaskItem(&item, data)
		}
		if (item.Kind == PlanKindDecision || item.Kind == PlanKindTask) && !planLocalNamePattern.MatchString(stem) {
			item.Invalid = fmt.Errorf("draft-local name %q must match [a-z0-9][a-z0-9-]*", stem)
		}
		if item.Invalid != nil {
			diagnostics = append(diagnostics, PlanDiagnostic{ItemID: item.ID, Rule: planRuleLayout, Reason: fmt.Sprintf("%s: %v", item.Path, item.Invalid)})
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, diagnostics, nil
}

// splitPlanFrontMatter separates a `---` delimited YAML front matter block
// from the text after it. CRLF is normalized to LF first.
func splitPlanFrontMatter(data []byte) (frontMatter string, body string, present bool, err error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return "", text, false, nil
	}
	rest := text[len("---\n"):]
	if strings.HasPrefix(rest, "---\n") || rest == "---" {
		return "", strings.TrimPrefix(strings.TrimPrefix(rest, "---"), "\n"), true, nil
	}
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		if strings.HasSuffix(rest, "\n---") {
			return rest[:len(rest)-len("\n---")], "", true, nil
		}
		return "", "", true, fmt.Errorf("front matter opened with --- is not closed by a --- line")
	}
	return rest[:end], rest[end+len("\n---\n"):], true, nil
}

func decodePlanYAML(text string, target any) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	decoder := yaml.NewDecoder(strings.NewReader(text))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	// Only a clean end of input ends the item. A malformed or additional
	// document after the first is a parse failure, never a silently
	// shortened item.
	var extra yaml.Node
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("trailing YAML document: %w", err)
	}
	return fmt.Errorf("holds more than one YAML document")
}

func parsePlanDocumentItem(item *PlanItem, data []byte) error {
	frontMatter, body, present, err := splitPlanFrontMatter(data)
	item.Content = body
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("front matter with mode (create or revise) is required")
	}
	var fields planDocumentFrontMatter
	if err := decodePlanYAML(frontMatter, &fields); err != nil {
		return fmt.Errorf("front matter: %w", err)
	}
	item.Mode, item.Title, item.Category = strings.TrimSpace(fields.Mode), strings.TrimSpace(fields.Title), strings.TrimSpace(fields.Category)
	if fields.BaseVersion != nil {
		item.BaseVersion = *fields.BaseVersion
	}
	switch item.Mode {
	case PlanModeCreate:
		if fields.BaseVersion != nil {
			return fmt.Errorf("base_version applies only to mode: revise")
		}
	case PlanModeRevise:
		if item.Title != "" || item.Category != "" {
			return fmt.Errorf("title and category apply only to a created System Design document")
		}
	default:
		return fmt.Errorf("front matter mode must be create or revise, not %q", fields.Mode)
	}
	if item.Kind == PlanKindRequirement && (item.Title != "" || item.Category != "") {
		return fmt.Errorf("a requirement takes its title from its H1 heading; remove title and category")
	}
	return nil
}

func parsePlanDecisionItem(item *PlanItem, data []byte) error {
	var fields planDecisionFile
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if err := decodePlanYAML(text, &fields); err != nil {
		return fmt.Errorf("decision YAML: %w", err)
	}
	item.Decision = PlanDecisionFields{
		Statement:            strings.TrimSpace(fields.Statement),
		Context:              strings.TrimSpace(fields.Context),
		AlternativesRejected: strings.TrimSpace(fields.AlternativesRejected),
	}
	if fields.Supersedes.Kind != 0 {
		node := fields.Supersedes
		item.Decision.Supersedes = &node
	}
	return nil
}

func parsePlanTaskItem(item *PlanItem, data []byte) error {
	frontMatter, body, _, err := splitPlanFrontMatter(data)
	item.Content = body
	if err != nil {
		return err
	}
	var fields planTaskFrontMatter
	if err := decodePlanYAML(frontMatter, &fields); err != nil {
		return fmt.Errorf("front matter: %w", err)
	}
	item.Task = PlanTaskFields{
		Repo:            strings.TrimSpace(fields.Repo),
		BaseBranch:      strings.TrimSpace(fields.BaseBranch),
		DependsOn:       trimPlanList(fields.DependsOn),
		RequirementIDs:  trimPlanList(fields.RequirementIDs),
		SystemDesignIDs: trimPlanList(fields.SystemDesignIDs),
		PlanApproval:    fields.PlanApproval,
		MergeApproval:   fields.MergeApproval,
	}
	return nil
}

func trimPlanList(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, strings.TrimSpace(value))
	}
	return result
}

// planRequirementContent normalizes a requirement item the way the factory
// stores it: CRLF becomes LF and ParseRequirementDocument trims the document
// edges (conveyor:internal/httpapi/requirements.go#operatorRequirementVersion).
func planRequirementContent(content string) (pipeline.RequirementDocument, error) {
	return pipeline.ParseRequirementDocument(strings.ReplaceAll(content, "\r\n", "\n"))
}

// planSupersedes returns the scalar supersedes value; ok is false when the
// node is absent.
func planSupersedes(node *yaml.Node) (value string, ok bool, err error) {
	if node == nil {
		return "", false, nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return "", true, fmt.Errorf("supersedes must name one DEC-n, not a %s", planYAMLKind(node))
	}
	return strings.TrimSpace(node.Value), true, nil
}

func planYAMLKind(node *yaml.Node) string {
	switch {
	case node.Kind == yaml.SequenceNode:
		return "list"
	case node.Kind == yaml.MappingNode:
		return "mapping"
	case node.Tag == "!!null":
		return "null value"
	default:
		return "non-scalar value"
	}
}

// BuildPlanItemRequest is the single source of the request push sends for an
// item (LP-5). Content is normalized with the factory's rules; draft-local IDs
// stay in place; optional task gates appear only when the item states them.
func BuildPlanItemRequest(item PlanItem) (PlanItemRequest, error) {
	if item.Invalid != nil {
		return PlanItemRequest{}, fmt.Errorf("%s: %w", item.ID, item.Invalid)
	}
	switch item.Kind {
	case PlanKindRequirement:
		document, err := planRequirementContent(item.Content)
		if err != nil {
			return PlanItemRequest{}, fmt.Errorf("%s: requirement content: %w", item.ID, err)
		}
		if item.Mode == PlanModeRevise {
			return PlanItemRequest{Method: "POST", Path: "/v1/requirements/" + item.ID + "/versions", Body: map[string]any{"content": document.Markdown}}, nil
		}
		title, _ := core.DocumentHeadingTitle(document.Markdown)
		return PlanItemRequest{Method: "POST", Path: "/v1/requirements", Body: map[string]any{
			"id": item.ID, "title": title, "content": document.Markdown, "origin": string(core.RequirementOriginOperator),
		}}, nil
	case PlanKindSystemDesign:
		content := core.NormalizeSystemDesignContent(item.Content)
		if item.Mode == PlanModeRevise {
			return PlanItemRequest{Method: "POST", Path: "/v1/system-designs/" + item.ID + "/versions", Body: map[string]any{"content": content}}, nil
		}
		return PlanItemRequest{Method: "POST", Path: "/v1/system-designs", Body: map[string]any{
			"id": item.ID, "title": item.Title, "category": item.Category, "content": content,
		}}, nil
	case PlanKindDecision:
		body := map[string]any{
			"id":                    item.ID,
			"statement":             item.Decision.Statement,
			"context":               item.Decision.Context,
			"alternatives_rejected": item.Decision.AlternativesRejected,
		}
		supersedes, present, err := planSupersedes(item.Decision.Supersedes)
		if err != nil {
			return PlanItemRequest{}, fmt.Errorf("%s: %w", item.ID, err)
		}
		if present {
			body["supersedes"] = supersedes
		}
		return PlanItemRequest{Method: "POST", Path: "/v1/decisions", Body: body}, nil
	case PlanKindTask:
		body := map[string]any{
			"body":              strings.TrimSpace(item.Content),
			"repo":              item.Task.Repo,
			"depends_on":        planStringList(item.Task.DependsOn),
			"requirement_ids":   planStringList(item.Task.RequirementIDs),
			"system_design_ids": planStringList(item.Task.SystemDesignIDs),
		}
		if item.Task.BaseBranch != "" {
			body["base_branch"] = item.Task.BaseBranch
		}
		if item.Task.PlanApproval != nil {
			body["spec_approval"] = *item.Task.PlanApproval
		}
		if item.Task.MergeApproval != nil {
			body["merge_approval"] = *item.Task.MergeApproval
		}
		return PlanItemRequest{Method: "POST", Path: "/v1/tasks", Body: body, Multipart: true}, nil
	}
	return PlanItemRequest{}, fmt.Errorf("%s: unknown item kind %q", item.ID, item.Kind)
}

func planStringList(values []string) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

// planItemBaseText returns the base copy as normalized text.
func planItemBaseText(item PlanItem) string {
	return string(bytes.ReplaceAll(item.Base, []byte("\r\n"), []byte("\n")))
}
