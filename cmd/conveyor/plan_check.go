package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/spf13/cobra"
)

// planAcceptanceForm is the "When X, the system shall Y" shape every
// acceptance criterion must carry.
var planAcceptanceForm = regexp.MustCompile(`^When\b.*\bshall\b`)

// planCheckFailure is one failed check. Item is the item ID (or the draft
// folder for draft-level failures) the failure names.
type planCheckFailure struct {
	Item    string
	Message string
}

func (f planCheckFailure) String() string { return f.Item + ": " + f.Message }

// planCheckCmd runs the deterministic, offline draft checks. No client,
// credentials, or network access participates.
func planCheckCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "check <draft-dir>",
		Short: "Run offline checks over a planning draft",
		Long: "Run deterministic, offline checks over a planning draft and print every failure by item ID. " +
			"The draft folder must be git-ignored and hold no tracked file. No network access participates.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			failures, err := runPlanCheck(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			for _, failure := range failures {
				fmt.Fprintf(cmd.OutOrStdout(), "plan check: %s\n", failure)
			}
			if len(failures) > 0 {
				return fmt.Errorf("plan check failed: %d problem(s)", len(failures))
			}
			return nil
		},
	}
	return command
}

// runPlanCheck loads the draft and evaluates every check. Draft-level load
// failures are reported alongside item-level failures, and a partial draft
// still contributes the item checks it can support.
func runPlanCheck(ctx context.Context, dir string) ([]planCheckFailure, error) {
	draft, loadErr := loadPlanDraft(dir)
	var failures []planCheckFailure
	for _, err := range flattenPlanErrors(loadErr) {
		failures = append(failures, planCheckFailure{Item: "draft", Message: err.Error()})
	}
	if draft == nil {
		return failures, nil
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return failures, fmt.Errorf("plan draft %q: resolve path: %w", dir, err)
	}
	folderFailures, err := checkPlanDraftFolder(ctx, absDir)
	if err != nil {
		return failures, err
	}
	failures = append(failures, folderFailures...)
	failures = append(failures, checkPlanDraftItems(ctx, draft, absDir)...)
	return failures, nil
}

// checkPlanDraftFolder fails when the draft folder is not git-ignored or
// holds a tracked file.
func checkPlanDraftFolder(ctx context.Context, absDir string) ([]planCheckFailure, error) {
	rel, root, err := planGitRoot(ctx, absDir)
	if err != nil {
		return []planCheckFailure{{Item: absDir, Message: err.Error()}}, nil
	}
	var failures []planCheckFailure
	if _, code, err := planGit(ctx, root, "check-ignore", "-q", "--", rel); err != nil {
		return failures, err
	} else if code != 0 {
		failures = append(failures, planCheckFailure{Item: rel, Message: "draft folder is not git-ignored"})
	}
	tracked, _, err := planGit(ctx, root, "ls-files", "-z", "--", rel)
	if err != nil {
		return failures, err
	}
	if names := splitPlanGitPaths(tracked); len(names) > 0 {
		sort.Strings(names)
		failures = append(failures, planCheckFailure{Item: rel, Message: "draft folder holds tracked file(s): " + strings.Join(names, ", ")})
	}
	return failures, nil
}

// checkPlanDraftItems evaluates the item-level rules: fence presence is
// enforced while loading; here are acceptance-criteria form, governs globs
// matching tracked paths, REQ/AC/task coverage, decision citations, task docs
// declarations, and design-to-decision push order.
func checkPlanDraftItems(ctx context.Context, draft *planDraft, absDir string) []planCheckFailure {
	var failures []planCheckFailure
	tracked := planTrackedFiles(ctx, absDir)

	type criterion struct {
		qualified string
		short     string
		parent    string
	}
	var criteria []criterion
	knownRequirementIDs := make(map[string]bool)
	for _, requirement := range draft.Raw.Requirements {
		knownRequirementIDs[requirement.ID] = true
		for _, statement := range requirement.Statements {
			itemID := requirement.ID + "/" + statement.ID
			knownRequirementIDs[itemID] = true
			if len(statement.AcceptanceCriteria) == 0 {
				failures = append(failures, planCheckFailure{Item: itemID, Message: "requirement has no acceptance criteria"})
			}
			for _, accepted := range statement.AcceptanceCriteria {
				qualified := requirement.ID + "/" + accepted.ID
				knownRequirementIDs[qualified] = true
				criteria = append(criteria, criterion{qualified: qualified, short: accepted.ID, parent: itemID})
				if !planAcceptanceForm.MatchString(strings.TrimSpace(accepted.Statement)) {
					failures = append(failures, planCheckFailure{Item: itemID, Message: fmt.Sprintf("%s statement is not in \"When X, the system shall Y\" form", accepted.ID)})
				}
			}
		}
	}

	for _, design := range draft.Raw.Designs {
		for _, scope := range design.Governs {
			for _, glob := range scope.Paths {
				if !planGlobMatchesTracked(glob, tracked) {
					failures = append(failures, planCheckFailure{Item: design.ID, Message: fmt.Sprintf("governs glob %q matches no tracked path", glob)})
				}
			}
		}
	}

	covered := make(map[string]bool)
	for _, task := range draft.Raw.Tasks {
		for _, id := range task.Governing {
			covered[id] = true
		}
		if len(task.DocsPaths) == 0 && strings.TrimSpace(task.DocsNoneReason) == "" {
			failures = append(failures, planCheckFailure{Item: task.ID, Message: "task names no docs paths and no docs: none reason"})
		}
	}
	for _, accepted := range criteria {
		if !covered[accepted.qualified] {
			failures = append(failures, planCheckFailure{Item: accepted.parent, Message: fmt.Sprintf("%s is not covered by any task", accepted.short)})
		}
	}

	for _, decision := range draft.Raw.Decisions {
		if len(decision.Cites) == 0 {
			failures = append(failures, planCheckFailure{Item: decision.ID, Message: "decision cites no requirement"})
			continue
		}
		for _, cite := range decision.Cites {
			if !knownRequirementIDs[cite] {
				failures = append(failures, planCheckFailure{Item: decision.ID, Message: fmt.Sprintf("cites unknown requirement %q", cite)})
			}
		}
	}

	decisionIDs := make(map[string]bool, len(draft.Raw.Decisions))
	for _, decision := range draft.Raw.Decisions {
		decisionIDs[decision.ID] = true
	}
	for _, design := range draft.Raw.Designs {
		for _, ref := range decisionReferences(design.Body) {
			if !decisionIDs[ref] {
				failures = append(failures, planCheckFailure{Item: design.ID, Message: fmt.Sprintf("cites decision %s absent from the draft", ref)})
			}
		}
	}

	return failures
}

// planGlobMatchesTracked reports whether a governs glob matches at least one
// tracked, repository-relative path.
func planGlobMatchesTracked(glob string, tracked []string) bool {
	for _, path := range tracked {
		if core.MatchGovernedPath(glob, path) {
			return true
		}
	}
	return false
}

// planTrackedFiles lists the repository's tracked paths; a git failure yields
// no paths, so the governs-match check reports against an empty set.
func planTrackedFiles(ctx context.Context, absDir string) []string {
	_, root, err := planGitRoot(ctx, absDir)
	if err != nil {
		return nil
	}
	out, _, err := planGit(ctx, root, "ls-files", "-z")
	if err != nil {
		return nil
	}
	return splitPlanGitPaths(out)
}

// planGitRoot resolves the repository root containing absDir and returns
// absDir relative to it, slash-separated for git pathspecs.
func planGitRoot(ctx context.Context, absDir string) (rel, root string, err error) {
	out, code, err := planGit(ctx, absDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", err
	}
	root = strings.TrimSpace(string(out))
	if code != 0 || root == "" {
		return "", "", fmt.Errorf("plan draft %s: not inside a git repository", absDir)
	}
	rel, err = filepath.Rel(root, absDir)
	if err != nil {
		return "", "", fmt.Errorf("plan draft %s: %w", absDir, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("plan draft %s: not inside git repository %s", absDir, root)
	}
	return filepath.ToSlash(rel), root, nil
}

// planGit runs git with the same hardened invocation the kit validator uses.
// A non-zero exit is returned as its code, not an error; only a failure to
// start git is an error.
func planGit(ctx context.Context, dir string, args ...string) ([]byte, int, error) {
	argv := append([]string{"-c", "core.fsmonitor=false", "-C", dir}, args...)
	command := exec.CommandContext(ctx, "git", argv...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.Bytes(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.Bytes(), exitErr.ExitCode(), nil
	}
	return nil, -1, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
}

func splitPlanGitPaths(data []byte) []string {
	var paths []string
	for _, field := range strings.Split(string(data), "\x00") {
		if field != "" {
			paths = append(paths, field)
		}
	}
	return paths
}

// flattenPlanErrors expands an errors.Join tree into its leaves so every
// load problem is reported.
func flattenPlanErrors(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, child := range joined.Unwrap() {
			out = append(out, flattenPlanErrors(child)...)
		}
		return out
	}
	return []error{err}
}
