package main

// `conveyor plan ask <draft-dir> [--port N]`: the large grill-round form. The
// draft holds round.yml with the round's questions, their options, and the
// recommended option; the page serves them on 127.0.0.1 and blocks until the
// owner submits. One submit writes answers.json with one answer per question
// and the command exits.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	// planAskRoundFile is the draft-relative questions file a grill round is
	// served from.
	planAskRoundFile = "round.yml"
	// planAskAnswersFile is the draft-relative output file one submit writes.
	planAskAnswersFile = "answers.json"
)

// planRound is the parsed round.yml:
//
//	round: 1
//	questions:
//	  - id: Q1
//	    prompt: Which storage backs the queue?
//	    options:
//	      - Postgres
//	      - SQLite
//	    recommended: Postgres
type planRound struct {
	Round     int            `yaml:"round"`
	Questions []planQuestion `yaml:"questions"`
	// Dir is the draft directory, filled after parsing; never read from YAML.
	Dir string `yaml:"-"`
}

// planQuestion is one grill question. Recommended names one of Options.
type planQuestion struct {
	ID          string   `yaml:"id"`
	Prompt      string   `yaml:"prompt"`
	Options     []string `yaml:"options"`
	Recommended string   `yaml:"recommended"`
}

// planAnswersFile is answers.json: one answer per question, written by one
// submit.
type planAnswersFile struct {
	Round   int          `json:"round"`
	Answers []planAnswer `json:"answers"`
}

// planAnswer is one question's answer. Option is the chosen option label when
// the owner picked one; Answer is always the answer text, which is the typed
// free text when "Other" was used.
type planAnswer struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	Option   string `json:"option,omitempty"`
	Answer   string `json:"answer"`
}

// planAskCmd serves the local grill-round form.
func planAskCmd() *cobra.Command {
	port := 0
	command := &cobra.Command{
		Use:   "ask <draft-dir>",
		Short: "Serve the local grill-round form for a planning draft",
		Long: "Serve one local page on 127.0.0.1 and block until the owner submits or closes it. " +
			"The page is built from round.yml; one submit writes answers.json with one answer per question and exits. " +
			"--port 0 picks a free port.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runPlanAsk(ctx, args[0], port, cmd.OutOrStdout())
		},
	}
	command.Flags().IntVar(&port, "port", 0, "local port to serve on (0 picks a free port)")
	return command
}

// planAskPage is the ask server's state.
type planAskPage struct {
	server *planPageServer
	dir    string
}

// runPlanAsk binds the page and blocks until it submits or closes.
func runPlanAsk(ctx context.Context, dir string, port int, stdout io.Writer) error {
	if _, err := loadPlanRound(dir); err != nil {
		return err
	}
	page := &planAskPage{dir: dir}
	server, err := newPlanPageServer(port)
	if err != nil {
		return err
	}
	page.server = server
	mux := http.NewServeMux()
	registerPlanPageRoutes(mux, server, page.handleIndex, page.handleSubmit)
	server.http.Handler = mux
	return runPlanPage(ctx, server, stdout)
}

func (p *planAskPage) handleIndex(w http.ResponseWriter, r *http.Request) {
	round, err := loadPlanRound(p.dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := buildPlanAskView(round, planAskPreviousAnswers(p.dir), "")
	view.Token = p.server.token
	renderPlanPage(w, view)
}

func (p *planAskPage) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !p.server.beginSubmit() {
		http.Error(w, "already submitted", http.StatusConflict)
		return
	}
	recorded := false
	defer func() { p.server.endSubmit(recorded) }()
	round, err := loadPlanRound(p.dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	answers := make([]planAnswer, 0, len(round.Questions))
	var missing []string
	for i, question := range round.Questions {
		// The selected option wins when one is chosen; the free-text box is
		// the answer only when no option was selected.
		option := strings.TrimSpace(r.PostFormValue(fmt.Sprintf("answer.%d", i)))
		answer := option
		if answer == "" {
			answer = strings.TrimSpace(r.PostFormValue(fmt.Sprintf("answer_free.%d", i)))
		}
		if answer == "" {
			missing = append(missing, question.ID)
			continue
		}
		answers = append(answers, planAnswer{ID: question.ID, Question: question.Prompt, Option: option, Answer: answer})
	}
	if len(missing) > 0 {
		message := "answer every question first: " + strings.Join(missing, ", ")
		view := buildPlanAskView(round, planAskPreviousAnswers(p.dir), message)
		view.Token = p.server.token
		renderPlanPageStatus(w, http.StatusBadRequest, view)
		return
	}
	file := planAnswersFile{Round: round.Round, Answers: answers}
	if err := writePlanAnswers(p.dir, &file); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	recorded = true
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!doctype html><html><body><h1>Answers sent (round %d)</h1><p>%d answer(s) written to %s. You can close this tab.</p></body></html>\n", file.Round, len(answers), planAskAnswersFile)
	p.server.finish()
}

// buildPlanAskView renders the ask page. A previous option answer re-selects
// its radio; a previous free-text answer pre-fills the free-text box — never
// both, so a re-render never turns a chosen option into typed text.
func buildPlanAskView(round *planRound, previous map[string]planAnswer, errorText string) planPageView {
	view := planPageView{
		Mode:    planPageAsk,
		Title:   "Conveyor plan ask — " + planDraftName(round.Dir),
		Command: "conveyor plan ask " + round.Dir,
		Round:   round.Round,
		Intro:   fmt.Sprintf("Round %d: %d question(s). Pick an option or type an answer for each, then send.", round.Round, len(round.Questions)),
		Error:   errorText,
	}
	for index, question := range round.Questions {
		prior := previous[question.ID]
		card := planPageCard{
			Index:  index,
			ID:     question.ID,
			Prompt: question.Prompt,
		}
		if prior.Option == "" {
			card.Answer = prior.Answer
		}
		for _, option := range question.Options {
			card.Options = append(card.Options, planPageOption{Text: option, Recommended: option == question.Recommended, Selected: option == prior.Option})
		}
		view.Questions = append(view.Questions, card)
	}
	return view
}

// loadPlanRound reads and strictly validates <dir>/round.yml. An absent file,
// unknown fields, an empty question list, a question without an id or prompt, a
// duplicate id, or a recommended option that is not among the options is an
// error.
func loadPlanRound(dir string) (*planRound, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("plan draft %q: resolve path: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("plan draft %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("plan draft %q is not a directory", dir)
	}
	path := filepath.Join(abs, planAskRoundFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("plan ask: %s: no %s in the draft; write the round's questions first", dir, planAskRoundFile)
	}
	if err != nil {
		return nil, fmt.Errorf("plan ask: %s: %w", planAskRoundFile, err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	var round planRound
	if err := decoder.Decode(&round); err != nil {
		return nil, fmt.Errorf("plan ask: %s: %w", planAskRoundFile, err)
	}
	round.Dir = filepath.Clean(dir)
	if len(round.Questions) == 0 {
		return nil, fmt.Errorf("plan ask: %s: at least one question is required", planAskRoundFile)
	}
	seen := make(map[string]bool, len(round.Questions))
	for _, question := range round.Questions {
		if strings.TrimSpace(question.ID) == "" {
			return nil, fmt.Errorf("plan ask: %s: every question needs an id", planAskRoundFile)
		}
		if seen[question.ID] {
			return nil, fmt.Errorf("plan ask: %s: duplicate question id %q", planAskRoundFile, question.ID)
		}
		seen[question.ID] = true
		if strings.TrimSpace(question.Prompt) == "" {
			return nil, fmt.Errorf("plan ask: %s: question %s needs a prompt", planAskRoundFile, question.ID)
		}
		if question.Recommended != "" && !planRoundHasOption(question, question.Recommended) {
			return nil, fmt.Errorf("plan ask: %s: question %s recommends %q, which is not one of its options", planAskRoundFile, question.ID, question.Recommended)
		}
	}
	return &round, nil
}

func planRoundHasOption(question planQuestion, option string) bool {
	for _, candidate := range question.Options {
		if candidate == option {
			return true
		}
	}
	return false
}

// readPlanAnswers reads answers.json. An absent file is an empty answers file,
// not an error.
func readPlanAnswers(dir string) (*planAnswersFile, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("plan draft %q: resolve path: %w", dir, err)
	}
	data, err := os.ReadFile(filepath.Join(abs, planAskAnswersFile))
	if errors.Is(err, os.ErrNotExist) {
		return &planAnswersFile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", planAskAnswersFile, err)
	}
	var file planAnswersFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", planAskAnswersFile, err)
	}
	return &file, nil
}

// planAskPreviousAnswers reads answers.json when present so a re-render keeps
// what was already answered, including whether the answer was a chosen option
// or typed free text. An absent or unreadable file yields no answers.
func planAskPreviousAnswers(dir string) map[string]planAnswer {
	file, err := readPlanAnswers(dir)
	if err != nil {
		return nil
	}
	previous := make(map[string]planAnswer, len(file.Answers))
	for _, answer := range file.Answers {
		previous[answer.ID] = answer
	}
	return previous
}

// writePlanAnswers writes answers.json, marshaling the full file before the
// write so a single local reader never sees a partial file.
func writePlanAnswers(dir string, file *planAnswersFile) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", planAskAnswersFile, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, planAskAnswersFile), data, 0o600); err != nil {
		return fmt.Errorf("%s: %w", planAskAnswersFile, err)
	}
	return nil
}
