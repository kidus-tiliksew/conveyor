package logqueue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/jobindex"
)

// Start from the job index (component-durable-queue; req-quiet-idle-runtime
// AC-5.1). Before its first catch-up a workspace's poll loop repairs the
// index, loads only the unfinished rows and the events of their current
// generation, and tails from the position read before those rows, so
// start-up reads do not grow with finished jobs.

// loadRetryFirst and loadRetryMax bound the delay before a failed catch-up,
// an index load or a tail, is tried again. Tests shorten loadRetryFirst.
var (
	loadRetryFirst = 5 * time.Second
	loadRetryMax   = time.Minute
)

func (rt *Runtime) loaded(ws *workspace) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return ws.loaded
}

// load seeds the workspace's in-memory index from the job index. A log
// without a job index is tailed from position zero instead.
func (rt *Runtime) load(ctx context.Context, ws *workspace) error {
	reader, ok := jobindex.ReaderOf(rt.log)
	if !ok {
		rt.markLoaded(ws, 0, nil)
		return nil
	}
	repaired, err := reader.RepairJobIndex(ctx, ws.id)
	if errors.Is(err, jobindex.ErrUnsupported) {
		rt.markLoaded(ws, 0, nil)
		return nil
	}
	if err != nil {
		return fmt.Errorf("repair job index: %w", err)
	}
	if repaired > 0 {
		rt.opts.Logf("logqueue: workspace %s: rebuilt %d job index rows that did not match their streams", ws.id, repaired)
	}
	position, rows, err := reader.UnfinishedJobs(ctx, ws.id)
	if err != nil {
		return fmt.Errorf("read unfinished jobs: %w", err)
	}
	jobs := make(map[eventlog.StreamID]*Job, len(rows))
	for _, row := range rows {
		job, err := rt.loadRow(ctx, ws, reader, row)
		if err != nil {
			return err
		}
		jobs[row.Stream] = &job
	}
	rt.markLoaded(ws, position, jobs)
	// The loaded jobs reach the workers and the alarm the way a catch-up's
	// folded events do.
	rt.wakeWorkers(ws)
	signal(ws.alarmIndex)
	rt.opts.Logf("logqueue: workspace %s: loaded %d unfinished jobs from the job index at position %d", ws.id, len(rows), position)
	return nil
}

func (rt *Runtime) markLoaded(ws *workspace, position eventlog.Position, jobs map[eventlog.StreamID]*Job) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for stream, job := range jobs {
		if current, ok := ws.jobs[stream]; ok && current.Head >= job.Head {
			continue
		}
		ws.jobs[stream] = job
	}
	if position > ws.position {
		ws.position = position
	}
	ws.loaded = true
}

// loadRow folds a job from its index row and the events of its current
// generation, or from its whole stream when the two disagree.
func (rt *Runtime) loadRow(ctx context.Context, ws *workspace, reader jobindex.Reader, row jobindex.Row) (Job, error) {
	job, ok, err := jobindex.LoadCurrent(ctx, reader, row)
	if err != nil {
		return Job{}, fmt.Errorf("load %s: %w", row.Stream, err)
	}
	if ok {
		return job, nil
	}
	rt.opts.Logf("logqueue: workspace %s: job index row for %s disagrees with its current generation; folding the whole stream", ws.id, row.Stream)
	job, err = Load(ctx, rt.log, ws.id, row.Stream)
	if err != nil {
		return Job{}, fmt.Errorf("load %s: %w", row.Stream, err)
	}
	return job, nil
}

// loadStream folds one stream that is not in memory from its current index
// row, falling back to a full fold for a log without a job index.
func (rt *Runtime) loadStream(ctx context.Context, ws *workspace, stream eventlog.StreamID) (Job, error) {
	if reader, ok := jobindex.ReaderOf(rt.log); ok {
		row, found, err := reader.JobIndexRow(ctx, ws.id, stream)
		switch {
		case err == nil && found:
			return rt.loadRow(ctx, ws, reader, row)
		case err != nil && !errors.Is(err, jobindex.ErrUnsupported):
			return Job{}, err
		}
	}
	return Load(ctx, rt.log, ws.id, stream)
}

// unknownStreams lists the job streams in events that are not in memory
// and whose first event here follows earlier history, such as a finished
// job enqueued again after start. Their generation comes from the index.
func (rt *Runtime) unknownStreams(ws *workspace, events []eventlog.Event) []eventlog.StreamID {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	seen := map[eventlog.StreamID]bool{}
	var out []eventlog.StreamID
	for _, event := range events {
		if event.Stream.Type() != StreamType || seen[event.Stream] {
			continue
		}
		seen[event.Stream] = true
		if _, ok := ws.jobs[event.Stream]; !ok && event.Version > 1 {
			out = append(out, event.Stream)
		}
	}
	return out
}

func nextLoadRetry(previous time.Duration) time.Duration {
	if previous <= 0 {
		return loadRetryFirst
	}
	if previous*2 > loadRetryMax {
		return loadRetryMax
	}
	return previous * 2
}
