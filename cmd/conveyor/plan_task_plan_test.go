package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planFixturePlan is an execution plan that passes ParsePlan with an empty
// decomposition, the rule submit_plan applies.
const planFixturePlan = "# Plan\n\n## Approach\n\nAdd the check following draft:dec/use-cli.\n\n## Files touched\n\n- cmd/check.go\n\n## Ordering\n\n1. Check.\n\n## Risks\n\nNone.\n\n## Done criteria\n\n- The check prints failures.\n"

const planFixtureBaseCommit = "0123456789abcdef0123456789abcdef01234567"

// planTaskWith returns the build task item with extra front matter lines.
func planTaskWith(lines ...string) string {
	return strings.Replace(planFixtureBuildTask, "---\nrepo: conveyor\n", "---\nrepo: conveyor\n"+strings.Join(lines, "\n")+"\n", 1)
}

// component-cli-onboarding invariant 27; req-task-plan-state AC-2.2, AC-2.5,
// AC-2.6: each plan-state or plan-file rule fails offline and prints
// `<item-id>: <rule>: <reason>`.
func TestPlanCheckTaskPlanRules(t *testing.T) {
	cases := []struct {
		name      string
		task      string
		plan      string
		extra     map[string]string
		prefix    string
		fragments []string
	}{
		{
			name:      "orphan plan file",
			task:      planFixtureBuildTask,
			extra:     map[string]string{"tasks/lonely.plan.md": planFixturePlan},
			prefix:    "draft:task/lonely: LP-2: ",
			fragments: []string{"tasks/lonely.plan.md has no tasks/lonely.md beside it"},
		},
		{
			name:      "plan beside a skipped base copy",
			task:      planFixtureBuildTask,
			extra:     map[string]string{"tasks/foo.base.md": "# Base\n", "tasks/foo.base.plan.md": planFixturePlan},
			prefix:    "draft:task/foo.base: LP-2: ",
			fragments: []string{"tasks/foo.base.plan.md has no tasks/foo.base.md beside it"},
		},
		{
			name:      "plan beside another plan file",
			task:      planFixtureBuildTask,
			plan:      planFixturePlan,
			extra:     map[string]string{"tasks/build.plan.plan.md": planFixturePlan},
			prefix:    "draft:task/build.plan: LP-2: ",
			fragments: []string{"tasks/build.plan.plan.md has no tasks/build.plan.md beside it"},
		},
		{
			name:      "plan missing a required heading",
			task:      planFixtureBuildTask,
			plan:      strings.Replace(planFixturePlan, "## Risks\n\nNone.\n\n", "", 1),
			prefix:    "draft:task/build: req-task-plan-state AC-2.2: ",
			fragments: []string{"build.plan.md", "plan requires a recognizable risks heading"},
		},
		{
			name:      "plan with a conveyor fence",
			task:      planFixtureBuildTask,
			plan:      planFixturePlan + "\n```conveyor:requirements\n- id: REQ-1\n```\n",
			prefix:    "draft:task/build: req-task-plan-state AC-2.2: ",
			fragments: []string{"plans cannot contain conveyor: machine fences"},
		},
		{
			name:      "empty plan file",
			task:      planFixtureBuildTask,
			plan:      "\n",
			prefix:    "draft:task/build: req-task-plan-state AC-2.2: ",
			fragments: []string{"plan markdown is required"},
		},
		{
			name:      "plan_state with a plan file",
			task:      planTaskWith("plan_state: needed"),
			plan:      planFixturePlan,
			prefix:    "draft:task/build: req-task-plan-state AC-2.6: ",
			fragments: []string{"an item with a plan file states no plan_state or plan_approval"},
		},
		{
			name:      "plan_approval with a plan file",
			task:      planTaskWith("plan_approval: true"),
			plan:      planFixturePlan,
			prefix:    "draft:task/build: req-task-plan-state AC-2.6: ",
			fragments: []string{"an item with a plan file states no plan_state or plan_approval"},
		},
		{
			name:      "plan_approval with plan_state",
			task:      planTaskWith("plan_state: needed", "plan_approval: true"),
			prefix:    "draft:task/build: req-task-plan-state AC-2.6: ",
			fragments: []string{"plan_state and its legacy alias plan_approval are both stated"},
		},
		{
			name:      "plan_state outside the intake values",
			task:      planTaskWith("plan_state: awaiting_approval"),
			prefix:    "draft:task/build: req-task-plan-state AC-2.6: ",
			fragments: []string{`plan_state must be not_required or needed, not "awaiting_approval"`},
		},
		{
			name:      "malformed plan_base_commit",
			task:      planTaskWith("plan_base_commit: " + strings.ToUpper(planFixtureBaseCommit)),
			plan:      planFixturePlan,
			prefix:    "draft:task/build: req-task-plan-state AC-2.5: ",
			fragments: []string{"plan_base_commit must be a full commit ID of 40 lowercase hexadecimal characters"},
		},
		{
			name:      "short plan_base_commit",
			task:      planTaskWith("plan_base_commit: abc1234"),
			plan:      planFixturePlan,
			prefix:    "draft:task/build: req-task-plan-state AC-2.5: ",
			fragments: []string{"plan_base_commit must be a full commit ID", `"abc1234"`},
		},
		{
			name:      "plan_base_commit without a plan",
			task:      planTaskWith("plan_base_commit: " + planFixtureBaseCommit),
			prefix:    "draft:task/build: req-task-plan-state AC-2.5: ",
			fragments: []string{"plan_base_commit applies only with a plan file tasks/build.plan.md"},
		},
		{
			name:      "plan citing an undefined draft-local item",
			task:      planFixtureBuildTask,
			plan:      strings.Replace(planFixturePlan, "draft:dec/use-cli", "draft:dec/absent", 1),
			prefix:    "draft:task/build: AC-1.6: ",
			fragments: []string{"draft:dec/absent"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newPlanFixture(t)
			writePlanFile(t, root, "tasks/build.md", tc.task)
			if tc.plan != "" {
				writePlanFile(t, root, "tasks/build.plan.md", tc.plan)
			}
			for path, content := range tc.extra {
				writePlanFile(t, root, path, content)
			}
			expectPlanFailure(t, root, tc.prefix, tc.fragments...)
		})
	}
}

// component-cli-onboarding invariant 27: a legacy plan_approval alone, a
// plan_state alone, and a valid plan file with its base commit pass, and a
// plan file is part of its item, never an item of its own.
func TestPlanCheckTaskPlanPasses(t *testing.T) {
	cases := map[string]struct{ task, plan string }{
		"legacy plan_approval alone":     {task: planTaskWith("plan_approval: true")},
		"legacy plan_approval false":     {task: planTaskWith("plan_approval: false")},
		"plan_state alone":               {task: planTaskWith("plan_state: not_required")},
		"plan file with its base commit": {task: planTaskWith("plan_base_commit: " + planFixtureBaseCommit), plan: planFixturePlan},
		"plan file without a commit":     {task: planFixtureBuildTask, plan: strings.ReplaceAll(planFixturePlan, "\n", "\r\n")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := newPlanFixture(t)
			writePlanFile(t, root, "tasks/build.md", tc.task)
			if tc.plan != "" {
				writePlanFile(t, root, "tasks/build.plan.md", tc.plan)
			}
			out, err := runPlanCommand(t, "check", root)
			if err != nil || strings.TrimSpace(out) != "plan check: 5 item(s) passed with 0 warning(s)" {
				t.Fatalf("check: err=%v out=%q", err, out)
			}
			draft := readPlanFixture(t, root)
			build := draft.Item("draft:task/build")
			if len(build) != 1 || build[0].Task.HasPlan != (tc.plan != "") || len(draft.OrphanPlans) != 0 {
				t.Fatalf("build items = %+v, orphans = %v", build, draft.OrphanPlans)
			}
		})
	}
}

// component-cli-onboarding invariants 28 and 30; req-local-planning AC-3.7,
// AC-4.7; req-task-plan-state AC-2.3: the request carries plan fields only
// with a plan file, sends a legacy plan_approval as plan_state, and binds the
// plan in the digest.
func TestPlanTaskPlanRequests(t *testing.T) {
	root := newPlanFixture(t)
	writePlanFile(t, root, "tasks/build.md", planTaskWith("plan_base_commit: "+planFixtureBaseCommit))
	writePlanFile(t, root, "tasks/build.plan.md", strings.ReplaceAll(planFixturePlan, "\n", "\r\n")+"\r\n")
	writePlanFile(t, root, "tasks/legacy.md", "---\nrepo: conveyor\nplan_approval: true\n---\nShip req-demo AC-1.1.\n")
	writePlanFile(t, root, "tasks/stated.md", "---\nrepo: conveyor\nplan_state: not_required\n---\nShip req-demo AC-1.1.\n")
	writePlanFile(t, root, "tasks/unpinned.md", "---\nrepo: conveyor\n---\nShip req-demo AC-1.1.\n")
	writePlanFile(t, root, "tasks/unpinned.plan.md", planFixturePlan)
	draft := readPlanFixture(t, root)
	request := func(id string) map[string]any {
		t.Helper()
		built, err := BuildPlanItemRequest(draft.Item(id)[0])
		if err != nil {
			t.Fatal(err)
		}
		return built.Body
	}

	build := request("draft:task/build")
	if build["plan"] != strings.TrimSpace(planFixturePlan) || build["plan_base_commit"] != planFixtureBaseCommit || build["plan_approved"] != true {
		t.Fatalf("planned task body = %#v", build)
	}
	for _, absent := range []string{"plan_state", "spec_approval"} {
		if _, ok := build[absent]; ok {
			t.Fatalf("planned task carries %s: %#v", absent, build)
		}
	}
	unpinned := request("draft:task/unpinned")
	if _, ok := unpinned["plan_base_commit"]; ok || unpinned["plan_approved"] != true || unpinned["plan"] != strings.TrimSpace(planFixturePlan) {
		t.Fatalf("plan without a base commit = %#v", unpinned)
	}
	if legacy := request("draft:task/legacy"); legacy["plan_state"] != "needed" {
		t.Fatalf("legacy plan_approval true = %#v", legacy)
	}
	if stated := request("draft:task/stated"); stated["plan_state"] != "not_required" {
		t.Fatalf("stated plan_state = %#v", stated)
	}
	for _, id := range []string{"draft:task/legacy", "draft:task/stated", "draft:task/docs"} {
		body := request(id)
		for _, absent := range []string{"spec_approval", "plan", "plan_base_commit", "plan_approved"} {
			if _, ok := body[absent]; ok {
				t.Fatalf("%s without a plan file carries %s: %#v", id, absent, body)
			}
		}
	}

	// The digest covers the plan and its base commit (invariant 28).
	before := planDigests(t, root)["draft:task/build"]
	writePlanFile(t, root, "tasks/build.plan.md", strings.Replace(planFixturePlan, "None.", "A slow check.", 1))
	if after := planDigests(t, root)["draft:task/build"]; after == before {
		t.Fatal("a changed plan file kept the item's digest")
	}
	writePlanFile(t, root, "tasks/build.plan.md", planFixturePlan)
	writePlanFile(t, root, "tasks/build.md", planTaskWith("plan_base_commit: "+strings.Repeat("f", 40)))
	if after := planDigests(t, root)["draft:task/build"]; after == before {
		t.Fatal("a changed plan_base_commit kept the item's digest")
	}
}

// req-local-planning AC-2.3, AC-4.7; req-task-plan-state AC-2.3: push sends
// the plan as approved with its base commit, sends a legacy plan_approval as
// plan_state, and refuses an item whose plan file changed after approval.
func TestPlanPushTaskPlans(t *testing.T) {
	f := newPlanFakeFactory(t)
	root := newPlanFixture(t)
	writePlanFile(t, root, "tasks/build.md", planTaskWith("plan_base_commit: "+planFixtureBaseCommit))
	writePlanFile(t, root, "tasks/build.plan.md", planFixturePlan)
	writePlanFile(t, root, "tasks/legacy.md", "---\nrepo: conveyor\nplan_approval: false\n---\nShip req-demo AC-1.1.\n")
	approveAllPlanItems(t, root)
	pushPlanThrough(t, f, root, PlanLayerTasks)

	// An approval made before the plan file changed no longer permits push.
	writePlanFile(t, root, "tasks/build.plan.md", strings.Replace(planFixturePlan, "None.", "A slow check.", 1))
	expectPlanRefusal(t, f, root, PlanLayerTasks, "draft:task/build: AC-3.1:", "changed since approval")
	// Adding a plan file to an approved item changes it too.
	writePlanFile(t, root, "tasks/build.plan.md", planFixturePlan)
	writePlanFile(t, root, "tasks/docs.plan.md", planFixturePlan)
	expectPlanRefusal(t, f, root, PlanLayerTasks, "draft:task/docs: AC-3.1:", "changed since approval")
	if out, err := runPlanCommand(t, "status", root); err != nil || !strings.Contains(out, "changed since approval") {
		t.Fatalf("status after adding a plan file: err=%v\n%s", err, out)
	}
	if err := os.Remove(filepath.Join(root, "tasks", "docs.plan.md")); err != nil {
		t.Fatal(err)
	}

	mustPushPlan(t, root, PlanLayerTasks)
	bodies := map[string]map[string]any{}
	for _, post := range f.postsTo("/v1/tasks") {
		for _, id := range []string{"build", "legacy", "docs"} {
			if strings.Contains(post.IntakeKey, ":draft:task/"+id+":") {
				bodies[id] = post.Body
			}
		}
	}
	build := bodies["build"]
	decision := planPushRecord(t, root, "draft:dec/use-cli").ServerID
	wantPlan := strings.Replace(strings.TrimSpace(planFixturePlan), "draft:dec/use-cli", decision, 1)
	if build["plan"] != wantPlan || build["plan_base_commit"] != planFixtureBaseCommit || build["plan_approved"] != true {
		t.Fatalf("planned task body = %#v", build)
	}
	if _, ok := build["plan_state"]; ok {
		t.Fatalf("planned task carries plan_state: %#v", build)
	}
	if legacy := bodies["legacy"]; legacy["plan_state"] != "not_required" {
		t.Fatalf("legacy task body = %#v", legacy)
	}
	for _, id := range []string{"legacy", "docs"} {
		for _, absent := range []string{"spec_approval", "plan", "plan_base_commit", "plan_approved"} {
			if _, ok := bodies[id][absent]; ok {
				t.Fatalf("%s carries %s without a plan file: %#v", id, absent, bodies[id])
			}
		}
	}
}
