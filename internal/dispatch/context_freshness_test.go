package dispatch

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/lineagecontext"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestInProcessContextFreshnessCountsOnlyWholeInputAndRefreshes(t *testing.T) {
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), store.Actor{ID: "dispatcher", Role: core.ActorSystem})
	st := store.NewMemory()
	task := core.Task{ID: "input-task", Workspace: "demo", State: core.TaskRunning}
	if e := st.CreateTask(ctx, task); e != nil {
		t.Fatal(e)
	}
	job := core.Job{ID: "input-job", TaskID: task.ID, State: core.JobRunning, Stage: core.StageReview, Runner: "in-process", StartedAt: time.Now()}
	if e := st.CreateJob(ctx, job); e != nil {
		t.Fatal(e)
	}
	var artifacts []core.Artifact
	for _, body := range []string{"whole", "truncated", "omitted"} {
		a, e := st.CreateArtifact(ctx, core.Artifact{Name: body + ".md", ContentType: "text/markdown", TaskID: task.ID}, []byte(body))
		if e != nil {
			t.Fatal(e)
		}
		artifacts = append(artifacts, a)
	}
	cfg := &config.Config{Workspace: "demo"}
	r, e := lineagecontext.Assemble(ctx, st, cfg, []core.LineageNode{{Type: core.LineageTask, ID: task.ID}}, task.ID, false)
	if e != nil {
		t.Fatal(e)
	}
	input := inprocess.Input{ContextFreshness: store.ProjectContext(r.Snapshot, nil, ""), Attachments: []inprocess.Attachment{{ID: artifacts[0].ID, Content: []byte("whole")}, {ID: artifacts[1].ID, Content: []byte("trunc")}}}
	d := &Dispatcher{Store: st}
	f := d.observeInProcessInput(ctx, task, job, input)
	if !f.ObservationRecorded {
		t.Fatal(f)
	}
	states := map[string]core.ContextDelivery{}
	for _, s := range f.Deliveries {
		states[s.ArtifactID] = s
	}
	if !states[artifacts[0].ID].Fetched || !states[artifacts[1].ID].Truncated || states[artifacts[1].ID].Fetched || !states[artifacts[2].ID].Omitted {
		t.Fatal(states)
	}
	before, _ := st.ListEvents(ctx, task.ID)
	d.observeInProcessInput(ctx, task, job, input)
	after, _ := st.ListEvents(ctx, task.ID)
	if len(before) != len(after) {
		t.Fatal("provider receipt replay duplicated history")
	}
	a, e := st.CreateArtifact(ctx, core.Artifact{Name: "later.md", ContentType: "text/markdown", TaskID: task.ID}, []byte("later"))
	if e != nil {
		t.Fatal(e)
	}
	refreshed := d.refreshInProcessVerdict(ctx, cfg, task, job, input)
	if refreshed.ComparisonStatus != "changed" || refreshed.Additions.Count != 1 || refreshed.Additions.Items[0].ArtifactID != a.ID || refreshed.UnfetchedAdditions != 1 || !refreshed.ObservationRecorded {
		t.Fatal(refreshed)
	}
	if summary := d.priorContextDiagnostic(ctx, task); !strings.Contains(summary, `"unfetched_additions":1`) || !strings.Contains(summary, job.ID) {
		t.Fatal("next input lost prior verdict diagnostic", summary)
	}
}
