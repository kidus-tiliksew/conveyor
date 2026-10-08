package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

// runLegacyVerificationEvidence pins the legacy verification_evidence byte
// policy on every backend: a declared recording is byte-checked through the
// shared MP4/WebM container check, and a refusal writes no artifact row or
// role link. A file name or declared type never establishes the type
// (req-review-gates-evidence AC-8.1; component-artifacts).
func runLegacyVerificationEvidence(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := newAggregateOrder(t, x)
	claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "legacy-evidence", ClientToken: "legacy-evidence-token", ClaimantID: "worker", WorkerID: "worker", Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	create := func(claim bool, name, media string, content []byte) (core.Artifact, error) {
		if claim {
			return st.CreateClaimedVerificationEvidence(ctx, store.ClaimedVerificationEvidenceRequest{WorkOrderID: order.ID, WorkerID: "worker", SessionID: claimed.SessionID, ClientToken: "legacy-evidence-token", Name: name, ContentType: media}, content)
		}
		return st.CreateArtifact(ctx, core.Artifact{Name: name, ContentType: media, Role: core.ArtifactRoleVerificationEvidence, TaskID: order.TaskID}, content)
	}
	links := func() int {
		t.Helper()
		artifacts, err := st.ListArtifacts(ctx)
		requireOK(t, err)
		n := 0
		for _, a := range artifacts {
			if a.TaskID == order.TaskID {
				n++
			}
		}
		return n
	}
	for _, path := range []struct {
		name  string
		claim bool
	}{{"Direct", false}, {"Claimed", true}} {
		t.Run(path.name, func(t *testing.T) {
			seed := "legacy-" + path.name
			for _, tc := range []struct {
				name, media, want string
				content           []byte
			}{
				{"capture.mp4", `VIDEO/MP4; codecs="avc1"`, "video/mp4", testimage.MP4(seed + "-mp4")},
				{"capture.webm", "video/webm", "video/webm", testimage.WebM(seed + "-webm")},
				{"proof.png", "image/png", "image/png", testimage.PNG(seed)},
				{"proof.jpg", "image/jpeg", "image/jpeg", testimage.JPEG(seed)},
			} {
				artifact, err := create(path.claim, tc.name, tc.media, tc.content)
				requireOK(t, err)
				if artifact.ContentType != tc.want || artifact.Role != core.ArtifactRoleVerificationEvidence || artifact.TaskID != order.TaskID || !artifact.EligibleVerificationEvidence() {
					t.Fatalf("%s stored as %+v", tc.name, artifact)
				}
			}
			mp4 := testimage.MP4(seed + "-refused-mp4")
			for _, tc := range []struct {
				name, media string
				content     []byte
			}{
				{"empty.mp4", "video/mp4", nil},
				{"spoofed.mp4", "video/mp4", testimage.PNG(seed + "-spoofed")},
				{"capture.webm", "video/webm", []byte(seed + " plain text named capture.webm")},
				{"truncated.mp4", "video/mp4", mp4[:len(mp4)-3]},
				{"cross.webm", "video/webm", testimage.MP4(seed + "-cross")},
				{"cross.mp4", "video/mp4", testimage.WebM(seed + "-cross")},
				{"capture.gif", "image/gif", testimage.GIF(seed)},
			} {
				before := links()
				if _, err := create(path.claim, tc.name, tc.media, tc.content); err == nil {
					t.Fatalf("%s declared %s was admitted", tc.name, tc.media)
				}
				if after := links(); after != before {
					t.Fatalf("%s refusal wrote %d artifact link(s)", tc.name, after-before)
				}
				if len(tc.content) > 0 {
					if _, _, err := st.GetArtifact(ctx, verificationSHA(tc.content)); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("%s refusal left artifact bytes: %v", tc.name, err)
					}
				}
			}
		})
	}
}
