package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RequirementListPageEgressBaselineRevision is the commit whose delivery
// lineage walk and delivery event statement the requirement-list-page
// baseline copies inline.
const RequirementListPageEgressBaselineRevision = "inline-queries@34e959bd02afc71f096cc0f8c973fbaa9cb74c0f"

// RequirementListPageDeliveryBudget is the delivery walk budget of the
// requirement list page (httpapi requirementViews).
var RequirementListPageDeliveryBudget = core.LineageTraversalBudget{MaxDepth: 3, MaxNodes: 256, MaxLinks: 1024}

// Requirement list page fixture shape: roots share blueprints, every
// blueprint materializes delivery tasks, and each root also serves one task
// directly.
const (
	requirementListPageRoots      = 6
	requirementListPageBlueprints = 4
	requirementListPageChildren   = 2
)

// RequirementListPageReads are the two reads this measurement varies: the
// batched delivery walk and the delivery events of the reached tasks. Every
// other read of the page is the public store method in both variants.
type RequirementListPageReads struct {
	DeliveryLineage func(ctx context.Context, requirementIDs []string, budget core.LineageTraversalBudget) (map[string][]core.LineageLink, error)
	DeliveryEvents  func(ctx context.Context, taskIDs []string) (map[string][]core.Event, error)
}

// SeedRequirementListPageEgress seeds the requirement list page through the
// public store API in a workspace of its own, so other cases' documents do
// not enter the page: roots that share blueprints, materialized and directly
// serving delivery tasks whose merge and review events carry padBytes of
// fields no classification reads, and noise delivery events on unrelated
// tasks that the page never reaches. It returns the workspace; each variant
// reads under store.WithWorkspace with it.
func SeedRequirementListPageEgress(t testing.TB, ctx context.Context, st store.Backend, noise, padBytes int) (string, map[string]int) {
	t.Helper()
	workspace := "egress-requirement-list-" + core.NewTaskID()
	ctx = store.WithWorkspace(ctx, workspace)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", padBytes)
	roots := make([]core.Requirement, requirementListPageRoots)
	for i := range roots {
		requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: fmt.Sprintf("req-list-page-%d-%s", i, core.NewTaskID()), Title: fmt.Sprintf("List page %d", i)}, core.RequirementVersion{
			Content: "# List page requirement.", Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Deliver the list page."}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if requirement, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
			t.Fatal(err)
		}
		roots[i] = requirement
	}
	newTask := func(title, parent string) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: workspace, Title: title, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
		if parent != "" {
			task.ParentTaskID, task.OriginSpecVersion = parent, 1
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	appendEvent := func(event core.Event) {
		t.Helper()
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	deliver := func(task core.Task, index int) {
		head := fmt.Sprintf("%040x", index+1)
		appendEvent(core.Event{TaskID: task.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{
			"review_round": 1, "verdict": "approve", "approved_head_sha": head, "summary": pad,
			"reviews": []map[string]any{{"verdict": "approve", "reviewed_commit_sha": head, "summary": pad, "feedback": pad}},
		})})
		appendEvent(core.Event{TaskID: task.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{
			"url": "https://example.test/pull/" + task.ID, "title": task.Title, "head_sha": head, "approved_head_sha": head,
			"factory_review_validated": true, "merge_message": pad,
		})})
	}
	deliveryTasks := 0
	for b := range requirementListPageBlueprints {
		blueprint := newTask(fmt.Sprintf("List page blueprint %d", b), "")
		// Each blueprint serves two or three roots, so roots share sources.
		for r := range roots {
			if r%requirementListPageBlueprints == b || (r+1)%requirementListPageBlueprints == b {
				appendEvent(core.Event{TaskID: blueprint.ID, Kind: "requirement.serves_confirmed", Payload: core.JSONPayload(map[string]any{"requirement_id": roots[r].ID})})
			}
		}
		appendEvent(core.Event{TaskID: blueprint.ID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": 1})})
		for c := range requirementListPageChildren {
			deliver(newTask(fmt.Sprintf("List page delivery %d.%d", b, c), blueprint.ID), deliveryTasks)
			deliveryTasks++
		}
	}
	for r, root := range roots {
		task := newTask(fmt.Sprintf("List page direct delivery %d", r), "")
		appendEvent(core.Event{TaskID: task.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": root.ID, "version": root.CurrentVersion, "context": pad})})
		deliver(task, deliveryTasks)
		deliveryTasks++
	}
	// Unrelated delivery history: the same event kinds with the same padding
	// on tasks no listed requirement reaches.
	noiseTasks := []core.Task{newTask("List page noise A", ""), newTask("List page noise B", "")}
	for i := range noise {
		appendEvent(core.Event{TaskID: noiseTasks[i%len(noiseTasks)].ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"url": "https://example.test/noise", "merge_message": pad})})
	}
	return workspace, map[string]int{"roots": requirementListPageRoots, "shared_blueprints": requirementListPageBlueprints, "delivery_tasks": deliveryTasks,
		"noise_events": noise, "noise_payload_bytes": padBytes}
}

// ReadRequirementListPage performs the requirement list page's store reads
// in the handler's order and digests what the page uses: requirement rows,
// delivery links, the classification fields of each delivery event,
// version summaries, requirement events, and task summaries.
func ReadRequirementListPage(ctx context.Context, st store.Store, reads RequirementListPageReads) (ReadEgressOutcome, error) {
	requirements, err := st.ListRequirements(ctx, false)
	if err != nil {
		return ReadEgressOutcome{}, err
	}
	ids := make([]string, len(requirements))
	selections := make([]store.DocumentVersionSelection, len(requirements))
	for i, requirement := range requirements {
		ids[i] = requirement.ID
		selections[i] = store.DocumentVersionSelection{ID: requirement.ID, CurrentVersion: requirement.CurrentVersion}
	}
	budget := RequirementListPageDeliveryBudget
	budget.Workspace, _ = store.WorkspaceFromContext(ctx)
	lineage, err := reads.DeliveryLineage(ctx, ids, budget)
	if err != nil {
		return ReadEgressOutcome{}, err
	}
	taskSet := map[string]bool{}
	for _, links := range lineage {
		for _, link := range links {
			if link.DstType == core.LineageTask || link.DstType == core.LineageBlueprint {
				taskSet[link.DstID] = true
			}
		}
	}
	taskIDs := make([]string, 0, len(taskSet))
	for id := range taskSet {
		taskIDs = append(taskIDs, id)
	}
	sort.Strings(taskIDs)
	events, err := reads.DeliveryEvents(ctx, taskIDs)
	if err != nil {
		return ReadEgressOutcome{}, err
	}
	versions, err := st.ListRequirementVersionSummaries(ctx, selections)
	if err != nil {
		return ReadEgressOutcome{}, err
	}
	requirementEvents, err := st.ListRequirementEventsForRequirements(ctx, ids)
	if err != nil {
		return ReadEgressOutcome{}, err
	}
	tasks, err := st.ListRequirementDetailTasks(ctx, taskIDs)
	if err != nil {
		return ReadEgressOutcome{}, err
	}
	page := struct {
		Requirements      []core.Requirement
		Lineage           map[string][]string
		Deliveries        map[string][]requirementListPageDelivery
		Versions          map[string][]core.RequirementVersion
		RequirementEvents map[string][]int64
		Tasks             map[string]store.RequirementDetailTask
	}{Requirements: requirements, Lineage: map[string][]string{}, Deliveries: map[string][]requirementListPageDelivery{},
		Versions: versions, RequirementEvents: map[string][]int64{}, Tasks: tasks}
	for id, links := range lineage {
		page.Lineage[id] = deliveryLinkKeys(links)
	}
	deliveries := 0
	for taskID, taskEvents := range events {
		// A task with no delivery events is the same as an absent task.
		for _, event := range taskEvents {
			page.Deliveries[taskID] = append(page.Deliveries[taskID], requirementListPageDeliveryOf(event))
			deliveries++
		}
	}
	for id, list := range requirementEvents {
		for _, event := range list {
			page.RequirementEvents[id] = append(page.RequirementEvents[id], event.ID)
		}
	}
	if deliveries == 0 {
		return ReadEgressOutcome{}, fmt.Errorf("requirement list page read no delivery events")
	}
	digest, err := ReadEgressDigest(page)
	return ReadEgressOutcome{Digest: digest, Successes: deliveries}, err
}

// requirementListPageDelivery is one delivery event as classification reads
// it: the event identity and the payload fields of its kind.
type requirementListPageDelivery struct {
	ID     int64
	TaskID string
	Kind   string
	At     time.Time
	Fields any
}

func requirementListPageDeliveryOf(event core.Event) requirementListPageDelivery {
	delivery := requirementListPageDelivery{ID: event.ID, TaskID: event.TaskID, Kind: event.Kind, At: event.At.UTC()}
	switch event.Kind {
	case "merge.confirmed", "merge.reconciled":
		var merge struct {
			URL, Title, TaskTitle                     string
			HeadSHA, ReviewedHeadSHA, ApprovedHeadSHA string
			FactoryReviewValidated                    *bool
		}
		var raw struct {
			URL                    string `json:"url"`
			Title                  string `json:"title"`
			TaskTitle              string `json:"task_title"`
			HeadSHA                string `json:"head_sha"`
			ReviewedHeadSHA        string `json:"reviewed_head_sha"`
			ApprovedHeadSHA        string `json:"approved_head_sha"`
			FactoryReviewValidated *bool  `json:"factory_review_validated"`
		}
		if json.Unmarshal(event.Payload, &raw) != nil {
			delivery.Fields = "malformed"
			return delivery
		}
		merge.URL, merge.Title, merge.TaskTitle = raw.URL, raw.Title, raw.TaskTitle
		merge.HeadSHA, merge.ReviewedHeadSHA, merge.ApprovedHeadSHA, merge.FactoryReviewValidated = raw.HeadSHA, raw.ReviewedHeadSHA, raw.ApprovedHeadSHA, raw.FactoryReviewValidated
		delivery.Fields = merge
	case "review.round_completed":
		var round struct {
			Verdict         string `json:"verdict"`
			ApprovedHeadSHA string `json:"approved_head_sha"`
			Reviews         []struct {
				Verdict           string `json:"verdict"`
				ReviewedCommitSHA string `json:"reviewed_commit_sha"`
			} `json:"reviews"`
		}
		if json.Unmarshal(event.Payload, &round) != nil {
			delivery.Fields = "malformed"
			return delivery
		}
		delivery.Fields = round
	default:
		var attachment struct {
			ID          string `json:"id"`
			Version     int    `json:"version"`
			Unconfirmed bool   `json:"unconfirmed"`
		}
		if json.Unmarshal(event.Payload, &attachment) != nil {
			delivery.Fields = "malformed"
			return delivery
		}
		delivery.Fields = attachment
	}
	return delivery
}

// LegacyFetchRequirementDeliveryLineage is store.FetchRequirementDeliveryLineage
// at RequirementListPageEgressBaselineRevision, the measurement baseline: it
// also fetches reached tasks as sources. Do not edit it to follow the store.
func LegacyFetchRequirementDeliveryLineage(ctx context.Context, ids []string, budget core.LineageTraversalBudget, fetch func(context.Context, []core.LineageNode, int) (map[core.LineageNode][]core.LineageLink, error)) (map[string][]core.LineageLink, error) {
	out := make(map[string][]core.LineageLink, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	type walk struct {
		id       string
		frontier []core.LineageNode
		seen     map[core.LineageNode]bool
		links    []core.LineageLink
	}
	walks := make([]walk, 0, len(ids))
	roots := map[string]bool{}
	for _, id := range ids {
		if roots[id] {
			continue
		}
		roots[id] = true
		root := core.LineageNode{Type: core.LineageRequirement, ID: id}
		walks = append(walks, walk{id: id, frontier: []core.LineageNode{root}, seen: map[core.LineageNode]bool{root: true}})
	}
	adjacency := map[core.LineageNode][]core.LineageLink{}
	for range budget.MaxDepth {
		needed := []core.LineageNode{}
		requested := map[core.LineageNode]bool{}
		for _, w := range walks {
			if len(w.links) > budget.MaxLinks {
				continue
			}
			for _, node := range w.frontier {
				if _, ok := adjacency[node]; !ok && !requested[node] {
					requested[node] = true
					needed = append(needed, node)
				}
			}
		}
		sort.Slice(needed, func(i, j int) bool {
			if needed[i].Type != needed[j].Type {
				return needed[i].Type < needed[j].Type
			}
			return needed[i].ID < needed[j].ID
		})
		for start := 0; start < len(needed); start += 100 {
			end := min(start+100, len(needed))
			selected, err := fetch(ctx, needed[start:end], budget.MaxLinks+1)
			if err != nil {
				return nil, err
			}
			for _, node := range needed[start:end] {
				links := selected[node]
				sort.Slice(links, func(i, j int) bool { return legacyDeliveryLinkKey(links[i]) < legacyDeliveryLinkKey(links[j]) })
				adjacency[node] = links
			}
		}
		active := false
		for i := range walks {
			w := &walks[i]
			next := []core.LineageNode{}
			if len(w.links) > budget.MaxLinks {
				continue
			}
			for _, node := range w.frontier {
				for _, link := range adjacency[node] {
					w.links = append(w.links, link)
					if len(w.links) > budget.MaxLinks {
						break
					}
					dst := core.LineageNode{Type: link.DstType, ID: link.DstID}
					if !w.seen[dst] {
						w.seen[dst] = true
						next = append(next, dst)
					}
				}
				if len(w.links) > budget.MaxLinks {
					break
				}
			}
			w.frontier = next
			active = active || len(next) > 0 && len(w.links) <= budget.MaxLinks
		}
		if !active {
			break
		}
	}
	for _, w := range walks {
		if len(w.links) > 0 {
			out[w.id] = w.links
		}
	}
	return out, nil
}

func legacyDeliveryLinkKey(link core.LineageLink) string {
	return strings.Join([]string{link.Workspace, string(link.SrcType), link.SrcID, string(link.DstType), link.DstID, link.Kind}, "\x00")
}
