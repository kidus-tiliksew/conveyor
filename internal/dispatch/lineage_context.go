package dispatch

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/lineagecontext"
)

type lineageContextMemoKey struct{}

// The memo is installed for one synchronous dispatch call and is never shared
// with worker goroutines; buildStageInput and its summary reads are serial.
type lineageContextMemoEntry struct {
	result lineagecontext.Result
	err    error
}

func (d *Dispatcher) lineageContext(ctx context.Context, cfg *config.Config, taskID string) (result lineagecontext.Result, resultErr error) {
	if memo, ok := ctx.Value(lineageContextMemoKey{}).(map[string]lineageContextMemoEntry); ok {
		if cached, exists := memo[taskID]; exists {
			return cached.result, cached.err
		}
		defer func() { memo[taskID] = lineageContextMemoEntry{result: result, err: resultErr} }()
	}
	return lineagecontext.Assemble(ctx, d.Store, cfg, []core.LineageNode{{Type: core.LineageTask, ID: taskID}}, taskID, false)
}
