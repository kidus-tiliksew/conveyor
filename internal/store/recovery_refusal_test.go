package store

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestWorkOrderRecoveryRefusalMessages(t *testing.T) {
	cases := []struct {
		name  string
		task  core.Task
		order core.WorkOrder
		sibs  []core.WorkOrder
		want  string
	}{
		{
			name:  "terminal",
			task:  core.Task{ID: "t", State: core.TaskClosed, NextStage: core.StageVerify},
			order: core.WorkOrder{ID: "o", TaskID: "t", Stage: core.StageVerify, State: core.WorkOrderStale, HeadSHA: "head"},
			want:  "work order o cannot be recovered after its task reached the terminal task state",
		},
		{
			name:  "verify off",
			task:  core.Task{ID: "t", State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "head"},
			order: core.WorkOrder{ID: "o", TaskID: "t", Stage: core.StageVerify, State: core.WorkOrderStale, HeadSHA: "head"},
			want:  "verify order o cannot be claimed: verify stage is off in task t's frozen policy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := WorkOrderRecoveryRefusal(tc.task, tc.order, tc.sibs)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
}
