package main

import (
	"bufio"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// planPageURL matches the served page's URL in the command's stdout.
var planPageURL = regexp.MustCompile(`http://127\.0\.0\.1:\d+/`)

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

// planPostForm posts a form to the page and returns the response body and
// status; it fails the test on a transport error.
func planPostForm(t *testing.T, pageURL string, values url.Values) (int, string) {
	t.Helper()
	response, err := http.PostForm(strings.TrimRight(pageURL, "/")+"/submit", values)
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
	response, err := http.Get(strings.TrimRight(pageURL, "/") + "/")
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
