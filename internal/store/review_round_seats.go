package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Review round seat sources (feature-review-seat-minimum RS-2; DEC-77,
// DEC-78; req-review-gates-evidence AC-2.4, AC-2.7).
const (
	ReviewSeatSourceDeclared = "declared"
	ReviewSeatSourcePlanned  = "planned"
	ReviewSeatSourceCarried  = "carried"
	ReviewSeatSourceMinimum  = "minimum"
)

// ErrReviewRoundSeatsInvalid reports round seat metadata that does not match
// the task's minimum, seat floor, or recorded provenance when a round-building
// command revalidates it inside its task transaction.
var ErrReviewRoundSeatsInvalid = errors.New("review round seats invalid")

// ValidReviewSeatSource reports whether source is one of the four RS-2 sources.
func ValidReviewSeatSource(source string) bool {
	switch source {
	case ReviewSeatSourceDeclared, ReviewSeatSourcePlanned, ReviewSeatSourceCarried, ReviewSeatSourceMinimum:
		return true
	}
	return false
}

// ReviewRoundSeats is one persisted review_round_seats row. A round with
// orders and no record is a legacy round; no record is manufactured for it.
type ReviewRoundSeats struct {
	TaskID                    string    `json:"task_id"`
	ReviewRound               int       `json:"review_round"`
	SeatCount                 int       `json:"seat_count"`
	SeatCountSource           string    `json:"seat_count_source"`
	MinimumSeatCount          int       `json:"minimum_seat_count"`
	ImplementationWorkOrderID string    `json:"implementation_work_order_id"`
	CreatedAt                 time.Time `json:"created_at"`
}

// Validate is the record check every backend runs before insert, because
// SingleStore does not enforce the PostgreSQL CHECK constraints.
func (r ReviewRoundSeats) Validate() error {
	switch {
	case r.TaskID == "":
		return fmt.Errorf("%w: task is required", ErrReviewRoundSeatsInvalid)
	case r.ReviewRound < 1:
		return fmt.Errorf("%w: review round %d is not positive", ErrReviewRoundSeatsInvalid, r.ReviewRound)
	case r.SeatCount < 1:
		return fmt.Errorf("%w: seat count %d is not positive", ErrReviewRoundSeatsInvalid, r.SeatCount)
	case r.MinimumSeatCount < 1:
		return fmt.Errorf("%w: minimum seat count %d is not positive", ErrReviewRoundSeatsInvalid, r.MinimumSeatCount)
	case !ValidReviewSeatSource(r.SeatCountSource):
		return fmt.Errorf("%w: seat count source %q is unknown", ErrReviewRoundSeatsInvalid, r.SeatCountSource)
	}
	return nil
}

// ReviewRoundSeatInputs are the explicit inputs of the RS-2 resolution.
type ReviewRoundSeatInputs struct {
	// Round is the review round being opened.
	Round int
	// MinimumSeatCount is the task's minimum from ReviewSeatMinimum.
	MinimumSeatCount int
	// PlannedSeatCount is the latest approved plan version's review_seats.
	PlannedSeatCount *int
	// ImplementationWorkOrderID and DeclaredSeatCount describe the latest
	// submitted implement order.
	ImplementationWorkOrderID string
	DeclaredSeatCount         *int
	// DeclarationConsumed is true when any persisted round record names the
	// implementation order, so its declaration fires once.
	DeclarationConsumed bool
	// PreviousRound is the highest round below Round, or 0. PreviousSeatCount
	// is its record's seat count, or its non-superseded seat orders for a
	// legacy round (PreviousRecorded false).
	PreviousRound     int
	PreviousSeatCount int
	PreviousRecorded  bool
}

// ReviewRoundSeatResolution is the resolved seat count of one new round and
// the metadata its record and review.round_created event carry.
type ReviewRoundSeatResolution struct {
	SeatCount                 int    `json:"seat_count"`
	Source                    string `json:"seat_count_source"`
	MinimumSeatCount          int    `json:"minimum_seat_count"`
	FloorSeatCount            int    `json:"floor_seat_count"`
	ImplementationWorkOrderID string `json:"implementation_work_order_id,omitempty"`
}

// Record is the review_round_seats row for this resolution.
func (r ReviewRoundSeatResolution) Record(taskID string, round int, at time.Time) ReviewRoundSeats {
	return ReviewRoundSeats{TaskID: taskID, ReviewRound: round, SeatCount: r.SeatCount, SeatCountSource: r.Source,
		MinimumSeatCount: r.MinimumSeatCount, ImplementationWorkOrderID: r.ImplementationWorkOrderID, CreatedAt: at}
}

// ReviewRoundCreatedPayload is the review.round_created payload for a newly
// committed round. Callers add retry_request_id for a retried round.
func ReviewRoundCreatedPayload(round int, seats ReviewRoundSeatResolution) map[string]any {
	return map[string]any{"review_round": round, "seat_count": seats.SeatCount,
		"seat_count_source": seats.Source, "minimum_seat_count": seats.MinimumSeatCount}
}

// ReviewSeatMinimum is the task's review seat minimum (DEC-77): the frozen
// seat-list length when the setup freezes a seat list, otherwise the effective
// workspace seat-list length, and 1 when either is zero. A seat list frozen
// empty therefore has minimum 1, and a task without a frozen seat list
// resolves against its workspace policy without persisting one.
func ReviewSeatMinimum(setup config.ExecutionSetup, workspaceSeats int) int {
	count := workspaceSeats
	if setup.Review.Seats != nil {
		count = len(setup.Review.Seats)
	}
	if count < 1 {
		return 1
	}
	return count
}

// ReviewSeatFloor is the larger of the minimum and the planned count (DEC-78).
func ReviewSeatFloor(minimum int, planned *int) int {
	if minimum < 1 {
		minimum = 1
	}
	if planned != nil && *planned > minimum {
		return *planned
	}
	return minimum
}

// ResolveReviewRoundSeats applies the first matching RS-2 row. It is pure: it
// performs no I/O and mutates nothing.
func ResolveReviewRoundSeats(in ReviewRoundSeatInputs) ReviewRoundSeatResolution {
	minimum := in.MinimumSeatCount
	if minimum < 1 {
		minimum = 1
	}
	floor := ReviewSeatFloor(minimum, in.PlannedSeatCount)
	result := ReviewRoundSeatResolution{MinimumSeatCount: minimum, FloorSeatCount: floor, ImplementationWorkOrderID: in.ImplementationWorkOrderID}
	switch {
	case in.ImplementationWorkOrderID != "" && in.DeclaredSeatCount != nil && !in.DeclarationConsumed:
		result.SeatCount, result.Source = *in.DeclaredSeatCount, ReviewSeatSourceDeclared
	case in.PreviousRound > 0 && in.PreviousSeatCount >= floor:
		result.SeatCount, result.Source = in.PreviousSeatCount, ReviewSeatSourceCarried
	case in.PlannedSeatCount != nil && *in.PlannedSeatCount > minimum:
		result.SeatCount, result.Source = *in.PlannedSeatCount, ReviewSeatSourcePlanned
	default:
		result.SeatCount, result.Source = minimum, ReviewSeatSourceMinimum
	}
	return result
}

// ReviewRoundSeatInputsFor builds the resolution inputs for round from the
// task's stored orders, events, round records, and approved plan count.
func ReviewRoundSeatInputsFor(minimum, round int, orders []core.WorkOrder, events []core.Event, records []ReviewRoundSeats, planned *int) ReviewRoundSeatInputs {
	in := ReviewRoundSeatInputs{Round: round, MinimumSeatCount: minimum, PlannedSeatCount: copySeatCount(planned)}
	var implementation *core.WorkOrder
	for i := range orders {
		order := orders[i]
		if order.Stage != core.StageImplement || (order.State != core.WorkOrderSubmitted && order.State != core.WorkOrderCompleted) {
			continue
		}
		if implementation == nil || order.CreatedAt.After(implementation.CreatedAt) ||
			(order.CreatedAt.Equal(implementation.CreatedAt) && order.ID > implementation.ID) {
			implementation = &orders[i]
		}
	}
	if implementation != nil {
		in.ImplementationWorkOrderID = implementation.ID
		in.DeclaredSeatCount = copySeatCount(implementation.DeclaredReviewSeats)
	}
	recorded := map[int]ReviewRoundSeats{}
	for _, record := range records {
		recorded[record.ReviewRound] = record
		if in.ImplementationWorkOrderID != "" && record.ImplementationWorkOrderID == in.ImplementationWorkOrderID {
			in.DeclarationConsumed = true
		}
	}
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound < round && order.ReviewRound > in.PreviousRound {
			in.PreviousRound = order.ReviewRound
		}
	}
	for number := range recorded {
		if number < round && number > in.PreviousRound {
			in.PreviousRound = number
		}
	}
	if in.PreviousRound == 0 {
		return in
	}
	if record, ok := recorded[in.PreviousRound]; ok {
		in.PreviousSeatCount, in.PreviousRecorded = record.SeatCount, true
		return in
	}
	superseded := SupersededReviewWorkOrders(events)
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound == in.PreviousRound && !superseded[order.ID] {
			in.PreviousSeatCount++
		}
	}
	return in
}

// ReviewRoundSeatReader is the public read surface the resolution inputs come
// from outside a backend transaction.
type ReviewRoundSeatReader interface {
	ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error)
	ListEvents(ctx context.Context, taskID string) ([]core.Event, error)
	ListReviewRoundSeats(ctx context.Context, taskID string) ([]ReviewRoundSeats, error)
	GetApprovedSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error)
}

// LoadReviewRoundSeatInputs reads the resolution inputs for round through the
// public store surface.
func LoadReviewRoundSeatInputs(ctx context.Context, reader ReviewRoundSeatReader, taskID string, minimum, round int) (ReviewRoundSeatInputs, error) {
	orders, err := reader.ListTaskWorkOrders(ctx, taskID)
	if err != nil {
		return ReviewRoundSeatInputs{}, err
	}
	events, err := reader.ListEvents(ctx, taskID)
	if err != nil {
		return ReviewRoundSeatInputs{}, err
	}
	records, err := reader.ListReviewRoundSeats(ctx, taskID)
	if err != nil {
		return ReviewRoundSeatInputs{}, err
	}
	var planned *int
	plan, ok, err := reader.GetApprovedSpecVersion(ctx, taskID)
	if err != nil {
		return ReviewRoundSeatInputs{}, err
	}
	if ok {
		planned = plan.ReviewSeats
	}
	return ReviewRoundSeatInputsFor(minimum, round, orders, events, records, planned), nil
}

// CheckReviewRoundSeats revalidates caller-resolved metadata inside the
// round-building command's task transaction, against the stored setup, the
// approved plan count, and the persisted records read in that transaction.
func CheckReviewRoundSeats(want ReviewRoundSeatResolution, setup config.ExecutionSetup, planned *int, records []ReviewRoundSeats, orderCount int) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrReviewRoundSeatsInvalid}, args...)...)
	}
	if !ValidReviewSeatSource(want.Source) {
		return invalid("seat count source %q is unknown", want.Source)
	}
	if want.SeatCount < 1 || want.SeatCount != orderCount {
		return invalid("seat count %d does not match %d seat orders", want.SeatCount, orderCount)
	}
	if want.MinimumSeatCount < 1 {
		return invalid("minimum seat count %d is not positive", want.MinimumSeatCount)
	}
	minimum := ReviewSeatMinimum(setup, want.MinimumSeatCount)
	if want.MinimumSeatCount != minimum {
		return invalid("minimum seat count %d does not match the task minimum %d", want.MinimumSeatCount, minimum)
	}
	floor := ReviewSeatFloor(minimum, planned)
	if want.SeatCount < floor {
		return invalid("seat count %d is below the seat floor %d", want.SeatCount, floor)
	}
	switch want.Source {
	case ReviewSeatSourceMinimum:
		if want.SeatCount != minimum {
			return invalid("minimum source with seat count %d, minimum %d", want.SeatCount, minimum)
		}
	case ReviewSeatSourcePlanned:
		if planned == nil || *planned <= minimum || want.SeatCount != *planned {
			return invalid("planned source with seat count %d does not match the approved plan", want.SeatCount)
		}
	case ReviewSeatSourceDeclared:
		if want.ImplementationWorkOrderID == "" {
			return invalid("declared source without an implementation work order")
		}
		for _, record := range records {
			if record.ImplementationWorkOrderID == want.ImplementationWorkOrderID {
				return invalid("implementation %s declaration was already used by round %d", want.ImplementationWorkOrderID, record.ReviewRound)
			}
		}
	}
	return nil
}

// PolicyHandoffSeatInputs are the round seat inputs a backend loads inside its
// policy-change transaction for PlanTaskPolicyChange, never from the request.
type PolicyHandoffSeatInputs struct {
	Events           []core.Event
	Records          []ReviewRoundSeats
	PlannedSeatCount *int
}

func copySeatCount(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func reviewRoundSeatsKey(workspace, taskID string, round int) string {
	return fmt.Sprintf("%s\x00%s\x00%d", workspace, taskID, round)
}

// ListReviewRoundSeats returns the selected workspace's round records for a
// task, ordered by round.
func (m *memory) ListReviewRoundSeats(ctx context.Context, taskID string) ([]ReviewRoundSeats, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return []ReviewRoundSeats{}, nil
	}
	if selected, scoped := WorkspaceFromContext(ctx); scoped && selected != "" && task.Workspace != selected {
		return []ReviewRoundSeats{}, nil
	}
	return m.reviewRoundSeatsLocked(task.Workspace, taskID), nil
}

func (m *memory) reviewRoundSeatsLocked(workspace, taskID string) []ReviewRoundSeats {
	records := []ReviewRoundSeats{}
	for _, record := range m.reviewRoundSeats {
		if record.workspace == workspace && record.TaskID == taskID {
			records = append(records, record.ReviewRoundSeats)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ReviewRound < records[j].ReviewRound })
	return records
}

func (m *memory) reviewRoundSeatsExistLocked(workspace, taskID string, round int) bool {
	_, ok := m.reviewRoundSeats[reviewRoundSeatsKey(workspace, taskID, round)]
	return ok
}

func (m *memory) putReviewRoundSeatsLocked(workspace string, record ReviewRoundSeats) {
	m.reviewRoundSeats[reviewRoundSeatsKey(workspace, record.TaskID, record.ReviewRound)] = memoryReviewRoundSeats{workspace: workspace, ReviewRoundSeats: record}
}

// approvedPlannedSeatsLocked mirrors GetApprovedSpecVersion: the newest
// approved version's count, copied.
func (m *memory) approvedPlannedSeatsLocked(taskID string) *int {
	versions := m.specs[taskID]
	for index := len(versions) - 1; index >= 0; index-- {
		if versions[index].Approved {
			return copySeatCount(versions[index].ReviewSeats)
		}
	}
	return nil
}

type memoryReviewRoundSeats struct {
	workspace string
	ReviewRoundSeats
}

// Operator review-seat addition (feature-review-seat-minimum RS-5;
// req-review-gates-evidence AC-2.5, AC-2.6; DEC-77, DEC-78).

var (
	// ErrReviewRoundNotOpen refuses an addition to a task without an open
	// review round (HTTP 409 review_round_not_open).
	ErrReviewRoundNotOpen = errors.New("review_round_not_open")
	// ErrReviewRoundSettled refuses an addition to a round that already has
	// its review.round_completed event (HTTP 409 review_round_settled).
	ErrReviewRoundSettled = errors.New("review_round_settled")
	// ErrReviewSeatAdditionConflict refuses a request_id reused with a
	// different task, reason, or count (HTTP 409 review_seat_addition_conflict).
	ErrReviewSeatAdditionConflict = errors.New("review_seat_addition_conflict")
	// ErrReviewSeatAdditionInvalid refuses malformed or unrepresentable
	// addition inputs (HTTP 400 invalid_review_seats).
	ErrReviewSeatAdditionInvalid = errors.New("invalid_review_seats")
	// ErrReviewSeatAdditionForbidden refuses an addition without an
	// authenticated user operator; agents never perform the act.
	ErrReviewSeatAdditionForbidden = errors.New("review_seat_addition_forbidden")
)

// MaxReviewSeatCount is the largest seat count the INT columns represent.
const MaxReviewSeatCount = math.MaxInt32

// maxReviewSeatRequestIDLength is the SingleStore VARCHAR(255) request_id.
const maxReviewSeatRequestIDLength = 255

// ReviewSeatAdditionRequest is one operator seat addition. WorkspaceReviewSeats
// and QueueTimeout are trusted, service-derived inputs: no HTTP body supplies
// them and the replay identity excludes them.
type ReviewSeatAdditionRequest struct {
	TaskID    string `json:"task_id"`
	RequestID string `json:"request_id"`
	Reason    string `json:"reason"`
	Add       int    `json:"add"`
	// WorkspaceReviewSeats is the effective workspace seat-list length, the
	// minimum source for a legacy round's carried record.
	WorkspaceReviewSeats int `json:"-"`
	// QueueTimeout is the queue clock of the added seat orders.
	QueueTimeout time.Duration `json:"-"`
}

// Validate refuses inputs the addition cannot represent. Strings keep their
// exact identity; only their blankness is judged after trimming.
func (r ReviewSeatAdditionRequest) Validate() error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrReviewSeatAdditionInvalid}, args...)...)
	}
	text := func(name, value string) error {
		switch {
		case strings.TrimSpace(value) == "":
			return invalid("%s is required", name)
		case !utf8.ValidString(value):
			return invalid("%s must be valid UTF-8", name)
		case strings.ContainsRune(value, 0):
			return invalid("%s must not contain NUL", name)
		}
		return nil
	}
	if strings.TrimSpace(r.TaskID) == "" {
		return invalid("task is required")
	}
	if err := text("request_id", r.RequestID); err != nil {
		return err
	}
	if utf8.RuneCountInString(r.RequestID) > maxReviewSeatRequestIDLength {
		return invalid("request_id is longer than %d characters", maxReviewSeatRequestIDLength)
	}
	if err := text("reason", r.Reason); err != nil {
		return err
	}
	if r.Add < 1 || r.Add > MaxReviewSeatCount {
		return invalid("add must be an integer from 1 to %d", MaxReviewSeatCount)
	}
	return nil
}

// ReviewSeatAdditionResult is the stable result of one addition, rebuilt from
// its ledger row on replay.
type ReviewSeatAdditionResult struct {
	RequestID      string `json:"request_id"`
	TaskID         string `json:"task_id"`
	ReviewRound    int    `json:"review_round"`
	PriorSeatCount int    `json:"prior_seat_count"`
	SeatCount      int    `json:"seat_count"`
}

// ReviewSeatAddition is one review_seat_additions ledger row.
type ReviewSeatAddition struct {
	RequestID      string    `json:"request_id"`
	TaskID         string    `json:"task_id"`
	ReviewRound    int       `json:"review_round"`
	PriorSeatCount int       `json:"prior_seat_count"`
	SeatCount      int       `json:"seat_count"`
	Reason         string    `json:"reason"`
	ActorID        string    `json:"actor_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// Result reconstructs the addition's result from the ledger row alone.
func (a ReviewSeatAddition) Result() ReviewSeatAdditionResult {
	return ReviewSeatAdditionResult{RequestID: a.RequestID, TaskID: a.TaskID, ReviewRound: a.ReviewRound, PriorSeatCount: a.PriorSeatCount, SeatCount: a.SeatCount}
}

// ReviewSeatAdditionReplay answers a request whose workspace and request_id
// already have a ledger row: the original result when the task, the exact
// reason, and the count delta match, otherwise a conflict. The actor and the
// derived round and head are not part of the identity.
func ReviewSeatAdditionReplay(prior ReviewSeatAddition, request ReviewSeatAdditionRequest) (ReviewSeatAdditionResult, error) {
	if prior.TaskID != request.TaskID || prior.Reason != request.Reason || prior.SeatCount-prior.PriorSeatCount != request.Add {
		return ReviewSeatAdditionResult{}, fmt.Errorf("%w: request_id %s was already used for different inputs", ErrReviewSeatAdditionConflict, request.RequestID)
	}
	return prior.Result(), nil
}

// ReviewSeatAdditionPlan is everything one admitted addition writes.
type ReviewSeatAdditionPlan struct {
	Jobs   []core.Job
	Orders []core.WorkOrder
	// Record is the round's record after the addition; RecordExists tells
	// the backend to update seat_count instead of inserting a carried record.
	Record       ReviewRoundSeats
	RecordExists bool
	Ledger       ReviewSeatAddition
	Payload      map[string]any
	Result       ReviewSeatAdditionResult
}

// ReviewSeatAdditionInputs are the task state a backend reads inside its
// task-serialized span for PlanReviewSeatAddition.
type ReviewSeatAdditionInputs struct {
	Workspace string
	Task      core.Task
	// Orders are the task's work orders; only review-stage orders count.
	Orders  []core.WorkOrder
	Events  []core.Event
	Records []ReviewRoundSeats
	Request ReviewSeatAdditionRequest
	ActorID string
	Now     time.Time
}

// PlanReviewSeatAddition applies the RS-5 rules to the task's locked state.
// It is pure: it performs no I/O and mutates nothing.
func PlanReviewSeatAddition(in ReviewSeatAdditionInputs) (ReviewSeatAdditionPlan, error) {
	request, task := in.Request, in.Task
	if err := request.Validate(); err != nil {
		return ReviewSeatAdditionPlan{}, err
	}
	if core.TaskTerminal(task.State) {
		return ReviewSeatAdditionPlan{}, fmt.Errorf("%w: task %s is %s", ErrReviewRoundNotOpen, task.ID, task.State)
	}
	round := 0
	for _, order := range in.Orders {
		if order.TaskID == task.ID && order.Stage == core.StageReview && order.ReviewRound > round {
			round = order.ReviewRound
		}
	}
	if round < 1 {
		return ReviewSeatAdditionPlan{}, fmt.Errorf("%w: task %s has no review round", ErrReviewRoundNotOpen, task.ID)
	}
	if reviewRoundSettled(in.Events, round) {
		return ReviewSeatAdditionPlan{}, fmt.Errorf("%w: review round %d of task %s has settled", ErrReviewRoundSettled, round, task.ID)
	}
	superseded := SupersededReviewWorkOrders(in.Events)
	var members []core.WorkOrder
	for _, order := range in.Orders {
		if order.TaskID != task.ID || order.Stage != core.StageReview || order.ReviewRound != round {
			continue
		}
		if superseded[order.ID] {
			return ReviewSeatAdditionPlan{}, fmt.Errorf("%w: review round %d of task %s was superseded by a policy handoff", ErrReviewRoundNotOpen, round, task.ID)
		}
		members = append(members, order)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ReviewSeat < members[j].ReviewSeat })
	prior := len(members)
	for i, order := range members {
		if order.ReviewSeat != i+1 {
			return ReviewSeatAdditionPlan{}, fmt.Errorf("review round %d of task %s seats are not contiguous from 1", round, task.ID)
		}
	}
	if request.Add > MaxReviewSeatCount-prior {
		return ReviewSeatAdditionPlan{}, fmt.Errorf("%w: %d seats plus %d exceeds %d", ErrReviewSeatAdditionInvalid, prior, request.Add, MaxReviewSeatCount)
	}
	seatCount := prior + request.Add
	now := in.Now.UTC()
	timeout := request.QueueTimeout
	if timeout <= 0 {
		timeout = config.DefaultWorkOrderQueueTimeout
	}
	template := members[0]
	plan := ReviewSeatAdditionPlan{Jobs: make([]core.Job, 0, request.Add), Orders: make([]core.WorkOrder, 0, request.Add)}
	ids := make([]string, 0, request.Add)
	for seat := prior + 1; seat <= seatCount; seat++ {
		id := fmt.Sprintf("%s-review-%d-seat-%d", task.ID, round, seat)
		plan.Jobs = append(plan.Jobs, core.Job{ID: id, TaskID: task.ID, Stage: core.StageReview, Harness: "external-mcp", AuthMode: "byoa", Runner: "external", Confinement: "none", State: core.JobPending})
		plan.Orders = append(plan.Orders, core.WorkOrder{
			ID: id, TaskID: task.ID, JobID: id, Stage: core.StageReview,
			State: core.WorkOrderQueued, Claimable: true,
			ReviewRound: round, ReviewSeat: seat, ReviewKind: template.ReviewKind,
			ReviewScope: template.ReviewScope, BaselineSHA: template.BaselineSHA, HeadSHA: template.HeadSHA,
			// The added seat reviews the same sealed context as seat 1
			// (component-verification-service).
			VerificationContextID: template.VerificationContextID,
			ExecutionTimeoutText:  template.ExecutionTimeoutText,
			QueueEnteredAt:        now, QueueDeadline: now.Add(timeout), CreatedAt: now,
		})
		ids = append(ids, id)
	}
	for _, record := range in.Records {
		if record.TaskID == task.ID && record.ReviewRound == round {
			plan.Record, plan.RecordExists = record, true
		}
	}
	if plan.RecordExists {
		plan.Record.SeatCount = seatCount
	} else {
		plan.Record = ReviewRoundSeats{TaskID: task.ID, ReviewRound: round, SeatCount: seatCount, SeatCountSource: ReviewSeatSourceCarried,
			MinimumSeatCount: ReviewSeatMinimum(task.SetupContract, request.WorkspaceReviewSeats), CreatedAt: now}
	}
	if err := plan.Record.Validate(); err != nil {
		return ReviewSeatAdditionPlan{}, err
	}
	plan.Ledger = ReviewSeatAddition{RequestID: request.RequestID, TaskID: task.ID, ReviewRound: round, PriorSeatCount: prior, SeatCount: seatCount, Reason: request.Reason, ActorID: in.ActorID, CreatedAt: now}
	plan.Result = plan.Ledger.Result()
	plan.Payload = map[string]any{"request_id": request.RequestID, "workspace_id": in.Workspace, "task_id": task.ID, "review_round": round,
		"prior_seat_count": prior, "seat_count": seatCount, "reason": request.Reason, "actor": in.ActorID, "work_order_ids": ids}
	return plan, nil
}

// RequireReviewSeatOperator returns the user actor an addition requires.
func RequireReviewSeatOperator(ctx context.Context) (Actor, error) {
	actor, err := RequireActor(ctx)
	if err != nil {
		return Actor{}, fmt.Errorf("%w: %w", ErrReviewSeatAdditionForbidden, err)
	}
	if actor.Role != core.ActorUser {
		return Actor{}, fmt.Errorf("%w: actor role %s is not a user", ErrReviewSeatAdditionForbidden, actor.Role)
	}
	return actor, nil
}

// reviewRoundSettled reports whether round has its review.round_completed.
func reviewRoundSettled(events []core.Event, round int) bool {
	for _, event := range events {
		if event.Kind != "review.round_completed" {
			continue
		}
		var payload struct {
			ReviewRound int `json:"review_round"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.ReviewRound == round {
			return true
		}
	}
	return false
}

type memoryReviewSeatAddition struct {
	workspace string
	ReviewSeatAddition
}

func reviewSeatAdditionKey(workspace, requestID string) string {
	return workspace + "\x00" + requestID
}

// AddReviewSeatsCommand is the memory RS-5 command. It holds m.mu throughout
// and validates every refusal before it mutates anything.
func (m *memory) AddReviewSeatsCommand(ctx context.Context, lease taskops.TaskLease, request ReviewSeatAdditionRequest) (ReviewSeatAdditionResult, error) {
	if err := request.Validate(); err != nil {
		return ReviewSeatAdditionResult{}, err
	}
	if _, err := RequireActor(ctx); err != nil {
		return ReviewSeatAdditionResult{}, fmt.Errorf("%w: %w", ErrReviewSeatAdditionForbidden, err)
	}
	actor, err := RequireReviewSeatOperator(ctx)
	if err != nil {
		return ReviewSeatAdditionResult{}, err
	}
	if !lease.ValidForCommand(request.TaskID, string(core.WorkOrderCmdCreate)) {
		return ReviewSeatAdditionResult{}, fmt.Errorf("review seat addition requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, _ := WorkspaceFromContext(ctx)
	key := reviewSeatAdditionKey(workspace, request.RequestID)
	if prior, ok := m.reviewSeatAdditions[key]; ok {
		return ReviewSeatAdditionReplay(prior.ReviewSeatAddition, request)
	}
	task, ok := m.tasks[request.TaskID]
	if !ok || (workspace != "" && task.Workspace != workspace) {
		return ReviewSeatAdditionResult{}, fmt.Errorf("%w: task %s", ErrNotFound, request.TaskID)
	}
	var orders []core.WorkOrder
	for _, order := range m.workOrders {
		if order.TaskID == task.ID && order.Stage == core.StageReview {
			orders = append(orders, order)
		}
	}
	now := time.Now().UTC()
	plan, err := PlanReviewSeatAddition(ReviewSeatAdditionInputs{Workspace: task.Workspace, Task: task, Orders: orders, Events: m.events[task.ID],
		Records: m.reviewRoundSeatsLocked(task.Workspace, task.ID), Request: request, ActorID: actor.ID, Now: now})
	if err != nil {
		return ReviewSeatAdditionResult{}, err
	}
	for i, job := range plan.Jobs {
		if _, _, exists := m.findJobLocked(job.ID); exists {
			return ReviewSeatAdditionResult{}, fmt.Errorf("%w: job %s already exists", ErrDispatchJobConflict, job.ID)
		}
		if _, exists := m.workOrders[plan.Orders[i].ID]; exists {
			return ReviewSeatAdditionResult{}, fmt.Errorf("%w: work order %s already exists", ErrDispatchJobConflict, plan.Orders[i].ID)
		}
	}
	for i, job := range plan.Jobs {
		m.jobs[task.ID] = append(m.jobs[task.ID], job)
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "job.created", Payload: core.JSONPayload(job), At: now})
		order := plan.Orders[i]
		order.UpdatedAt = now
		m.workOrders[order.ID] = order
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "work_order.created", Payload: WorkOrderEventPayload(order), At: now})
	}
	m.putReviewRoundSeatsLocked(task.Workspace, plan.Record)
	m.reviewSeatAdditions[key] = memoryReviewSeatAddition{workspace: workspace, ReviewSeatAddition: plan.Ledger}
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "review.seats_added", Payload: core.JSONPayload(plan.Payload), At: now})
	return plan.Result, nil
}
