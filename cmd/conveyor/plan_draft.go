package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"gopkg.in/yaml.v3"
)

// planItemKind is the draft tier an item belongs to; items are pushed in this
// order (requirements, decisions, designs, tasks).
type planItemKind string

const (
	planItemRequirement planItemKind = "requirement"
	planItemDecision    planItemKind = "decision"
	planItemDesign      planItemKind = "design"
	planItemTask        planItemKind = "task"
)

// planNote is the never-hashed plain-language wrapper for one item, read from
// notes.yml so improving a headline or example never invalidates an approval.
type planNote struct {
	Headline string `yaml:"headline"`
	Example  string `yaml:"example"`
	Why      string `yaml:"why"`
}

// planItem is one reviewable and pushable draft item. ID is the item's stable
// identity: "<doc-id>/REQ-n" for a requirement, "D1" for a decision, the
// design ID for a design, and the local task ID for a task. File is the
// draft-relative path holding the item. Normative is the canonical text over
// which Hash is computed — the item's normative fields only, never its note.
// Links are the IDs this item cites.
type planItem struct {
	ID        string
	Kind      planItemKind
	File      string
	Normative string
	Links     []string
	Note      *planNote
}

// planDraft is a parsed planning draft. Items are in stable push order:
// requirements, decisions, designs, tasks, and within a kind by ID. Raw keeps
// the parsed per-file structures plan push serializes.
type planDraft struct {
	Dir   string
	Brief string
	Items []planItem
	Raw   planDraftFiles
}

// planDraftFiles holds the parsed form of every normative draft file so plan
// push can emit proposal payloads without re-reading or re-parsing. Each
// slice is in file order (requirements and designs sorted by file name).
type planDraftFiles struct {
	// Brief is brief.md, pushed as an informative reference document.
	Brief string
	// Requirements is one entry per requirements/<doc>.md file.
	Requirements []draftRequirement
	// Decisions is decisions.yml.
	Decisions []draftDecision
	// Designs is one entry per designs/<id>.md file.
	Designs []draftDesign
	// Tasks is tasks.yml.
	Tasks []draftTask
}

// draftRequirement is one requirements/<doc>.md file: its document ID (the
// file base name), H1 title, raw markdown body (prose plus the single
// conveyor:requirements fence), and the fence's parsed statements.
type draftRequirement struct {
	ID         string
	Title      string
	Body       string
	Statements []core.RequirementStatement
}

// draftDecision is one decisions.yml entry. decisions.yml is a top-level YAML
// list:
//
//   - id: D1
//     statement: <why this course of action>
//     context: <the constraints that forced the choice>
//     alternatives:
//   - <rejected alternative>
//     cites:
//   - <doc-id>/REQ-n
//
// cites names the requirement IDs the decision depends on; push refuses a
// decision whose cited requirement is not confirmed.
type draftDecision struct {
	ID           string   `yaml:"id"`
	Statement    string   `yaml:"statement"`
	Context      string   `yaml:"context"`
	Alternatives []string `yaml:"alternatives"`
	Cites        []string `yaml:"cites"`
}

// draftDesign is one designs/<id>.md file: its document ID (the file base
// name), H1 title, raw markdown body (including the single conveyor:governs
// fence), and the fence's parsed scopes.
type draftDesign struct {
	ID      string
	Title   string
	Body    string
	Governs []core.GovernedScope
}

// draftTask is one tasks.yml entry. tasks.yml is a top-level YAML list:
//
//   - id: T1
//     title: <task title>
//     body: <task body>
//     depends_on: [T0]
//     governing:
//   - <doc-id>/REQ-n
//   - <doc-id>/AC-n.m
//   - <design-id>
//     docs_paths:
//   - docs/<path>
//     docs_none_reason: <why no docs path is updated>
//
// governing names every corpus ID the task is governed by and covers:
// requirement document IDs, qualified REQ and AC IDs, and design IDs. A task
// must name docs_paths or a docs_none_reason.
type draftTask struct {
	ID             string   `yaml:"id"`
	Title          string   `yaml:"title"`
	Body           string   `yaml:"body"`
	DependsOn      []string `yaml:"depends_on"`
	Governing      []string `yaml:"governing"`
	DocsPaths      []string `yaml:"docs_paths"`
	DocsNoneReason string   `yaml:"docs_none_reason"`
}

// planReviewFile is review.json: the owner's per-item verdicts from one review
// round, written by `conveyor plan review` and read by the agent.
type planReviewFile struct {
	Round int               `json:"round"`
	Items []planReviewEntry `json:"items"`
}

// planReviewEntry is one item's verdict. Verdict is one of approve,
// request_change, question, or pending. File is the draft-relative path that
// holds the item, so a comment routes back to the planner with its item and
// file. Normative is the item's text at the moment of the verdict; the review
// page uses it to show a before/after view when a later render finds the item's
// hash changed. Both are additive fields: an older review.json without them
// still decodes.
type planReviewEntry struct {
	ID        string `json:"id"`
	File      string `json:"file,omitempty"`
	Hash      string `json:"hash"`
	Verdict   string `json:"verdict"`
	Comment   string `json:"comment"`
	Normative string `json:"normative,omitempty"`
}

// planDecisionReference matches a draft-local decision ID (D1, D2, …) inside a
// design body. Confirmed IDs such as DEC-34 do not match.
var planDecisionReference = regexp.MustCompile(`\bD[1-9][0-9]*\b`)

// loadPlanDraft reads and parses a draft directory. It parses as much as it
// can: when a file fails to parse the remaining files still load, the partial
// draft is returned alongside the joined error, and `plan check` reports every
// problem at once. An absent brief.md, decisions.yml, tasks.yml, or notes.yml
// is an empty value, not an error.
func loadPlanDraft(dir string) (*planDraft, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("plan draft %q: resolve path: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("plan draft %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("plan draft %q is not a directory", dir)
	}
	draft := &planDraft{Dir: filepath.Clean(dir)}
	var problems []error

	if data, err := os.ReadFile(filepath.Join(abs, "brief.md")); err == nil {
		draft.Brief = string(data)
		draft.Raw.Brief = string(data)
	} else if !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, fmt.Errorf("brief.md: %w", err))
	}

	requirementFiles, _ := filepath.Glob(filepath.Join(abs, "requirements", "*.md"))
	sort.Strings(requirementFiles)
	for _, path := range requirementFiles {
		name := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Errorf("requirements/%s: %w", name, err))
			continue
		}
		document, err := pipeline.ParseRequirementDocument(string(data))
		if err != nil {
			problems = append(problems, fmt.Errorf("requirements/%s: %w", name, err))
			continue
		}
		draft.Raw.Requirements = append(draft.Raw.Requirements, draftRequirement{
			ID:         planFileBase(name),
			Title:      firstMarkdownHeading(document.Markdown),
			Body:       document.Markdown,
			Statements: document.Statements,
		})
	}

	if data, ok := readPlanYAML(abs, "decisions.yml", &problems); ok {
		if err := yaml.Unmarshal(data, &draft.Raw.Decisions); err != nil {
			problems = append(problems, fmt.Errorf("decisions.yml: %w", err))
			draft.Raw.Decisions = nil
		}
	}

	designFiles, _ := filepath.Glob(filepath.Join(abs, "designs", "*.md"))
	sort.Strings(designFiles)
	for _, path := range designFiles {
		name := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Errorf("designs/%s: %w", name, err))
			continue
		}
		body := string(data)
		scopes, err := core.ParseGovernedScopes(body)
		if err != nil {
			problems = append(problems, fmt.Errorf("designs/%s: %w", name, err))
			continue
		}
		draft.Raw.Designs = append(draft.Raw.Designs, draftDesign{
			ID:      planFileBase(name),
			Title:   firstMarkdownHeading(body),
			Body:    body,
			Governs: scopes,
		})
	}

	if data, ok := readPlanYAML(abs, "tasks.yml", &problems); ok {
		if err := yaml.Unmarshal(data, &draft.Raw.Tasks); err != nil {
			problems = append(problems, fmt.Errorf("tasks.yml: %w", err))
			draft.Raw.Tasks = nil
		}
	}

	notes := loadPlanNotes(abs, &problems)

	draft.Items = buildPlanItems(draft.Raw, notes)
	return draft, errors.Join(problems...)
}

// loadPlanNotes reads notes.yml: a top-level YAML list of
//
//   - id: <item ID>
//     headline: <plain-language headline>
//     example: <concrete example>
//     why: <why this item matters>
func loadPlanNotes(absDir string, problems *[]error) map[string]*planNote {
	type noteEntry struct {
		ID       string `yaml:"id"`
		planNote `yaml:",inline"`
	}
	data, ok := readPlanYAML(absDir, "notes.yml", problems)
	if !ok {
		return nil
	}
	var entries []noteEntry
	if err := yaml.Unmarshal(data, &entries); err != nil {
		*problems = append(*problems, fmt.Errorf("notes.yml: %w", err))
		return nil
	}
	notes := make(map[string]*planNote, len(entries))
	for i := range entries {
		note := entries[i].planNote
		notes[entries[i].ID] = &note
	}
	return notes
}

// readPlanYAML reads an optional top-level YAML list file. A missing or
// all-whitespace file reports no data and no problem; an unreadable file is a
// problem.
func readPlanYAML(absDir, name string, problems *[]error) ([]byte, bool) {
	data, err := os.ReadFile(filepath.Join(absDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		*problems = append(*problems, fmt.Errorf("%s: %w", name, err))
		return nil, false
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, false
	}
	return data, true
}

// buildPlanItems flattens the parsed files into review items in stable push
// order and attaches notes by item ID.
func buildPlanItems(raw planDraftFiles, notes map[string]*planNote) []planItem {
	var items []planItem
	for _, requirement := range raw.Requirements {
		for _, statement := range requirement.Statements {
			items = append(items, planItem{
				ID:        requirement.ID + "/" + statement.ID,
				Kind:      planItemRequirement,
				File:      "requirements/" + requirement.ID + ".md",
				Normative: requirementNormative(statement),
			})
		}
	}
	for _, decision := range raw.Decisions {
		items = append(items, planItem{
			ID:        decision.ID,
			Kind:      planItemDecision,
			File:      "decisions.yml",
			Normative: decisionNormative(decision),
			Links:     append([]string(nil), decision.Cites...),
		})
	}
	for _, design := range raw.Designs {
		items = append(items, planItem{
			ID:        design.ID,
			Kind:      planItemDesign,
			File:      "designs/" + design.ID + ".md",
			Normative: normalizePlanText(design.Body),
			Links:     decisionReferences(design.Body),
		})
	}
	for _, task := range raw.Tasks {
		items = append(items, planItem{
			ID:        task.ID,
			Kind:      planItemTask,
			File:      "tasks.yml",
			Normative: taskNormative(task),
			Links:     append([]string(nil), task.Governing...),
		})
	}
	order := map[planItemKind]int{
		planItemRequirement: 0,
		planItemDecision:    1,
		planItemDesign:      2,
		planItemTask:        3,
	}
	sort.SliceStable(items, func(i, j int) bool {
		if order[items[i].Kind] != order[items[j].Kind] {
			return order[items[i].Kind] < order[items[j].Kind]
		}
		return items[i].ID < items[j].ID
	})
	for i := range items {
		if note, ok := notes[items[i].ID]; ok {
			items[i].Note = note
		}
	}
	return items
}

// Hash is the approval identity of one item: the SHA-256 of its ID and
// canonical normative text. Notes are never part of it.
func (it planItem) Hash() string {
	sum := sha256.Sum256([]byte(it.ID + "\n" + it.Normative))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// requirementNormative canonicalizes a requirement statement, its optional
// user story, and its acceptance criteria in fixed field order.
func requirementNormative(statement core.RequirementStatement) string {
	var b strings.Builder
	b.WriteString("statement: ")
	b.WriteString(strings.TrimSpace(statement.Statement))
	b.WriteString("\n")
	if story := statement.UserStory; story != nil {
		b.WriteString("as_a: ")
		b.WriteString(strings.TrimSpace(story.AsA))
		b.WriteString("\n")
		b.WriteString("i_want: ")
		b.WriteString(strings.TrimSpace(story.IWant))
		b.WriteString("\n")
		b.WriteString("so_that: ")
		b.WriteString(strings.TrimSpace(story.SoThat))
		b.WriteString("\n")
	}
	for _, criterion := range statement.AcceptanceCriteria {
		b.WriteString(criterion.ID)
		b.WriteString(": ")
		b.WriteString(strings.TrimSpace(criterion.Statement))
		b.WriteString("\n")
	}
	return normalizePlanText(b.String())
}

// decisionNormative canonicalizes a decision's statement, context, and
// rejected alternatives in fixed field order.
func decisionNormative(decision draftDecision) string {
	var b strings.Builder
	b.WriteString("statement: ")
	b.WriteString(strings.TrimSpace(decision.Statement))
	b.WriteString("\ncontext: ")
	b.WriteString(strings.TrimSpace(decision.Context))
	b.WriteString("\nalternatives:\n")
	writePlanList(&b, decision.Alternatives)
	return normalizePlanText(b.String())
}

// taskNormative canonicalizes a task's title, body, dependencies, governing
// IDs, and docs decision in fixed field order.
func taskNormative(task draftTask) string {
	var b strings.Builder
	b.WriteString("title: ")
	b.WriteString(strings.TrimSpace(task.Title))
	b.WriteString("\nbody: ")
	b.WriteString(strings.TrimSpace(task.Body))
	b.WriteString("\ndepends_on:\n")
	writePlanList(&b, task.DependsOn)
	b.WriteString("governing:\n")
	writePlanList(&b, task.Governing)
	b.WriteString("docs_paths:\n")
	writePlanList(&b, task.DocsPaths)
	b.WriteString("docs_none_reason: ")
	b.WriteString(strings.TrimSpace(task.DocsNoneReason))
	b.WriteString("\n")
	return normalizePlanText(b.String())
}

func writePlanList(b *strings.Builder, values []string) {
	for _, value := range values {
		b.WriteString("- ")
		b.WriteString(strings.TrimSpace(value))
		b.WriteString("\n")
	}
}

// normalizePlanText is the single normalization the approval hash depends on:
// LF line endings, trailing whitespace trimmed from every line, and no
// trailing blank lines.
func normalizePlanText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// decisionReferences returns the sorted, de-duplicated draft decision IDs a
// design body cites.
func decisionReferences(body string) []string {
	matches := planDecisionReference.FindAllString(body, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	refs := make([]string, 0, len(matches))
	for _, match := range matches {
		if seen[match] {
			continue
		}
		seen[match] = true
		refs = append(refs, match)
	}
	sort.Strings(refs)
	return refs
}

// firstMarkdownHeading returns the first "# <title>" heading's text.
func firstMarkdownHeading(text string) string {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			return strings.TrimSpace(trimmed[2:])
		}
	}
	return ""
}

// planFileBase is a file name without its extension.
func planFileBase(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// readPlanReview reads review.json. An absent file is an empty review, not an
// error.
func readPlanReview(dir string) (*planReviewFile, error) {
	data, err := os.ReadFile(filepath.Join(dir, "review.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &planReviewFile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("review.json: %w", err)
	}
	var review planReviewFile
	if err := json.Unmarshal(data, &review); err != nil {
		return nil, fmt.Errorf("review.json: %w", err)
	}
	return &review, nil
}

// writePlanReview writes review.json atomically enough for a single local
// reader: the full file is marshaled before the write.
func writePlanReview(dir string, review *planReviewFile) error {
	data, err := json.MarshalIndent(review, "", "  ")
	if err != nil {
		return fmt.Errorf("review.json: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, "review.json"), data, 0o600); err != nil {
		return fmt.Errorf("review.json: %w", err)
	}
	return nil
}
