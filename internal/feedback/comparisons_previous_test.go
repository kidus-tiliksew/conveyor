package feedback

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestPreviousDeployVersion proves the predecessor of one deploy: the latest
// deploy of the service before it with another version, anchored to that
// deploy, so a version that returns after a stop finds the version before
// its return, and PreviousVersion keeps its answer for the newest deploy
// (component-feedback-reports invariant 13).
func TestPreviousDeployVersion(t *testing.T) {
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	deploy := func(id, version string, offset time.Duration, report int64) store.FeedbackDeploy {
		return store.FeedbackDeploy{ID: id, EnvironmentID: "env-1", Service: "api", FeedbackVersion: store.FeedbackVersion{Version: version}, DeployedAt: at.Add(offset), Origin: store.FeedbackOrigin{Report: report}}
	}
	deploys := []store.FeedbackDeploy{
		deploy("d-1", "1", 0, 1),
		deploy("d-2", "2", time.Hour, 2),
		deploy("d-3", "1", 2*time.Hour, 3),
		{ID: "d-web", EnvironmentID: "env-1", Service: "web", FeedbackVersion: store.FeedbackVersion{Version: "9"}, DeployedAt: at.Add(90 * time.Minute), Origin: store.FeedbackOrigin{Report: 4}},
	}
	if _, found := PreviousDeployVersion(deploys, deploys[0]); found {
		t.Fatal("the first deploy has a previous version")
	}
	if previous, found := PreviousDeployVersion(deploys, deploys[1]); !found || previous.Version != "1" || previous.Service != "api" || previous.EnvironmentID != "env-1" {
		t.Fatalf("before d-2=%+v found=%v", previous, found)
	}
	if previous, found := PreviousDeployVersion(deploys, deploys[2]); !found || previous.Version != "2" {
		t.Fatalf("before the return of version 1=%+v found=%v, want version 2", previous, found)
	}
	ref := store.FeedbackVersionRef{EnvironmentID: "env-1", Service: "api", FeedbackVersion: store.FeedbackVersion{Version: "1"}}
	if previous, found := PreviousVersion(deploys, ref); !found || previous.Version != "2" {
		t.Fatalf("PreviousVersion=%+v found=%v, want version 2", previous, found)
	}
}
