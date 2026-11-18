package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/spf13/cobra"
)

type boardDiffOptions struct {
	asJSON, reset bool
	since         string
	// consumer names the snapshot owner. consumerSet tells an explicit empty
	// --consumer, a usage error, from an omitted flag.
	consumer    string
	consumerSet bool
}
type boardDiffSummary struct {
	Open      int                        `json:"open"`
	States    map[string]int             `json:"states"`
	Stages    map[string]int             `json:"stages"`
	Decisions []boardDiffSummaryDecision `json:"decisions"`
}
type boardDiffSummaryDecision struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Label string `json:"label"`
}
type boardDiffOutput struct {
	Position    string                `json:"position"`
	FirstRun    bool                  `json:"first_run,omitempty"`
	ResetReason string                `json:"reset_reason,omitempty"`
	Changes     []boardDiffChange     `json:"changes"`
	Incomplete  []boardDiffIncomplete `json:"incomplete,omitempty"`
	Summary     *boardDiffSummary     `json:"summary,omitempty"`
}

func boardDiffCmd() *cobra.Command {
	var options boardDiffOptions
	cmd := &cobra.Command{Use: "diff", Short: "Show board changes since the last check", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		options.consumerSet = cmd.Flags().Changed("consumer")
		return runBoardDiff(newClient(), cmd.OutOrStdout(), options)
	}}
	cmd.Flags().BoolVar(&options.asJSON, "json", false, "print changes as JSON")
	cmd.Flags().StringVar(&options.since, "since", "", "read changes since a position without saving")
	cmd.Flags().BoolVar(&options.reset, "reset", false, "replace the saved snapshot")
	cmd.Flags().StringVar(&options.consumer, "consumer", "", "use the snapshot of this consumer name")
	cmd.MarkFlagsMutuallyExclusive("since", "reset")
	return cmd
}

func runBoardDiff(c *client, w io.Writer, options boardDiffOptions) error {
	if options.reset && options.since != "" {
		return fmt.Errorf("--since and --reset cannot be used together")
	}
	// Check the consumer name before any request or snapshot access.
	if options.consumerSet && options.consumer == "" {
		return fmt.Errorf("--consumer must not be empty")
	}
	if _, err := boardDiffSnapshotName(options.consumer); err != nil {
		return err
	}
	stateless := options.since != ""
	if !stateless {
		// One run per snapshot: hold the lock from before the read until after
		// the write, a reset included. A --since run takes no lock
		// (component-next-action State and failure).
		unlock, err := lockBoardDiffState(c.base, c.workspace, options.consumer)
		if err != nil {
			return err
		}
		defer unlock()
	}
	now := time.Now().UTC()
	state := boardDiffState{}
	reason := ""
	first := false
	if stateless {
		p, err := time.Parse(time.RFC3339, options.since)
		if err != nil || strings.ContainsAny(options.since, ".,") {
			return fmt.Errorf("--since must be an RFC 3339 position with second precision")
		}
		state.Position = p
	} else if options.reset {
		first = true
		reason = "reset requested"
	} else {
		var err error
		state, reason, err = loadBoardDiffState(c.base, c.workspace, options.consumer, now)
		if err != nil {
			return err
		}
		first = reason != ""
		if reason == "missing snapshot" {
			reason = ""
		}
	}
	var feed boardDiffFeed
	var err error
	if !first {
		feed, err = c.readBoardDiffFeed(state.Position)
		if err != nil {
			return err
		}
		if feed.ResetReason != "" {
			if stateless {
				return fmt.Errorf("%s", feed.ResetReason)
			}
			first = true
			reason = feed.ResetReason
		}
	}
	result := boardDiffOutput{Changes: []boardDiffChange{}}
	if first {
		var summary boardDiffSummary
		state, summary, err = c.seedBoardDiff(now)
		if err != nil {
			return err
		}
		result.FirstRun = true
		result.ResetReason = reason
		result.Summary = &summary
	} else {
		result.Changes, state, result.Incomplete, err = c.diffBoardActivities(state, feed)
		if err != nil {
			return err
		}
		// The server's Date is the position even when a task's history is incomplete.
		state.Position = feed.Position
	}
	result.Position = state.Position.UTC().Format(time.RFC3339)
	var output bytes.Buffer
	if options.asJSON {
		err = json.NewEncoder(&output).Encode(result)
	} else {
		err = writeBoardDiffText(&output, result)
	}
	if err != nil {
		return err
	}
	if _, err = w.Write(output.Bytes()); err != nil {
		return err
	}
	if !stateless {
		state.SavedAt = now
		if err = saveBoardDiffState(c.base, c.workspace, options.consumer, state); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) seedBoardDiff(now time.Time) (boardDiffState, boardDiffSummary, error) {
	state := boardDiffState{Tasks: map[string]boardDiffTask{}, Recent: map[string]time.Time{}}
	summary := boardDiffSummary{States: map[string]int{}, Stages: map[string]int{}, Decisions: []boardDiffSummaryDecision{}}
	feed, err := c.readBoardDiffFeedMode(now, true)
	if err != nil {
		return state, summary, err
	}
	state.Position = feed.Position
	for _, row := range feed.Items {
		if !row.LastEventAt.Before(feed.Position.Add(-2 * time.Minute)) {
			state.Recent[row.Task.ID] = row.LastEventAt
		}
	}
	for offset := 0; ; {
		q := url.Values{"limit": {"100"}, "offset": {strconv.Itoa(offset)}}
		body, _, err := c.getRawWithHeader("/v1/board?" + q.Encode())
		if err != nil {
			return state, summary, err
		}
		var page boardPage
		if err = json.Unmarshal(body, &page); err != nil {
			return state, summary, fmt.Errorf("decode board: %w", err)
		}
		for _, item := range page.Items {
			boardDiffSeedItem(&state, &summary, item)
		}
		offset += len(page.Items)
		if offset >= page.Total {
			break
		}
		if len(page.Items) == 0 {
			return state, summary, fmt.Errorf("board page ended before total")
		}
	}
	return state, summary, nil
}
func boardDiffSeedItem(state *boardDiffState, summary *boardDiffSummary, item store.BoardItem) {
	title := item.Title
	if runes := []rune(title); len(runes) > 80 {
		title = string(runes[:80])
	}
	stage := string(item.Stage)
	if stage == "" {
		stage = "unknown"
	}
	decision := ""
	if item.NextAction.Judgment {
		decision = item.NextAction.Label
		if len(summary.Decisions) < 20 {
			summary.Decisions = append(summary.Decisions, boardDiffSummaryDecision{ID: item.ID, Title: item.Title, Label: decision})
		}
	}
	state.Tasks[item.ID] = boardDiffTask{Title: title, State: string(item.State), Stage: stage, PullRequest: item.PullRequest, Decision: decision}
	summary.Open++
	summary.States[string(item.State)]++
	summary.Stages[stage]++
}

func formatBoardDiffDuration(seconds int64) string {
	duration := (time.Duration(seconds) * time.Second).String()
	if strings.HasSuffix(duration, "m0s") {
		duration = strings.TrimSuffix(duration, "0s")
	}
	return duration
}
func writeBoardDiffText(w io.Writer, out boardDiffOutput) error {
	if out.ResetReason != "" {
		if _, err := fmt.Fprintf(w, "Reset: %s\n", out.ResetReason); err != nil {
			return err
		}
	}
	if out.Summary != nil {
		if _, err := fmt.Fprintf(w, "Open tasks: %d\n", out.Summary.Open); err != nil {
			return err
		}
		for _, group := range []struct {
			name   string
			counts map[string]int
		}{{"State", out.Summary.States}, {"Stage", out.Summary.Stages}} {
			keys := make([]string, 0, len(group.counts))
			for k := range group.counts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if _, err := fmt.Fprintf(w, "%s %s: %d\n", group.name, k, group.counts[k]); err != nil {
					return err
				}
			}
		}
		for _, d := range out.Summary.Decisions {
			if _, err := fmt.Fprintf(w, "%s  %s  %s\n", d.ID, d.Title, d.Label); err != nil {
				return err
			}
		}
	}
	for _, change := range out.Changes {
		if _, err := fmt.Fprintf(w, "%s  %s\n", change.ID, change.Title); err != nil {
			return err
		}
		if change.Created {
			if _, err := fmt.Fprintln(w, "  created"); err != nil {
				return err
			}
		}
		for _, tr := range change.Transitions {
			duration := ""
			if tr.Kind == "stage" {
				if tr.PreviousSeconds == nil {
					duration = "  (unknown duration)"
				} else {
					duration = fmt.Sprintf("  (%s %s)", tr.From, formatBoardDiffDuration(*tr.PreviousSeconds))
				}
			}
			if _, err := fmt.Fprintf(w, "  %s → %s  %s%s\n", tr.From, tr.To, tr.At.Format("15:04"), duration); err != nil {
				return err
			}
		}
		if change.Decision != nil {
			verb := "cleared"
			if change.Decision.Opened {
				verb = "opened"
			}
			if _, err := fmt.Fprintf(w, "  decision %s: %s\n", verb, change.Decision.Label); err != nil {
				return err
			}
		}
		if change.PullRequest != nil {
			if _, err := fmt.Fprintf(w, "  PR #%d opened\n", change.PullRequest.Number); err != nil {
				return err
			}
		}
		if change.Terminal {
			if _, err := fmt.Fprintln(w, "  terminal"); err != nil {
				return err
			}
		}
	}
	for _, item := range out.Incomplete {
		if _, err := fmt.Fprintf(w, "%s: %s\n", item.ID, item.Reason); err != nil {
			return err
		}
	}
	return nil
}
