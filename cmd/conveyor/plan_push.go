package main

// Layered push for the planning studio. `conveyor plan push` posts one
// approved draft layer at a time as unconfirmed proposals: requirements first
// (with brief.md as an informative reference document), then decisions,
// designs, and tasks. Every layer reads live confirmation state and refuses
// while a document it cites is still unconfirmed; the command never confirms.
// It is the only planning verb that contacts the server.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/spf13/cobra"
)

const (
	// planPushStateFile records, inside the draft, which items a previous push
	// already posted and the server ID each one received. It is the only memory
	// across layers: a later layer resolves a draft-local decision or task ID
	// through it, and a re-run skips an item whose hash it already posted.
	planPushStateFile = "push.json"
	// planDefaultDesignCategory is used when a design file declares no category
	// front matter. System Design categories are operator-named and immutable
	// after creation, so the draft names one explicitly when it matters.
	planDefaultDesignCategory = "Architecture"
)

// planPushLayerNames maps the --layer flag values to draft item kinds.
var planPushLayerNames = map[string]planItemKind{
	"requirements": planItemRequirement,
	"decisions":    planItemDecision,
	"designs":      planItemDesign,
	"tasks":        planItemTask,
}

// planPushLayerLabels maps a draft item kind back to its --layer value.
var planPushLayerLabels = map[planItemKind]string{
	planItemRequirement: "requirements",
	planItemDecision:    "decisions",
	planItemDesign:      "designs",
	planItemTask:        "tasks",
}

// planPushOptions carries one push invocation's resolved choices.
type planPushOptions struct {
	Layer  planItemKind
	DryRun bool
	Repo   string
	Base   string
}

func planPushCmd() *cobra.Command {
	var (
		layer  string
		dryRun bool
		repo   string
		base   string
	)
	command := &cobra.Command{
		Use:   "push <draft-dir> --layer requirements|decisions|designs|tasks [--dry-run]",
		Short: "Push an approved planning draft to Conveyor as proposals, one layer at a time",
		Long: "Push one approved layer of a planning draft as unconfirmed proposals. " +
			"Push in order: requirements (with brief.md as an informative reference document), " +
			"then decisions, designs, and tasks. A layer is refused while any document it cites " +
			"is unconfirmed, and this command never confirms anything. " +
			"--dry-run prints the layer's proposal payloads without posting and without running " +
			"the pre-push review hook.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := parsePlanPushLayer(layer)
			if err != nil {
				return err
			}
			options := planPushOptions{Layer: kind, DryRun: dryRun, Repo: repo, Base: base}
			return runPlanPush(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], options)
		},
	}
	command.Flags().StringVar(&layer, "layer", "", "layer to push: requirements, decisions, designs, or tasks")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "print the layer's proposal payloads without posting and without running the pre-push review hook")
	command.Flags().StringVar(&repo, "repo", "", "target repository name; defaults to the workspace's single repository")
	command.Flags().StringVar(&base, "base", "", "target base branch; defaults to the repository's configured base")
	return command
}

// parsePlanPushLayer resolves the --layer flag value to a draft item kind.
func parsePlanPushLayer(layer string) (planItemKind, error) {
	kind, ok := planPushLayerNames[strings.TrimSpace(layer)]
	if !ok {
		return "", fmt.Errorf("plan push: --layer must be one of requirements, decisions, designs, tasks")
	}
	return kind, nil
}

// runPlanPush loads and validates the draft, runs the pre-push hook before the
// requirements layer, then posts the requested layer. Dry runs print payloads
// and never post, never run the hook, and never write push.json.
func runPlanPush(ctx context.Context, stdout, stderr io.Writer, dir string, options planPushOptions) error {
	draft, loadErr := loadPlanDraft(dir)
	if loadErr != nil {
		return fmt.Errorf("plan push: draft %q did not load: %s", dir, strings.TrimSpace(loadErr.Error()))
	}
	if len(draft.Items) == 0 {
		return fmt.Errorf("plan push: draft %q has no reviewable items", dir)
	}
	if err := requirePlanApproval(draft.Dir, draft); err != nil {
		return err
	}
	absDir, err := filepath.Abs(draft.Dir)
	if err != nil {
		return fmt.Errorf("plan push: resolve draft path: %w", err)
	}

	// The hook runs once over the whole approved draft, before the requirements
	// layer. A dry run never runs it, and no other layer runs it.
	if options.Layer == planItemRequirement && !options.DryRun {
		if _, root, rootErr := planGitRoot(ctx, absDir); rootErr == nil {
			hashes := make([]string, 0, len(draft.Items))
			for _, item := range draft.Items {
				hashes = append(hashes, item.Hash())
			}
			if _, hookErr := runPrePushReview(ctx, root, absDir, hashes, stdout, stderr); hookErr != nil {
				return fmt.Errorf("plan push: %w", hookErr)
			}
		}
	}

	c := newClient()
	state, err := readPlanPushState(absDir)
	if err != nil {
		return fmt.Errorf("plan push: %w", err)
	}

	switch options.Layer {
	case planItemRequirement:
		return pushPlanRequirements(ctx, stdout, c, draft, state, absDir, options)
	case planItemDecision:
		return pushPlanDecisions(ctx, stdout, c, draft, state, absDir, options)
	case planItemDesign:
		return pushPlanDesigns(ctx, stdout, c, draft, state, absDir, options)
	case planItemTask:
		return pushPlanTasks(ctx, stdout, c, draft, state, absDir, options)
	default:
		return fmt.Errorf("plan push: unsupported layer %q", options.Layer)
	}
}

// requirePlanApproval refuses a push unless every draft item carries an approve
// verdict at its current content hash.
func requirePlanApproval(dir string, draft *planDraft) error {
	review, err := readPlanReview(dir)
	if err != nil {
		return fmt.Errorf("plan push: %w", err)
	}
	verdicts := make(map[string]planReviewEntry, len(review.Items))
	for _, entry := range review.Items {
		verdicts[entry.ID] = entry
	}
	var problems []string
	for _, item := range draft.Items {
		entry, ok := verdicts[item.ID]
		switch {
		case !ok:
			problems = append(problems, item.ID+": no review verdict")
		case entry.Verdict != "approve":
			problems = append(problems, fmt.Sprintf("%s: verdict is %q, not approve", item.ID, entry.Verdict))
		case entry.Hash != item.Hash():
			problems = append(problems, item.ID+": content changed since approval")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("plan push refused: %d item(s) are not approved at their current content:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	return nil
}

// planPushState is the draft-local record of what a prior push posted.
type planPushState struct {
	Items map[string]planPushStateItem `json:"items"`
}

// planPushStateItem is one posted item: the approved hash it was posted at, the
// server ID it received, and the document version when the server mints one.
type planPushStateItem struct {
	Kind     string `json:"kind"`
	Hash     string `json:"hash"`
	ServerID string `json:"server_id,omitempty"`
	Version  int    `json:"version,omitempty"`
}

func readPlanPushState(dir string) (*planPushState, error) {
	data, err := os.ReadFile(filepath.Join(dir, planPushStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return &planPushState{Items: map[string]planPushStateItem{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", planPushStateFile, err)
	}
	var state planPushState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%s: %w", planPushStateFile, err)
	}
	if state.Items == nil {
		state.Items = map[string]planPushStateItem{}
	}
	return &state, nil
}

func writePlanPushState(dir string, state *planPushState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", planPushStateFile, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, planPushStateFile), data, 0o600); err != nil {
		return fmt.Errorf("%s: %w", planPushStateFile, err)
	}
	return nil
}

// planPushContentHash hashes a document-level payload so an unchanged re-run
// does not post it again. Item-level hashes come from planItem.Hash.
func planPushContentHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// planItemHashByID returns the approval hash of the item with this ID, or the
// empty string when the draft holds no such item.
func planItemHashByID(draft *planDraft, id string) string {
	for _, item := range draft.Items {
		if item.ID == id {
			return item.Hash()
		}
	}
	return ""
}

// planPrintPayload writes one dry-run line: the layer label, the item or
// document ID, and the compact JSON payload.
func planPrintPayload(stdout io.Writer, layer, itemID string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%s %s %s\n", layer, itemID, data); err != nil {
		return err
	}
	return nil
}

// --- payload shapes (wire-compatible with internal/httpapi) ---

type planReferencePayload struct {
	Name        string `json:"name"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
}

type planRequirementPayload struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

type planDecisionPayload struct {
	Statement            string `json:"statement"`
	Context              string `json:"context"`
	AlternativesRejected string `json:"alternatives_rejected"`
}

type planDesignPayload struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Content  string `json:"content"`
}

type planTaskPayload struct {
	Body            string   `json:"body"`
	Repo            string   `json:"repo"`
	BaseBranch      string   `json:"base_branch"`
	Source          string   `json:"source"`
	DependsOn       []string `json:"depends_on,omitempty"`
	RequirementIDs  []string `json:"requirement_ids,omitempty"`
	SystemDesignIDs []string `json:"system_design_ids,omitempty"`
}

// --- requirements layer ---

func pushPlanRequirements(ctx context.Context, stdout io.Writer, c *client, draft *planDraft, state *planPushState, dir string, options planPushOptions) error {
	layer := planPushLayerLabels[planItemRequirement]
	brief := strings.TrimSpace(draft.Brief)
	if brief == "" && len(draft.Raw.Requirements) == 0 {
		if !options.DryRun {
			fmt.Fprintf(stdout, "plan push %s: nothing to push\n", layer)
		}
		return nil
	}
	if options.DryRun {
		if brief != "" {
			payload := planReferencePayload{Name: planBriefName(draft), Filename: "brief.md", ContentType: "text/markdown"}
			if err := planPrintPayload(stdout, layer, "brief.md", payload); err != nil {
				return err
			}
		}
		for _, requirement := range draft.Raw.Requirements {
			payload := planRequirementPayload{ID: requirement.ID, Title: requirement.Title, Content: requirement.Body}
			if err := planPrintPayload(stdout, layer, requirement.ID, payload); err != nil {
				return err
			}
		}
		return nil
	}

	if brief != "" {
		payload := planReferencePayload{Name: planBriefName(draft), Filename: "brief.md", ContentType: "text/markdown"}
		hash := planPushContentHash("reference", payload.Name, draft.Brief)
		if item, ok := state.Items["brief.md"]; !ok || item.Hash != hash {
			var response struct {
				Document core.ReferenceDocument `json:"document"`
			}
			if err := c.planPushReferenceDocument(ctx, payload, draft.Brief, &response); err != nil {
				return fmt.Errorf("plan push: reference document brief.md: %w", err)
			}
			state.Items["brief.md"] = planPushStateItem{Kind: "reference", Hash: hash, ServerID: response.Document.ID}
			if err := writePlanPushState(dir, state); err != nil {
				return fmt.Errorf("plan push: %w", err)
			}
		}
	}
	if len(draft.Raw.Requirements) == 0 {
		return nil
	}

	existing, _, err := c.planRequirementDocuments(ctx)
	if err != nil {
		return fmt.Errorf("plan push: read requirements: %w", err)
	}
	for _, requirement := range draft.Raw.Requirements {
		hash := planPushContentHash("requirement", requirement.ID, requirement.Title, requirement.Body)
		if item, ok := state.Items[requirement.ID]; ok && item.Hash == hash {
			continue
		}
		payload := planRequirementPayload{ID: requirement.ID, Title: requirement.Title, Content: requirement.Body}
		if existing[requirement.ID] {
			var version core.RequirementVersion
			path := "/v1/requirements/" + url.PathEscape(requirement.ID) + "/versions"
			if err := c.planPushDo(ctx, http.MethodPost, path, payload, &version); err != nil {
				return fmt.Errorf("plan push: revise requirement %s: %w", requirement.ID, err)
			}
			state.Items[requirement.ID] = planPushStateItem{Kind: "requirement", Hash: hash, ServerID: requirement.ID, Version: version.Version}
		} else {
			var response struct {
				Requirement core.Requirement        `json:"requirement"`
				Version     core.RequirementVersion `json:"version"`
			}
			if err := c.planPushDo(ctx, http.MethodPost, "/v1/requirements", payload, &response); err != nil {
				return fmt.Errorf("plan push: create requirement %s: %w", requirement.ID, err)
			}
			state.Items[requirement.ID] = planPushStateItem{Kind: "requirement", Hash: hash, ServerID: response.Requirement.ID, Version: response.Version.Version}
		}
		if err := writePlanPushState(dir, state); err != nil {
			return fmt.Errorf("plan push: %w", err)
		}
	}
	return nil
}

// --- decisions layer ---

func pushPlanDecisions(ctx context.Context, stdout io.Writer, c *client, draft *planDraft, state *planPushState, dir string, options planPushOptions) error {
	layer := planPushLayerLabels[planItemDecision]
	if len(draft.Raw.Decisions) == 0 {
		if !options.DryRun {
			fmt.Fprintf(stdout, "plan push %s: nothing to push\n", layer)
		}
		return nil
	}

	_, requirements, err := c.planRequirementDocuments(ctx)
	if err != nil {
		return fmt.Errorf("plan push: read requirements: %w", err)
	}
	if err := checkPlanDecisionPrerequisites(draft, requirements); err != nil {
		return err
	}

	if options.DryRun {
		for _, decision := range draft.Raw.Decisions {
			if err := planPrintPayload(stdout, layer, decision.ID, planDecisionPayload{
				Statement:            decision.Statement,
				Context:              decision.Context,
				AlternativesRejected: strings.Join(decision.Alternatives, "\n"),
			}); err != nil {
				return err
			}
		}
		return nil
	}

	for _, decision := range draft.Raw.Decisions {
		hash := planItemHashByID(draft, decision.ID)
		if item, ok := state.Items[decision.ID]; ok && item.Hash == hash && item.ServerID != "" {
			continue
		}
		payload := planDecisionPayload{
			Statement:            decision.Statement,
			Context:              decision.Context,
			AlternativesRejected: strings.Join(decision.Alternatives, "\n"),
		}
		var response core.Decision
		if err := c.planPushDo(ctx, http.MethodPost, "/v1/decisions", payload, &response); err != nil {
			return fmt.Errorf("plan push: propose decision %s: %w", decision.ID, err)
		}
		state.Items[decision.ID] = planPushStateItem{Kind: "decision", Hash: hash, ServerID: response.ID}
		if err := writePlanPushState(dir, state); err != nil {
			return fmt.Errorf("plan push: %w", err)
		}
	}
	return nil
}

// --- designs layer ---

func pushPlanDesigns(ctx context.Context, stdout io.Writer, c *client, draft *planDraft, state *planPushState, dir string, options planPushOptions) error {
	layer := planPushLayerLabels[planItemDesign]
	if len(draft.Raw.Designs) == 0 {
		if !options.DryRun {
			fmt.Fprintf(stdout, "plan push %s: nothing to push\n", layer)
		}
		return nil
	}

	decisions, err := c.planDecisionDocuments(ctx)
	if err != nil {
		return fmt.Errorf("plan push: read decisions: %w", err)
	}
	if err := checkPlanDesignPrerequisites(draft, state, decisions); err != nil {
		return err
	}

	if options.DryRun {
		for _, design := range draft.Raw.Designs {
			if err := planPrintPayload(stdout, layer, design.ID, planDesignPayload{
				ID:       design.ID,
				Title:    design.Title,
				Category: planDesignCategory(dir, design.ID),
				Content:  design.Body,
			}); err != nil {
				return err
			}
		}
		return nil
	}

	existing, _, err := c.planSystemDesignDocuments(ctx)
	if err != nil {
		return fmt.Errorf("plan push: read system designs: %w", err)
	}
	for _, design := range draft.Raw.Designs {
		hash := planItemHashByID(draft, design.ID)
		if item, ok := state.Items[design.ID]; ok && item.Hash == hash && item.ServerID != "" {
			continue
		}
		payload := planDesignPayload{
			ID:       design.ID,
			Title:    design.Title,
			Category: planDesignCategory(dir, design.ID),
			Content:  design.Body,
		}
		if existing[design.ID] {
			var version core.SystemDesignVersion
			path := "/v1/system-designs/" + url.PathEscape(design.ID) + "/versions"
			if err := c.planPushDo(ctx, http.MethodPost, path, payload, &version); err != nil {
				return fmt.Errorf("plan push: revise system design %s: %w", design.ID, err)
			}
			state.Items[design.ID] = planPushStateItem{Kind: "design", Hash: hash, ServerID: design.ID, Version: version.Version}
		} else {
			var response struct {
				Document core.SystemDesign        `json:"document"`
				Version  core.SystemDesignVersion `json:"version"`
			}
			if err := c.planPushDo(ctx, http.MethodPost, "/v1/system-designs", payload, &response); err != nil {
				return fmt.Errorf("plan push: create system design %s: %w", design.ID, err)
			}
			state.Items[design.ID] = planPushStateItem{Kind: "design", Hash: hash, ServerID: response.Document.ID, Version: response.Version.Version}
		}
		if err := writePlanPushState(dir, state); err != nil {
			return fmt.Errorf("plan push: %w", err)
		}
	}
	return nil
}

// --- tasks layer ---

func pushPlanTasks(ctx context.Context, stdout io.Writer, c *client, draft *planDraft, state *planPushState, dir string, options planPushOptions) error {
	layer := planPushLayerLabels[planItemTask]
	if len(draft.Raw.Tasks) == 0 {
		if !options.DryRun {
			fmt.Fprintf(stdout, "plan push %s: nothing to push\n", layer)
		}
		return nil
	}

	_, requirements, err := c.planRequirementDocuments(ctx)
	if err != nil {
		return fmt.Errorf("plan push: read requirements: %w", err)
	}
	_, designs, err := c.planSystemDesignDocuments(ctx)
	if err != nil {
		return fmt.Errorf("plan push: read system designs: %w", err)
	}
	if err := checkPlanTaskPrerequisites(draft, state, requirements, designs); err != nil {
		return err
	}

	repoName, base, err := c.planTargetRepo(options.Repo, options.Base)
	if err != nil {
		return fmt.Errorf("plan push: %w", err)
	}

	designIDs := planDesignIDs(draft)
	if options.DryRun {
		for _, task := range draft.Raw.Tasks {
			payload, buildErr := buildPlanTaskPayload(task, state, repoName, base, designIDs, false)
			if buildErr != nil {
				return fmt.Errorf("plan push: %w", buildErr)
			}
			if err := planPrintPayload(stdout, layer, task.ID, payload); err != nil {
				return err
			}
		}
		return nil
	}

	for _, task := range draft.Raw.Tasks {
		hash := planItemHashByID(draft, task.ID)
		if item, ok := state.Items[task.ID]; ok && item.Hash == hash && item.ServerID != "" {
			continue
		}
		payload, buildErr := buildPlanTaskPayload(task, state, repoName, base, designIDs, true)
		if buildErr != nil {
			return fmt.Errorf("plan push: %w", buildErr)
		}
		var created core.Task
		if err := c.planPushDo(ctx, http.MethodPost, "/v1/tasks", payload, &created); err != nil {
			return fmt.Errorf("plan push: create task %s: %w", task.ID, err)
		}
		state.Items[task.ID] = planPushStateItem{Kind: "task", Hash: hash, ServerID: created.ID}
		if err := writePlanPushState(dir, state); err != nil {
			return fmt.Errorf("plan push: %w", err)
		}
	}
	return nil
}

// --- prerequisite checks ---

func checkPlanDecisionPrerequisites(draft *planDraft, requirements map[string]bool) error {
	var missing []string
	for _, decision := range draft.Raw.Decisions {
		for _, cite := range decision.Cites {
			if !requirements[planCitationDocument(cite)] {
				missing = append(missing, fmt.Sprintf("%s cites unconfirmed requirement %s", decision.ID, cite))
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("plan push refused: decisions layer has unconfirmed prerequisites:\n  %s", strings.Join(missing, "\n  "))
	}
	return nil
}

func checkPlanDesignPrerequisites(draft *planDraft, state *planPushState, decisions map[string]bool) error {
	var missing []string
	for _, design := range draft.Raw.Designs {
		for _, local := range decisionReferences(design.Body) {
			serverID := planResolveServerDecision(state, local)
			switch {
			case serverID == "":
				missing = append(missing, fmt.Sprintf("%s cites decision %s that has not been pushed", design.ID, local))
			case !decisions[serverID]:
				missing = append(missing, fmt.Sprintf("%s cites unconfirmed decision %s", design.ID, serverID))
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("plan push refused: designs layer has unconfirmed prerequisites:\n  %s", strings.Join(missing, "\n  "))
	}
	return nil
}

func checkPlanTaskPrerequisites(draft *planDraft, state *planPushState, requirements, designs map[string]bool) error {
	designIDs := planDesignIDs(draft)
	var missing []string
	for _, task := range draft.Raw.Tasks {
		for _, governing := range task.Governing {
			requirementDoc, designID := planGoverningReference(governing, designIDs)
			if requirementDoc != "" {
				if !requirements[requirementDoc] {
					missing = append(missing, fmt.Sprintf("%s is governed by unconfirmed requirement %s", task.ID, governing))
				}
				continue
			}
			if !designs[planResolveServerDesign(state, designID)] {
				missing = append(missing, fmt.Sprintf("%s is governed by unconfirmed design %s", task.ID, governing))
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("plan push refused: tasks layer has unconfirmed prerequisites:\n  %s", strings.Join(missing, "\n  "))
	}
	return nil
}

// planCitationDocument returns the document ID a corpus citation names:
// "<doc>/REQ-n" and "<doc>/AC-n.m" yield <doc>, a bare ID yields itself.
func planCitationDocument(citation string) string {
	if doc, _, ok := strings.Cut(strings.TrimSpace(citation), "/"); ok {
		return strings.TrimSpace(doc)
	}
	return strings.TrimSpace(citation)
}

// planGoverningReference classifies one task governing entry. A qualified
// requirement citation yields its document ID; an unqualified entry that names
// a draft design yields that design ID; anything else is treated as a bare
// requirement document ID.
func planGoverningReference(governing string, designIDs map[string]bool) (requirementDoc, designID string) {
	value := strings.TrimSpace(governing)
	if doc, _, ok := strings.Cut(value, "/"); ok {
		return strings.TrimSpace(doc), ""
	}
	if designIDs[value] {
		return "", value
	}
	return value, ""
}

// planDesignIDs is the set of design document IDs declared in the draft.
func planDesignIDs(draft *planDraft) map[string]bool {
	ids := make(map[string]bool, len(draft.Raw.Designs))
	for _, design := range draft.Raw.Designs {
		ids[design.ID] = true
	}
	return ids
}

// planResolveServerDecision maps a draft-local decision ID (D1) to its
// server-minted ID through push.json; an unpublished decision resolves to "".
func planResolveServerDecision(state *planPushState, local string) string {
	if item, ok := state.Items[local]; ok {
		return item.ServerID
	}
	return ""
}

// planResolveServerDesign maps a draft design ID to its server document ID.
// Designs post under their own ID, so an unrecorded design resolves to itself.
func planResolveServerDesign(state *planPushState, designID string) string {
	if item, ok := state.Items[designID]; ok && item.ServerID != "" {
		return item.ServerID
	}
	return designID
}

// --- task payload composition ---

func buildPlanTaskPayload(task draftTask, state *planPushState, repo, base string, designIDs map[string]bool, strict bool) (planTaskPayload, error) {
	var body strings.Builder
	body.WriteString(strings.TrimSpace(task.Title))
	body.WriteString("\n\n")
	body.WriteString(strings.TrimSpace(task.Body))
	switch {
	case strings.TrimSpace(task.DocsNoneReason) != "":
		body.WriteString("\n\ndocs: none - ")
		body.WriteString(strings.TrimSpace(task.DocsNoneReason))
	case len(task.DocsPaths) > 0:
		body.WriteString("\n\ndocs paths:\n")
		for _, path := range task.DocsPaths {
			body.WriteString("- ")
			body.WriteString(strings.TrimSpace(path))
			body.WriteString("\n")
		}
	}

	var requirementIDs, designServerIDs []string
	for _, governing := range task.Governing {
		requirementDoc, designID := planGoverningReference(governing, designIDs)
		if requirementDoc != "" {
			requirementIDs = append(requirementIDs, requirementDoc)
			continue
		}
		serverID := planResolveServerDesign(state, designID)
		designServerIDs = append(designServerIDs, serverID)
	}

	dependsOn, err := resolvePlanTaskDependencies(task, state, strict)
	if err != nil {
		return planTaskPayload{}, err
	}

	return planTaskPayload{
		Body:            strings.TrimSpace(body.String()),
		Repo:            repo,
		BaseBranch:      base,
		Source:          "cli",
		DependsOn:       dedupePlanIDs(dependsOn),
		RequirementIDs:  dedupePlanIDs(requirementIDs),
		SystemDesignIDs: dedupePlanIDs(designServerIDs),
	}, nil
}

func resolvePlanTaskDependencies(task draftTask, state *planPushState, strict bool) ([]string, error) {
	var resolved []string
	for _, dependency := range task.DependsOn {
		id := strings.TrimSpace(dependency)
		if id == "" {
			continue
		}
		serverID := ""
		if item, ok := state.Items[id]; ok {
			serverID = item.ServerID
		}
		if serverID == "" {
			if strict {
				return nil, fmt.Errorf("task %s depends on %s which has not been pushed", task.ID, id)
			}
			serverID = id
		}
		resolved = append(resolved, serverID)
	}
	return resolved, nil
}

// dedupePlanIDs removes empty and duplicate IDs while preserving order.
func dedupePlanIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	var out []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// --- brief and design helpers ---

func planBriefName(draft *planDraft) string {
	if title := firstMarkdownHeading(draft.Brief); title != "" {
		return title
	}
	return "Planning brief"
}

// planDesignCategory reads an optional YAML front matter `category:` from a
// design file, defaulting to planDefaultDesignCategory.
func planDesignCategory(dir, id string) string {
	data, err := os.ReadFile(filepath.Join(dir, "designs", id+".md"))
	if err == nil {
		if category := planFrontMatterCategory(string(data)); category != "" {
			return category
		}
	}
	return planDefaultDesignCategory
}

func planFrontMatterCategory(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok || strings.TrimSpace(name) != "category" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// --- server reads and writes ---

// planPushDo issues one authenticated JSON request against the control plane.
func (c *client) planPushDo(ctx context.Context, method, path string, body any, out any) error {
	if c.configErr != nil {
		return c.configErr
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.workspace != "" {
		request.Header.Set("X-Workspace-ID", c.workspace)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		message, _ := io.ReadAll(response.Body)
		return fmt.Errorf("%s: %s", response.Status, bytes.TrimSpace(message))
	}
	if response.StatusCode == http.StatusNoContent || out == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}

// planPushReferenceDocument uploads brief.md as an informative reference
// document (multipart name + file).
func (c *client) planPushReferenceDocument(ctx context.Context, payload planReferencePayload, content string, out any) error {
	if c.configErr != nil {
		return c.configErr
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("name", payload.Name); err != nil {
		return err
	}
	part, err := writer.CreateFormFile("file", payload.Filename)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(part, content); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/reference-documents", &body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.workspace != "" {
		request.Header.Set("X-Workspace-ID", c.workspace)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		message, _ := io.ReadAll(response.Body)
		return fmt.Errorf("%s: %s", response.Status, bytes.TrimSpace(message))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}

// planRequirementDocuments reads every living requirement document's existence
// and confirmation state.
func (c *client) planRequirementDocuments(ctx context.Context) (exists, confirmed map[string]bool, err error) {
	var summaries []struct {
		Requirement core.Requirement `json:"requirement"`
	}
	if err := c.planPushDo(ctx, http.MethodGet, "/v1/requirements", nil, &summaries); err != nil {
		return nil, nil, err
	}
	exists, confirmed = map[string]bool{}, map[string]bool{}
	for _, summary := range summaries {
		exists[summary.Requirement.ID] = true
		if summary.Requirement.CurrentVersion > 0 {
			confirmed[summary.Requirement.ID] = true
		}
	}
	return exists, confirmed, nil
}

// planDecisionDocuments reads every decision's confirmation state.
func (c *client) planDecisionDocuments(ctx context.Context) (map[string]bool, error) {
	var decisions []core.Decision
	if err := c.planPushDo(ctx, http.MethodGet, "/v1/decisions", nil, &decisions); err != nil {
		return nil, err
	}
	confirmed := map[string]bool{}
	for _, decision := range decisions {
		if decision.Status == core.DecisionConfirmed {
			confirmed[decision.ID] = true
		}
	}
	return confirmed, nil
}

// planSystemDesignDocuments reads every system design document's existence and
// confirmation state.
func (c *client) planSystemDesignDocuments(ctx context.Context) (exists, confirmed map[string]bool, err error) {
	var summaries []struct {
		Document core.SystemDesign `json:"document"`
	}
	if err := c.planPushDo(ctx, http.MethodGet, "/v1/system-designs", nil, &summaries); err != nil {
		return nil, nil, err
	}
	exists, confirmed = map[string]bool{}, map[string]bool{}
	for _, summary := range summaries {
		exists[summary.Document.ID] = true
		if summary.Document.CurrentVersion > 0 {
			confirmed[summary.Document.ID] = true
		}
	}
	return exists, confirmed, nil
}

// planTargetRepo resolves the repository and base branch a task layer files
// into: an explicit choice wins, otherwise the workspace's single repository.
func (c *client) planTargetRepo(repoName, base string) (string, string, error) {
	document, err := c.getWorkspaceConfig()
	if err != nil {
		return "", "", fmt.Errorf("read workspace repositories: %w", err)
	}
	repos := document.Document.Repos
	if repoName != "" {
		for _, repo := range repos {
			if repo.Name != repoName {
				continue
			}
			if base == "" {
				base = repo.Base
			}
			if strings.TrimSpace(base) == "" {
				return "", "", fmt.Errorf("repository %s has no base branch", repo.Name)
			}
			return repo.Name, base, nil
		}
		return "", "", fmt.Errorf("workspace has no repository named %q", repoName)
	}
	if len(repos) != 1 {
		return "", "", fmt.Errorf("workspace declares %d repositories; pass --repo", len(repos))
	}
	repo := repos[0]
	if base == "" {
		base = repo.Base
	}
	if strings.TrimSpace(base) == "" {
		return "", "", fmt.Errorf("repository %s has no base branch", repo.Name)
	}
	return repo.Name, base, nil
}
