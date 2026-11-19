package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/crateinsight"
)

// CrateState is a crate's one state axis: open or closed
// (component-crates invariant 6).
type CrateState string

const (
	CrateOpen   CrateState = "open"
	CrateClosed CrateState = "closed"
)

// Valid reports whether the state is open or closed.
func (s CrateState) Valid() bool { return s == CrateOpen || s == CrateClosed }

// Crate event kinds (component-crates, Events). The four crate kinds are
// workspace events. The two membership kinds are task events.
const (
	CrateCreatedEvent      = "crate.created"
	CrateUpdatedEvent      = "crate.updated"
	CrateClosedEvent       = "crate.closed"
	CrateReopenedEvent     = "crate.reopened"
	TaskCrateSetEvent      = "task.crate_set"
	TaskCrateClearedEvent  = "task.crate_cleared"
	crateNameMaxCharacters = 120
	crateDateLayout        = "2006-01-02"
)

// CrateActivityKinds are the member task event kinds a crate's activity read
// returns (component-crates invariant 13; req-crates AC-5.5).
var CrateActivityKinds = []string{
	"task.created", TaskCrateSetEvent, "pull_request.opened", "review.round_completed",
	"merge.confirmed", "merge.reconciled", "task.cancelled",
}

// Crate is one crate record with its member counts. RequirementIDs is the
// declared list, sorted. The JSON form is the crate row of component-crates
// Public surfaces.
type Crate struct {
	ID              string     `json:"id"`
	Workspace       string     `json:"-"`
	Name            string     `json:"name"`
	TargetDate      *string    `json:"target_date"`
	State           CrateState `json:"state"`
	Shipped         bool       `json:"shipped"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	ClosedAt        *time.Time `json:"closed_at"`
	OpenTaskCount   int        `json:"open_task_count"`
	MergedTaskCount int        `json:"merged_task_count"`
	TotalTaskCount  int        `json:"total_task_count"`
	RequirementIDs  []string   `json:"-"`
}

// CrateRecord is a crate with the IDs of its member tasks, ordered by
// creation time and then ID.
type CrateRecord struct {
	Crate
	TaskIDs []string `json:"-"`
}

// CrateInput is a create request after trimming. TargetDate is nil for no date.
type CrateInput struct {
	Name           string
	TargetDate     *string
	RequirementIDs []string
}

// CratePatch is an edit request. A nil member leaves that field unchanged.
// TargetDateSet distinguishes an explicit null target date from an omitted
// one.
type CratePatch struct {
	Name           *string
	TargetDateSet  bool
	TargetDate     *string
	RequirementIDs *[]string
	State          *CrateState
}

// CrateListQuery selects crates by state: open, closed, or every state when
// State is empty.
type CrateListQuery struct {
	State CrateState
}

// CrateActivityQuery is one activity page request. Cursor is the opaque value
// a previous page returned.
type CrateActivityQuery struct {
	Limit  int
	Cursor string
}

// CrateActivityEntry is one member task event in a crate's activity.
type CrateActivityEntry struct {
	Event     core.Event
	TaskTitle string
}

// CrateActivityPage is one page of a crate's activity, newest first.
// NextCursor is empty on the last page.
type CrateActivityPage struct {
	Entries    []CrateActivityEntry
	NextCursor string
}

// MaxCrateActivityLimit bounds one activity page.
const MaxCrateActivityLimit = 200

// CrateStore keeps crate records and task membership (component-crates;
// component-persistence). Every mutation runs in one transaction with its
// validation and its one audit event, so a refused request records nothing.
// A crate of another workspace answers CrateNotFoundError. SetTaskCrate and
// task intake serialize with closing a crate, so a closed crate never gains a
// task (component-crates invariants 3 to 8).
type CrateStore interface {
	CreateCrate(ctx context.Context, input CrateInput) (Crate, error)
	UpdateCrate(ctx context.Context, id string, patch CratePatch) (Crate, error)
	GetCrate(ctx context.Context, id string) (CrateRecord, error)
	ListCrates(ctx context.Context, query CrateListQuery) ([]Crate, error)
	ListCrateActivity(ctx context.Context, id string, query CrateActivityQuery) (CrateActivityPage, error)
	// CrateNames returns the name of each listed crate of the workspace.
	CrateNames(ctx context.Context, ids []string) (map[string]string, error)
	// ListCrateMembers returns the member tasks of each listed crate of the
	// workspace with their ID, title, and state, ordered by creation time and
	// then ID. A crate with no member, or not in the workspace, has no entry.
	// It reads no next action and no task context, so the work-order crate
	// field and the MCP crate list stay lean reads (component-crates
	// invariants 14 and 20).
	ListCrateMembers(ctx context.Context, ids []string) (map[string][]CrateMemberTask, error)
	// ReadCrateInsightInputs returns the inputs of component-crates
	// invariant 22: the crate row and, for each current member ordered by
	// creation time and then ID, its task row and its events of the kinds
	// CrateInsightEventKinds lists in (at, id) order, each
	// work_order.claimed event with the stage of the order it names. Events
	// match members by workspace and task ID, so a member's events from
	// before it joined are included. On PostgreSQL and SingleStore it runs a
	// fixed number of set-based statements whatever the member count. A
	// crate that is missing or in another workspace is CrateNotFoundError.
	ReadCrateInsightInputs(ctx context.Context, id string) (CrateInsightInputs, error)
	// ListCrateMembersLimited returns at most limit member tasks of one crate
	// of the workspace, leaving out excludeTaskID, in the ListCrateMembers
	// order: creation time and then ID. An empty excludeTaskID leaves every
	// member eligible. more is true only when another eligible member follows
	// the returned ones. An empty or unknown crate, an empty crateID, or a
	// crate of another workspace returns no member and more false. The read fetches at most
	// limit plus one rows whatever the crate's size, so the work-order crate
	// field stays bounded (component-crates invariant 20; DEC-105(4)). A
	// limit below 1, or one whose successor overflows int, is a
	// CrateFieldError.
	ListCrateMembersLimited(ctx context.Context, crateID, excludeTaskID string, limit int) (members []CrateMemberTask, more bool, err error)
	// SetTaskCrate makes the task a member of the open crate crateID only.
	SetTaskCrate(ctx context.Context, taskID, crateID string) (core.Task, error)
	// ClearTaskCrate makes the task a member of no crate.
	ClearTaskCrate(ctx context.Context, taskID string) (core.Task, error)
}

// CrateMemberTask is one member task of a crate as ListCrateMembers reads
// it: the task's ID, title, and state.
type CrateMemberTask struct {
	ID    string         `json:"id"`
	Title string         `json:"title"`
	State core.TaskState `json:"state"`
}

// CrateNotFoundError names a crate that is missing or in another workspace.
type CrateNotFoundError struct{ ID string }

func (e *CrateNotFoundError) Error() string { return fmt.Sprintf("crate %s was not found", e.ID) }

// CrateClosedError names a closed crate that a task would join.
type CrateClosedError struct{ ID string }

func (e *CrateClosedError) Error() string {
	return fmt.Sprintf("crate %s is closed, so no task can join it", e.ID)
}

// CrateFieldError names the crate field that holds a value the rules refuse
// (component-crates invariant 4).
type CrateFieldError struct {
	Field   string
	Message string
}

func (e *CrateFieldError) Error() string { return e.Field + ": " + e.Message }

// CrateRefusal reports whether err is a crate refusal the routes answer with
// a 4xx code.
func CrateRefusal(err error) bool {
	var notFound *CrateNotFoundError
	var closed *CrateClosedError
	var field *CrateFieldError
	return errors.As(err, &notFound) || errors.As(err, &closed) || errors.As(err, &field)
}

// NormalizeJoinCrateID trims the crate a membership act joins and refuses a
// blank one, which would otherwise read as a removal.
func NormalizeJoinCrateID(crateID string) (string, error) {
	crateID = strings.TrimSpace(crateID)
	if crateID == "" {
		return "", &CrateFieldError{Field: "crate_id", Message: "crate_id is required"}
	}
	return crateID, nil
}

// NewCrateID mints crate-<yymmdd>-<6 hex> (feature-crates CR-1).
func NewCrateID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "crate-" + time.Now().UTC().Format("060102") + "-" + hex.EncodeToString(b)
}

// CrateTime is the time every backend records: UTC at the microsecond
// precision both durable backends keep.
func CrateTime(now time.Time) time.Time { return now.UTC().Truncate(time.Microsecond) }

// NormalizeCrateName trims the name and applies the 1 to 120 character rule.
func NormalizeCrateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", &CrateFieldError{Field: "name", Message: "name must not be empty"}
	}
	if utf8.RuneCountInString(name) > crateNameMaxCharacters {
		return "", &CrateFieldError{Field: "name", Message: "name must be at most 120 characters"}
	}
	return name, nil
}

// NormalizeCrateTargetDate accepts nil or a real calendar date written
// YYYY-MM-DD.
func NormalizeCrateTargetDate(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	// Years 1000 to 9999 fit every backend's date column: SingleStore's DATE
	// starts at 1000, and PostgreSQL has no year 0.
	parsed, err := time.Parse(crateDateLayout, *value)
	if err != nil || parsed.Format(crateDateLayout) != *value || parsed.Year() < 1000 {
		return nil, &CrateFieldError{Field: "target_date", Message: "target_date must be a calendar date from 1000-01-01 to 9999-12-31 written YYYY-MM-DD, or null"}
	}
	normalized := *value
	return &normalized, nil
}

// NormalizeCrateRequirementIDs trims, deduplicates, and sorts the declared
// list. An empty ID is refused.
func NormalizeCrateRequirementIDs(ids []string) ([]string, error) {
	normalized, err := NormalizeTaskContextInput(TaskContextInput{RequirementIDs: ids})
	if err != nil {
		return nil, &CrateFieldError{Field: "requirement_ids", Message: err.Error()}
	}
	out := append([]string{}, normalized.RequirementIDs...)
	sort.Strings(out)
	return out, nil
}

// NormalizeCrateInput applies the field rules to a create request.
func NormalizeCrateInput(input CrateInput) (CrateInput, error) {
	var err error
	if input.Name, err = NormalizeCrateName(input.Name); err != nil {
		return CrateInput{}, err
	}
	if input.TargetDate, err = NormalizeCrateTargetDate(input.TargetDate); err != nil {
		return CrateInput{}, err
	}
	if input.RequirementIDs, err = NormalizeCrateRequirementIDs(input.RequirementIDs); err != nil {
		return CrateInput{}, err
	}
	return input, nil
}

// NormalizeCratePatch applies the field rules to the members an edit names.
func NormalizeCratePatch(patch CratePatch) (CratePatch, error) {
	if patch.Name != nil {
		name, err := NormalizeCrateName(*patch.Name)
		if err != nil {
			return CratePatch{}, err
		}
		patch.Name = &name
	}
	if patch.TargetDateSet {
		date, err := NormalizeCrateTargetDate(patch.TargetDate)
		if err != nil {
			return CratePatch{}, err
		}
		patch.TargetDate = date
	} else {
		patch.TargetDate = nil
	}
	if patch.RequirementIDs != nil {
		ids, err := NormalizeCrateRequirementIDs(*patch.RequirementIDs)
		if err != nil {
			return CratePatch{}, err
		}
		patch.RequirementIDs = &ids
	}
	if patch.State != nil && !patch.State.Valid() {
		return CratePatch{}, &CrateFieldError{Field: "state", Message: "state must be open or closed"}
	}
	return patch, nil
}

// CrateChange is the old and new value of one changed field.
type CrateChange struct {
	Old any `json:"old"`
	New any `json:"new"`
}

// CrateEventPayload is the payload of every crate workspace event. The
// workspace ID scopes the event on the volatile backend, which keeps
// workspace events without a task in one list.
type CrateEventPayload struct {
	WorkspaceID string                 `json:"workspace_id"`
	CrateID     string                 `json:"crate_id"`
	Changes     map[string]CrateChange `json:"changes"`
}

// CrateCreatedPayload records every field of a new crate as a change from
// null (component-crates invariant 8; req-crates AC-1.2).
func CrateCreatedPayload(crate Crate) json.RawMessage {
	return core.JSONPayload(CrateEventPayload{WorkspaceID: crate.Workspace, CrateID: crate.ID, Changes: map[string]CrateChange{
		"name":            {Old: nil, New: crate.Name},
		"target_date":     {Old: nil, New: crate.TargetDate},
		"requirement_ids": {Old: nil, New: nonNilStrings(crate.RequirementIDs)},
		"state":           {Old: nil, New: crate.State},
	}})
}

// ApplyCratePatch applies a normalized patch to the current crate and
// returns the next crate, the event kind, and the event payload. An empty
// kind means the patch changes nothing and records nothing.
func ApplyCratePatch(current Crate, patch CratePatch, now time.Time) (Crate, string, json.RawMessage) {
	next := current
	next.RequirementIDs = append([]string(nil), current.RequirementIDs...)
	changes := map[string]CrateChange{}
	if patch.Name != nil && *patch.Name != current.Name {
		changes["name"] = CrateChange{Old: current.Name, New: *patch.Name}
		next.Name = *patch.Name
	}
	if patch.TargetDateSet && !sameCrateDate(current.TargetDate, patch.TargetDate) {
		changes["target_date"] = CrateChange{Old: current.TargetDate, New: patch.TargetDate}
		next.TargetDate = patch.TargetDate
	}
	if patch.RequirementIDs != nil && !slices.Equal(nonNilStrings(current.RequirementIDs), nonNilStrings(*patch.RequirementIDs)) {
		changes["requirement_ids"] = CrateChange{Old: nonNilStrings(current.RequirementIDs), New: nonNilStrings(*patch.RequirementIDs)}
		next.RequirementIDs = append([]string(nil), *patch.RequirementIDs...)
	}
	kind := CrateUpdatedEvent
	if patch.State != nil && *patch.State != current.State {
		changes["state"] = CrateChange{Old: current.State, New: *patch.State}
		next.State = *patch.State
		if next.State == CrateClosed {
			kind = CrateClosedEvent
			closedAt := CrateTime(now)
			next.ClosedAt = &closedAt
		} else {
			kind = CrateReopenedEvent
			next.ClosedAt = nil
		}
	}
	if len(changes) == 0 {
		return current, "", nil
	}
	return next, kind, core.JSONPayload(CrateEventPayload{WorkspaceID: current.Workspace, CrateID: current.ID, Changes: changes})
}

func sameCrateDate(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// TaskCrateMovePayload is the payload of task.crate_set and
// task.crate_cleared: the crate the task joined and the crate it left, each
// empty when there is none (component-crates invariant 8; req-crates AC-2.2,
// AC-2.3).
type TaskCrateMovePayload struct {
	TaskID      string `json:"task_id"`
	CrateID     string `json:"crate_id,omitempty"`
	LeftCrateID string `json:"left_crate_id,omitempty"`
}

// TaskCrateMoveEvent returns the membership event for a move from left to
// joined. An empty joined is a removal. The caller records nothing when
// joined equals left.
func TaskCrateMoveEvent(taskID, joined, left string, at time.Time) core.Event {
	kind := TaskCrateSetEvent
	if joined == "" {
		kind = TaskCrateClearedEvent
	}
	return core.Event{TaskID: taskID, Kind: kind, At: at,
		Payload: core.JSONPayload(TaskCrateMovePayload{TaskID: taskID, CrateID: joined, LeftCrateID: left})}
}

// CrateMember is the state of one member task, the input of the counts.
type CrateMember struct {
	CrateID string
	State   core.TaskState
}

// ApplyCrateCounts sets the counts and shipped from the member states
// (component-crates invariant 9; req-crates AC-5.1, AC-5.2).
func ApplyCrateCounts(crate *Crate, states []core.TaskState) {
	crate.TotalTaskCount, crate.MergedTaskCount, crate.OpenTaskCount = 0, 0, 0
	for _, state := range states {
		crate.TotalTaskCount++
		switch state {
		case core.TaskMerged:
			crate.MergedTaskCount++
		case core.TaskClosed:
		default:
			crate.OpenTaskCount++
		}
	}
	crate.Shipped = crate.State == CrateOpen && crate.TotalTaskCount > 0 && crate.OpenTaskCount == 0
}

// ApplyCrateMemberCounts groups member states by crate and applies the counts
// to each crate.
func ApplyCrateMemberCounts(crates []Crate, members []CrateMember) {
	byCrate := map[string][]core.TaskState{}
	for _, member := range members {
		byCrate[member.CrateID] = append(byCrate[member.CrateID], member.State)
	}
	for i := range crates {
		ApplyCrateCounts(&crates[i], byCrate[crates[i].ID])
	}
}

// SortCrates orders crates by target date, soonest first, then crates with no
// date, then name, then ID (component-crates invariant 10; req-crates AC-5.1).
func SortCrates(crates []Crate) {
	sort.SliceStable(crates, func(i, j int) bool {
		a, b := crates[i], crates[j]
		switch {
		case a.TargetDate != nil && b.TargetDate == nil:
			return true
		case a.TargetDate == nil && b.TargetDate != nil:
			return false
		case a.TargetDate != nil && *a.TargetDate != *b.TargetDate:
			return *a.TargetDate < *b.TargetDate
		case a.Name != b.Name:
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
}

// CrateListMatches reports whether a crate's state passes the list query.
func CrateListMatches(query CrateListQuery, state CrateState) bool {
	return query.State == "" || query.State == state
}

// CrateRequirementCoverage is one requirement with the number of member tasks
// whose active context attachments hold it (component-crates invariant 11).
type CrateRequirementCoverage struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	TaskCount int    `json:"task_count"`
}

// CrateDetail is the crate read before its member rows gain their next
// actions: the crate row, the declared requirements with their coverage, the
// requirements members carry that the crate does not declare, and the member
// tasks ordered by creation time and then ID.
type CrateDetail struct {
	Crate
	Requirements           []CrateRequirementCoverage
	UndeclaredRequirements []CrateRequirementCoverage
	Tasks                  []core.Task
}

// ReadCrateDetail assembles the crate read from the record, the member tasks
// the crate task filter selects, and each member's active requirement
// attachments (component-crates invariants 9 and 11; req-crates AC-5.4). A
// declared requirement with a count of 0 is the uncovered mark.
func ReadCrateDetail(ctx context.Context, st interface {
	Store
	CrateStore
}, id string) (CrateDetail, error) {
	record, err := st.GetCrate(ctx, id)
	if err != nil {
		return CrateDetail{}, err
	}
	members, err := st.ListTasksFiltered(ctx, TaskFilter{Crate: record.ID})
	if err != nil {
		return CrateDetail{}, err
	}
	sort.SliceStable(members, func(i, j int) bool {
		if !members[i].CreatedAt.Equal(members[j].CreatedAt) {
			return members[i].CreatedAt.Before(members[j].CreatedAt)
		}
		return members[i].ID < members[j].ID
	})
	counts := map[string]int{}
	titles := map[string]string{}
	for _, member := range members {
		events, err := st.ListTaskContextEvents(ctx, member.ID)
		if err != nil {
			return CrateDetail{}, err
		}
		active, _ := ActiveTaskContextReferences(events)
		for requirementID := range active {
			counts[requirementID]++
		}
	}
	title := func(requirementID string) (string, bool, error) {
		if cached, ok := titles[requirementID]; ok {
			return cached, true, nil
		}
		document, err := st.GetRequirement(ctx, requirementID)
		if errors.Is(err, ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		titles[requirementID] = document.Title
		return document.Title, document.CurrentVersion > 0, nil
	}
	detail := CrateDetail{Crate: record.Crate, Requirements: []CrateRequirementCoverage{}, UndeclaredRequirements: []CrateRequirementCoverage{}, Tasks: members}
	declared := map[string]bool{}
	for _, requirementID := range record.RequirementIDs {
		declared[requirementID] = true
		name, _, err := title(requirementID)
		if err != nil {
			return CrateDetail{}, err
		}
		detail.Requirements = append(detail.Requirements, CrateRequirementCoverage{ID: requirementID, Title: name, TaskCount: counts[requirementID]})
	}
	undeclared := make([]string, 0, len(counts))
	for requirementID := range counts {
		if !declared[requirementID] {
			undeclared = append(undeclared, requirementID)
		}
	}
	sort.Strings(undeclared)
	for _, requirementID := range undeclared {
		// An attachment counts as the task context read counts it: only a
		// requirement with a confirmed version is an attachment.
		name, confirmed, err := title(requirementID)
		if err != nil {
			return CrateDetail{}, err
		}
		if !confirmed {
			continue
		}
		detail.UndeclaredRequirements = append(detail.UndeclaredRequirements, CrateRequirementCoverage{ID: requirementID, Title: name, TaskCount: counts[requirementID]})
	}
	return detail, nil
}

// CrateActivityCursor is the (at, id) position after which an activity page
// continues, older first.
type CrateActivityCursor struct {
	At time.Time
	ID int64
}

// EncodeCrateActivityCursor returns the opaque cursor after entry.
func EncodeCrateActivityCursor(event core.Event) string {
	raw := event.At.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(event.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCrateActivityCursor parses a cursor. An empty cursor is the first
// page and returns nil.
func DecodeCrateActivityCursor(cursor string) (*CrateActivityCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, &CrateFieldError{Field: "cursor", Message: "cursor is not a value a previous page returned"}
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return nil, &CrateFieldError{Field: "cursor", Message: "cursor is not a value a previous page returned"}
	}
	parsedAt, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, &CrateFieldError{Field: "cursor", Message: "cursor is not a value a previous page returned"}
	}
	parsedID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, &CrateFieldError{Field: "cursor", Message: "cursor is not a value a previous page returned"}
	}
	return &CrateActivityCursor{At: parsedAt.UTC(), ID: parsedID}, nil
}

// CheckCrateMemberLimit refuses a ListCrateMembersLimited limit below 1 or
// one whose successor overflows int, because every backend reads limit plus
// one rows to learn whether more members exist.
func CheckCrateMemberLimit(limit int) error {
	if limit < 1 || limit == math.MaxInt {
		return &CrateFieldError{Field: "limit", Message: "limit must be at least 1 and less than the largest int"}
	}
	return nil
}

// NormalizeCrateActivityQuery validates the limit and decodes the cursor.
func NormalizeCrateActivityQuery(query CrateActivityQuery) (int, *CrateActivityCursor, error) {
	limit := query.Limit
	if limit <= 0 || limit > MaxCrateActivityLimit {
		return 0, nil, &CrateFieldError{Field: "limit", Message: "limit must be between 1 and 200"}
	}
	cursor, err := DecodeCrateActivityCursor(query.Cursor)
	return limit, cursor, err
}

// CrateActivityBefore reports whether event sorts after the cursor in the
// newest-first order, so it belongs on the next page.
func CrateActivityBefore(event core.Event, cursor *CrateActivityCursor) bool {
	if cursor == nil {
		return true
	}
	if !event.At.Equal(cursor.At) {
		return event.At.Before(cursor.At)
	}
	return event.ID < cursor.ID
}

// SortCrateActivity orders entries newest first by (at, id).
func SortCrateActivity(entries []CrateActivityEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].Event, entries[j].Event
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		return a.ID > b.ID
	})
}

// PageCrateActivity cuts a newest-first list that holds at most limit+1
// entries into a page and its next cursor.
func PageCrateActivity(entries []CrateActivityEntry, limit int) CrateActivityPage {
	page := CrateActivityPage{Entries: entries}
	if len(entries) > limit {
		page.Entries = entries[:limit]
		page.NextCursor = EncodeCrateActivityCursor(page.Entries[limit-1].Event)
	}
	if page.Entries == nil {
		page.Entries = []CrateActivityEntry{}
	}
	return page
}

// The crate insight input read (component-crates invariant 22;
// feature-crate-insights CI-1). The read is pure: it changes no record and
// appends no event. Its result is the input of internal/crateinsight.

// CrateInsightInputs is the insight read's result, the input of
// crateinsight.Compute.
type CrateInsightInputs = crateinsight.Inputs

// CrateInsightMember is one current member of CrateInsightInputs.
type CrateInsightMember = crateinsight.Member

// CrateInsightEvent is one member event of CrateInsightInputs.
type CrateInsightEvent = crateinsight.Event

// CrateInsightEventKinds are the member event kinds the read returns
// (component-crates invariant 22).
var CrateInsightEventKinds = crateinsight.InsightEventKinds

// CrateInsightEventRow is one member event a backend's set-based read
// returns, with the task it belongs to.
type CrateInsightEventRow struct {
	TaskID string
	Event  CrateInsightEvent
}

// CrateInsightClaimStage returns the stage a work_order.claimed payload
// names. The claim records the whole order, so its stage field names the
// stage; an empty result means the payload names none, and the caller reads
// the stage from the order the payload's id names instead.
func CrateInsightClaimStage(payload json.RawMessage) (core.Stage, string) {
	var order struct {
		ID    string     `json:"id"`
		Stage core.Stage `json:"stage"`
	}
	if json.Unmarshal(payload, &order) != nil {
		return "", ""
	}
	return order.Stage, order.ID
}

// CrateInsightRow converts the crate row to the computation's crate row.
func CrateInsightRow(crate Crate) crateinsight.Crate {
	row := crateinsight.Crate{
		ID: crate.ID, Name: crate.Name, State: string(crate.State), Shipped: crate.Shipped,
		CreatedBy: crate.CreatedBy, CreatedAt: crate.CreatedAt, OpenTaskCount: crate.OpenTaskCount,
		MergedTaskCount: crate.MergedTaskCount, TotalTaskCount: crate.TotalTaskCount,
	}
	if crate.TargetDate != nil {
		date := *crate.TargetDate
		row.TargetDate = &date
	}
	if crate.ClosedAt != nil {
		at := crate.ClosedAt.UTC()
		row.ClosedAt = &at
	}
	return row
}

// AssembleCrateInsightInputs joins a backend's rows into the read's result.
// tasks holds the current member rows. Each event row joins the member it
// names; a row of another task is dropped. The crate's counts come from the
// member states (invariant 9). Members are ordered by creation time and then
// ID, and each member's events by (at, id).
func AssembleCrateInsightInputs(crate Crate, tasks []core.Task, events []CrateInsightEventRow) CrateInsightInputs {
	members := slices.Clone(tasks)
	sort.SliceStable(members, func(i, j int) bool {
		if !members[i].CreatedAt.Equal(members[j].CreatedAt) {
			return members[i].CreatedAt.Before(members[j].CreatedAt)
		}
		return members[i].ID < members[j].ID
	})
	states := make([]core.TaskState, len(members))
	byTask := make(map[string]int, len(members))
	result := CrateInsightInputs{Members: make([]CrateInsightMember, len(members))}
	for i, task := range members {
		states[i] = task.State
		byTask[task.ID] = i
		task.CreatedAt = task.CreatedAt.UTC()
		result.Members[i] = CrateInsightMember{Task: task, Events: []CrateInsightEvent{}}
	}
	ApplyCrateCounts(&crate, states)
	result.Crate = CrateInsightRow(crate)
	for _, row := range events {
		i, ok := byTask[row.TaskID]
		if !ok {
			continue
		}
		event := row.Event
		event.At = event.At.UTC()
		if event.Kind == "work_order.claimed" {
			event.Payload = nil
		} else {
			event.Stage = ""
		}
		result.Members[i].Events = append(result.Members[i].Events, event)
	}
	for i := range result.Members {
		sort.SliceStable(result.Members[i].Events, func(a, b int) bool {
			left, right := result.Members[i].Events[a], result.Members[i].Events[b]
			if !left.At.Equal(right.At) {
				return left.At.Before(right.At)
			}
			return left.ID < right.ID
		})
	}
	return result
}
