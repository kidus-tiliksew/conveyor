package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

const planValidRequirement = `# Alpha

The alpha capability.

` + "```conveyor:requirements" + `
- id: REQ-1
  statement: The system shall alpha.
  acceptance_criteria:
    - id: AC-1.1
      statement: When alpha runs, the system shall record it.
` + "```" + `
`

const planValidDecisions = `- id: D1
  statement: Approach A.
  context: Approach B was slower.
  alternatives:
    - Approach B
  cites:
    - req-alpha/REQ-1
`

const planValidDesign = `# Mechanism

Body cites D1.

` + "```conveyor:governs" + `
- repo: conveyor
  paths:
    - "*.go"
` + "```" + `
`

const planValidTasks = `- id: T1
  title: Implement alpha
  body: Do the work.
  depends_on: []
  governing:
    - req-alpha/REQ-1
    - req-alpha/AC-1.1
    - mechanism
  docs_paths:
    - docs/alpha.md
  docs_none_reason: ""
`

// planFixtureRepo creates a git repository whose draft folder is git-ignored,
// writes the supplied draft files under <root>/draft, and commits a tracked
// file plus .gitignore.
func planFixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	planGitFixture(t, root, "init", "-q")
	planGitFixture(t, root, "config", "user.name", "Fixture")
	planGitFixture(t, root, "config", "user.email", "fixture@example.invalid")
	planWriteFile(t, filepath.Join(root, ".gitignore"), "/draft/\n")
	planWriteFile(t, filepath.Join(root, "app.go"), "package app\n")
	for name, content := range files {
		planWriteFile(t, filepath.Join(root, "draft", name), content)
	}
	planGitFixture(t, root, "add", ".gitignore", "app.go")
	planGitFixture(t, root, "commit", "-qm", "fixture")
	return root
}

func planValidDraftFiles() map[string]string {
	return map[string]string{
		"brief.md":                  "# Brief\n\nGoal.\n",
		"requirements/req-alpha.md": planValidRequirement,
		"decisions.yml":             planValidDecisions,
		"designs/mechanism.md":      planValidDesign,
		"tasks.yml":                 planValidTasks,
		"notes.yml":                 "- id: req-alpha/REQ-1\n  headline: Alpha\n  example: Run alpha.\n  why: Needed.\n",
	}
}

func planGitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
}

func planWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runPlanCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := planCmd()
	command.SilenceUsage = true
	command.SilenceErrors = true
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), err
}

func TestPlanCheckPassesOnValidDraft(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	out, err := runPlanCLI(t, "check", filepath.Join(root, "draft"))
	if err != nil {
		t.Fatalf("valid draft failed check: %v\n%s", err, out)
	}
	if strings.Contains(out, "plan check:") {
		t.Fatalf("valid draft reported failures:\n%s", out)
	}
}

func TestPlanCheckNamesEveryFailingItem(t *testing.T) {
	files := map[string]string{
		"requirements/req-alpha.md": `# Alpha

Prose.

` + "```conveyor:requirements" + `
- id: REQ-1
  statement: The system shall alpha.
  acceptance_criteria:
    - id: AC-1.1
      statement: Records it.
- id: REQ-2
  statement: The system shall beta.
` + "```" + `
`,
		"decisions.yml": `- id: D1
  statement: Approach A.
  context: Because.
  alternatives: []
  cites: []
`,
		"designs/mechanism.md": `# Mechanism

Body cites D9.

` + "```conveyor:governs" + `
- repo: conveyor
  paths:
    - "*.go"
` + "```" + `
`,
		"tasks.yml": `- id: T1
  title: Implement alpha
  body: Do it.
  governing:
    - req-alpha/REQ-1
  docs_paths:
    - docs/alpha.md
- id: T2
  title: Later
  body: Later.
  docs_none_reason: ""
`,
	}
	root := planFixtureRepo(t, files)
	out, err := runPlanCLI(t, "check", filepath.Join(root, "draft"))
	if err == nil {
		t.Fatalf("broken draft passed check:\n%s", out)
	}
	for _, want := range []string{
		"req-alpha/REQ-1",
		"req-alpha/REQ-2",
		"D1",
		"T2",
		"mechanism",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check output does not name %q:\n%s", want, out)
		}
	}
}

func TestPlanCheckRejectsUnignoredDraftFolder(t *testing.T) {
	root := t.TempDir()
	planGitFixture(t, root, "init", "-q")
	planGitFixture(t, root, "config", "user.name", "Fixture")
	planGitFixture(t, root, "config", "user.email", "fixture@example.invalid")
	for name, content := range planValidDraftFiles() {
		planWriteFile(t, filepath.Join(root, "draft", name), content)
	}
	planWriteFile(t, filepath.Join(root, "app.go"), "package app\n")
	planGitFixture(t, root, "add", "-A")
	planGitFixture(t, root, "commit", "-qm", "fixture")

	out, err := runPlanCLI(t, "check", filepath.Join(root, "draft"))
	if err == nil {
		t.Fatalf("tracked draft folder passed check:\n%s", out)
	}
	if !strings.Contains(out, "draft/") || (!strings.Contains(out, "not git-ignored") && !strings.Contains(out, "tracked file")) {
		t.Fatalf("check did not name the tracked/unignored folder:\n%s", out)
	}
}

func TestPlanItemHashNormalizesNormativeText(t *testing.T) {
	base := requirementNormative(core.RequirementStatement{
		ID:        "REQ-1",
		Statement: "The system shall alpha.",
		AcceptanceCriteria: []core.AcceptanceCriterion{
			{ID: "AC-1.1", Statement: "When alpha runs, the system shall record it.  "},
		},
	})
	want := "statement: The system shall alpha.\nAC-1.1: When alpha runs, the system shall record it."
	if base != want {
		t.Fatalf("normative = %q want %q", base, want)
	}
	normalized := normalizePlanText("statement: The system shall alpha.\r\nAC-1.1: When alpha runs, the system shall record it.\t\r\n")
	if base != normalized {
		t.Fatalf("LF/trailing-whitespace normalization mismatch: %q vs %q", base, normalized)
	}
	first := planItem{ID: "req-alpha/REQ-1", Kind: planItemRequirement, Normative: base}
	second := planItem{ID: first.ID, Kind: first.Kind, Normative: normalized}
	if first.Hash() != second.Hash() {
		t.Fatalf("normalization-equivalent text hashed differently: %s vs %s", first.Hash(), second.Hash())
	}
	other := planItem{ID: first.ID, Kind: first.Kind, Normative: "statement: The system shall beta.\nAC-1.1: When alpha runs, the system shall record it."}
	if first.Hash() == other.Hash() {
		t.Fatal("normative change did not change the hash")
	}
	noted := first
	noted.Note = &planNote{Headline: "Alpha", Example: "Run it.", Why: "Needed."}
	if first.Hash() != noted.Hash() {
		t.Fatal("notes must never be hashed")
	}
	criteria := []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "When alpha runs, the system shall record it.  "}}
	story := func(iWant string) core.RequirementStatement {
		return core.RequirementStatement{
			ID:                 "REQ-1",
			Statement:          "The system shall alpha.",
			UserStory:          &core.RequirementUserStory{AsA: "operator", IWant: iWant, SoThat: "an outcome"},
			AcceptanceCriteria: criteria,
		}
	}
	storyBase := planItem{ID: first.ID, Kind: first.Kind, Normative: requirementNormative(story("alpha"))}
	storyEdited := planItem{ID: first.ID, Kind: first.Kind, Normative: requirementNormative(story("beta"))}
	if storyBase.Hash() == storyEdited.Hash() {
		t.Fatal("editing a user story must change the item hash")
	}
}

func TestLoadPlanDraftOrdersItemsAndAttachesNotes(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	draft, err := loadPlanDraft(filepath.Join(root, "draft"))
	if err != nil {
		t.Fatalf("load draft: %v", err)
	}
	wantKinds := []planItemKind{planItemRequirementDocument, planItemRequirement, planItemDecision, planItemDesign, planItemTask}
	wantIDs := []string{"req-alpha", "req-alpha/REQ-1", "D1", "mechanism", "T1"}
	if len(draft.Items) != len(wantIDs) {
		t.Fatalf("items = %d want %d: %+v", len(draft.Items), len(wantIDs), draft.Items)
	}
	for i, item := range draft.Items {
		if item.Kind != wantKinds[i] || item.ID != wantIDs[i] {
			t.Fatalf("item %d = (%s,%s) want (%s,%s)", i, item.Kind, item.ID, wantKinds[i], wantIDs[i])
		}
	}
	if draft.Items[1].Note == nil || draft.Items[1].Note.Headline != "Alpha" {
		t.Fatalf("requirement note not attached: %+v", draft.Items[1].Note)
	}
	if len(draft.Items[4].Links) != 3 {
		t.Fatalf("task links = %v", draft.Items[4].Links)
	}
	if len(draft.Raw.Requirements) != 1 || draft.Raw.Requirements[0].Title != "Alpha" {
		t.Fatalf("requirement raw = %+v", draft.Raw.Requirements)
	}
}

func TestPlanReviewRoundTrip(t *testing.T) {
	dir := t.TempDir()
	empty, err := readPlanReview(dir)
	if err != nil || empty.Round != 0 || len(empty.Items) != 0 {
		t.Fatalf("absent review = %+v, %v", empty, err)
	}
	want := &planReviewFile{Round: 2, Items: []planReviewEntry{{ID: "D1", Hash: "sha256:abc", Verdict: "approve", Comment: "ok"}}}
	if err := writePlanReview(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := readPlanReview(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Round != 2 || len(got.Items) != 1 || got.Items[0].Verdict != "approve" {
		t.Fatalf("round trip = %+v", got)
	}
}

// planDraftItemHash returns the approval hash of one item, failing when the
// draft holds no such item.
func planDraftItemHash(t *testing.T, draft *planDraft, id string) string {
	t.Helper()
	for _, item := range draft.Items {
		if item.ID == id {
			return item.Hash()
		}
	}
	t.Fatalf("draft has no item %q", id)
	return ""
}

func TestPlanCheckRejectsEmptyAndDuplicateItemIDs(t *testing.T) {
	design := "# T1\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - \"*.go\"\n```\n"
	task := "- id: T1\n  title: One\n  body: One.\n  docs_paths: [docs/a.md]\n"
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name: "duplicate within a kind",
			files: map[string]string{
				"tasks.yml": "- id: T1\n  title: One\n  body: One.\n  docs_paths: [docs/a.md]\n" +
					"- id: T1\n  title: Two\n  body: Two.\n  docs_paths: [docs/a.md]\n",
			},
			want: []string{`item ID "T1"`, "tasks.yml"},
		},
		{
			name: "duplicate across kinds",
			files: map[string]string{
				"designs/T1.md": design,
				"tasks.yml":     task,
			},
			want: []string{`item ID "T1"`, "designs/T1.md", "tasks.yml"},
		},
		{
			name: "empty id",
			files: map[string]string{
				"tasks.yml": "- id: \"\"\n  title: One\n  body: One.\n  docs_paths: [docs/a.md]\n",
			},
			want: []string{"item ID is empty", "tasks.yml"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := planFixtureRepo(t, tc.files)
			dir := filepath.Join(root, "draft")
			out, err := runPlanCLI(t, "check", dir)
			if err == nil {
				t.Fatalf("check passed a draft with a bad item ID:\n%s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("check output does not name %q:\n%s", want, out)
				}
			}
			// review loads the draft through the same path, so it refuses the
			// same draft instead of rendering ambiguous items.
			if _, err := buildPlanReviewView(dir); err == nil {
				t.Fatal("plan review accepted a draft with a bad item ID")
			}
		})
	}
}

func TestPlanRequirementDocumentItemTracksTitleAndProse(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	dir := filepath.Join(root, "draft")
	path := filepath.Join(dir, "requirements/req-alpha.md")

	base, err := loadPlanDraft(dir)
	if err != nil {
		t.Fatalf("load draft: %v", err)
	}
	documentBase := planDraftItemHash(t, base, "req-alpha")
	statementBase := planDraftItemHash(t, base, "req-alpha/REQ-1")

	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	planWriteFile(t, path, strings.Replace(string(original), "The alpha capability.", "The alpha capability, restated.", 1))
	prose, err := loadPlanDraft(dir)
	if err != nil {
		t.Fatalf("load prose-edited draft: %v", err)
	}
	if planDraftItemHash(t, prose, "req-alpha") == documentBase {
		t.Fatal("a prose edit outside the fence did not change the document item hash")
	}
	if planDraftItemHash(t, prose, "req-alpha/REQ-1") != statementBase {
		t.Fatal("a prose edit reset a statement item")
	}

	restated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	planWriteFile(t, path, strings.Replace(string(restated), "# Alpha\n", "# Alpha capabilities\n", 1))
	titled, err := loadPlanDraft(dir)
	if err != nil {
		t.Fatalf("load title-edited draft: %v", err)
	}
	if planDraftItemHash(t, titled, "req-alpha") == planDraftItemHash(t, prose, "req-alpha") {
		t.Fatal("a title edit did not change the document item hash")
	}
	if planDraftItemHash(t, titled, "req-alpha/REQ-1") != statementBase {
		t.Fatal("a title edit reset a statement item")
	}

	titledData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	planWriteFile(t, path, strings.Replace(string(titledData), "The system shall alpha.", "The system shall alpha now.", 1))
	statement, err := loadPlanDraft(dir)
	if err != nil {
		t.Fatalf("load statement-edited draft: %v", err)
	}
	if planDraftItemHash(t, statement, "req-alpha") != planDraftItemHash(t, titled, "req-alpha") {
		t.Fatal("a statement edit reset the document item")
	}
	if planDraftItemHash(t, statement, "req-alpha/REQ-1") == statementBase {
		t.Fatal("a statement edit did not reset its statement item")
	}
}
