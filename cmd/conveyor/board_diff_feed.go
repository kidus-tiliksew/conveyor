package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

const boardDiffActivityLimit = 200
const boardDiffMaxChanged = 1000

type boardDiffActivity struct {
	Task struct {
		ID    string         `json:"id"`
		Title string         `json:"title"`
		State core.TaskState `json:"state"`
	} `json:"task"`
	LatestStage core.Stage          `json:"latest_stage"`
	LastEventAt time.Time           `json:"last_event_at"`
	NextAction  *nextaction.Compact `json:"next_action"`
}

type boardDiffFeed struct {
	Items       []boardDiffActivity
	Position    time.Time
	ResetReason string
}

func (c *client) readBoardDiffFeed(position time.Time) (boardDiffFeed, error) {
	var result boardDiffFeed
	query := url.Values{"updated_from": {position.Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)}, "limit": {strconv.Itoa(boardDiffActivityLimit)}}
	for offset := 0; ; {
		query.Set("offset", strconv.Itoa(offset))
		body, headers, err := c.getRawWithHeader("/v1/activity?" + query.Encode())
		if err != nil {
			return boardDiffFeed{}, err
		}
		if offset == 0 {
			result.Position, err = http.ParseTime(headers.Get("Date"))
			if err != nil {
				return boardDiffFeed{}, fmt.Errorf("invalid activity Date header: %w", err)
			}
			result.Position = result.Position.UTC()
		}
		total, err := strconv.Atoi(headers.Get("X-Conveyor-Total"))
		if err != nil || total < 0 {
			return boardDiffFeed{}, fmt.Errorf("invalid activity X-Conveyor-Total %q", headers.Get("X-Conveyor-Total"))
		}
		if total > boardDiffMaxChanged {
			result.ResetReason = "more than 1000 changed tasks"
			return result, nil
		}
		var page []boardDiffActivity
		if err := json.Unmarshal(body, &page); err != nil {
			return boardDiffFeed{}, fmt.Errorf("decode activity: %w", err)
		}
		result.Items = append(result.Items, page...)
		offset += len(page)
		if offset >= total {
			return result, nil
		}
		if len(page) == 0 {
			return boardDiffFeed{}, fmt.Errorf("activity page ended before total")
		}
	}
}
