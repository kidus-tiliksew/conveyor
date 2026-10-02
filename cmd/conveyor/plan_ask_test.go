package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const planSixQuestionRound = `round: 1
questions:
  - id: Q1
    prompt: Which storage backs the queue?
    options: [Postgres, SQLite]
    recommended: Postgres
  - id: Q2
    prompt: Which retry strategy?
    options: [Exponential, Fixed]
    recommended: Exponential
  - id: Q3
    prompt: Where do drafts live?
    options: [Git-ignored folder, Committed folder]
    recommended: Git-ignored folder
  - id: Q4
    prompt: Who confirms each layer?
    options: [The operator, The agent]
    recommended: The operator
  - id: Q5
    prompt: When does the hook run?
    options: [Before the requirements layer, After every layer]
    recommended: Before the requirements layer
  - id: Q6
    prompt: Which base branch?
    options: [main]
    recommended: main
`

func planAskAnswersFileExists(t *testing.T, dir string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, planAskAnswersFile))
	return err == nil
}

func TestPlanAskWritesOneAnswerPerQuestion(t *testing.T) {
	dir := t.TempDir()
	planWriteFile(t, filepath.Join(dir, planAskRoundFile), planSixQuestionRound)

	pageURL, wait := startPlanPageCommand(t, planAskCmd(), dir, "--port", "0")
	if status, body := planGetPage(t, pageURL); status != http.StatusOK {
		t.Fatalf("render status = %d: %s", status, body)
	} else {
		for _, id := range []string{"Q1", "Q2", "Q3", "Q4", "Q5", "Q6"} {
			if !strings.Contains(body, id) {
				t.Fatalf("rendered page is missing question %s", id)
			}
		}
	}

	values := url.Values{}
	for i := range 5 {
		values.Set(fmt.Sprintf("answer.%d", i), "Postgres")
	}
	values.Set("answer_free.5", "my own answer")
	status, body := planPostForm(t, pageURL, values)
	if status != http.StatusOK {
		t.Fatalf("submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("ask command: %v", err)
	}

	file, err := readPlanAnswers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if file.Round != 1 {
		t.Fatalf("round = %d, want 1", file.Round)
	}
	if len(file.Answers) != 6 {
		t.Fatalf("answers = %d, want one per question (6)", len(file.Answers))
	}
	if file.Answers[0].ID != "Q1" || file.Answers[0].Answer != "Postgres" || file.Answers[0].Option != "Postgres" {
		t.Fatalf("Q1 answer = %+v", file.Answers[0])
	}
	if file.Answers[5].ID != "Q6" || file.Answers[5].Answer != "my own answer" || file.Answers[5].Option != "" {
		t.Fatalf("Q6 free-text answer = %+v", file.Answers[5])
	}
}

func TestPlanAskRequiresEveryAnswer(t *testing.T) {
	dir := t.TempDir()
	planWriteFile(t, filepath.Join(dir, planAskRoundFile), planSixQuestionRound)

	pageURL, wait := startPlanPageCommand(t, planAskCmd(), dir, "--port", "0")
	values := url.Values{}
	for i := range 5 {
		values.Set(fmt.Sprintf("answer.%d", i), "Postgres")
	}
	status, body := planPostForm(t, pageURL, values)
	if status != http.StatusBadRequest {
		t.Fatalf("missing-answer status = %d, want 400: %s", status, body)
	}
	if planAskAnswersFileExists(t, dir) {
		t.Fatal("a partial submit must not write answers.json")
	}

	values.Set("answer.5", "main")
	if status, body := planPostForm(t, pageURL, values); status != http.StatusOK {
		t.Fatalf("complete submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("ask command: %v", err)
	}
	file, err := readPlanAnswers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Answers) != 6 {
		t.Fatalf("answers = %d, want 6", len(file.Answers))
	}
}

func TestPlanAskRerenderPrefillsPreviousAnswers(t *testing.T) {
	dir := t.TempDir()
	planWriteFile(t, filepath.Join(dir, planAskRoundFile), planSixQuestionRound)
	if err := writePlanAnswers(dir, &planAnswersFile{Round: 1, Answers: []planAnswer{{ID: "Q1", Question: "Which storage backs the queue?", Answer: "SQLite"}}}); err != nil {
		t.Fatal(err)
	}
	pageURL, wait := startPlanPageCommand(t, planAskCmd(), dir, "--port", "0")
	status, body := planGetPage(t, pageURL)
	if status != http.StatusOK {
		t.Fatalf("render status = %d", status)
	}
	if !strings.Contains(body, `value="SQLite"`) {
		t.Fatal("re-render did not prefill the previous answer")
	}
	values := url.Values{}
	for i := range 6 {
		values.Set(fmt.Sprintf("answer.%d", i), "A")
	}
	if status, body := planPostForm(t, pageURL, values); status != http.StatusOK {
		t.Fatalf("submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("ask command: %v", err)
	}
}

func TestPlanAskLoadRoundRejectsBadRounds(t *testing.T) {
	cases := map[string]string{
		"empty questions": `round: 1
questions: []
`,
		"duplicate id": `round: 1
questions:
  - id: Q1
    prompt: One?
  - id: Q1
    prompt: Two?
`,
		"missing prompt": `round: 1
questions:
  - id: Q1
`,
		"unknown field": `round: 1
questions:
  - id: Q1
    prompt: One?
    surprise: true
`,
		"recommended not an option": `round: 1
questions:
  - id: Q1
    prompt: One?
    options: [A, B]
    recommended: C
`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			planWriteFile(t, filepath.Join(dir, planAskRoundFile), content)
			if _, err := loadPlanRound(dir); err == nil {
				t.Fatalf("%s: expected an error", name)
			}
		})
	}
}

func TestPlanAskAbsentRoundFileFails(t *testing.T) {
	if _, err := loadPlanRound(t.TempDir()); err == nil {
		t.Fatal("absent round.yml must fail")
	}
}

func TestPlanAskRerenderReselectsOptionAndRecordsChange(t *testing.T) {
	dir := t.TempDir()
	planWriteFile(t, filepath.Join(dir, planAskRoundFile), planSixQuestionRound)
	// Round 1 answered Q1 by choosing the SQLite option.
	if err := writePlanAnswers(dir, &planAnswersFile{Round: 1, Answers: []planAnswer{{ID: "Q1", Question: "Which storage backs the queue?", Option: "SQLite", Answer: "SQLite"}}}); err != nil {
		t.Fatal(err)
	}

	pageURL, wait := startPlanPageCommand(t, planAskCmd(), dir, "--port", "0")
	status, body := planGetPage(t, pageURL)
	if status != http.StatusOK {
		t.Fatalf("render status = %d", status)
	}
	if !strings.Contains(body, `value="SQLite" checked`) {
		t.Fatal("re-render did not re-select the previously chosen option")
	}
	if strings.Contains(body, `name="answer_free.0" value="SQLite"`) {
		t.Fatal("a previously chosen option must not populate the free-text box")
	}

	values := url.Values{}
	for i := range 6 {
		values.Set(fmt.Sprintf("answer.%d", i), "Postgres")
	}
	if status, body := planPostForm(t, pageURL, values); status != http.StatusOK {
		t.Fatalf("submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("ask command: %v", err)
	}
	file, err := readPlanAnswers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if file.Answers[0].Option != "Postgres" || file.Answers[0].Answer != "Postgres" {
		t.Fatalf("changed selection = %+v, want option Postgres", file.Answers[0])
	}
}

func TestPlanAskOptionWinsOverFreeTextWhenBothPresent(t *testing.T) {
	dir := t.TempDir()
	planWriteFile(t, filepath.Join(dir, planAskRoundFile), planSixQuestionRound)

	pageURL, wait := startPlanPageCommand(t, planAskCmd(), dir, "--port", "0")
	values := url.Values{}
	for i := range 6 {
		values.Set(fmt.Sprintf("answer.%d", i), "Postgres")
		values.Set(fmt.Sprintf("answer_free.%d", i), "typed")
	}
	if status, body := planPostForm(t, pageURL, values); status != http.StatusOK {
		t.Fatalf("submit status = %d: %s", status, body)
	}
	if err := wait(); err != nil {
		t.Fatalf("ask command: %v", err)
	}
	file, err := readPlanAnswers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if file.Answers[0].Option != "Postgres" || file.Answers[0].Answer != "Postgres" {
		t.Fatalf("answer with both fields set = %+v, want the selected option", file.Answers[0])
	}
}
