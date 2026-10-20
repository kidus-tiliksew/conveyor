package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
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
