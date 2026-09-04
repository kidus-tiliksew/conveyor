package memlog

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/logtest"
)

func TestMemlogConformance(t *testing.T) {
	logtest.Run(t, func(t *testing.T) eventlog.Store { return New() })
}
