package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// planPageURL matches the served page's URL in the command's stdout, including
// the anti-CSRF token the command prints in the query.
var planPageURL = regexp.MustCompile(`http://127\.0\.0\.1:\d+/\?t=[0-9a-f]+`)

// startPlanPageCommand runs a plan page command in the background and returns
// the URL it printed plus a wait function that returns the command's error. The
// page has no other stdout, so reading one line is enough.
func startPlanPageCommand(t *testing.T, command *cobra.Command, args ...string) (string, func() error) {
	t.Helper()
	reader, writer := io.Pipe()
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(writer)
	command.SetErr(writer)
	command.SetArgs(args)
	done := make(chan error, 1)
	go func() { done <- command.Execute() }()
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		t.Fatalf("read plan page URL: %v", err)
	}
	found := planPageURL.FindString(line)
	if found == "" {
		t.Fatalf("plan page printed no URL: %q", line)
	}
	wait := func() error {
		execErr := <-done
		_ = writer.Close()
		return execErr
	}
	return found, wait
}

// planSubmitTarget splits the printed page URL into the submit URL and the
// page token a legitimate submit must carry.
func planSubmitTarget(t *testing.T, pageURL string) (string, string) {
	t.Helper()
	parsed, err := url.Parse(pageURL)
	if err != nil {
		t.Fatalf("parse page URL %q: %v", pageURL, err)
	}
	return "http://" + parsed.Host + "/submit", parsed.Query().Get("t")
}

// planPostForm posts a form to the page and returns the response body and
// status; it fails the test on a transport error. It adds the page token the
// printed URL carries, so every submit is a legitimate same-origin submit.
func planPostForm(t *testing.T, pageURL string, values url.Values) (int, string) {
	t.Helper()
	submitURL, token := planSubmitTarget(t, pageURL)
	values.Set("token", token)
	response, err := http.PostForm(submitURL, values)
	if err != nil {
		t.Fatalf("post form: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response.StatusCode, string(body)
}

// planGetPage fetches the index page and returns its status and body.
func planGetPage(t *testing.T, pageURL string) (int, string) {
	t.Helper()
	response, err := http.Get(pageURL)
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	return response.StatusCode, string(body)
}

// planCardState is one rendered review card's machine-readable state.
type planCardState struct {
	Verdict string
	Changed bool
}

// planRenderedCards parses each review card's data-id/data-verdict (and
// data-changed) attributes from a rendered page.
var planRenderedCards = regexp.MustCompile(`data-id="([^"]+)"[^>]*data-verdict="([^"]+)"[^>]*data-changed="([^"]+)"`)

func parsePlanRenderedCards(t *testing.T, body string) map[string]planCardState {
	t.Helper()
	cards := make(map[string]planCardState)
	for _, match := range planRenderedCards.FindAllStringSubmatch(body, -1) {
		cards[match[1]] = planCardState{Verdict: match[2], Changed: match[3] == "true"}
	}
	return cards
}

// planFullAskValues answers every question of planSixQuestionRound.
func planFullAskValues() url.Values {
	values := url.Values{}
	for i := range 6 {
		values.Set(fmt.Sprintf("answer.%d", i), "Postgres")
	}
	return values
}

// planRawPost posts values with the raw control over headers a forged request
// needs; a "Host" entry overrides the request Host instead of the header.
func planRawPost(t *testing.T, submitURL string, values url.Values, headers map[string]string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, submitURL, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for name, value := range headers {
		if name == "Host" {
			request.Host = value
			continue
		}
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post raw: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response.StatusCode, string(body)
}

func TestPlanPageRejectsForgedSubmits(t *testing.T) {
	dir := t.TempDir()
	planWriteFile(t, filepath.Join(dir, planAskRoundFile), planSixQuestionRound)

	pageURL, wait := startPlanPageCommand(t, planAskCmd(), dir, "--port", "0")
	submitURL, token := planSubmitTarget(t, pageURL)

	// A POST without the token is rejected and writes nothing.
	if status, body := planRawPost(t, submitURL, planFullAskValues(), nil); status != http.StatusForbidden {
		t.Fatalf("tokenless submit status = %d, want 403: %s", status, body)
	}
	// A wrong token is rejected too.
	wrong := planFullAskValues()
	wrong.Set("token", strings.Repeat("0", len(token)))
	if status, body := planRawPost(t, submitURL, wrong, nil); status != http.StatusForbidden {
		t.Fatalf("wrong-token submit status = %d, want 403: %s", status, body)
	}
	// A cross-site Origin is rejected even with the right token.
	legit := planFullAskValues()
	legit.Set("token", token)
	if status, body := planRawPost(t, submitURL, legit, map[string]string{"Origin": "http://evil.example"}); status != http.StatusForbidden {
		t.Fatalf("cross-origin submit status = %d, want 403: %s", status, body)
	}
	// A rebound Host is rejected.
	if status, body := planRawPost(t, submitURL, legit, map[string]string{"Host": "evil.example"}); status != http.StatusForbidden {
		t.Fatalf("rebound-host submit status = %d, want 403: %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(dir, planAskAnswersFile)); !os.IsNotExist(err) {
		t.Fatalf("a forged submit wrote %s (stat err = %v)", planAskAnswersFile, err)
	}
	// The rejections did not consume the one accepted submit.
	if status, body := planPostForm(t, pageURL, planFullAskValues()); status != http.StatusOK {
		t.Fatalf("legitimate submit after forged ones status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("ask command: %v", err)
	}
}

func TestPlanPageConcurrentSubmitsRecordOnce(t *testing.T) {
	root := planFixtureRepo(t, planValidDraftFiles())
	dir := filepath.Join(root, "draft")

	pageURL, wait := startPlanPageCommand(t, planReviewCmd(), dir, "--port", "0")
	submitURL, token := planSubmitTarget(t, pageURL)
	values := planReviewForm(t, dir, planApproveAll, planNoComment)
	values.Set("token", token)

	const submissions = 8
	statuses := make([]int, submissions)
	var group sync.WaitGroup
	for i := range submissions {
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := http.PostForm(submitURL, values)
			if err != nil {
				statuses[i] = -1
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			statuses[i] = response.StatusCode
		}()
	}
	group.Wait()

	// The first accepted submit shuts the server down, so a late request
	// may fail to connect instead of receiving 409; both mean "not recorded".
	accepted := 0
	for i, status := range statuses {
		switch status {
		case http.StatusOK:
			accepted++
		case http.StatusConflict, -1:
		default:
			t.Fatalf("submit %d status = %d, want 200, 409, or a refused connection", i, status)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted submits = %d, want exactly 1", accepted)
	}
	if err := wait(); err != nil {
		t.Fatalf("review command: %v", err)
	}
	review, err := readPlanReview(dir)
	if err != nil {
		t.Fatalf("read review: %v", err)
	}
	if review.Round != 1 {
		t.Fatalf("round = %d, want the single accepted submit", review.Round)
	}
}
