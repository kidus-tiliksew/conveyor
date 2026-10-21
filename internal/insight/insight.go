package insight

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// Actors lists next-action actors in the order groups and the status mix
// present them (AC-2.1).
var Actors = []string{nextaction.ActorOperator, nextaction.ActorAgent, nextaction.ActorFactory, nextaction.ActorDependency, nextaction.ActorNone}

const (
	day = 24 * time.Hour
	// forecastWindow and throughputWindow are the trailing delivery windows
	// of AC-3.2 and AC-3.5.
	forecastWindow   = 14 * day
	throughputWindow = 28 * day
	weeklyBuckets    = 8
)

var criterionIDPattern = regexp.MustCompile(`^AC-[1-9][0-9]*\.[1-9][0-9]*$`)

// open reports whether a serving task is open: its current state is neither
// merged nor closed (RI-3).
func open(task ServingTask) bool {
	return !core.TaskTerminal(task.Task.State)
}

// actorOf returns the group a compact next action belongs to. A missing map
// entry or an actor outside the five known actors belongs to none; the
// compact value itself is never rewritten.
func actorOf(action nextaction.Compact) string {
	if slices.Contains(Actors, action.Actor) {
		return action.Actor
	}
	return nextaction.ActorNone
}

func ref(task ServingTask, actions map[string]nextaction.Compact) TaskRef {
	return TaskRef{ID: task.Task.ID, Title: task.Task.Title, NextAction: actions[task.Task.ID]}
}

// sortedTasks returns a copy of the serving tasks ordered by ID.
func sortedTasks(inputs Inputs) []ServingTask {
	tasks := slices.Clone(inputs.Tasks)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Task.ID < tasks[j].Task.ID })
	return tasks
}

// Groups groups the open serving tasks by next-action actor in the order
// operator, agent, factory, dependency, none, with tasks sorted by ID inside
// each group (AC-2.1). Every actor appears, with an empty task list when no
// open task names it. now is unused: grouping is time-independent.
func Groups(inputs Inputs, actions map[string]nextaction.Compact, now time.Time) []Group {
	byActor := map[string][]TaskRef{}
	for _, task := range sortedTasks(inputs) {
		if !open(task) {
			continue
		}
		actor := actorOf(actions[task.Task.ID])
		byActor[actor] = append(byActor[actor], ref(task, actions))
	}
	groups := make([]Group, 0, len(Actors))
	for _, actor := range Actors {
		tasks := byActor[actor]
		if tasks == nil {
			tasks = []TaskRef{}
		}
		groups = append(groups, Group{Actor: actor, Count: len(tasks), Tasks: tasks})
	}
	return groups
}

// CriticalPath returns the longest chain of open serving tasks in which each
// task depends directly on the one before it (AC-2.2). Edges are restricted
// to open serving tasks at both ends. A longer chain wins; between chains of
// equal length the one whose ID sequence sorts first lexicographically wins.
// The chain is listed from the first dependency to the last dependent. The
// dependency graph is acyclic (component-task-lifecycle); a recursion-stack
// set still skips any edge that would revisit a task on the current walk.
func CriticalPath(inputs Inputs, actions map[string]nextaction.Compact, now time.Time) CriticalPathResult {
	tasks := map[string]ServingTask{}
	var ids []string
	for _, task := range sortedTasks(inputs) {
		if !open(task) {
			continue
		}
		if _, seen := tasks[task.Task.ID]; seen {
			continue
		}
		tasks[task.Task.ID] = task
		ids = append(ids, task.Task.ID)
	}
	best := map[string][]string{}
	onStack := map[string]bool{}
	var chain func(id string) []string
	chain = func(id string) []string {
		if memo, ok := best[id]; ok {
			return memo
		}
		onStack[id] = true
		var winner []string
		dependencies := slices.Clone(tasks[id].DependsOn)
		sort.Strings(dependencies)
		for _, dependency := range slices.Compact(dependencies) {
			if _, ok := tasks[dependency]; !ok || onStack[dependency] {
				continue
			}
			if candidate := chain(dependency); better(candidate, winner) {
				winner = candidate
			}
		}
		onStack[id] = false
		result := append(slices.Clone(winner), id)
		best[id] = result
		return result
	}
	var winner []string
	for _, id := range ids {
		if candidate := chain(id); better(candidate, winner) {
			winner = candidate
		}
	}
	result := CriticalPathResult{Tasks: make([]TaskRef, 0, len(winner)), Length: len(winner)}
	for _, id := range winner {
		result.Tasks = append(result.Tasks, ref(tasks[id], actions))
	}
	return result
}

// better reports whether chain a beats chain b: longer first, then the
// lexicographically smaller ID sequence.
func better(a, b []string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return slices.Compare(a, b) < 0
}

// utcDay truncates an instant to its UTC calendar day.
func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// isoWeekStart returns the Monday at UTC midnight that begins t's ISO week.
func isoWeekStart(t time.Time) time.Time {
	d := utcDay(t)
	offset := (int(d.Weekday()) + 6) % 7
	return d.AddDate(0, 0, -offset)
}

// withinTrailing reports whether at lies in the window [now-window, now]:
// the lower boundary is inclusive, and a time after now is excluded.
func withinTrailing(at, now time.Time, window time.Duration) bool {
	return !at.IsZero() && !at.After(now) && !at.Before(now.Add(-window))
}

// Progress computes the Charts tab's data (AC-3.1 to AC-3.5) at instant now.
//
//   - Daily: one point per UTC day from the earliest ServingSince day through
//     now's day. Scope on day d counts tasks whose ServingSince day is d or
//     earlier and whose ClosedAt is zero or on a later day; delivered counts
//     scope tasks whose MergedAt day is d or earlier; open is their
//     difference.
//   - Forecast: recent is the number of MergedAt times in [now-14d, now].
//     When recent is above zero the rate is recent/14 per day and the day is
//     now's day plus ceil(open/rate) days, open being the current open count;
//     otherwise nil.
//   - Weekly: the eight most recent ISO weeks, oldest first, counting MergedAt
//     times no later than now; only the week holding now is in progress.
//   - Delivered counts tasks in the merged state, Total counts tasks with a
//     zero ClosedAt (req-board-map AC-1.3), Open counts open tasks.
//     Throughput is the MergedAt times in [now-28d, now] divided by 4. The
//     median cycle time is over tasks with a MergedAt, from CreatedAt to
//     MergedAt in seconds, averaging the middle pair of an even sample.
//   - StatusMix partitions every serving task by its current state: merged,
//     closed, or open by next-action actor.
func Progress(inputs Inputs, actions map[string]nextaction.Compact, now time.Time) ProgressResult {
	now = now.UTC()
	today := utcDay(now)
	tasks := sortedTasks(inputs)
	result := ProgressResult{Daily: []DailyPoint{}, Weekly: make([]WeeklyDelivered, 0, weeklyBuckets)}

	var start time.Time
	for _, task := range tasks {
		// RI-2 always supplies the attachment time; a zero value never
		// stretches the series back to year one.
		if since := utcDay(task.ServingSince); !task.ServingSince.IsZero() && (start.IsZero() || since.Before(start)) {
			start = since
		}
	}
	if !start.IsZero() {
		for d := start; !d.After(today); d = d.AddDate(0, 0, 1) {
			point := DailyPoint{Day: d}
			for _, task := range tasks {
				if utcDay(task.ServingSince).After(d) {
					continue
				}
				if !task.ClosedAt.IsZero() && !utcDay(task.ClosedAt).After(d) {
					continue
				}
				point.Scope++
				if !task.MergedAt.IsZero() && !utcDay(task.MergedAt).After(d) {
					point.Delivered++
				}
			}
			point.Open = point.Scope - point.Delivered
			result.Daily = append(result.Daily, point)
		}
	}

	openByActor := map[string]int{}
	recent14, recent28 := 0, 0
	var cycles []float64
	for _, task := range tasks {
		switch task.Task.State {
		case core.TaskMerged:
			result.Delivered++
			result.StatusMix.Delivered++
		case core.TaskClosed:
			result.StatusMix.Closed++
		default:
			result.Open++
			openByActor[actorOf(actions[task.Task.ID])]++
		}
		if task.ClosedAt.IsZero() {
			result.Total++
		}
		if withinTrailing(task.MergedAt, now, forecastWindow) {
			recent14++
		}
		if withinTrailing(task.MergedAt, now, throughputWindow) {
			recent28++
		}
		if !task.MergedAt.IsZero() {
			cycles = append(cycles, task.MergedAt.Sub(task.Task.CreatedAt).Seconds())
		}
	}
	result.StatusMix.Open = make([]ActorCount, 0, len(Actors))
	for _, actor := range Actors {
		result.StatusMix.Open = append(result.StatusMix.Open, ActorCount{Actor: actor, Count: openByActor[actor]})
	}
	if recent14 > 0 {
		days := (result.Open*14 + recent14 - 1) / recent14
		result.Forecast = &Forecast{Day: today.AddDate(0, 0, days), RatePerDay: float64(recent14) / 14}
	}
	result.ThroughputPerWeek = float64(recent28) / 4
	if len(cycles) > 0 {
		sort.Float64s(cycles)
		middle := len(cycles) / 2
		median := cycles[middle]
		if len(cycles)%2 == 0 {
			median = (cycles[middle-1] + cycles[middle]) / 2
		}
		result.MedianCycleSeconds = &median
	}

	current := isoWeekStart(now)
	for i := weeklyBuckets - 1; i >= 0; i-- {
		start := current.AddDate(0, 0, -7*i)
		end := start.AddDate(0, 0, 7)
		year, week := start.ISOWeek()
		bucket := WeeklyDelivered{WeekStart: start, ISOYear: year, ISOWeek: week, InProgress: i == 0}
		for _, task := range tasks {
			merged := task.MergedAt
			if merged.IsZero() || merged.After(now) || merged.Before(start) || !merged.Before(end) {
				continue
			}
			bucket.Delivered++
		}
		result.Weekly = append(result.Weekly, bucket)
	}
	return result
}

// Associations computes the Documents tab (AC-4.1 to AC-4.3) over
// caller-supplied candidates only: designs carry their current confirmed
// content and pending-version count, and decisions are every workspace
// decision (dismissed ones are skipped here).
//
//   - Designs are the union of documents in serving tasks' Designs and
//     documents whose content names the requirement ID as a whole token,
//     sorted by attached task count descending, then ID.
//   - Decisions meet at least one test: the statement, context, or rejected
//     alternatives name the requirement; the requirement's current confirmed
//     content cites the decision; or a serving task body cites it. They sort
//     by naming the requirement, then by being cited by it, then by citing
//     task count descending, then by decision number.
//   - Acceptance criteria are every AC-n.m of the current statements in
//     document order, each with the serving tasks whose bodies cite it.
//
// actions and now are unused: associations are time- and action-independent.
func Associations(inputs Inputs, actions map[string]nextaction.Compact, now time.Time, designs []DesignInput, decisions []core.Decision) AssociationsResult {
	requirementID := inputs.Requirement.ID
	tasks := sortedTasks(inputs)
	attached := map[string][]string{}
	for _, task := range tasks {
		for _, designID := range task.Designs {
			if !slices.Contains(attached[designID], task.Task.ID) {
				attached[designID] = append(attached[designID], task.Task.ID)
			}
		}
	}
	result := AssociationsResult{Designs: []DesignAssociation{}, Decisions: []DecisionAssociation{}, AcceptanceCriteria: []CriterionAssociation{}}
	seenDesign := map[string]bool{}
	for _, design := range designs {
		if seenDesign[design.ID] {
			continue
		}
		seenDesign[design.ID] = true
		ids := slices.Clone(attached[design.ID])
		sort.Strings(ids)
		names := ContainsToken(design.Content, requirementID)
		if len(ids) == 0 && !names {
			continue
		}
		if ids == nil {
			ids = []string{}
		}
		result.Designs = append(result.Designs, DesignAssociation{
			ID: design.ID, Title: design.Title, CurrentVersion: design.CurrentVersion,
			RevisionPending: design.PendingVersionCount > 0, AttachedTaskCount: len(ids),
			NamesRequirement: names, AttachedTaskIDs: ids,
		})
	}
	sort.SliceStable(result.Designs, func(i, j int) bool {
		a, b := result.Designs[i], result.Designs[j]
		if a.AttachedTaskCount != b.AttachedTaskCount {
			return a.AttachedTaskCount > b.AttachedTaskCount
		}
		return a.ID < b.ID
	})

	seenDecision := map[string]bool{}
	for _, decision := range decisions {
		if decision.Status == core.DecisionDismissed || seenDecision[decision.ID] {
			continue
		}
		seenDecision[decision.ID] = true
		names := ContainsToken(decision.Statement, requirementID) || ContainsToken(decision.Context, requirementID) || ContainsToken(decision.AlternativesRejected, requirementID)
		cited := containsDecisionToken(inputs.Requirement.Content, decision.ID)
		citing := []string{}
		for _, task := range tasks {
			if containsDecisionToken(task.Task.Body, decision.ID) && !slices.Contains(citing, task.Task.ID) {
				citing = append(citing, task.Task.ID)
			}
		}
		if !names && !cited && len(citing) == 0 {
			continue
		}
		result.Decisions = append(result.Decisions, DecisionAssociation{
			ID: decision.ID, Statement: decision.Statement, Context: decision.Context, Status: decision.Status,
			NamesRequirement: names, CitedByRequirement: cited, CitingTaskIDs: citing,
		})
	}
	sort.SliceStable(result.Decisions, func(i, j int) bool {
		a, b := result.Decisions[i], result.Decisions[j]
		if a.NamesRequirement != b.NamesRequirement {
			return a.NamesRequirement
		}
		if a.CitedByRequirement != b.CitedByRequirement {
			return a.CitedByRequirement
		}
		if len(a.CitingTaskIDs) != len(b.CitingTaskIDs) {
			return len(a.CitingTaskIDs) > len(b.CitingTaskIDs)
		}
		an, aok := decisionNumber(a.ID)
		bn, bok := decisionNumber(b.ID)
		if aok && bok && an != bn {
			return an < bn
		}
		if aok != bok {
			return aok
		}
		return a.ID < b.ID
	})

	for _, statement := range inputs.Requirement.Statements {
		for _, criterion := range statement.AcceptanceCriteria {
			citing := []string{}
			for _, task := range tasks {
				if containsCriterionToken(task.Task.Body, criterion.ID) && !slices.Contains(citing, task.Task.ID) {
					citing = append(citing, task.Task.ID)
				}
			}
			result.AcceptanceCriteria = append(result.AcceptanceCriteria, CriterionAssociation{ID: criterion.ID, Statement: criterion.Statement, CitingTaskIDs: citing})
			result.Total++
			if len(citing) > 0 {
				result.CitedCount++
			}
		}
	}
	return result
}

func decisionNumber(id string) (int, bool) {
	digits, ok := strings.CutPrefix(id, "DEC-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil && n > 0
}

// tokenBoundary reports whether r may sit next to an identifier: a letter,
// digit, hyphen, or underscore, in any script, may not.
func tokenBoundary(r rune) bool {
	return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_')
}

// ContainsToken reports whether content names token as a whole token, as
// req-requirement-insights defines it: an occurrence not preceded or
// followed by a letter, a digit, a hyphen, or an underscore.
func ContainsToken(content, token string) bool {
	return containsWholeToken(content, token, false)
}

// containsDecisionToken applies core.ContainsDecisionToken, the corpus DEC-n
// rule, together with the requirement's whole-token boundary: the corpus
// rule's word boundary alone accepts a hyphen-adjacent occurrence.
func containsDecisionToken(content, decisionID string) bool {
	return core.ContainsDecisionToken(content, decisionID) && ContainsToken(content, decisionID)
}

// containsCriterionToken matches an AC-n.m whole token that is also not
// followed by a period and a digit, so AC-1.1 never matches AC-1.10 or
// AC-1.1.2.
func containsCriterionToken(content, criterionID string) bool {
	return criterionIDPattern.MatchString(criterionID) && containsWholeToken(content, criterionID, true)
}

func containsWholeToken(content, token string, criterion bool) bool {
	if token == "" {
		return false
	}
	for offset := 0; offset <= len(content)-len(token); {
		index := strings.Index(content[offset:], token)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(token)
		offset = start + 1
		if before, size := utf8.DecodeLastRuneInString(content[:start]); size > 0 && !tokenBoundary(before) {
			continue
		}
		after, size := utf8.DecodeRuneInString(content[end:])
		if size > 0 && !tokenBoundary(after) {
			continue
		}
		if criterion && size > 0 && after == '.' {
			if next, nextSize := utf8.DecodeRuneInString(content[end+size:]); nextSize > 0 && unicode.IsDigit(next) {
				continue
			}
		}
		return true
	}
	return false
}

// fingerprintDocument is the canonical headline input (RI-3, RI-4). Its
// field order is frozen: changing it changes every stored fingerprint.
type fingerprintDocument struct {
	RequirementID  string            `json:"requirement_id"`
	CurrentVersion int               `json:"current_version"`
	Tasks          []fingerprintTask `json:"tasks"`
}

type fingerprintTask struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	State        string            `json:"state"`
	NextAction   fingerprintAction `json:"next_action"`
	DependsOn    []string          `json:"depends_on"`
	ServingSince string            `json:"serving_since"`
}

type fingerprintAction struct {
	Label string `json:"label"`
	Actor string `json:"actor"`
	Kind  string `json:"kind"`
}

// FingerprintDocument returns the canonical JSON document Fingerprint hashes,
// the exact bytes RI-4 sends to the model as untrusted data. It holds the
// requirement ID and current confirmed version, then every serving task in
// ID order with its id, title, state, next-action label, actor, and kind,
// sorted depends_on, and serving_since in RFC 3339 UTC. Nothing else enters:
// no body, creation or terminal time, design, compact summary, deadline, or
// blocker list, and not now. The JSON has no insignificant whitespace and no
// HTML escaping.
func FingerprintDocument(inputs Inputs, actions map[string]nextaction.Compact, now time.Time) []byte {
	document := fingerprintDocument{RequirementID: inputs.Requirement.ID, CurrentVersion: inputs.Requirement.CurrentVersion, Tasks: []fingerprintTask{}}
	for _, task := range sortedTasks(inputs) {
		action := actions[task.Task.ID]
		dependsOn := slices.Clone(task.DependsOn)
		sort.Strings(dependsOn)
		if dependsOn == nil {
			dependsOn = []string{}
		}
		document.Tasks = append(document.Tasks, fingerprintTask{
			ID: task.Task.ID, Title: task.Task.Title, State: string(task.Task.State),
			NextAction: fingerprintAction{Label: action.Label, Actor: action.Actor, Kind: action.Kind},
			DependsOn:  dependsOn, ServingSince: task.ServingSince.UTC().Format(time.RFC3339),
		})
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		// The document holds only strings, integers, and slices of them.
		panic("insight: encode fingerprint document: " + err.Error())
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
}

// Fingerprint is the lowercase hex SHA-256 of FingerprintDocument (RI-3). It
// changes exactly when the headline's input changes (AC-2.6).
func Fingerprint(inputs Inputs, actions map[string]nextaction.Compact, now time.Time) string {
	sum := sha256.Sum256(FingerprintDocument(inputs, actions, now))
	return hex.EncodeToString(sum[:])
}
