package main

// `conveyor plan review <draft-dir> [--port N]`: the item-by-item approval page.
// It serves one page on 127.0.0.1, prints the URL, and blocks until the owner
// submits "Send review" or closes the command. "Send review" writes review.json
// as {round, items:[{id, file, hash, verdict, comment, normative}]} and the
// command exits. Approvals are bound to planItem.Hash(); a render that finds a
// different hash returns that item to pending and shows the approved text
// before the change.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// planItemTabLabel maps an item kind to its tab label.
var planItemTabLabel = map[planItemKind]string{
	planItemRequirementDocument: "Requirement documents",
	planItemRequirement:         "Requirements",
	planItemDecision:            "Decisions",
	planItemDesign:              "Designs",
	planItemTask:                "Tasks",
}

// planItemTabOrder is the fixed tab order; it matches the push order.
var planItemTabOrder = []planItemKind{planItemRequirementDocument, planItemRequirement, planItemDecision, planItemDesign, planItemTask}

// planReviewCmd serves the local review page.
func planReviewCmd() *cobra.Command {
	port := 0
	command := &cobra.Command{
		Use:   "review <draft-dir>",
		Short: "Serve the local item-by-item review page for a planning draft",
		Long: "Serve one local page on 127.0.0.1 and block until the owner submits or closes it. " +
			"Each item is approved against its normalized content hash; \"Send review\" writes review.json and exits. " +
			"--port 0 picks a free port.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runPlanReview(ctx, args[0], port, cmd.OutOrStdout())
		},
	}
	command.Flags().IntVar(&port, "port", 0, "local port to serve on (0 picks a free port)")
	return command
}

// planReviewPage is the review server's state.
type planReviewPage struct {
	server *planPageServer
	dir    string
}

// runPlanReview binds the page and blocks until it submits or closes.
func runPlanReview(ctx context.Context, dir string, port int, stdout io.Writer) error {
	if draft, err := loadPlanDraft(dir); draft == nil {
		return err
	}
	page := &planReviewPage{dir: dir}
	server, err := newPlanPageServer(port)
	if err != nil {
		return err
	}
	page.server = server
	mux := http.NewServeMux()
	registerPlanPageRoutes(mux, server, page.handleIndex, page.handleSubmit)
	server.http.Handler = mux
	return runPlanPage(ctx, server, stdout)
}

// planReviewItem is one item plus the review state rendered for it.
type planReviewItem struct {
	item          planItem
	index         int
	verdict       string
	comment       string
	before        string
	changed       bool
	linkedChanged bool
}

func (p *planReviewPage) handleIndex(w http.ResponseWriter, r *http.Request) {
	view, err := buildPlanReviewView(p.dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view.Token = p.server.token
	renderPlanPage(w, view)
}

func (p *planReviewPage) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !p.server.beginSubmit() {
		http.Error(w, "already submitted", http.StatusConflict)
		return
	}
	recorded := false
	defer func() { p.server.endSubmit(recorded) }()
	draft, err := loadPlanDraft(p.dir)
	if draft == nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	prior, err := readPlanReview(p.dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type submittedVerdict struct{ hash, verdict, comment string }
	submitted := make(map[string]submittedVerdict)
	for i := 0; ; i++ {
		id := r.PostFormValue(fmt.Sprintf("item.%d.id", i))
		if id == "" {
			break
		}
		submitted[id] = submittedVerdict{
			hash:    r.PostFormValue(fmt.Sprintf("item.%d.hash", i)),
			verdict: r.PostFormValue(fmt.Sprintf("item.%d.verdict", i)),
			comment: r.PostFormValue(fmt.Sprintf("item.%d.comment", i)),
		}
	}
	entries := make([]planReviewEntry, 0, len(draft.Items))
	for _, item := range draft.Items {
		verdict := planReviewVerdict(submitted[item.ID].verdict)
		// An approval binds to the hash the owner saw on the page. A missing
		// posted hash, or one that no longer matches the item on disk, records
		// pending so what the owner approves is exactly what is pushed.
		if posted := submitted[item.ID].hash; posted == "" || posted != item.Hash() {
			verdict = "pending"
		}
		entries = append(entries, planReviewEntry{
			ID:        item.ID,
			File:      item.File,
			Hash:      item.Hash(),
			Verdict:   verdict,
			Comment:   submitted[item.ID].comment,
			Normative: item.Normative,
		})
	}
	review := &planReviewFile{Round: prior.Round + 1, Items: entries}
	if err := writePlanReview(p.dir, review); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	recorded = true
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!doctype html><html><body><h1>Review sent (round %d)</h1><p>%d item(s) written to review.json. You can close this tab.</p></body></html>\n", review.Round, len(entries))
	p.server.finish()
}

// planReviewVerdict clamps a submitted verdict to one of the four known values.
func planReviewVerdict(value string) string {
	switch value {
	case "approve", "request_change", "question":
		return value
	default:
		return "pending"
	}
}

// buildPlanReviewView loads the draft and its prior review and renders the
// review page view. A malformed review.json or an unreadable draft is an error;
// item-level load problems are shown as a banner.
func buildPlanReviewView(dir string) (planPageView, error) {
	draft, loadErr := loadPlanDraft(dir)
	if draft == nil {
		return planPageView{}, loadErr
	}
	prior, err := readPlanReview(dir)
	if err != nil {
		return planPageView{}, err
	}
	states := computePlanReviewStates(draft, prior)

	view := planPageView{
		Mode:    planPageReview,
		Title:   "Conveyor plan review — " + planDraftName(dir),
		Command: "conveyor plan review " + draft.Dir,
		Goal:    planReviewGoal(draft),
		Round:   prior.Round,
		Matrix:  planTraceability(draft),
	}
	if loadErr != nil {
		view.Error = loadErr.Error()
	}
	if view.Goal == "" {
		view.Goal = "(no brief.md)"
	}

	total := 0
	approved := 0
	for _, kind := range planItemTabOrder {
		tab := planPageTab{ID: string(kind), Label: planItemTabLabel[kind]}
		for _, state := range states {
			if state.item.Kind != kind {
				continue
			}
			tab.Total++
			total++
			if state.verdict == "approve" {
				tab.Approved++
				approved++
			}
			tab.Items = append(tab.Items, planReviewCard(state))
		}
		view.Tabs = append(view.Tabs, tab)
	}
	view.Summary = fmt.Sprintf("%d of %d approved", approved, total)
	return view, nil
}

// planReviewCard maps one item's state onto the shared card view.
func planReviewCard(state planReviewItem) planPageCard {
	item := state.item
	card := planPageCard{
		Index:         state.index,
		ID:            item.ID,
		File:          item.File,
		Hash:          item.Hash(),
		Normative:     item.Normative,
		Links:         item.Links,
		Verdict:       state.verdict,
		Comment:       state.comment,
		Before:        state.before,
		Changed:       state.changed,
		LinkedChanged: state.linkedChanged,
	}
	if item.Note != nil {
		card.Headline = item.Note.Headline
		card.Example = item.Note.Example
		card.Why = item.Note.Why
	}
	return card
}

// computePlanReviewStates merges the current draft with the prior review. An
// entry whose hash differs from the current item returns to pending and carries
// the previously approved text for the before/after view. An approved item
// whose linked item changed keeps its approval and is flagged, never cascaded.
func computePlanReviewStates(draft *planDraft, prior *planReviewFile) []planReviewItem {
	priorByID := make(map[string]planReviewEntry)
	if prior != nil {
		for _, entry := range prior.Items {
			priorByID[entry.ID] = entry
		}
	}
	current := make(map[string]string, len(draft.Items))
	for _, item := range draft.Items {
		current[item.ID] = item.Hash()
	}
	states := make([]planReviewItem, 0, len(draft.Items))
	for index, item := range draft.Items {
		state := planReviewItem{item: item, index: index, verdict: "pending"}
		if entry, ok := priorByID[item.ID]; ok {
			state.comment = entry.Comment
			if entry.Hash == current[item.ID] {
				state.verdict = planReviewVerdict(entry.Verdict)
			} else {
				state.changed = true
				state.before = entry.Normative
			}
		}
		states = append(states, state)
	}
	for i := range states {
		if states[i].verdict != "approve" {
			continue
		}
		for _, link := range states[i].item.Links {
			hash, ok := current[link]
			if !ok {
				continue
			}
			if entry, ok := priorByID[link]; ok && entry.Hash != hash {
				states[i].linkedChanged = true
				break
			}
		}
	}
	return states
}

// planReviewGoal is the overview goal text: brief.md, trimmed.
func planReviewGoal(draft *planDraft) string {
	return strings.TrimSpace(draft.Brief)
}

// planDraftName is the draft directory's base name for page titles.
func planDraftName(dir string) string {
	cleaned := strings.TrimRight(dir, "/")
	if cleaned == "" {
		return dir
	}
	if i := strings.LastIndex(cleaned, "/"); i >= 0 {
		return cleaned[i+1:]
	}
	return cleaned
}

// planTraceability builds the requirement-by-task matrix: one row per
// requirement item, one column per task, marked where the task's governing list
// names the requirement, its document, or one of its acceptance criteria.
func planTraceability(draft *planDraft) *planTraceMatrix {
	var taskIDs []string
	governing := make(map[string]map[string]bool)
	for _, item := range draft.Items {
		if item.Kind != planItemTask {
			continue
		}
		taskIDs = append(taskIDs, item.ID)
		set := make(map[string]bool, len(item.Links))
		for _, link := range item.Links {
			set[link] = true
		}
		governing[item.ID] = set
	}
	if len(taskIDs) == 0 {
		return nil
	}
	criteriaByDoc := make(map[string][]string)
	for _, requirement := range draft.Raw.Requirements {
		for _, statement := range requirement.Statements {
			for _, criterion := range statement.AcceptanceCriteria {
				criteriaByDoc[requirement.ID] = append(criteriaByDoc[requirement.ID], requirement.ID+"/"+criterion.ID)
			}
		}
	}
	var rows []planTraceRow
	for _, item := range draft.Items {
		if item.Kind != planItemRequirement {
			continue
		}
		document := planRequirementDocID(item.ID)
		row := planTraceRow{Label: item.ID, Cells: make([]bool, len(taskIDs))}
		for column, taskID := range taskIDs {
			set := governing[taskID]
			if set[item.ID] || set[document] {
				row.Cells[column] = true
				continue
			}
			for _, criterion := range criteriaByDoc[document] {
				if set[criterion] {
					row.Cells[column] = true
					break
				}
			}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	return &planTraceMatrix{Columns: taskIDs, Rows: rows}
}
