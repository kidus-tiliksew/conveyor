package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func readPlanFixture(t *testing.T, root string) *PlanDraft {
	t.Helper()
	draft, _, err := ReadPlanDraft(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return draft
}

func planDigests(t *testing.T, root string) map[string]string {
	t.Helper()
	digests := map[string]string{}
	for _, item := range readPlanFixture(t, root).Items {
		digest, err := PlanItemDigest(item)
		if err != nil {
			t.Fatalf("%s: %v", item.ID, err)
		}
		digests[item.ID] = digest
	}
	return digests
}

func approveAllPlanItems(t *testing.T, root string) map[string]string {
	t.Helper()
	digests := planDigests(t, root)
	args := []string{"approve", root}
	for id, digest := range digests {
		args = append(args, id+"@"+digest)
	}
	if out, err := runPlanCommand(t, args...); err != nil {
		t.Fatalf("approve: %v\n%s", err, out)
	}
	return digests
}

func planStatusStates(t *testing.T, root string) map[string]string {
	t.Helper()
	states := map[string]string{}
	for _, row := range PlanStatus(readPlanFixture(t, root)) {
		if row.Err != nil {
			t.Fatalf("%s: %v", row.ItemID, row.Err)
		}
		states[row.ItemID] = row.State
	}
	return states
}

// LP-5 request shapes: every kind and mode, factory-normalized content,
// omitted versus explicit gates, and unresolved draft-local IDs.
func TestPlanItemRequestBodies(t *testing.T) {
	root := newPlanFixture(t)
	writePlanFile(t, root, "requirements/req-old.md", "---\r\nmode: revise\r\nbase_version: 2\r\n---\r\n\r\n"+strings.ReplaceAll(strings.TrimPrefix(planFixtureRequirement, "---\nmode: create\n---\n"), "\n", "\r\n")+"\r\n\r\n")
	writePlanFile(t, root, "requirements/req-old.base.md", "# Old\n")
	writePlanFile(t, root, "system-designs/component-old.md", "---\nmode: revise\nbase_version: 7\n---\n\n"+strings.SplitN(planFixtureDesign, "---\n", 3)[2]+"\n\n")
	writePlanFile(t, root, "system-designs/component-old.base.md", "# Old\n")
	writePlanFile(t, root, "decisions/replace.yaml", planFixtureDecision+"supersedes: DEC-9\n")
	writePlanFile(t, root, "tasks/gated.md", "---\nrepo: conveyor\nbase_branch: release\nplan_approval: false\nmerge_approval: true\ndepends_on: [draft:task/build]\n---\n\nShip it after draft:task/build.\n\n")
	draft := readPlanFixture(t, root)
	requirementContent := strings.TrimSpace(strings.TrimPrefix(planFixtureRequirement, "---\nmode: create\n---\n"))
	designContent := strings.TrimSpace(strings.SplitN(planFixtureDesign, "---\n", 3)[2])
	for _, tc := range []struct {
		id        string
		path      string
		multipart bool
		body      map[string]any
	}{
		{"req-demo", "/v1/requirements", false, map[string]any{"id": "req-demo", "title": "Demo capability", "content": requirementContent, "origin": "operator"}},
		{"req-old", "/v1/requirements/req-old/versions", false, map[string]any{"content": requirementContent}},
		{"feature-demo", "/v1/system-designs", false, map[string]any{"id": "feature-demo", "title": "Demo design", "category": "Feature design", "content": designContent}},
		{"component-old", "/v1/system-designs/component-old/versions", false, map[string]any{"content": designContent}},
		{"draft:dec/use-cli", "/v1/decisions", false, map[string]any{"id": "draft:dec/use-cli", "statement": "Check drafts in the CLI, as req-demo REQ-1 requires.", "context": "req-demo AC-1.1 needs an offline check.", "alternatives_rejected": "A server-side check was rejected."}},
		{"draft:dec/replace", "/v1/decisions", false, map[string]any{"id": "draft:dec/replace", "statement": "Check drafts in the CLI, as req-demo REQ-1 requires.", "context": "req-demo AC-1.1 needs an offline check.", "alternatives_rejected": "A server-side check was rejected.", "supersedes": "DEC-9"}},
		{"draft:task/build", "/v1/tasks", true, map[string]any{"body": "Implement req-demo AC-1.1 as feature-demo describes, following draft:dec/use-cli.", "repo": "conveyor", "depends_on": []any{}, "requirement_ids": []any{"req-demo"}, "system_design_ids": []any{"feature-demo"}}},
		{"draft:task/gated", "/v1/tasks", true, map[string]any{"body": "Ship it after draft:task/build.", "repo": "conveyor", "base_branch": "release", "spec_approval": false, "merge_approval": true, "depends_on": []any{"draft:task/build"}, "requirement_ids": []any{}, "system_design_ids": []any{}}},
	} {
		items := draft.Item(tc.id)
		if len(items) != 1 {
			t.Fatalf("%s: %d items", tc.id, len(items))
		}
		request, err := BuildPlanItemRequest(items[0])
		if err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		if request.Method != "POST" || request.Path != tc.path || request.Multipart != tc.multipart || !reflect.DeepEqual(request.Body, tc.body) {
			t.Fatalf("%s request = %#v\nwant path=%s multipart=%v body=%#v", tc.id, request, tc.path, tc.multipart, tc.body)
		}
		// The digest covers exactly the body the builder returns.
		envelope, err := planDigestEnvelope(items[0])
		if err != nil || !reflect.DeepEqual(envelope["request_body"], request.Body) {
			t.Fatalf("%s envelope body differs: %v", tc.id, err)
		}
	}
	// An omitted gate is absent from the body, not false (AC-3.7).
	request, _ := BuildPlanItemRequest(draft.Item("draft:task/build")[0])
	for _, gate := range []string{"spec_approval", "merge_approval", "base_branch"} {
		if _, ok := request.Body[gate]; ok {
			t.Fatalf("omitted %s is present: %#v", gate, request.Body)
		}
	}
	// An item with layout problems has no request.
	writePlanFile(t, root, "tasks/broken.md", "---\nrepo: [x\n---\nBody\n")
	if _, err := BuildPlanItemRequest(readPlanFixture(t, root).Item("draft:task/broken")[0]); err == nil {
		t.Fatal("broken task built a request")
	}
}

// LP-4: the digest is sha256 over canonical JSON with sorted keys and no
// insignificant whitespace, and it is stable across reads and key order.
func TestPlanDigestCanonicalAndStable(t *testing.T) {
	canonical, err := planCanonicalJSON(map[string]any{"b": 1, "a": map[string]any{"d": "<x&y>", "c": []any{2, 1}}})
	if err != nil || string(canonical) != `{"a":{"c":[2,1],"d":"<x&y>"},"b":1}` {
		t.Fatalf("canonical = %s, %v", canonical, err)
	}
	item := PlanItem{ID: "draft:dec/x", Kind: PlanKindDecision, Layer: PlanLayerDecisions, Decision: PlanDecisionFields{Statement: "S <a&b>", Context: "C", AlternativesRejected: "A"}}
	pinned := `{"base_content_sha256":null,"base_version":0,"item_id":"draft:dec/x","kind":"decision","mode":"","request_body":{"alternatives_rejected":"A","context":"C","id":"draft:dec/x","statement":"S <a&b>"}}`
	sum := sha256.Sum256([]byte(pinned))
	digest, err := PlanItemDigest(item)
	if err != nil || digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %s, %v; want sha256 of %s", digest, err, pinned)
	}
	revision := PlanItem{ID: "component-x", Kind: PlanKindSystemDesign, Layer: PlanLayerSystemDesigns, Mode: PlanModeRevise, BaseVersion: 3, HasBase: true, Base: []byte("base\r\n"), Content: "\r\n# X\r\nbody\r\n"}
	baseSum := sha256.Sum256([]byte("base\r\n"))
	pinned = `{"base_content_sha256":"` + hex.EncodeToString(baseSum[:]) + `","base_version":3,"item_id":"component-x","kind":"system_design","mode":"revise","request_body":{"content":"# X\nbody"}}`
	sum = sha256.Sum256([]byte(pinned))
	if digest, err := PlanItemDigest(revision); err != nil || digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("revision digest = %s, %v; want sha256 of %s", digest, err, pinned)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) {
		t.Fatalf("digest format %q", digest)
	}

	root := newPlanFixture(t)
	first := planDigests(t, root)
	if second := planDigests(t, root); !reflect.DeepEqual(first, second) {
		t.Fatalf("digests changed between reads:\n%v\n%v", first, second)
	}
	writePlanFile(t, root, "tasks/build.md", "---\nsystem_design_ids: [feature-demo]\nrequirement_ids: [req-demo]\nrepo: conveyor\n---\nImplement req-demo AC-1.1 as feature-demo describes, following draft:dec/use-cli.\n")
	writePlanFile(t, root, "system-designs/feature-demo.md", strings.ReplaceAll(planFixtureDesign, "\n", "\r\n"))
	if third := planDigests(t, root); !reflect.DeepEqual(first, third) {
		t.Fatalf("front matter order or CRLF changed a digest:\n%v\n%v", first, third)
	}
}

// AC-2.4: edits to content no push sends keep every approval.
func TestPlanApprovalNotesOnly(t *testing.T) {
	root := newPlanFixture(t)
	approveAllPlanItems(t, root)
	writePlanFile(t, root, "notes/review.md", "Changed reviewer notes.\n")
	writePlanFile(t, root, "notes/round-2/summary.md", "More notes.\n")
	writePlanFile(t, root, "README.md", "Draft readme.\n")
	writePlanFile(t, root, "requirements/scratch.txt", "Not an item.\n")
	for id, state := range planStatusStates(t, root) {
		if state != PlanItemApproved {
			t.Fatalf("%s is %q after a notes-only edit", id, state)
		}
	}
}

// AC-2.3: a change to pushed content, base version, or base copy marks only
// that item changed since approval.
func TestPlanApprovalItemChanges(t *testing.T) {
	body := strings.TrimPrefix(planFixtureRequirement, "---\nmode: create\n---\n")
	for name, tc := range map[string]struct {
		item   string
		mutate func(t *testing.T, root string)
	}{
		"task body": {"draft:task/docs", func(t *testing.T, root string) {
			writePlanFile(t, root, "tasks/docs.md", planFixtureDocsTask+"More detail.\n")
		}},
		"task gate": {"draft:task/docs", func(t *testing.T, root string) {
			writePlanFile(t, root, "tasks/docs.md", strings.Replace(planFixtureDocsTask, "repo: conveyor\n", "repo: conveyor\nmerge_approval: false\n", 1))
		}},
		"decision field": {"draft:dec/use-cli", func(t *testing.T, root string) {
			writePlanFile(t, root, "decisions/use-cli.yaml", strings.Replace(planFixtureDecision, "was rejected", "is rejected", 1))
		}},
		"base version": {"req-demo", func(t *testing.T, root string) {
			writePlanFile(t, root, "requirements/req-demo.md", "---\nmode: revise\nbase_version: 3\n---\n"+body)
		}},
		"base copy": {"req-demo", func(t *testing.T, root string) {
			writePlanFile(t, root, "requirements/req-demo.base.md", body+"\nEdited.\n")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			root := newPlanFixture(t)
			if name == "base version" || name == "base copy" {
				writePlanFile(t, root, "requirements/req-demo.md", "---\nmode: revise\nbase_version: 2\n---\n"+body)
				writePlanFile(t, root, "requirements/req-demo.base.md", body)
			}
			approveAllPlanItems(t, root)
			tc.mutate(t, root)
			for id, state := range planStatusStates(t, root) {
				want := PlanItemApproved
				if id == tc.item {
					want = PlanItemChanged
				}
				if state != want {
					t.Fatalf("%s is %q, want %q", id, state, want)
				}
			}
		})
	}
}

// AC-2.1, AC-2.2: approve records every named item or none, naming each
// failing item.
func TestPlanApproveAllOrNothing(t *testing.T) {
	root := newPlanFixture(t)
	digests := planDigests(t, root)
	out, err := runPlanCommand(t, "approve", root,
		"req-demo@"+digests["req-demo"],
		"draft:task/unknown@"+digests["req-demo"],
		"draft:dec/use-cli@sha256:stale",
		"no-digest")
	if err == nil || !strings.Contains(err.Error(), "3 item(s) failed; no approval recorded") {
		t.Fatalf("err = %v", err)
	}
	for _, line := range []string{
		"draft:task/unknown: the draft defines no such item",
		"draft:dec/use-cli: current content digest is " + digests["draft:dec/use-cli"] + ", not sha256:stale",
		"no-digest: argument must be <item-id>@<digest>",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("output missing %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "approved ") {
		t.Fatalf("failed batch printed success:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, ".plan")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed batch wrote state: %v", err)
	}

	out, err = runPlanCommand(t, "approve", root, "req-demo@"+digests["req-demo"], "draft:task/build@"+digests["draft:task/build"])
	if err != nil || out != "approved req-demo@"+digests["req-demo"]+"\napproved draft:task/build@"+digests["draft:task/build"]+"\n" {
		t.Fatalf("approve: %v\n%s", err, out)
	}
	state, err := LoadPlanState(root)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(state.DraftID) {
		t.Fatalf("draft_id = %q", state.DraftID)
	}
	if !reflect.DeepEqual(state.Approvals, map[string]string{"req-demo": digests["req-demo"], "draft:task/build": digests["draft:task/build"]}) {
		t.Fatalf("approvals = %v", state.Approvals)
	}

	// A stale pair in a later batch leaves the earlier approvals untouched.
	before, _ := os.ReadFile(planStatePath(root))
	if _, err := runPlanCommand(t, "approve", root, "draft:task/docs@"+digests["draft:task/docs"], "req-demo@sha256:old"); err == nil {
		t.Fatal("stale batch succeeded")
	}
	after, _ := os.ReadFile(planStatePath(root))
	if string(before) != string(after) {
		t.Fatalf("stale batch changed state:\n%s\n%s", before, after)
	}

	// A malformed state is an error, never a reset.
	writePlanFile(t, root, ".plan/state.json", "{not json")
	if _, err := runPlanCommand(t, "approve", root, "draft:task/docs@"+digests["draft:task/docs"]); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("corrupt state: %v", err)
	}
	if data, _ := os.ReadFile(planStatePath(root)); string(data) != "{not json" {
		t.Fatalf("corrupt state was rewritten: %s", data)
	}
	if _, err := runPlanCommand(t, "status", root); err == nil {
		t.Fatal("status accepted a corrupt state")
	}
}

// LP-2: state is replaced atomically; a failed write, sync, or rename keeps
// the previous state and leaves no temporary file. draft_id is stable.
func TestPlanApproveAtomicReplacement(t *testing.T) {
	root := newPlanFixture(t)
	digests := planDigests(t, root)
	if _, err := runPlanCommand(t, "approve", root, "req-demo@"+digests["req-demo"]); err != nil {
		t.Fatal(err)
	}
	first, err := LoadPlanState(root)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(planStatePath(root))
	injected := errors.New("injected")
	original := planStateWriter
	t.Cleanup(func() { planStateWriter = original })
	for name, inject := range map[string]func(){
		"write":  func() { planStateWriter.write = func(*os.File, []byte) (int, error) { return 0, injected } },
		"sync":   func() { planStateWriter.sync = func(*os.File) error { return injected } },
		"rename": func() { planStateWriter.rename = func(string, string) error { return injected } },
	} {
		t.Run(name, func(t *testing.T) {
			planStateWriter = original
			inject()
			out, err := runPlanCommand(t, "approve", root, "draft:task/build@"+digests["draft:task/build"])
			if err == nil || !errors.Is(err, injected) || strings.Contains(out, "approved") {
				t.Fatalf("err=%v out=%q", err, out)
			}
			if after, _ := os.ReadFile(planStatePath(root)); string(after) != string(before) {
				t.Fatalf("state changed after %s failure", name)
			}
			entries, _ := os.ReadDir(filepath.Join(root, ".plan"))
			if len(entries) != 1 || entries[0].Name() != "state.json" {
				t.Fatalf(".plan entries after %s failure: %v", name, entries)
			}
		})
	}
	planStateWriter = original
	if _, err := runPlanCommand(t, "approve", root, "draft:task/build@"+digests["draft:task/build"]); err != nil {
		t.Fatal(err)
	}
	second, err := LoadPlanState(root)
	if err != nil {
		t.Fatal(err)
	}
	if second.DraftID != first.DraftID || second.Approvals["req-demo"] != digests["req-demo"] || second.Approvals["draft:task/build"] != digests["draft:task/build"] {
		t.Fatalf("second approval: %+v (first draft_id %s)", second, first.DraftID)
	}
}

// LP-2, LP-5: approve keeps every push record that push wrote, intent and
// posted alike.
func TestPlanApprovePreservesPushRecords(t *testing.T) {
	root := newPlanFixture(t)
	digests := planDigests(t, root)
	intentAt := time.Date(2026, 10, 20, 7, 0, 0, 123456789, time.FixedZone("", 2*3600))
	state := PlanState{
		DraftID:   "11111111-2222-4333-8444-555555555555",
		Approvals: map[string]string{"req-demo": digests["req-demo"]},
		Pushes: map[string]PlanPushRecord{
			"req-demo":          {Digest: digests["req-demo"], Phase: "posted", IntentAt: &intentAt, ServerID: "req-demo", Version: 1},
			"draft:dec/use-cli": {Digest: "sha256:old", Phase: "intent", IntentAt: &intentAt, ChosenID: "DEC-90"},
			"draft:task/build":  {Digest: "sha256:old", Phase: "intent", IntakeKey: "plan:11111111-2222-4333-8444-555555555555:draft:task/build:sha256:old"},
		},
	}
	if err := SavePlanState(root, state); err != nil {
		t.Fatal(err)
	}
	var beforePushes map[string]any
	data, _ := os.ReadFile(planStatePath(root))
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	beforePushes = raw["pushes"].(map[string]any)
	if _, err := runPlanCommand(t, "approve", root, "draft:task/docs@"+digests["draft:task/docs"]); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(planStatePath(root))
	raw = nil
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["pushes"], beforePushes) || raw["draft_id"] != state.DraftID {
		t.Fatalf("push records changed:\n%v\n%v", beforePushes, raw["pushes"])
	}
	loaded, err := LoadPlanState(root)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Pushes["req-demo"].IntentAt.Equal(intentAt) || loaded.Approvals["req-demo"] != digests["req-demo"] {
		t.Fatalf("loaded = %+v", loaded)
	}
}

// AC-2.5: status prints ID, layer, current digest, state, and the pushed
// server ID and version, and changes nothing.
func TestPlanStatusFields(t *testing.T) {
	root := newPlanFixture(t)
	digests := planDigests(t, root)
	state := PlanState{
		DraftID: "11111111-2222-4333-8444-555555555555",
		Approvals: map[string]string{
			"req-demo":          digests["req-demo"],
			"draft:dec/use-cli": "sha256:previous",
			"draft:task/build":  digests["draft:task/build"],
		},
		Pushes: map[string]PlanPushRecord{
			"req-demo":          {Digest: digests["req-demo"], Phase: "posted", ServerID: "req-demo", Version: 4},
			"draft:dec/use-cli": {Digest: "sha256:previous", Phase: "posted", ServerID: "DEC-91"},
			"draft:task/build":  {Digest: digests["draft:task/build"], Phase: "intent"},
		},
	}
	if err := SavePlanState(root, state); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(planStatePath(root))
	out, err := runPlanCommand(t, "status", root)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	rows := map[string][]string{}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "ITEM LAYER DIGEST STATE PUSHED" {
		t.Fatalf("header = %q", lines[0])
	}
	for _, line := range lines[1:] {
		fields := regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(line), -1)
		rows[fields[0]] = fields
	}
	want := map[string][]string{
		"req-demo":          {"req-demo", "requirements", digests["req-demo"], "approved", "req-demo v4"},
		"draft:dec/use-cli": {"draft:dec/use-cli", "decisions", digests["draft:dec/use-cli"], "changed since approval", "DEC-91"},
		"feature-demo":      {"feature-demo", "system-designs", digests["feature-demo"], "pending", "-"},
		"draft:task/build":  {"draft:task/build", "tasks", digests["draft:task/build"], "approved", "intent"},
		"draft:task/docs":   {"draft:task/docs", "tasks", digests["draft:task/docs"], "pending", "-"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %q\nwant %q", rows, want)
	}
	if after, _ := os.ReadFile(planStatePath(root)); string(after) != string(before) {
		t.Fatal("status changed state")
	}
	// An item that cannot be digested is reported and fails status.
	writePlanFile(t, root, "tasks/broken.md", "---\nrepo: [x\n---\nBody\n")
	out, err = runPlanCommand(t, "status", root)
	if err == nil || !strings.Contains(out, "draft:task/broken") || !strings.Contains(out, "invalid") {
		t.Fatalf("broken item: %v\n%s", err, out)
	}
}
