package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// planReviewForm builds the whole-page review form for a draft. verdict and
// comment are called per item.
func planReviewForm(t *testing.T, dir string, verdict, comment func(planItem) string) url.Values {
	t.Helper()
	draft, err := loadPlanDraft(dir)
	if err != nil && draft == nil {
		t.Fatalf("load draft: %v", err)
	}
	values := url.Values{}
	for i, item := range draft.Items {
		values.Set(fmt.Sprintf("item.%d.id", i), item.ID)
		values.Set(fmt.Sprintf("item.%d.hash", i), item.Hash())
		values.Set(fmt.Sprintf("item.%d.verdict", i), verdict(item))
		values.Set(fmt.Sprintf("item.%d.comment", i), comment(item))
	}
	return values
}

func planApproveAll(planItem) string { return "approve" }
func planNoComment(planItem) string  { return "" }

func TestPlanReviewWritesOneEntryPerItem(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	dir := filepath.Join(root, "draft")

	pageURL, wait := startPlanPageCommand(t, planReviewCmd(), dir, "--port", "0")
	status, body := planPostForm(t, pageURL, planReviewForm(t, dir, planApproveAll, planNoComment))
	if status != http.StatusOK {
		t.Fatalf("submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("review command: %v", err)
	}

	draft, err := loadPlanDraft(dir)
	if err != nil {
		t.Fatalf("reload draft: %v", err)
	}
	review, err := readPlanReview(dir)
	if err != nil {
		t.Fatalf("read review: %v", err)
	}
	if review.Round != 1 {
		t.Fatalf("round = %d, want 1", review.Round)
	}
	if len(review.Items) != len(draft.Items) {
		t.Fatalf("entries = %d, want one per item (%d)", len(review.Items), len(draft.Items))
	}
	for i, entry := range review.Items {
		item := draft.Items[i]
		if entry.ID != item.ID || entry.File != item.File || entry.Hash != item.Hash() {
			t.Fatalf("entry %d = %+v, want id/file/hash of %+v", i, entry, item)
		}
		if entry.Verdict != "approve" {
			t.Fatalf("entry %s verdict = %q, want approve", entry.ID, entry.Verdict)
		}
		if entry.Normative != item.Normative {
			t.Fatalf("entry %s stored a different normative text", entry.ID)
		}
	}
}

func TestPlanReviewNormativeEditResetsOnlyThatItem(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	dir := filepath.Join(root, "draft")

	pageURL, wait := startPlanPageCommand(t, planReviewCmd(), dir, "--port", "0")
	if status, body := planPostForm(t, pageURL, planReviewForm(t, dir, planApproveAll, planNoComment)); status != http.StatusOK {
		t.Fatalf("submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("first review: %v", err)
	}

	tasksPath := filepath.Join(dir, "tasks.yml")
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), "title: Implement alpha", "title: Implement alpha v2", 1)
	planWriteFile(t, tasksPath, edited)

	pageURL, wait = startPlanPageCommand(t, planReviewCmd(), dir, "--port", "0")
	status, body := planGetPage(t, pageURL)
	if status != http.StatusOK {
		t.Fatalf("render status = %d", status)
	}
	cards := parsePlanRenderedCards(t, body)
	if len(cards) == 0 {
		t.Fatal("no rendered cards found")
	}
	for id, card := range cards {
		if id == "T1" {
			if card.Verdict != "pending" || !card.Changed {
				t.Fatalf("edited item T1 = %+v, want pending and changed", card)
			}
			continue
		}
		if card.Verdict != "approve" || card.Changed {
			t.Fatalf("untouched item %s = %+v, want unchanged approve", id, card)
		}
	}
	if !strings.Contains(body, "Implement alpha\nbody:") {
		t.Fatal("changed card did not show the previously approved text")
	}
	if status, body := planPostForm(t, pageURL, planReviewForm(t, dir, planApproveAll, planNoComment)); status != http.StatusOK {
		t.Fatalf("final submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("second review: %v", err)
	}
}

func TestPlanReviewNotesEditResetsNothing(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	dir := filepath.Join(root, "draft")

	pageURL, wait := startPlanPageCommand(t, planReviewCmd(), dir, "--port", "0")
	if status, body := planPostForm(t, pageURL, planReviewForm(t, dir, planApproveAll, planNoComment)); status != http.StatusOK {
		t.Fatalf("submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("first review: %v", err)
	}

	notesPath := filepath.Join(dir, "notes.yml")
	data, err := os.ReadFile(notesPath)
	if err != nil {
		t.Fatal(err)
	}
	planWriteFile(t, notesPath, strings.Replace(string(data), "headline: Alpha", "headline: Alpha capability", 1))

	pageURL, wait = startPlanPageCommand(t, planReviewCmd(), dir, "--port", "0")
	status, body := planGetPage(t, pageURL)
	if status != http.StatusOK {
		t.Fatalf("render status = %d", status)
	}
	cards := parsePlanRenderedCards(t, body)
	if len(cards) == 0 {
		t.Fatal("no rendered cards found")
	}
	for id, card := range cards {
		if card.Verdict != "approve" || card.Changed {
			t.Fatalf("notes edit reset %s = %+v", id, card)
		}
	}
	if !strings.Contains(body, "Alpha capability") {
		t.Fatal("the new headline was not rendered")
	}
	if status, body := planPostForm(t, pageURL, planReviewForm(t, dir, planApproveAll, planNoComment)); status != http.StatusOK {
		t.Fatalf("final submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("second review: %v", err)
	}
}

func TestPlanReviewRequestChangeCarriesItemAndFile(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	dir := filepath.Join(root, "draft")

	pageURL, wait := startPlanPageCommand(t, planReviewCmd(), dir, "--port", "0")
	values := planReviewForm(t, dir, func(item planItem) string {
		if item.ID == "T1" {
			return "request_change"
		}
		return "approve"
	}, func(item planItem) string {
		if item.ID == "T1" {
			return "split this task"
		}
		return ""
	})
	if status, body := planPostForm(t, pageURL, values); status != http.StatusOK {
		t.Fatalf("submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("review command: %v", err)
	}

	review, err := readPlanReview(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found *planReviewEntry
	for i := range review.Items {
		if review.Items[i].ID == "T1" {
			found = &review.Items[i]
		}
	}
	if found == nil {
		t.Fatal("no entry for T1")
	}
	if found.Verdict != "request_change" || found.Comment != "split this task" || found.File != "tasks.yml" {
		t.Fatalf("T1 entry = %+v, want request_change with comment and file tasks.yml", *found)
	}
}

func TestPlanReviewRoundIncrementsPerSubmit(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	dir := filepath.Join(root, "draft")

	for want := 1; want <= 2; want++ {
		pageURL, wait := startPlanPageCommand(t, planReviewCmd(), dir, "--port", "0")
		if status, body := planPostForm(t, pageURL, planReviewForm(t, dir, planApproveAll, planNoComment)); status != http.StatusOK {
			t.Fatalf("submit %d status = %d: %s", want, status, body)
		}
		if err := wait(); err != nil {
			t.Fatalf("review %d: %v", want, err)
		}
		review, err := readPlanReview(dir)
		if err != nil {
			t.Fatal(err)
		}
		if review.Round != want {
			t.Fatalf("round = %d, want %d", review.Round, want)
		}
	}
}

func TestPlanReviewLinkedChangeKeepsApproval(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	dir := filepath.Join(root, "draft")

	pageURL, wait := startPlanPageCommand(t, planReviewCmd(), dir, "--port", "0")
	if status, body := planPostForm(t, pageURL, planReviewForm(t, dir, planApproveAll, planNoComment)); status != http.StatusOK {
		t.Fatalf("submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("first review: %v", err)
	}

	requirementPath := filepath.Join(dir, "requirements/req-alpha.md")
	data, err := os.ReadFile(requirementPath)
	if err != nil {
		t.Fatal(err)
	}
	planWriteFile(t, requirementPath, strings.Replace(string(data), "The system shall alpha.", "The system shall alpha now.", 1))

	pageURL, wait = startPlanPageCommand(t, planReviewCmd(), dir, "--port", "0")
	status, body := planGetPage(t, pageURL)
	if status != http.StatusOK {
		t.Fatalf("render status = %d", status)
	}
	cards := parsePlanRenderedCards(t, body)
	if got := cards["req-alpha/REQ-1"]; got.Verdict != "pending" || !got.Changed {
		t.Fatalf("edited requirement = %+v, want pending and changed", got)
	}
	if got := cards["D1"]; got.Verdict != "approve" || got.Changed {
		t.Fatalf("decision citing the edited requirement = %+v, want approval kept", got)
	}
	if !strings.Contains(body, "linked item changed") {
		t.Fatal("the approved decision did not show the linked-item-changed badge")
	}
	if status, body := planPostForm(t, pageURL, planReviewForm(t, dir, planApproveAll, planNoComment)); status != http.StatusOK {
		t.Fatalf("final submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("second review: %v", err)
	}
}

func TestPlanReviewInvalidPortFailsBeforeServing(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	if err := runPlanReview(t.Context(), filepath.Join(root, "draft"), -1, io.Discard); err == nil {
		t.Fatal("negative port must fail")
	}
}

// planRenderedHashes parses each review card's data-id/data-hash attributes.
var planRenderedHashes = regexp.MustCompile(`data-id="([^"]+)"[^>]*data-hash="([^"]+)"`)

func planRenderedHash(t *testing.T, body, id string) string {
	t.Helper()
	for _, match := range planRenderedHashes.FindAllStringSubmatch(body, -1) {
		if match[1] == id {
			return match[2]
		}
	}
	t.Fatalf("no rendered hash for %s", id)
	return ""
}

func TestPlanReviewStaleHashRecordsPending(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	dir := filepath.Join(root, "draft")

	pageURL, wait := startPlanPageCommand(t, planReviewCmd(), dir, "--port", "0")
	status, body := planGetPage(t, pageURL)
	if status != http.StatusOK {
		t.Fatalf("render status = %d", status)
	}
	staleHash := planRenderedHash(t, body, "T1")

	// The draft changes after the page was rendered, so the hash the owner
	// approved no longer matches the item on disk.
	tasksPath := filepath.Join(dir, "tasks.yml")
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	planWriteFile(t, tasksPath, strings.Replace(string(data), "title: Implement alpha", "title: Implement alpha v2", 1))

	values := planReviewForm(t, dir, planApproveAll, planNoComment)
	draft, err := loadPlanDraft(dir)
	if err != nil && draft == nil {
		t.Fatalf("reload draft: %v", err)
	}
	for i, item := range draft.Items {
		if item.ID == "T1" {
			values.Set(fmt.Sprintf("item.%d.hash", i), staleHash)
		}
	}
	if status, body := planPostForm(t, pageURL, values); status != http.StatusOK {
		t.Fatalf("submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("review command: %v", err)
	}

	review, err := readPlanReview(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range review.Items {
		if entry.ID != "T1" {
			continue
		}
		if entry.Verdict != "pending" {
			t.Fatalf("stale-hash item T1 verdict = %q, want pending", entry.Verdict)
		}
		return
	}
	t.Fatal("no entry for T1")
}
