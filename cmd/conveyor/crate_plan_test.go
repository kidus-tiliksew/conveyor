package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// Planning draft task items and crates (component-crates invariant 16;
// req-crates AC-6.4). The tests are named TestPlanCrate* so the focused plan
// selector runs them.

// TestPlanCrateCheckRefusesEmptyCrate: plan check refuses a stated crate
// that is empty, blank, null, or not one value, names the item, and accepts a
// crate ID and an omitted field.
func TestPlanCrateCheckRefusesEmptyCrate(t *testing.T) {
	for _, tc := range []struct {
		name, frontMatter string
		refused           bool
	}{
		{"omitted", "", false},
		{"named", "crate: crate-261118-abcdef\n", false},
		{"empty string", "crate: \"\"\n", true},
		{"blank string", "crate: \"   \"\n", true},
		{"no value", "crate:\n", true},
		{"null", "crate: null\n", true},
		{"tilde", "crate: ~\n", true},
		{"list", "crate: [crate-a, crate-b]\n", true},
		{"mapping", "crate:\n  id: crate-a\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newPlanFixture(t)
			writePlanFile(t, root, "tasks/crated.md", "---\nrepo: conveyor\n"+tc.frontMatter+"---\nShip req-demo AC-1.1.\n")
			lines, err := checkPlanLines(t, root)
			refusal := ""
			for _, line := range lines {
				if strings.HasPrefix(line, "draft:task/crated: "+planRuleCrate+":") {
					refusal = line
				}
			}
			if tc.refused {
				if err == nil || refusal == "" || !strings.Contains(refusal, "crate must name one crate ID") {
					t.Fatalf("check err=%v lines=%v, want a crate refusal for draft:task/crated", err, lines)
				}
				return
			}
			if err != nil || refusal != "" {
				t.Fatalf("check err=%v lines=%v, want no crate refusal", err, lines)
			}
		})
	}
}

// TestPlanCrateRequestAndDigest: an item without crate keeps the request and
// digest it had before crates, an item that names a crate sends crate_id,
// and the digest covers the crate (component-crates invariant 16).
func TestPlanCrateRequestAndDigest(t *testing.T) {
	plain := PlanItem{ID: "draft:task/plain", Kind: PlanKindTask, Layer: PlanLayerTasks, Content: "Ship req-demo AC-1.1.\n", Task: PlanTaskFields{Repo: "conveyor", RequirementIDs: []string{"req-demo"}}}
	request, err := BuildPlanItemRequest(plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := request.Body["crate_id"]; found {
		t.Fatalf("an item without crate sends crate_id: %v", request.Body)
	}
	// The digest and the canonical bytes it covers are the values the base
	// commit computed for this item before crates existed.
	pinned := `{"base_content_sha256":null,"base_version":0,"item_id":"draft:task/plain","kind":"task","mode":"","request_body":{"body":"Ship req-demo AC-1.1.","depends_on":[],"repo":"conveyor","requirement_ids":["req-demo"],"system_design_ids":[]}}`
	sum := sha256.Sum256([]byte(pinned))
	const preCrateDigest = "sha256:6f7063d9d58eee0dd0bf5237e27f99d07ee5a45fd0071c10993e7a14a4c07a89"
	if "sha256:"+hex.EncodeToString(sum[:]) != preCrateDigest {
		t.Fatalf("pinned bytes digest to %x, want %s", sum, preCrateDigest)
	}
	if digest, err := PlanItemDigest(plain); err != nil || digest != preCrateDigest {
		t.Fatalf("digest without crate = %s, %v, want %s", digest, err, preCrateDigest)
	}

	// The same through a draft file, read from disk.
	root := newPlanDraftDir(t)
	writePlanFile(t, root, "tasks/plain.md", "---\nrepo: conveyor\nrequirement_ids: [req-demo]\nmerge_approval: false\n---\nShip req-demo AC-1.1.\n")
	draft, _, err := ReadPlanDraft(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	const preCrateFileDigest = "sha256:6a977ea074376039383f82dba6e510170653d977e291c5a09b060683e38fbb9a"
	if digest, err := PlanItemDigest(draft.Items[0]); err != nil || digest != preCrateFileDigest {
		t.Fatalf("file digest without crate = %s, %v, want %s", digest, err, preCrateFileDigest)
	}

	crated := plain
	crateID := "crate-261118-abcdef"
	crated.Task.Crate = &crateID
	request, err = BuildPlanItemRequest(crated)
	if err != nil {
		t.Fatal(err)
	}
	if request.Body["crate_id"] != crateID {
		t.Fatalf("an item with crate sends %v", request.Body)
	}
	cratedDigest, err := PlanItemDigest(crated)
	if err != nil || cratedDigest == preCrateDigest {
		t.Fatalf("digest with crate = %s, %v, want a digest other than %s", cratedDigest, err, preCrateDigest)
	}
	other := plain
	otherID := "crate-261118-fedcba"
	other.Task.Crate = &otherID
	if otherDigest, err := PlanItemDigest(other); err != nil || otherDigest == cratedDigest {
		t.Fatalf("digest with another crate = %s, %v, want it to differ from %s", otherDigest, err, cratedDigest)
	}

	// A stated empty crate sends no crate_id. Plan check refuses it first.
	empty := plain
	blank := ""
	empty.Task.Crate = &blank
	if request, err := BuildPlanItemRequest(empty); err != nil || request.Body["crate_id"] != nil {
		t.Fatalf("an empty crate built %v, %v", request.Body, err)
	}
}

// writeCratedTasks adds two task items to the fixture: draft:task/a-first
// names firstCrate and draft:task/b-second names secondCrate, each only when
// it is not empty.
func writeCratedTasks(t *testing.T, root, firstCrate, secondCrate string) {
	t.Helper()
	for name, crate := range map[string]string{"a-first": firstCrate, "b-second": secondCrate} {
		frontMatter := "repo: conveyor\n"
		if crate != "" {
			frontMatter += "crate: " + crate + "\n"
		}
		writePlanFile(t, root, "tasks/"+name+".md", "---\n"+frontMatter+"---\nShip req-demo AC-1.1 for "+name+".\n")
	}
}

// TestPlanCratePushCreatesTaskInCrate: a task item that names an open crate
// posts crate_id, and an item without crate posts none (req-crates AC-6.4).
func TestPlanCratePushCreatesTaskInCrate(t *testing.T) {
	f := newPlanFakeFactory(t)
	f.crates["crate-open"] = "open"
	root := newPlanFixture(t)
	writeCratedTasks(t, root, "crate-open", "")
	approveAllPlanItems(t, root)
	pushPlanThrough(t, f, root, PlanLayerTasks)
	mustPushPlan(t, root, PlanLayerTasks)
	byItem := map[string]map[string]any{}
	for _, post := range f.postsTo("/v1/tasks") {
		for _, name := range []string{"a-first", "b-second"} {
			if strings.Contains(post.IntakeKey, ":draft:task/"+name+":") {
				byItem[name] = post.Body
			}
		}
	}
	if byItem["a-first"]["crate_id"] != "crate-open" {
		t.Fatalf("crated item posted %v", byItem["a-first"])
	}
	if second, ok := byItem["b-second"]; !ok || second["crate_id"] != nil {
		t.Fatalf("item without crate posted %v", second)
	}
	if f.crateReads != 1 {
		t.Fatalf("crate reads = %d, want 1", f.crateReads)
	}
}

// TestPlanCratePushRefusesMissingOrClosedCrate: a missing or closed crate
// refuses the whole tasks layer, names the item, and posts no task, also when
// an earlier item of the layer names a valid crate (component-crates
// invariant 16; req-crates AC-6.4).
func TestPlanCratePushRefusesMissingOrClosedCrate(t *testing.T) {
	for _, tc := range []struct {
		name, crate, want string
	}{
		{"missing", "crate-missing", "does not hold"},
		{"closed", "crate-closed", "which is closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlanFakeFactory(t)
			f.crates["crate-open"] = "open"
			f.crates["crate-closed"] = "closed"
			root := newPlanFixture(t)
			writeCratedTasks(t, root, "crate-open", tc.crate)
			approveAllPlanItems(t, root)
			pushPlanThrough(t, f, root, PlanLayerTasks)
			out := expectPlanRefusal(t, f, root, PlanLayerTasks, "draft:task/b-second: "+planRuleCrate+":", tc.crate, tc.want)
			if strings.Contains(out, "draft:task/a-first: "+planRuleCrate) {
				t.Fatalf("the valid item was refused:\n%s", out)
			}
			if posts := f.postsTo("/v1/tasks"); len(posts) != 0 {
				t.Fatalf("a refused layer posted %d task(s)", len(posts))
			}
		})
	}
}

// TestPlanCratePushStopsOnCrateReadFailure: a crate read that fails for
// another reason stops the push before any task posts.
func TestPlanCratePushStopsOnCrateReadFailure(t *testing.T) {
	f := newPlanFakeFactory(t)
	root := newPlanFixture(t)
	writeCratedTasks(t, root, "", "crate-open")
	approveAllPlanItems(t, root)
	pushPlanThrough(t, f, root, PlanLayerTasks)
	f.crateFailure = http.StatusInternalServerError
	out, err := pushPlan(t, root, PlanLayerTasks)
	if err == nil || !strings.Contains(err.Error(), "read crate crate-open") || !strings.Contains(err.Error(), "nothing posted") {
		t.Fatalf("push err=%v out=%s, want the crate read failure and nothing posted", err, out)
	}
	if posts := f.postsTo("/v1/tasks"); len(posts) != 0 {
		t.Fatalf("push posted %d task(s) after a failed crate read", len(posts))
	}
}
