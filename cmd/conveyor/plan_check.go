package main

// Offline draft check (feature-local-planning-drafts LP-3; req-local-planning
// REQ-1 AC-1.1–AC-1.12). Content rules call the factory's own validators so a
// printed reason is the factory's reason. The check runs git locally and opens
// no network connection.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/cmd/conveyor/localgit"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
)

var (
	planDraftLocalTokenPattern = regexp.MustCompile(`draft:(dec|task)/[a-z0-9][a-z0-9-]*`)
	planShallPattern           = regexp.MustCompile(`(?i)\bshall\b`)
	planDecisionIDPattern      = regexp.MustCompile(`^DEC-[1-9][0-9]*$`)
	planStatementIDPattern     = regexp.MustCompile(`\b(?:REQ-[1-9][0-9]*|AC-[1-9][0-9]*\.[1-9][0-9]*)\b`)
	planRequirementDocPattern  = regexp.MustCompile(`req-[a-z0-9][a-z0-9-]*[a-z0-9]`)
	planCriterionPattern       = regexp.MustCompile(`\bAC-([1-9][0-9]*)\.([1-9][0-9]*)(?:\s*(?:–|—|-|to|through)\s*AC-([1-9][0-9]*)\.([1-9][0-9]*))?\b`)
)

const planMaxCriterionRange = 1000

// planGitContext is what the git inspection learned about the draft's
// containing working tree.
type planGitContext struct {
	inside    bool
	toplevel  string
	repoNames []string
	tracked   []string
}

type planChecker struct {
	draft       *PlanDraft
	index       map[string]int
	diagnostics []PlanDiagnostic
}

func (c *planChecker) fail(itemID, rule, format string, args ...any) {
	c.diagnostics = append(c.diagnostics, PlanDiagnostic{ItemID: itemID, Rule: rule, Reason: fmt.Sprintf(format, args...)})
}

func (c *planChecker) warn(itemID, rule, format string, args ...any) {
	c.diagnostics = append(c.diagnostics, PlanDiagnostic{ItemID: itemID, Rule: rule, Reason: fmt.Sprintf(format, args...), Warning: true})
}

// CheckPlanDraft applies AC-1.2–AC-1.12 to a draft read by ReadPlanDraft and
// returns every failure and warning, ordered by draft and then item order.
// It changes no file. The error reports only a cancelled context; a git
// execution failure is a failure diagnostic, never a pass.
func CheckPlanDraft(ctx context.Context, draft *PlanDraft) ([]PlanDiagnostic, error) {
	c := &planChecker{draft: draft, index: map[string]int{}}
	for index, item := range draft.Items {
		if _, ok := c.index[item.ID]; !ok {
			c.index[item.ID] = index
		}
	}
	git := c.checkGit(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hasTasks := false
	for _, item := range draft.Items {
		if item.Kind == PlanKindTask {
			hasTasks = true
		}
	}
	taskCitations := c.taskCriterionCitations()
	for _, item := range draft.Items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if item.Invalid != nil {
			continue
		}
		switch item.Kind {
		case PlanKindRequirement:
			c.checkRequirement(item, hasTasks, taskCitations)
		case PlanKindSystemDesign:
			c.checkSystemDesign(item, git)
		case PlanKindDecision:
			c.checkDecision(item)
		case PlanKindTask:
			c.checkTask(item)
		}
		c.checkCitations(item)
	}
	c.checkTaskCycles()
	c.checkDecisionCycles()
	sort.SliceStable(c.diagnostics, func(i, j int) bool {
		return c.order(c.diagnostics[i].ItemID) < c.order(c.diagnostics[j].ItemID)
	})
	return c.diagnostics, nil
}

func (c *planChecker) order(itemID string) int {
	if index, ok := c.index[itemID]; ok {
		return index
	}
	return -1
}

// planGit runs git in dir. outside reports git's "not a git repository"
// answer, which is the only failure that means the directory is outside
// every working tree.
func planGit(ctx context.Context, dir string, args ...string) (stdout []byte, exitCode int, stderr string, err error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	var out, errOut bytes.Buffer
	command.Stdout, command.Stderr = &out, &errOut
	runErr := command.Run()
	var exit *exec.ExitError
	if errors.As(runErr, &exit) {
		return out.Bytes(), exit.ExitCode(), strings.TrimSpace(errOut.String()), nil
	}
	if runErr != nil {
		return nil, -1, "", fmt.Errorf("cannot run git %s: %w", strings.Join(args, " "), runErr)
	}
	return out.Bytes(), 0, strings.TrimSpace(errOut.String()), nil
}

// checkGit applies AC-1.2 and gathers the tracked files AC-1.8 needs.
func (c *planChecker) checkGit(ctx context.Context) planGitContext {
	var git planGitContext
	dir := c.draft.Root
	gitFail := func(args []string, code int, stderr string, err error) {
		if err != nil {
			c.fail(planDraftItemID, "AC-1.2", "%v", err)
			return
		}
		c.fail(planDraftItemID, "AC-1.2", "git %s exited %d: %s", strings.Join(args, " "), code, stderr)
	}
	args := []string{"rev-parse", "--is-inside-work-tree"}
	out, code, stderr, err := planGit(ctx, dir, args...)
	if err != nil || (code != 0 && !(code == 128 && strings.Contains(stderr, "not a git repository"))) {
		gitFail(args, code, stderr, err)
		return git
	}
	if code != 0 || strings.TrimSpace(string(out)) != "true" {
		return git
	}
	git.inside = true
	for _, step := range [][]string{{"rev-parse", "--show-toplevel"}, {"rev-parse", "--show-prefix"}} {
		out, code, stderr, err = planGit(ctx, dir, step...)
		if err != nil || code != 0 {
			gitFail(step, code, stderr, err)
			return git
		}
		if step[1] == "--show-toplevel" {
			git.toplevel = strings.TrimSpace(string(out))
		}
	}
	prefix := strings.TrimSpace(string(out))
	args = []string{"check-ignore", "-q", "--", "."}
	_, code, stderr, err = planGit(ctx, dir, args...)
	switch {
	case err != nil || code > 1:
		gitFail(args, code, stderr, err)
	case code == 1:
		c.fail(planDraftItemID, "AC-1.2", "the draft directory is inside the git working tree %s and git does not ignore it; add it to .gitignore", git.toplevel)
	}
	args = []string{"ls-files", "-z"}
	out, code, stderr, err = planGit(ctx, git.toplevel, args...)
	if err != nil || code != 0 {
		gitFail(args, code, stderr, err)
		return git
	}
	var inside []string
	for _, file := range strings.Split(string(out), "\x00") {
		if file == "" {
			continue
		}
		git.tracked = append(git.tracked, file)
		if strings.HasPrefix(file, prefix) {
			inside = append(inside, strings.TrimPrefix(file, prefix))
		}
	}
	if len(inside) > 0 {
		shown := inside
		if len(shown) > 5 {
			shown = shown[:5]
		}
		more := ""
		if len(inside) > len(shown) {
			more = fmt.Sprintf(" and %d more", len(inside)-len(shown))
		}
		c.fail(planDraftItemID, "AC-1.2", "git tracks %d file(s) in the draft directory: %s%s", len(inside), strings.Join(shown, ", "), more)
	}
	git.repoNames = []string{filepath.Base(git.toplevel)}
	if identity, err := localgit.RepositoryOriginIdentity(ctx, git.toplevel); err == nil {
		if name := path.Base(strings.TrimPrefix(identity, "file:")); name != git.repoNames[0] {
			git.repoNames = append(git.repoNames, name)
		}
	}
	return git
}

func (c *planChecker) checkRequirement(item PlanItem, hasTasks bool, taskCitations map[string]map[string]bool) {
	document, err := planRequirementContent(item.Content)
	if err != nil {
		c.fail(item.ID, "AC-1.3", "%v", err)
	} else {
		for _, statement := range document.Statements {
			for _, criterion := range statement.AcceptanceCriteria {
				if !planShallPattern.MatchString(criterion.Statement) {
					c.fail(item.ID, "AC-1.3", "acceptance criterion %s contains no \"shall\"", criterion.ID)
				}
			}
		}
	}
	base, baseOK := c.checkRevisionBase(item)
	if err != nil || !hasTasks || (item.Mode == PlanModeRevise && !baseOK) {
		return
	}
	previous := map[string]string{}
	if item.Mode == PlanModeRevise {
		for _, statement := range base.Statements {
			for _, criterion := range statement.AcceptanceCriteria {
				previous[criterion.ID] = strings.TrimSpace(criterion.Statement)
			}
		}
	}
	for _, statement := range document.Statements {
		for _, criterion := range statement.AcceptanceCriteria {
			before, existed := previous[criterion.ID]
			if existed && before == strings.TrimSpace(criterion.Statement) {
				continue
			}
			if taskCitations[item.ID][criterion.ID] {
				continue
			}
			change := "added"
			if item.Mode == PlanModeRevise {
				change = "added relative to the base version"
				if existed {
					change = "reworded relative to the base version"
				}
			}
			c.fail(item.ID, "AC-1.7", "acceptance criterion %s is %s and no task item cites it", criterion.ID, change)
		}
	}
}

// checkRevisionBase applies AC-1.10. For a requirement it also parses the
// base copy with the factory parser so AC-1.7 can compare criteria.
func (c *planChecker) checkRevisionBase(item PlanItem) (pipeline.RequirementDocument, bool) {
	if item.Mode != PlanModeRevise {
		return pipeline.RequirementDocument{}, false
	}
	ok := true
	if item.BaseVersion < 1 {
		c.fail(item.ID, "AC-1.10", "a revision records no base_version; set the positive version it revises")
		ok = false
	}
	if !item.HasBase {
		c.fail(item.ID, "AC-1.10", "a revision holds no copy of its base version; write %s/%s%s", item.Layer, item.Name, planBaseSuffix)
		return pipeline.RequirementDocument{}, false
	}
	if strings.TrimSpace(string(item.Base)) == "" {
		c.fail(item.ID, "AC-1.10", "the base copy %s/%s%s is empty", item.Layer, item.Name, planBaseSuffix)
		return pipeline.RequirementDocument{}, false
	}
	if item.Kind != PlanKindRequirement {
		return pipeline.RequirementDocument{}, ok
	}
	base, err := planRequirementContent(planItemBaseText(item))
	if err != nil {
		c.fail(item.ID, "AC-1.10", "the base copy %s/%s%s is not a valid requirement: %v", item.Layer, item.Name, planBaseSuffix, err)
		return pipeline.RequirementDocument{}, false
	}
	return base, ok
}

func (c *planChecker) checkSystemDesign(item PlanItem, git planGitContext) {
	if item.Mode == PlanModeCreate {
		if item.Title == "" || item.Category == "" {
			c.fail(item.ID, "AC-1.4", "system design id, title, and category are required")
		}
		if err := core.ValidateSystemDesignID(item.ID); err != nil {
			c.fail(item.ID, "AC-1.4", "%v", err)
		}
	}
	c.checkRevisionBase(item)
	scopes, err := core.ParseGovernedScopes(core.NormalizeSystemDesignContent(item.Content))
	if err != nil {
		c.fail(item.ID, "AC-1.4", "%v", err)
		return
	}
	if !git.inside || len(git.repoNames) == 0 {
		return
	}
	for _, scope := range scopes {
		if !planContains(git.repoNames, scope.Repository) {
			continue
		}
		for _, glob := range scope.Paths {
			matcher, err := core.CompileGovernedPath(glob)
			if err != nil {
				c.fail(item.ID, "AC-1.4", "invalid governed path glob %q: %v", glob, err)
				continue
			}
			matched := false
			for _, file := range git.tracked {
				if matcher.Match(file) {
					matched = true
					break
				}
			}
			if !matched {
				c.warn(item.ID, "AC-1.8", "glob %q matches no file git tracks in repository %q", glob, scope.Repository)
			}
		}
	}
}

func (c *planChecker) checkDecision(item PlanItem) {
	decision := core.Decision{
		Statement:            item.Decision.Statement,
		Context:              item.Decision.Context,
		AlternativesRejected: item.Decision.AlternativesRejected,
		Origin:               core.DecisionOriginOperator,
	}
	if err := core.ValidateDecision(decision); err != nil {
		c.fail(item.ID, planRuleContent, "%v", err)
	}
	if !c.citesRequirement(item.Decision.Statement + "\n" + item.Decision.Context) {
		c.fail(item.ID, "AC-1.5", "statement and context cite no requirement by document ID with a REQ or AC identifier")
	}
	value, present, err := planSupersedes(item.Decision.Supersedes)
	switch {
	case err != nil:
		c.fail(item.ID, "AC-1.12", "%v", err)
	case !present:
	case strings.HasPrefix(value, "draft:"):
		c.fail(item.ID, "AC-1.12", "supersedes names draft-local decision %s; it must name one DEC-n", value)
	case !planDecisionIDPattern.MatchString(value):
		c.fail(item.ID, "AC-1.12", "supersedes must name exactly one DEC-n, not %q", value)
	}
}

// citesRequirement reports whether text names a requirement document ID (a
// draft requirement item or a `req-` document ID) followed by a REQ-n or
// AC-n.m identifier (AC-1.5).
func (c *planChecker) citesRequirement(text string) bool {
	first := -1
	for _, match := range planRequirementDocPattern.FindAllStringIndex(text, -1) {
		if planTokenBoundary(text, match[0], match[1]) {
			first = match[0]
			break
		}
	}
	for _, item := range c.draft.Items {
		if item.Kind != PlanKindRequirement {
			continue
		}
		for _, position := range planTokenPositions(text, item.ID) {
			if first < 0 || position < first {
				first = position
			}
		}
	}
	if first < 0 {
		return false
	}
	return planStatementIDPattern.MatchString(text[first:])
}

func (c *planChecker) checkTask(item PlanItem) {
	if item.Task.Repo == "" {
		c.fail(item.ID, "AC-1.9", "the task names no repository (repo)")
	}
	if strings.TrimSpace(item.Content) == "" {
		c.fail(item.ID, "AC-1.9", "the task body is empty")
	}
	for _, dependency := range item.Task.DependsOn {
		if !strings.HasPrefix(dependency, planTaskIDPrefix) || !planLocalNamePattern.MatchString(strings.TrimPrefix(dependency, planTaskIDPrefix)) {
			c.fail(item.ID, planRuleLayout, "depends_on entry %q is not a draft-local task ID (draft:task/<name>)", dependency)
		}
	}
}

// planCitationText is every pushed field of an item that can cite another
// item. A decision's supersedes is checked separately (AC-1.12).
func planCitationText(item PlanItem) string {
	switch item.Kind {
	case PlanKindDecision:
		return strings.Join([]string{item.Decision.Statement, item.Decision.Context, item.Decision.AlternativesRejected}, "\n")
	case PlanKindTask:
		parts := append([]string{item.Content}, item.Task.DependsOn...)
		parts = append(parts, item.Task.RequirementIDs...)
		parts = append(parts, item.Task.SystemDesignIDs...)
		return strings.Join(parts, "\n")
	}
	return item.Content
}

// planDraftLocalCitations returns the whole-token draft-local IDs in text, in
// first-appearance order without repeats.
func planDraftLocalCitations(text string) []string {
	var result []string
	seen := map[string]bool{}
	for _, match := range planDraftLocalTokenPattern.FindAllStringIndex(text, -1) {
		if match[0] > 0 && planWordByte(text[match[0]-1]) {
			continue
		}
		if match[1] < len(text) && (planWordByte(text[match[1]]) || text[match[1]] == '-') {
			continue
		}
		token := text[match[0]:match[1]]
		if !seen[token] {
			seen[token] = true
			result = append(result, token)
		}
	}
	return result
}

func planWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// planTokenBoundary reports whether text[start:end] is a whole document-ID
// token: not inside a longer identifier, path, or file name.
func planTokenBoundary(text string, start, end int) bool {
	if start > 0 {
		before := text[start-1]
		if planWordByte(before) || strings.IndexByte("-./:", before) >= 0 {
			return false
		}
	}
	if end < len(text) {
		after := text[end]
		if planWordByte(after) || after == '-' || after == '/' {
			return false
		}
		if after == '.' && end+1 < len(text) && planWordByte(text[end+1]) {
			return false
		}
	}
	return true
}

// planTokenPositions returns every whole-token occurrence of id in text.
func planTokenPositions(text, id string) []int {
	var positions []int
	if id == "" {
		return nil
	}
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], id)
		if index < 0 {
			break
		}
		start := offset + index
		if planTokenBoundary(text, start, start+len(id)) {
			positions = append(positions, start)
		}
		offset = start + 1
	}
	return positions
}

// checkCitations applies AC-1.6 (undefined draft-local IDs) and AC-1.11
// (citations of a later layer's draft item).
func (c *planChecker) checkCitations(item PlanItem) {
	text := planCitationText(item)
	layer := planLayerRank(item.Layer)
	for _, token := range planDraftLocalCitations(text) {
		cited := c.draft.Item(token)
		if len(cited) == 0 {
			c.fail(item.ID, "AC-1.6", "cites %s, which the draft does not define", token)
			continue
		}
		if planLayerRank(cited[0].Layer) > layer {
			c.fail(item.ID, "AC-1.11", "cites %s, an item of the later %s layer", token, cited[0].Layer)
		}
	}
	for _, other := range c.draft.Items {
		if other.ID == item.ID || (other.Kind != PlanKindRequirement && other.Kind != PlanKindSystemDesign) {
			continue
		}
		if planLayerRank(other.Layer) <= layer || len(planTokenPositions(text, other.ID)) == 0 {
			continue
		}
		c.fail(item.ID, "AC-1.11", "cites %s, an item of the later %s layer", other.ID, other.Layer)
	}
}

func planLayerRank(layer string) int {
	for index, candidate := range PlanLayers {
		if candidate == layer {
			return index
		}
	}
	return len(PlanLayers)
}

// checkTaskCycles applies the AC-1.9 cycle rule to depends_on.
func (c *planChecker) checkTaskCycles() {
	edges := map[string][]string{}
	var nodes []string
	for _, item := range c.draft.Items {
		if item.Kind != PlanKindTask || item.Invalid != nil {
			continue
		}
		nodes = append(nodes, item.ID)
		for _, dependency := range item.Task.DependsOn {
			if cited := c.draft.Item(dependency); len(cited) == 1 && cited[0].Kind == PlanKindTask {
				edges[item.ID] = append(edges[item.ID], dependency)
			}
		}
	}
	for _, component := range planCycles(nodes, edges) {
		members := strings.Join(component, ", ")
		for _, id := range component {
			c.fail(id, "AC-1.9", "depends_on forms a cycle among %s", members)
		}
	}
}

// checkDecisionCycles applies the AC-1.11 cycle rule to draft-local decision
// citations among decision items and names each offending citation.
func (c *planChecker) checkDecisionCycles() {
	edges := map[string][]string{}
	var nodes []string
	for _, item := range c.draft.Items {
		if item.Kind != PlanKindDecision || item.Invalid != nil {
			continue
		}
		nodes = append(nodes, item.ID)
		for _, token := range planDraftLocalCitations(planCitationText(item)) {
			if cited := c.draft.Item(token); len(cited) == 1 && cited[0].Kind == PlanKindDecision {
				edges[item.ID] = append(edges[item.ID], token)
			}
		}
	}
	for _, component := range planCycles(nodes, edges) {
		inCycle := map[string]bool{}
		for _, id := range component {
			inCycle[id] = true
		}
		for _, id := range component {
			var citations []string
			for _, target := range edges[id] {
				if inCycle[target] {
					citations = append(citations, target)
				}
			}
			c.fail(id, "AC-1.11", "cites %s; draft-local decision citations form a cycle among %s", strings.Join(citations, ", "), strings.Join(component, ", "))
		}
	}
}

// planCycles returns the strongly connected components that contain a cycle
// (more than one node, or one node with an edge to itself), each sorted.
func planCycles(nodes []string, edges map[string][]string) [][]string {
	index := 0
	indices, lowlinks := map[string]int{}, map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var result [][]string
	var connect func(string)
	connect = func(node string) {
		indices[node], lowlinks[node] = index, index
		index++
		stack = append(stack, node)
		onStack[node] = true
		for _, next := range edges[node] {
			if _, seen := indices[next]; !seen {
				connect(next)
				lowlinks[node] = min(lowlinks[node], lowlinks[next])
			} else if onStack[next] {
				lowlinks[node] = min(lowlinks[node], indices[next])
			}
		}
		if lowlinks[node] != indices[node] {
			return
		}
		var component []string
		for {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[top] = false
			component = append(component, top)
			if top == node {
				break
			}
		}
		selfLoop := planContains(edges[node], node)
		if len(component) > 1 || selfLoop {
			sort.Strings(component)
			result = append(result, component)
		}
	}
	sorted := append([]string(nil), nodes...)
	sort.Strings(sorted)
	for _, node := range sorted {
		if _, seen := indices[node]; !seen {
			connect(node)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i][0] < result[j][0] })
	return result
}

// taskCriterionCitations maps requirement item ID to the acceptance criteria
// that task items cite (AC-1.7). Each AC-n.m in a task body belongs to the
// nearest preceding draft requirement ID in the body, or, before any such ID,
// to every draft requirement the task lists in requirement_ids. A range such
// as `AC-1.1–AC-1.4` cites each criterion it spans.
func (c *planChecker) taskCriterionCitations() map[string]map[string]bool {
	citations := map[string]map[string]bool{}
	requirements := map[string]bool{}
	for _, item := range c.draft.Items {
		if item.Kind == PlanKindRequirement {
			requirements[item.ID] = true
		}
	}
	cite := func(document, criterion string) {
		if citations[document] == nil {
			citations[document] = map[string]bool{}
		}
		citations[document][criterion] = true
	}
	for _, task := range c.draft.Items {
		if task.Kind != PlanKindTask {
			continue
		}
		type mention struct {
			position int
			document string
		}
		var mentions []mention
		for id := range requirements {
			for _, position := range planTokenPositions(task.Content, id) {
				mentions = append(mentions, mention{position, id})
			}
		}
		sort.Slice(mentions, func(i, j int) bool { return mentions[i].position < mentions[j].position })
		var listed []string
		for _, id := range task.Task.RequirementIDs {
			if requirements[id] {
				listed = append(listed, id)
			}
		}
		for _, match := range planCriterionPattern.FindAllStringSubmatchIndex(task.Content, -1) {
			targets := listed
			for _, m := range mentions {
				if m.position > match[0] {
					break
				}
				targets = []string{m.document}
			}
			parent, _ := strconv.Atoi(task.Content[match[2]:match[3]])
			first, _ := strconv.Atoi(task.Content[match[4]:match[5]])
			last := first
			if match[6] >= 0 {
				endParent, _ := strconv.Atoi(task.Content[match[6]:match[7]])
				end, _ := strconv.Atoi(task.Content[match[8]:match[9]])
				if endParent == parent && end >= first && end-first < planMaxCriterionRange {
					last = end
				} else {
					// Not a range within one statement: both ends are
					// separate citations.
					for _, document := range targets {
						cite(document, fmt.Sprintf("AC-%d.%d", endParent, end))
					}
				}
			}
			for number := first; number <= last; number++ {
				for _, document := range targets {
					cite(document, fmt.Sprintf("AC-%d.%d", parent, number))
				}
			}
		}
	}
	return citations
}

func planContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
