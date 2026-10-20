package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/spf13/cobra"
)

// Fixture items for a draft that passes every check (req-local-planning
// REQ-1). Tests copy the fixture into t.TempDir and mutate one item.
const (
	planFixtureRequirement = "---\nmode: create\n---\n# Demo capability\n\nThe demo capability keeps drafts local.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: The CLI shall check drafts.\n  acceptance_criteria:\n    - id: AC-1.1\n      statement: When a draft is checked, the CLI shall print failures.\n```\n"
	planFixtureDecision    = "statement: Check drafts in the CLI, as req-demo REQ-1 requires.\ncontext: req-demo AC-1.1 needs an offline check.\nalternatives_rejected: A server-side check was rejected.\n"
	planFixtureDesign      = "---\nmode: create\ntitle: Demo design\ncategory: Feature design\n---\n# Demo design\n\nImplements req-demo REQ-1 under draft:dec/use-cli.\n\n```conveyor:governs\n- repo: elsewhere\n  paths:\n    - src/**\n```\n"
	planFixtureBuildTask   = "---\nrepo: conveyor\nrequirement_ids: [req-demo]\nsystem_design_ids: [feature-demo]\n---\nImplement req-demo AC-1.1 as feature-demo describes, following draft:dec/use-cli.\n"
	planFixtureDocsTask    = "---\nrepo: conveyor\ndepends_on: [draft:task/build]\n---\nDocument the check after draft:task/build.\n"
)

func writePlanFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isolatePlanGit keeps git from discovering a repository above dir, so a
// temporary directory is outside every working tree wherever tests run.
func isolatePlanGit(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// newPlanFixture writes the passing draft outside any git working tree.
func newPlanFixture(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	isolatePlanGit(t, base)
	root := filepath.Join(base, "draft")
	writePlanFixture(t, root)
	return root
}

func writePlanFixture(t *testing.T, root string) {
	t.Helper()
	writePlanFile(t, root, "requirements/req-demo.md", planFixtureRequirement)
	writePlanFile(t, root, "decisions/use-cli.yaml", planFixtureDecision)
	writePlanFile(t, root, "system-designs/feature-demo.md", planFixtureDesign)
	writePlanFile(t, root, "tasks/build.md", planFixtureBuildTask)
	writePlanFile(t, root, "tasks/docs.md", planFixtureDocsTask)
	writePlanFile(t, root, "notes/review.md", "Reviewer notes are never pushed.\n")
}

func planGitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v %s", strings.Join(args, " "), err, out)
	}
}

// newPlanRepo creates a git repository named "repo" with one tracked file
// and returns the repository and a draft directory inside it.
func newPlanRepo(t *testing.T, ignore bool) (string, string) {
	t.Helper()
	base := t.TempDir()
	isolatePlanGit(t, base)
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	planGitRun(t, repo, "init", "-q")
	writePlanFile(t, repo, "internal/app/main.go", "package app\n")
	if ignore {
		writePlanFile(t, repo, ".gitignore", "plan-draft/\n")
	}
	planGitRun(t, repo, "add", "internal/app/main.go")
	draft := filepath.Join(repo, "plan-draft")
	writePlanFixture(t, draft)
	return repo, draft
}

func runPlanCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "conveyor", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(planCmd())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"plan"}, args...))
	err := root.Execute()
	return out.String(), err
}

func checkPlanLines(t *testing.T, root string) ([]string, error) {
	t.Helper()
	out, err := runPlanCommand(t, "check", root)
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" && !strings.HasPrefix(line, "plan check:") {
			lines = append(lines, line)
		}
	}
	return lines, err
}

// expectPlanFailure asserts the check failed and printed a line starting with
// prefix and containing each fragment.
func expectPlanFailure(t *testing.T, root, prefix string, fragments ...string) []string {
	t.Helper()
	lines, err := checkPlanLines(t, root)
	if err == nil {
		t.Fatalf("check passed; want failure %q\n%s", prefix, strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(line, fragment) {
				matched = false
			}
		}
		if matched {
			return lines
		}
	}
	t.Fatalf("no line %q containing %q in:\n%s", prefix, fragments, strings.Join(lines, "\n"))
	return nil
}

func planLinesWith(lines []string, prefix string) []string {
	var found []string
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			found = append(found, line)
		}
	}
	return found
}

// LP-2: a malformed or extra YAML document after a decision's first document
// is an item-level parse failure, never a silently shortened item.
func TestPlanReadMalformedTrailingYAML(t *testing.T) {
	for name, tc := range map[string]struct{ trailer, reason string }{
		"malformed":       {"---\ncontext: [unterminated\n", "decisions/use-cli.yaml: decision YAML: trailing YAML document:"},
		"second document": {"---\ncontext: Another document.\n", "decisions/use-cli.yaml: decision YAML: holds more than one YAML document"},
	} {
		t.Run(name, func(t *testing.T) {
			root := newPlanFixture(t)
			writePlanFile(t, root, "decisions/use-cli.yaml", planFixtureDecision+tc.trailer)
			expectPlanFailure(t, root, "draft:dec/use-cli: LP-2: "+tc.reason)
			draft := readPlanFixture(t, root)
			if _, err := PlanItemDigest(draft.Item("draft:dec/use-cli")[0]); err == nil {
				t.Fatal("an item with a trailing YAML document has a digest")
			}
		})
	}
}

// REQ-1 / LP-2: the reader takes exactly the layout's paths, keeps notes and
// other files out, and reports layout problems by item.
func TestPlanReadDraftLayout(t *testing.T) {
	root := newPlanFixture(t)
	writePlanFile(t, root, "requirements/req-old.md", "---\nmode: revise\nbase_version: 3\n---\n"+strings.TrimPrefix(planFixtureRequirement, "---\nmode: create\n---\n"))
	writePlanFile(t, root, "requirements/req-old.base.md", "# Old\n")
	writePlanFile(t, root, "requirements/nested/req-x.md", "ignored")
	writePlanFile(t, root, "requirements/readme.txt", "ignored")
	writePlanFile(t, root, "decisions/Bad_Name.yaml", planFixtureDecision)
	writePlanFile(t, root, "decisions/typo.yaml", planFixtureDecision+"supersede: DEC-1\n")
	writePlanFile(t, root, "tasks/gated.md", "---\nrepo: conveyor\nplan_approval: false\n---\nBody.\n")
	writePlanFile(t, root, "system-designs/req-demo.md", planFixtureDesign)
	writePlanFile(t, root, "top-level.md", "ignored")

	draft, diagnostics, err := ReadPlanDraft(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, item := range draft.Items {
		ids = append(ids, item.Layer+" "+item.ID)
	}
	want := []string{
		"requirements req-demo", "requirements req-old",
		"decisions draft:dec/Bad_Name", "decisions draft:dec/typo", "decisions draft:dec/use-cli",
		"system-designs feature-demo", "system-designs req-demo",
		"tasks draft:task/build", "tasks draft:task/docs", "tasks draft:task/gated",
	}
	if strings.Join(ids, "|") != strings.Join(want, "|") {
		t.Fatalf("items = %q\nwant %q", ids, want)
	}
	old := draft.Item("req-old")[0]
	if old.Mode != PlanModeRevise || old.BaseVersion != 3 || !old.HasBase || string(old.Base) != "# Old\n" {
		t.Fatalf("revision item = %+v", old)
	}
	gated := draft.Item("draft:task/gated")[0]
	if gated.Task.PlanApproval == nil || *gated.Task.PlanApproval || gated.Task.MergeApproval != nil {
		t.Fatalf("gates = %v %v", gated.Task.PlanApproval, gated.Task.MergeApproval)
	}
	var rendered []string
	for _, diagnostic := range diagnostics {
		rendered = append(rendered, diagnostic.String())
	}
	joined := strings.Join(rendered, "\n")
	for _, fragment := range []string{
		`draft:dec/Bad_Name: LP-2: decisions/Bad_Name.yaml: draft-local name "Bad_Name" must match`,
		"draft:dec/typo: LP-2: decisions/typo.yaml: decision YAML:",
		"req-demo: LP-2: requirements/req-demo.md: item ID is defined by 2 files",
		"req-demo: LP-2: system-designs/req-demo.md: item ID is defined by 2 files",
	} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("diagnostics missing %q:\n%s", fragment, joined)
		}
	}
	if len(rendered) != 4 {
		t.Fatalf("diagnostics = %d, want 4:\n%s", len(rendered), joined)
	}
}

// AC-1.1–AC-1.12: one failing fixture per rule; each prints
// `<item-id>: <rule>: <reason>` and exits non-zero.
func TestPlanCheckRules(t *testing.T) {
	requirementBody := func(fence string) string {
		return "---\nmode: create\n---\n# Demo capability\n\nProse.\n\n" + fence
	}
	factoryReason := func(content string) string {
		_, err := pipeline.ParseRequirementDocument(strings.SplitN(content, "---\n", 3)[2])
		if err == nil {
			t.Fatalf("fixture unexpectedly valid: %q", content)
		}
		return err.Error()
	}

	t.Run("AC_1_2", func(t *testing.T) {
		t.Run("not ignored", func(t *testing.T) {
			_, draft := newPlanRepo(t, false)
			expectPlanFailure(t, draft, "draft: AC-1.2: ", "git does not ignore it")
		})
		t.Run("ignored passes", func(t *testing.T) {
			_, draft := newPlanRepo(t, true)
			lines, err := checkPlanLines(t, draft)
			if err != nil || len(lines) != 0 {
				t.Fatalf("ignored draft: err=%v lines=%q", err, lines)
			}
		})
		t.Run("tracked file under ignored directory", func(t *testing.T) {
			repo, draft := newPlanRepo(t, true)
			planGitRun(t, repo, "add", "-f", "plan-draft/notes/review.md")
			expectPlanFailure(t, draft, "draft: AC-1.2: ", "git tracks 1 file(s) in the draft directory: notes/review.md")
		})
		t.Run("git execution failure is not outside", func(t *testing.T) {
			root := newPlanFixture(t)
			t.Setenv("PATH", t.TempDir())
			expectPlanFailure(t, root, "draft: AC-1.2: ", "cannot run git")
		})
	})

	t.Run("AC_1_3", func(t *testing.T) {
		for name, content := range map[string]string{
			"heading":            "---\nmode: create\n---\nDemo capability\n\n```conveyor:requirements\n- id: REQ-1\n  statement: The CLI shall work.\n```\n",
			"missing fence":      requirementBody("No block.\n"),
			"duplicate fences":   requirementBody("```conveyor:requirements\n- id: REQ-1\n  statement: A shall b.\n```\n\n```conveyor:requirements\n- id: REQ-2\n  statement: A shall b.\n```\n"),
			"near-miss fence":    requirementBody("```conveyor:requirement\n- id: REQ-1\n  statement: A shall b.\n```\n"),
			"identifier outside": requirementBody("REQ-1: stray.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: A shall b.\n```\n"),
			"duplicate id":       requirementBody("```conveyor:requirements\n- id: REQ-1\n  statement: A shall b.\n- id: REQ-1\n  statement: C shall d.\n```\n"),
			"wrong parent":       requirementBody("```conveyor:requirements\n- id: REQ-1\n  statement: A shall b.\n  acceptance_criteria:\n    - id: AC-2.1\n      statement: X shall y.\n```\n"),
			"incomplete story":   requirementBody("```conveyor:requirements\n- id: REQ-1\n  statement: A shall b.\n  user_story:\n    as_a: operator\n    i_want: drafts\n```\n"),
		} {
			t.Run(name, func(t *testing.T) {
				root := newPlanFixture(t)
				writePlanFile(t, root, "requirements/req-demo.md", content)
				expectPlanFailure(t, root, "req-demo: AC-1.3: "+factoryReason(content))
			})
		}
		t.Run("criterion without shall", func(t *testing.T) {
			root := newPlanFixture(t)
			writePlanFile(t, root, "requirements/req-demo.md", strings.Replace(planFixtureRequirement, "the CLI shall print", "the CLI prints", 1))
			expectPlanFailure(t, root, `req-demo: AC-1.3: acceptance criterion AC-1.1 contains no "shall"`)
		})
	})

	t.Run("AC_1_4", func(t *testing.T) {
		designWith := func(fence string) string {
			return "---\nmode: create\ntitle: Demo design\ncategory: Feature design\n---\n# Demo design\n\n" + fence
		}
		for name, tc := range map[string]struct{ file, content, reason string }{
			"missing governs":  {"feature-demo", designWith("No fence.\n"), "system design must contain exactly one conveyor:governs fence"},
			"two governs":      {"feature-demo", designWith("```conveyor:governs\n- repo: a\n  paths: [x/**]\n```\n\n```conveyor:governs\n- repo: b\n  paths: [y/**]\n```\n"), "system design must contain exactly one conveyor:governs fence"},
			"character class":  {"feature-demo", designWith("```conveyor:governs\n- repo: a\n  paths:\n    - src/[ab]/*.go\n```\n"), `governed path glob "src/[ab]/*.go" uses unsupported character-class syntax`},
			"missing category": {"feature-demo", strings.Replace(planFixtureDesign, "category: Feature design\n", "", 1), "system design id, title, and category are required"},
			"invalid id":       {"bad+id", planFixtureDesign, "system design id must use letters, numbers, dot, underscore, or hyphen"},
		} {
			t.Run(name, func(t *testing.T) {
				root := newPlanFixture(t)
				writePlanFile(t, root, "system-designs/"+tc.file+".md", tc.content)
				expectPlanFailure(t, root, tc.file+": AC-1.4: "+tc.reason)
			})
		}
	})

	t.Run("AC_1_5", func(t *testing.T) {
		for name, decision := range map[string]string{
			"no citation":            "statement: Check drafts.\ncontext: Operators asked.\nalternatives_rejected: None.\n",
			"document without id":    "statement: Check drafts for req-demo.\ncontext: Operators asked.\nalternatives_rejected: None.\n",
			"identifier without doc": "statement: Check drafts for REQ-1.\ncontext: Operators asked.\nalternatives_rejected: None.\n",
		} {
			t.Run(name, func(t *testing.T) {
				root := newPlanFixture(t)
				writePlanFile(t, root, "decisions/use-cli.yaml", decision)
				expectPlanFailure(t, root, "draft:dec/use-cli: AC-1.5: statement and context cite no requirement")
			})
		}
		t.Run("decision content uses factory reason", func(t *testing.T) {
			root := newPlanFixture(t)
			writePlanFile(t, root, "decisions/use-cli.yaml", "statement: Check drafts per req-demo REQ-1.\ncontext: req-demo AC-1.1.\n")
			reason := core.ValidateDecision(core.Decision{Statement: "x", Context: "y", Origin: core.DecisionOriginOperator}).Error()
			expectPlanFailure(t, root, "draft:dec/use-cli: LP-3: "+reason)
		})
	})

	t.Run("AC_1_6", func(t *testing.T) {
		root := newPlanFixture(t)
		writePlanFile(t, root, "tasks/build.md", strings.Replace(planFixtureBuildTask, "following draft:dec/use-cli.", "following draft:dec/use, draft:dec/use-cli-extra, draft:task/missing and xdraft:dec/nope.", 1))
		lines := expectPlanFailure(t, root, "draft:task/build: AC-1.6: cites draft:dec/use, which the draft does not define")
		expectPlanFailure(t, root, "draft:task/build: AC-1.6: cites draft:dec/use-cli-extra,")
		expectPlanFailure(t, root, "draft:task/build: AC-1.6: cites draft:task/missing,")
		if got := planLinesWith(lines, "draft:task/build: AC-1.6:"); len(got) != 3 {
			t.Fatalf("AC-1.6 lines = %q", got)
		}
		root = newPlanFixture(t)
		writePlanFile(t, root, "tasks/docs.md", strings.Replace(planFixtureDocsTask, "[draft:task/build]", "[draft:task/absent]", 1))
		expectPlanFailure(t, root, "draft:task/docs: AC-1.6: cites draft:task/absent,")
	})

	t.Run("AC_1_7", func(t *testing.T) {
		twoCriteria := strings.Replace(planFixtureRequirement, "      statement: When a draft is checked, the CLI shall print failures.\n", "      statement: When a draft is checked, the CLI shall print failures.\n    - id: AC-1.2\n      statement: When a check fails, the CLI shall exit non-zero.\n", 1)
		t.Run("added criterion uncited", func(t *testing.T) {
			root := newPlanFixture(t)
			writePlanFile(t, root, "requirements/req-demo.md", twoCriteria)
			lines := expectPlanFailure(t, root, "req-demo: AC-1.7: acceptance criterion AC-1.2 is added and no task item cites it")
			if got := planLinesWith(lines, "req-demo: AC-1.7:"); len(got) != 1 {
				t.Fatalf("AC-1.7 lines = %q", got)
			}
		})
		t.Run("range citation covers", func(t *testing.T) {
			root := newPlanFixture(t)
			writePlanFile(t, root, "requirements/req-demo.md", twoCriteria)
			writePlanFile(t, root, "tasks/build.md", strings.Replace(planFixtureBuildTask, "req-demo AC-1.1", "req-demo AC-1.1–AC-1.2", 1))
			if lines, err := checkPlanLines(t, root); err != nil {
				t.Fatalf("range citation: %v %q", err, lines)
			}
		})
		t.Run("revision compares with base", func(t *testing.T) {
			root := newPlanFixture(t)
			base := strings.TrimPrefix(twoCriteria, "---\nmode: create\n---\n")
			revised := strings.Replace(base, "the CLI shall print failures.", "the CLI shall print every failure.", 1)
			revised = strings.Replace(revised, "the CLI shall exit non-zero.\n", "the CLI shall exit non-zero.\n    - id: AC-1.3\n      statement: When a check passes, the CLI shall exit zero.\n", 1)
			writePlanFile(t, root, "requirements/req-demo.md", "---\nmode: revise\nbase_version: 2\n---\n"+revised)
			writePlanFile(t, root, "requirements/req-demo.base.md", base)
			writePlanFile(t, root, "tasks/build.md", strings.Replace(planFixtureBuildTask, "req-demo AC-1.1", "req-demo REQ-1", 1))
			lines := expectPlanFailure(t, root, "req-demo: AC-1.7: acceptance criterion AC-1.1 is reworded relative to the base version and no task item cites it")
			expectPlanFailure(t, root, "req-demo: AC-1.7: acceptance criterion AC-1.3 is added relative to the base version")
			if got := planLinesWith(lines, "req-demo: AC-1.7:"); len(got) != 2 {
				t.Fatalf("unchanged AC-1.2 must not fail: %q", got)
			}
		})
		t.Run("same criterion ID in another document", func(t *testing.T) {
			root := newPlanFixture(t)
			writePlanFile(t, root, "requirements/req-other.md", planFixtureRequirement)
			lines := expectPlanFailure(t, root, "req-other: AC-1.7: acceptance criterion AC-1.1 is added")
			if got := planLinesWith(lines, "req-demo: AC-1.7:"); len(got) != 0 {
				t.Fatalf("req-demo is cited: %q", got)
			}
		})
		t.Run("external requirement citation is not credited to a draft requirement", func(t *testing.T) {
			for name, body := range map[string]string{
				"after draft mention":  "Implement req-demo AC-1.1 following draft:dec/use-cli. Separately implement req-external AC-1.2.\n",
				"before draft mention": "Implement req-external AC-1.2 first, then req-demo AC-1.1 following draft:dec/use-cli.\n",
				"listed requirement":   "Cover AC-1.1 as draft:dec/use-cli decides. Separately implement req-external AC-1.2.\n",
			} {
				t.Run(name, func(t *testing.T) {
					root := newPlanFixture(t)
					writePlanFile(t, root, "requirements/req-demo.md", twoCriteria)
					writePlanFile(t, root, "tasks/build.md", "---\nrepo: conveyor\nrequirement_ids: [req-demo]\n---\n"+body)
					lines := expectPlanFailure(t, root, "req-demo: AC-1.7: acceptance criterion AC-1.2 is added and no task item cites it")
					if got := planLinesWith(lines, "req-demo: AC-1.7:"); len(got) != 1 {
						t.Fatalf("AC-1.1 is cited; only AC-1.2 may fail: %q", got)
					}
				})
			}
		})
		t.Run("requirement_ids attribute leading criteria", func(t *testing.T) {
			root := newPlanFixture(t)
			writePlanFile(t, root, "tasks/build.md", "---\nrepo: conveyor\nrequirement_ids: [req-demo]\n---\nCover AC-1.1 as draft:dec/use-cli decides.\n")
			if lines, err := checkPlanLines(t, root); err != nil {
				t.Fatalf("requirement_ids attribution: %v %q", err, lines)
			}
		})
		t.Run("no tasks", func(t *testing.T) {
			root := newPlanFixture(t)
			writePlanFile(t, root, "requirements/req-demo.md", twoCriteria)
			for _, name := range []string{"build", "docs"} {
				if err := os.Remove(filepath.Join(root, "tasks", name+".md")); err != nil {
					t.Fatal(err)
				}
			}
			if lines, err := checkPlanLines(t, root); err != nil {
				t.Fatalf("draft without tasks: %v %q", err, lines)
			}
		})
	})

	t.Run("AC_1_8", func(t *testing.T) {
		repo, draft := newPlanRepo(t, true)
		// The containing repository is named by its directory and by its
		// origin's last path segment.
		planGitRun(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
		writePlanFile(t, draft, "system-designs/feature-demo.md", strings.Replace(planFixtureDesign, "- repo: elsewhere\n  paths:\n    - src/**\n", "- repo: repo\n  paths:\n    - internal/**\n    - web/**\n- repo: widget\n  paths:\n    - internal/app/*.go\n    - docs/*.md\n- repo: elsewhere\n  paths:\n    - nothing/**\n", 1))
		lines, err := checkPlanLines(t, draft)
		if err != nil {
			t.Fatalf("warning-only draft failed: %v %q", err, lines)
		}
		want := []string{
			`warning: feature-demo: AC-1.8: glob "web/**" matches no file git tracks in repository "repo"`,
			`warning: feature-demo: AC-1.8: glob "docs/*.md" matches no file git tracks in repository "widget"`,
		}
		if strings.Join(lines, "\n") != strings.Join(want, "\n") {
			t.Fatalf("lines = %q\nwant %q", lines, want)
		}
	})

	t.Run("AC_1_9", func(t *testing.T) {
		root := newPlanFixture(t)
		writePlanFile(t, root, "tasks/build.md", strings.Replace(planFixtureBuildTask, "repo: conveyor\n", "", 1))
		expectPlanFailure(t, root, "draft:task/build: AC-1.9: the task names no repository (repo)")
		root = newPlanFixture(t)
		writePlanFile(t, root, "tasks/docs.md", "---\nrepo: conveyor\n---\n\n  \n")
		expectPlanFailure(t, root, "draft:task/docs: AC-1.9: the task body is empty")
		root = newPlanFixture(t)
		writePlanFile(t, root, "tasks/build.md", strings.Replace(planFixtureBuildTask, "repo: conveyor\n", "repo: conveyor\ndepends_on: [draft:task/docs]\n", 1))
		writePlanFile(t, root, "tasks/self.md", "---\nrepo: conveyor\ndepends_on: [draft:task/self]\n---\nLoop.\n")
		writePlanFile(t, root, "tasks/server-id.md", "---\nrepo: conveyor\ndepends_on: [261020-abc123]\n---\nWrong reference.\n")
		lines := expectPlanFailure(t, root, "draft:task/build: AC-1.9: depends_on forms a cycle among draft:task/build, draft:task/docs")
		expectPlanFailure(t, root, "draft:task/docs: AC-1.9: depends_on forms a cycle among draft:task/build, draft:task/docs")
		expectPlanFailure(t, root, "draft:task/self: AC-1.9: depends_on forms a cycle among draft:task/self")
		expectPlanFailure(t, root, `draft:task/server-id: LP-2: depends_on entry "261020-abc123" is not a draft-local task ID`)
		if len(planLinesWith(lines, "draft:task/")) != 4 {
			t.Fatalf("lines = %q", lines)
		}
	})

	t.Run("AC_1_10", func(t *testing.T) {
		body := strings.TrimPrefix(planFixtureRequirement, "---\nmode: create\n---\n")
		root := newPlanFixture(t)
		writePlanFile(t, root, "requirements/req-demo.md", "---\nmode: revise\n---\n"+body)
		writePlanFile(t, root, "requirements/req-demo.base.md", body)
		expectPlanFailure(t, root, "req-demo: AC-1.10: a revision records no base_version")
		root = newPlanFixture(t)
		writePlanFile(t, root, "requirements/req-demo.md", "---\nmode: revise\nbase_version: 4\n---\n"+body)
		expectPlanFailure(t, root, "req-demo: AC-1.10: a revision holds no copy of its base version; write requirements/req-demo.base.md")
		root = newPlanFixture(t)
		writePlanFile(t, root, "requirements/req-demo.md", "---\nmode: revise\nbase_version: 4\n---\n"+body)
		writePlanFile(t, root, "requirements/req-demo.base.md", "Not a requirement\n")
		expectPlanFailure(t, root, "req-demo: AC-1.10: the base copy requirements/req-demo.base.md is not a valid requirement: requirement content must begin")
		root = newPlanFixture(t)
		writePlanFile(t, root, "system-designs/feature-demo.md", "---\nmode: revise\nbase_version: 2\n---\n"+strings.SplitN(planFixtureDesign, "---\n", 3)[2])
		writePlanFile(t, root, "system-designs/feature-demo.base.md", "\n")
		expectPlanFailure(t, root, "feature-demo: AC-1.10: the base copy system-designs/feature-demo.base.md is empty")
	})

	t.Run("AC_1_11", func(t *testing.T) {
		root := newPlanFixture(t)
		writePlanFile(t, root, "requirements/req-demo.md", strings.Replace(planFixtureRequirement, "keeps drafts local.", "keeps drafts local per draft:dec/use-cli and feature-demo; feature-demo.go and feature-demo-x are not citations.", 1))
		writePlanFile(t, root, "decisions/use-cli.yaml", planFixtureDecision+"alternatives_rejected: Skipping draft:task/build was rejected.\n")
		lines := expectPlanFailure(t, root, "req-demo: AC-1.11: cites draft:dec/use-cli, an item of the later decisions layer")
		expectPlanFailure(t, root, "req-demo: AC-1.11: cites feature-demo, an item of the later system-designs layer")
		if got := planLinesWith(lines, "req-demo: AC-1.11:"); len(got) != 2 {
			t.Fatalf("token boundaries: %q", got)
		}
		root = newPlanFixture(t)
		writePlanFile(t, root, "decisions/use-cli.yaml", "statement: Check drafts per req-demo REQ-1 after draft:task/build.\ncontext: req-demo AC-1.1.\nalternatives_rejected: None.\n")
		expectPlanFailure(t, root, "draft:dec/use-cli: AC-1.11: cites draft:task/build, an item of the later tasks layer")
		root = newPlanFixture(t)
		writePlanFile(t, root, "decisions/alpha.yaml", "statement: Alpha per req-demo REQ-1 and draft:dec/beta.\ncontext: req-demo AC-1.1.\nalternatives_rejected: None.\n")
		writePlanFile(t, root, "decisions/beta.yaml", "statement: Beta per req-demo REQ-1.\ncontext: req-demo AC-1.1 and draft:dec/alpha.\nalternatives_rejected: Not draft:dec/use-cli.\n")
		expectPlanFailure(t, root, "draft:dec/alpha: AC-1.11: cites draft:dec/beta; draft-local decision citations form a cycle among draft:dec/alpha, draft:dec/beta")
		expectPlanFailure(t, root, "draft:dec/beta: AC-1.11: cites draft:dec/alpha; draft-local decision citations form a cycle among draft:dec/alpha, draft:dec/beta")
	})

	t.Run("AC_1_12", func(t *testing.T) {
		for name, tc := range map[string]struct{ value, reason string }{
			"list":        {"[DEC-1, DEC-2]", "supersedes must name one DEC-n, not a list"},
			"two ids":     {`"DEC-1, DEC-2"`, `supersedes must name exactly one DEC-n, not "DEC-1, DEC-2"`},
			"draft-local": {"draft:dec/use-cli", "supersedes names draft-local decision draft:dec/use-cli; it must name one DEC-n"},
			"null":        {"null", "supersedes must name one DEC-n, not a null value"},
		} {
			t.Run(name, func(t *testing.T) {
				root := newPlanFixture(t)
				writePlanFile(t, root, "decisions/other.yaml", planFixtureDecision+"supersedes: "+tc.value+"\n")
				expectPlanFailure(t, root, "draft:dec/other: AC-1.12: "+tc.reason)
			})
		}
		root := newPlanFixture(t)
		writePlanFile(t, root, "decisions/other.yaml", planFixtureDecision+"supersedes: DEC-5\n")
		if lines, err := checkPlanLines(t, root); err != nil {
			t.Fatalf("one DEC-n: %v %q", err, lines)
		}
	})

	t.Run("independent failures accumulate", func(t *testing.T) {
		root := newPlanFixture(t)
		writePlanFile(t, root, "tasks/build.md", "---\nrequirement_ids: [req-demo]\n---\n\n")
		writePlanFile(t, root, "decisions/use-cli.yaml", "statement: x\ncontext: y\nalternatives_rejected: z\nsupersedes: [DEC-1]\n")
		lines := expectPlanFailure(t, root, "draft:dec/use-cli: AC-1.5:")
		for _, prefix := range []string{"draft:dec/use-cli: AC-1.12:", "draft:task/build: AC-1.9: the task names no repository", "draft:task/build: AC-1.9: the task body is empty", "req-demo: AC-1.7:"} {
			if len(planLinesWith(lines, prefix)) != 1 {
				t.Fatalf("missing %q in %q", prefix, lines)
			}
		}
		if !sort.SliceIsSorted(lines, func(i, j int) bool { return planLayerRankOfLine(lines[i]) < planLayerRankOfLine(lines[j]) }) {
			t.Fatalf("lines not in layer order: %q", lines)
		}
	})
}

func planLayerRankOfLine(line string) int {
	switch {
	case strings.HasPrefix(line, "req-"):
		return 0
	case strings.HasPrefix(line, "draft:dec/"):
		return 1
	case strings.HasPrefix(line, "draft:task/"):
		return 3
	}
	return 2
}

// AC-1.1: a passing draft inside an ignored repository directory exits zero
// and prints no failure line.
func TestPlanCheckPassingDraft(t *testing.T) {
	_, draft := newPlanRepo(t, true)
	out, err := runPlanCommand(t, "check", draft)
	if err != nil || strings.TrimSpace(out) != "plan check: 5 item(s) passed with 0 warning(s)" {
		t.Fatalf("passing draft: err=%v out=%q", err, out)
	}
}

// AC-1.8: warnings alone never fail the check.
func TestPlanCheckWarningOnly(t *testing.T) {
	_, draft := newPlanRepo(t, true)
	writePlanFile(t, draft, "system-designs/feature-demo.md", strings.Replace(planFixtureDesign, "repo: elsewhere", "repo: repo", 1))
	out, err := runPlanCommand(t, "check", draft)
	if err != nil {
		t.Fatalf("warning-only draft failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, `warning: feature-demo: AC-1.8: glob "src/**" matches no file git tracks in repository "repo"`) || !strings.Contains(out, "passed with 1 warning(s)") {
		t.Fatalf("output = %q", out)
	}
}

// AC-1.2: a draft outside every git working tree passes the git rule.
func TestPlanCheckOutsideGit(t *testing.T) {
	root := newPlanFixture(t)
	out, err := runPlanCommand(t, "check", root)
	if err != nil || strings.Contains(out, "AC-1.2") {
		t.Fatalf("outside git: err=%v out=%q", err, out)
	}
}

type planFailingTransport struct {
	t        *testing.T
	requests *atomic.Int32
}

func (f planFailingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.requests.Add(1)
	f.t.Errorf("plan command made a network request: %s %s", r.Method, r.URL)
	return nil, http.ErrUseLastResponse
}

// AC-1.1, AC-4.1, DEC-69: check, approve, and status make no network request,
// including when they fail.
func TestPlanCommandsOffline(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "token")
	transport := planFailingTransport{t: t, requests: &requests}
	previousTransport, previousClient := http.DefaultTransport, http.DefaultClient.Transport
	http.DefaultTransport, http.DefaultClient.Transport = transport, transport
	t.Cleanup(func() { http.DefaultTransport, http.DefaultClient.Transport = previousTransport, previousClient })

	root := newPlanFixture(t)
	if _, err := runPlanCommand(t, "check", root); err != nil {
		t.Fatalf("check: %v", err)
	}
	draft, _, err := ReadPlanDraft(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := PlanItemDigest(draft.Item("req-demo")[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runPlanCommand(t, "approve", root, "req-demo@"+digest); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := runPlanCommand(t, "approve", root, "draft:task/none@sha256:00"); err == nil {
		t.Fatal("approve of an unknown item succeeded")
	}
	if _, err := runPlanCommand(t, "status", root); err != nil {
		t.Fatalf("status: %v", err)
	}
	writePlanFile(t, root, "tasks/build.md", "---\nrepo: x\n---\n")
	if _, err := runPlanCommand(t, "check", root); err == nil {
		t.Fatal("failing check passed")
	}
	if requests.Load() != 0 {
		t.Fatalf("plan commands made %d request(s)", requests.Load())
	}
}

// DEC-69: main registers the plan group with exactly check, approve, and status.
func TestPlanCommandRegistration(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	registered := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "AddCommand" {
			return true
		}
		if receiver, ok := selector.X.(*ast.Ident); !ok || receiver.Name != "root" {
			return true
		}
		for _, arg := range call.Args {
			if inner, ok := arg.(*ast.CallExpr); ok {
				if ident, ok := inner.Fun.(*ast.Ident); ok && ident.Name == "planCmd" {
					registered = true
				}
			}
		}
		return true
	})
	if !registered {
		t.Fatal("main.go root.AddCommand does not register planCmd()")
	}
	var names []string
	for _, command := range planCmd().Commands() {
		names = append(names, command.Name())
	}
	if strings.Join(names, ",") != "approve,check,status" {
		t.Fatalf("plan subcommands = %q", names)
	}
}
