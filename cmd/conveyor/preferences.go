package main

// conveyor preferences shows and changes the signed-in person's stage
// preferences through GET and PUT /v1/me/stage-preferences
// (req-stage-preferences AC-2.2; component-stage-preferences). A change reads
// the current set, edits one stage or one review entry, and writes the whole
// set back with If-Match. On a version conflict it reads the set again and
// repeats the same edit on the new set, so a concurrent change to another
// stage or seat is kept, never overwritten by a stale copy (invariant 4).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kidus-tiliksew/conveyor/internal/stageprefs"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const stagePreferencesPath = "/v1/me/stage-preferences"

// maxPreferenceAttempts bounds the conflict retries of one change.
const maxPreferenceAttempts = 5

// preferenceTarget names what one change edits: the whole stage, the review
// primary, or one review seat (1-based).
type preferenceTarget struct {
	primary bool
	seat    int
}

func preferencesCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "preferences",
		Short: "Show and change your stage preferences",
		Long: "Show and change your stage preferences: the harness, model, and effort your agent sessions use for the plan, implement, verify, risk, and review stages. " +
			"The preferences belong to you and apply in every workspace. They are advice to your own sessions; no factory decision reads them.",
	}
	var asJSON bool
	show := &cobra.Command{
		Use:   "show",
		Short: "Show your stage preferences",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			set, err := readStagePreferences(newClient())
			if err != nil {
				return err
			}
			return printStagePreferences(cmd.OutOrStdout(), set, asJSON)
		},
	}
	show.Flags().BoolVar(&asJSON, "json", false, "print the set as JSON")

	var harness, model, effort string
	var setPrimary bool
	var setSeat int
	var setJSON bool
	set := &cobra.Command{
		Use:   "set <stage> --harness <h> --model <m> [--effort <e>] [--primary | --seat <n>]",
		Short: "Set one stage, the review primary, or one review seat",
		Long: "Set the entry for plan, implement, verify, or risk, or for review set the primary (the default, or --primary) or seat N (--seat N, 1 to 8). " +
			"A seat without its own entry uses the primary. Seat N can be set when seats 1 to N-1 are set. The command prints the resulting set.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := preferenceTargetFlags(cmd, args[0], setPrimary, setSeat)
			if err != nil {
				return err
			}
			entry := store.StagePreferenceEntry{Harness: harness, Model: model, Effort: effort}
			result, err := changeStagePreferences(newClient(), args[0], target, func(stages *store.StagePreferenceStages) error {
				return setStagePreferenceEntry(stages, args[0], target, entry)
			})
			if err != nil {
				return err
			}
			return printStagePreferences(cmd.OutOrStdout(), result, setJSON)
		},
	}
	set.Flags().StringVar(&harness, "harness", "", "harness name, such as claude-code or codex (required)")
	set.Flags().StringVar(&model, "model", "", "model name (required)")
	set.Flags().StringVar(&effort, "effort", "", "optional effort")
	set.Flags().BoolVar(&setPrimary, "primary", false, "set the review primary")
	set.Flags().IntVar(&setSeat, "seat", 0, "set review seat N (1 to 8)")
	set.Flags().BoolVar(&setJSON, "json", false, "print the resulting set as JSON")
	_ = set.MarkFlagRequired("harness")
	_ = set.MarkFlagRequired("model")

	var clearPrimary bool
	var clearSeat int
	var clearJSON bool
	clearCmd := &cobra.Command{
		Use:   "clear <stage> [--primary | --seat <n>]",
		Short: "Clear one stage, the review primary, or one review seat",
		Long: "Clear plan, implement, verify, risk, or the whole review stage. For review, --primary clears only the primary and --seat N removes seat N; later seats move up one position. " +
			"The command prints the resulting set.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := preferenceTargetFlags(cmd, args[0], clearPrimary, clearSeat)
			if err != nil {
				return err
			}
			result, err := changeStagePreferences(newClient(), args[0], target, func(stages *store.StagePreferenceStages) error {
				return clearStagePreferenceEntry(stages, args[0], target)
			})
			if err != nil {
				return err
			}
			return printStagePreferences(cmd.OutOrStdout(), result, clearJSON)
		},
	}
	clearCmd.Flags().BoolVar(&clearPrimary, "primary", false, "clear only the review primary")
	clearCmd.Flags().IntVar(&clearSeat, "seat", 0, "remove review seat N (1 to 8); later seats move up")
	clearCmd.Flags().BoolVar(&clearJSON, "json", false, "print the resulting set as JSON")

	command.AddCommand(show, set, clearCmd)
	return command
}

// preferenceTargetFlags checks the stage and its --primary and --seat flags
// before any request.
func preferenceTargetFlags(cmd *cobra.Command, stage string, primary bool, seat int) (preferenceTarget, error) {
	if err := stageprefs.ParseStageName("stage", stage); err != nil {
		return preferenceTarget{}, fmt.Errorf("unknown stage %q: use plan, implement, verify, risk, or review", stage)
	}
	seatSet := cmd.Flags().Changed("seat")
	if primary && seatSet {
		return preferenceTarget{}, errors.New("use --primary or --seat, not both")
	}
	if stage != stageprefs.StageReview && (primary || seatSet) {
		return preferenceTarget{}, fmt.Errorf("--primary and --seat apply only to review, not to %s", stage)
	}
	if seatSet && (seat < 1 || seat > stageprefs.MaxReviewSeats) {
		return preferenceTarget{}, fmt.Errorf("--seat must be a number from 1 to %d", stageprefs.MaxReviewSeats)
	}
	return preferenceTarget{primary: primary, seat: seat}, nil
}

// setStagePreferenceEntry applies one set command to stages. Review without
// --seat sets the primary.
func setStagePreferenceEntry(stages *store.StagePreferenceStages, stage string, target preferenceTarget, entry store.StagePreferenceEntry) error {
	value := entry
	switch stage {
	case stageprefs.StagePlan:
		stages.Plan = &value
	case stageprefs.StageImplement:
		stages.Implement = &value
	case stageprefs.StageVerify:
		stages.Verify = &value
	case stageprefs.StageRisk:
		stages.Risk = &value
	case stageprefs.StageReview:
		if stages.Review == nil {
			stages.Review = &store.ReviewStagePreference{Seats: []store.StagePreferenceEntry{}}
		}
		if target.seat == 0 {
			stages.Review.Primary = &value
			return nil
		}
		switch index := target.seat - 1; {
		case index < len(stages.Review.Seats):
			stages.Review.Seats[index] = value
		case index == len(stages.Review.Seats):
			stages.Review.Seats = append(stages.Review.Seats, value)
		default:
			return fmt.Errorf("seat %d cannot be set while seat %d is not set: set the seats in order", target.seat, len(stages.Review.Seats)+1)
		}
	}
	return nil
}

// clearStagePreferenceEntry applies one clear command to stages. Removing a
// seat moves every later seat up one position.
func clearStagePreferenceEntry(stages *store.StagePreferenceStages, stage string, target preferenceTarget) error {
	switch stage {
	case stageprefs.StagePlan:
		stages.Plan = nil
	case stageprefs.StageImplement:
		stages.Implement = nil
	case stageprefs.StageVerify:
		stages.Verify = nil
	case stageprefs.StageRisk:
		stages.Risk = nil
	case stageprefs.StageReview:
		switch {
		case target.primary:
			if stages.Review != nil {
				stages.Review.Primary = nil
			}
		case target.seat > 0:
			if stages.Review == nil || target.seat > len(stages.Review.Seats) {
				return fmt.Errorf("review seat %d is not set", target.seat)
			}
			index := target.seat - 1
			stages.Review.Seats = append(stages.Review.Seats[:index:index], stages.Review.Seats[index+1:]...)
		default:
			stages.Review = nil
		}
		if stages.Review != nil && stages.Review.Primary == nil && len(stages.Review.Seats) == 0 {
			stages.Review = nil
		}
	}
	return nil
}

func readStagePreferences(c *client) (store.StagePreferences, error) {
	var set store.StagePreferences
	if err := c.do(http.MethodGet, stagePreferencesPath, nil, &set); err != nil {
		return store.StagePreferences{}, stagePreferenceRequestError(err)
	}
	return set, nil
}

// changeStagePreferences reads the current set, applies edit to a copy, and
// writes it with If-Match. A 412 starts over from a fresh read, so the edit
// is repeated on the newer set rather than replaying a stale whole set. A seat
// edit addresses a position, so a retry refuses when the entry at that
// position changed: after a concurrent reorder or removal, the same position
// holds another entry, which the command must not overwrite or remove
// (component-stage-preferences invariant 4).
func changeStagePreferences(c *client, stage string, target preferenceTarget, edit func(*store.StagePreferenceStages) error) (store.StagePreferences, error) {
	var original *store.StagePreferenceEntry
	for attempt := 1; attempt <= maxPreferenceAttempts; attempt++ {
		current, err := readStagePreferences(c)
		if err != nil {
			return store.StagePreferences{}, err
		}
		if stage == stageprefs.StageReview && target.seat > 0 {
			seat := reviewSeatAt(current.Stages, target.seat)
			if attempt == 1 {
				original = seat
			} else if !sameSeatEntry(original, seat) {
				return store.StagePreferences{}, fmt.Errorf("review seat %d changed in another place while this command ran. Nothing was saved. Run conveyor preferences show, then run the command again", target.seat)
			}
		}
		next := store.CloneStagePreferenceStages(current.Stages)
		if err := edit(&next); err != nil {
			return store.StagePreferences{}, err
		}
		body, err := json.Marshal(map[string]any{"stages": next})
		if err != nil {
			return store.StagePreferences{}, err
		}
		var saved store.StagePreferences
		err = c.doHeaders(http.MethodPut, stagePreferencesPath, body, &saved, map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(current.Version, 10))})
		var status *apiStatusError
		if errors.As(err, &status) && status.Status == http.StatusPreconditionFailed {
			continue
		}
		if err != nil {
			return store.StagePreferences{}, stagePreferenceRequestError(err)
		}
		return saved, nil
	}
	return store.StagePreferences{}, fmt.Errorf("your stage preferences changed %d times while this command ran; nothing was saved, so run the command again", maxPreferenceAttempts)
}

// reviewSeatAt returns a copy of review seat n (1-based), or nil when that
// seat has no entry.
func reviewSeatAt(stages store.StagePreferenceStages, n int) *store.StagePreferenceEntry {
	if stages.Review == nil || n < 1 || n > len(stages.Review.Seats) {
		return nil
	}
	entry := stages.Review.Seats[n-1]
	return &entry
}

func sameSeatEntry(left, right *store.StagePreferenceEntry) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// stagePreferenceRequestError turns a 422 body into "field: message" and
// leaves every other failure as the server reported it.
func stagePreferenceRequestError(err error) error {
	var status *apiStatusError
	if !errors.As(err, &status) {
		return err
	}
	switch status.Status {
	case http.StatusNotFound:
		return fmt.Errorf("this server does not keep stage preferences: %w", err)
	case http.StatusUnprocessableEntity:
		message := status.Error()
		if start := strings.Index(message, "{"); start >= 0 {
			var body struct {
				Fields []struct {
					Field    string `json:"field"`
					Message  string `json:"message"`
					Position int    `json:"position"`
				} `json:"fields"`
			}
			if json.Unmarshal([]byte(message[start:]), &body) == nil && len(body.Fields) > 0 {
				field := body.Fields[0]
				if field.Position > 0 {
					return fmt.Errorf("stage preferences refused: %s (review seat %d) %s", field.Field, field.Position, field.Message)
				}
				return fmt.Errorf("stage preferences refused: %s %s", field.Field, field.Message)
			}
		}
	}
	return err
}

// printStagePreferences prints the whole set, one stage per line, or as JSON.
func printStagePreferences(out io.Writer, set store.StagePreferences, asJSON bool) error {
	if asJSON {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(set)
	}
	if set.Version == 0 {
		fmt.Fprintln(out, "Stage preferences: never saved. They apply in every workspace.")
	} else {
		updated := ""
		if set.UpdatedAt != nil {
			updated = ", updated " + set.UpdatedAt.UTC().Format("2006-01-02 15:04:05 UTC")
		}
		fmt.Fprintf(out, "Stage preferences, version %d%s. They apply in every workspace.\n", set.Version, updated)
	}
	line := func(label, value string) { fmt.Fprintf(out, "  %-10s %s\n", label, value) }
	line("plan", describePreferenceEntry(set.Stages.Plan))
	line("implement", describePreferenceEntry(set.Stages.Implement))
	line("verify", describePreferenceEntry(set.Stages.Verify))
	line("risk", describePreferenceEntry(set.Stages.Risk))
	review := set.Stages.Review
	if review == nil {
		line("review", "not set")
		return nil
	}
	line("review", "primary: "+describePreferenceEntry(review.Primary))
	for i := range review.Seats {
		line("", fmt.Sprintf("seat %d: %s", i+1, describePreferenceEntry(&review.Seats[i])))
	}
	if len(review.Seats) < stageprefs.MaxReviewSeats {
		line("", "A seat without its own entry uses the primary.")
	}
	return nil
}

func describePreferenceEntry(entry *store.StagePreferenceEntry) string {
	if entry == nil {
		return "not set"
	}
	text := entry.Harness + ", model " + entry.Model
	if entry.Effort != "" {
		text += ", effort " + entry.Effort
	}
	return text
}
