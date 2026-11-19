package dispatch

// Crate briefing generation (feature-crate-insights CI-2; component-crates
// invariants 29 to 32; req-crate-insights REQ-5, AC-5.2, AC-5.3, AC-5.4;
// DEC-142(1), (3), (5)). The briefing is informative text only: nothing here
// reads or writes the store, and the result never enters work-order or agent
// context (DEC-142(2)).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/crateinsight"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
)

// Crate briefing bounds in Unicode characters (component-crates invariant
// 32; req-crate-insights AC-5.1).
const (
	MaxCrateBriefingHeadlineCharacters = 200
	MaxCrateBriefingAccountCharacters  = 800
	MaxCrateBriefingReasonCharacters   = 160
	MaxCrateBriefingLookFirst          = 3
)

var (
	// ErrCrateBriefingUnconfigured reports a deployment without an in-process
	// agent or without a triage route that names a model (component-crates
	// invariant 29, through component-requirement-insights invariant 10).
	ErrCrateBriefingUnconfigured = errors.New("crate briefing model is not configured")
	// ErrCrateBriefingInvalid refuses a model reply that breaks
	// component-crates invariant 32. Every wrapped reason is fixed text, so
	// the error never echoes the reply.
	ErrCrateBriefingInvalid = errors.New("model returned an invalid crate briefing")
)

// CrateBriefingInput is the whole model-visible input (DEC-142(5)). It is
// deliberately narrow: no task body, transcript, event, or repository
// content can reach the prompt through it.
type CrateBriefingInput struct {
	// Document is crateinsight.BriefingDocument's canonical JSON, sent
	// verbatim. The stored fingerprint hashes these bytes after
	// crateinsight.BriefingInstructions (component-crates invariant 31).
	Document []byte
	// MemberIDs are the crate's current member task IDs. They never reach
	// the model: the reply checks keep a look_first entry only for a member
	// and refuse prose that holds a member's ID (invariant 32).
	MemberIDs []string
}

// CrateBriefing is one validated briefing with the model the result names
// and its token usage. Usage is stored telemetry only (DEC-1).
type CrateBriefing struct {
	Headline  string
	Account   string
	LookFirst []crateinsight.BriefingEntry
	Model     string
	TokensIn  int64
	TokensOut int64
}

// Prompt block markers. The document is single-line JSON, so no caller
// content can produce a line equal to the end marker.
const (
	crateBriefingDataBegin = "BEGIN CRATE_DATA"
	crateBriefingDataEnd   = "END CRATE_DATA"
)

// crateBriefingLineBreaks are the characters the headline check also counts
// as line breaks.
const crateBriefingLineBreaks = "\r\n\v\f\u0085  "

// crateBriefingIdentifier matches an identifier that req-crate-insights
// AC-5.3 keeps out of the prose: the dated ID shape of tasks, crates, and
// reference documents, such as 261119-fbe6d9 in crate-261119-d9973f;
// REQ-n, AC-n.m, or DEC-n; a requirement ID such as req-crate-insights; and
// a design ID such as component-crates. A hyphenated word such as
// crate-level is no identifier.
var crateBriefingIdentifier = regexp.MustCompile(`(?:\b\d{6}-[0-9a-f]{6}\b)|(?:\b(?:REQ|AC|DEC)-\d+(?:\.\d+)?\b)|(?:\breq-[a-z0-9]+(?:-[a-z0-9]+)*\b)|(?:\b(?:component|feature)-[a-z0-9]+(?:-[a-z0-9]+)+\b)`)

// GenerateCrateBriefing produces a crate briefing through the trusted
// in-process client on the triage route (DEC-142(1)).
//
// The route resolves as GenerateRequirementHeadline resolves it: a nil
// agent, a missing triage route, or a blank route model is
// ErrCrateBriefingUnconfigured; the model is
// config.ResolveControlPlaneModel("triage", route.Model); and the route's
// positive timeout bounds the call. The reply passes ValidateCrateBriefing
// before it is returned, so a refused reply is never stored.
func (d *Dispatcher) GenerateCrateBriefing(ctx context.Context, input CrateBriefingInput) (CrateBriefing, error) {
	if d.Agent == nil {
		return CrateBriefing{}, fmt.Errorf("%w: in-process agent is not configured", ErrCrateBriefingUnconfigured)
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return CrateBriefing{}, err
	}
	route := cfg.Routing.Triage
	if strings.TrimSpace(route.Model) == "" {
		return CrateBriefing{}, fmt.Errorf("%w: triage route with a model is not configured", ErrCrateBriefingUnconfigured)
	}
	prompt, err := CrateBriefingPrompt(input.Document)
	if err != nil {
		return CrateBriefing{}, err
	}
	model := config.ResolveControlPlaneModel(string(core.StageTriage), route.Model)
	runCtx := ctx
	cancel := func() {}
	if route.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, route.Timeout)
	}
	defer cancel()
	result, err := d.Agent.Run(runCtx, model, inprocess.Input{Prompt: prompt})
	if err != nil {
		return CrateBriefing{}, err
	}
	briefing, err := ValidateCrateBriefing(result.Output, input.MemberIDs)
	if err != nil {
		return CrateBriefing{}, err
	}
	briefing.Model, briefing.TokensIn, briefing.TokensOut = result.Model, result.TokensIn, result.TokensOut
	return briefing, nil
}

// CrateBriefingPrompt renders the fixed instructions followed by the one
// delimited untrusted-data block that holds the briefing document
// (component-crates invariant 30).
func CrateBriefingPrompt(document []byte) (string, error) {
	document = bytes.TrimSpace(document)
	if len(document) == 0 || !json.Valid(document) || bytes.ContainsAny(document, "\r\n") {
		return "", fmt.Errorf("crate briefing input requires the canonical briefing document")
	}
	var prompt strings.Builder
	prompt.WriteString(crateinsight.BriefingInstructions)
	prompt.WriteString("\n\n" + crateBriefingDataBegin + "\n")
	prompt.Write(document)
	prompt.WriteString("\n" + crateBriefingDataEnd + "\n")
	return prompt.String(), nil
}

// crateBriefingReply is the one JSON object the model must return. Pointer
// fields tell an absent or null field from an empty one.
type crateBriefingReply struct {
	Headline  *string               `json:"headline"`
	Account   *string               `json:"account"`
	LookFirst *[]crateBriefingEntry `json:"look_first"`
}

type crateBriefingEntry struct {
	TaskID *string `json:"task_id"`
	Tone   *string `json:"tone"`
	Reason *string `json:"reason"`
}

// invalidCrateBriefing wraps ErrCrateBriefingInvalid with a fixed reason.
func invalidCrateBriefing(reason string) error {
	return fmt.Errorf("%w: %s", ErrCrateBriefingInvalid, reason)
}

// ValidateCrateBriefing checks a model reply against component-crates
// invariant 32 and returns the trimmed briefing. The reply must be valid
// UTF-8 and exactly one JSON object holding headline, account, and
// look_first, with no other field and nothing after it. The headline and the
// account must be non-empty, at most 200 and 800 Unicode characters, and
// free of line breaks, a leading Markdown heading marker, and fences.
// look_first holds at most three entries, each naming a current member with
// the tone attention, info, or positive and a non-empty reason of at most
// 160 characters on one line. No text may hold a member's task ID or
// another identifier (req-crate-insights AC-5.3). Every refusal is
// ErrCrateBriefingInvalid with a fixed reason that never echoes the reply.
func ValidateCrateBriefing(output string, memberIDs []string) (CrateBriefing, error) {
	if !utf8.ValidString(output) {
		return CrateBriefing{}, invalidCrateBriefing("the reply is not valid UTF-8")
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(output)))
	decoder.DisallowUnknownFields()
	var reply crateBriefingReply
	if err := decoder.Decode(&reply); err != nil {
		return CrateBriefing{}, invalidCrateBriefing("the reply is not one JSON object of the briefing shape")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return CrateBriefing{}, invalidCrateBriefing("the reply holds output after the JSON object")
	}
	if reply.Headline == nil || reply.Account == nil || reply.LookFirst == nil {
		return CrateBriefing{}, invalidCrateBriefing("the reply lacks headline, account, or look_first")
	}
	headline, err := crateBriefingProse("headline", *reply.Headline, MaxCrateBriefingHeadlineCharacters)
	if err != nil {
		return CrateBriefing{}, err
	}
	account, err := crateBriefingProse("account", *reply.Account, MaxCrateBriefingAccountCharacters)
	if err != nil {
		return CrateBriefing{}, err
	}
	if len(*reply.LookFirst) > MaxCrateBriefingLookFirst {
		return CrateBriefing{}, invalidCrateBriefing("look_first holds more than three entries")
	}
	briefing := CrateBriefing{Headline: headline, Account: account, LookFirst: make([]crateinsight.BriefingEntry, 0, len(*reply.LookFirst))}
	for _, entry := range *reply.LookFirst {
		if entry.TaskID == nil || entry.Tone == nil || entry.Reason == nil {
			return CrateBriefing{}, invalidCrateBriefing("a look_first entry lacks task_id, tone, or reason")
		}
		if !slices.Contains(memberIDs, *entry.TaskID) {
			return CrateBriefing{}, invalidCrateBriefing("a look_first entry names a task that is not a member of the crate")
		}
		if !slices.Contains(crateinsight.BriefingTones, *entry.Tone) {
			return CrateBriefing{}, invalidCrateBriefing("a look_first entry has a tone other than attention, info, or positive")
		}
		reason := strings.TrimSpace(*entry.Reason)
		switch {
		case reason == "":
			return CrateBriefing{}, invalidCrateBriefing("a look_first reason is empty")
		case utf8.RuneCountInString(reason) > MaxCrateBriefingReasonCharacters:
			return CrateBriefing{}, invalidCrateBriefing(fmt.Sprintf("a look_first reason exceeds %d characters", MaxCrateBriefingReasonCharacters))
		case strings.ContainsAny(reason, crateBriefingLineBreaks):
			return CrateBriefing{}, invalidCrateBriefing("a look_first reason contains a line break")
		}
		briefing.LookFirst = append(briefing.LookFirst, crateinsight.BriefingEntry{TaskID: *entry.TaskID, Tone: *entry.Tone, Reason: reason})
	}
	texts := []string{briefing.Headline, briefing.Account}
	for _, entry := range briefing.LookFirst {
		texts = append(texts, entry.Reason)
	}
	for _, text := range texts {
		lower := strings.ToLower(text)
		for _, id := range memberIDs {
			if id != "" && strings.Contains(lower, strings.ToLower(id)) {
				return CrateBriefing{}, invalidCrateBriefing("the text holds a member task ID")
			}
		}
		if crateBriefingIdentifier.MatchString(text) {
			return CrateBriefing{}, invalidCrateBriefing("the text holds a task, requirement, or document identifier")
		}
	}
	return briefing, nil
}

// crateBriefingProse trims the headline or the account and refuses it when
// it is empty, longer than limit Unicode characters, or holds a line break,
// a leading Markdown heading marker, or a fence (component-crates invariant
// 32). The reasons name the field and never the text.
func crateBriefingProse(field, value string, limit int) (string, error) {
	text := strings.TrimSpace(value)
	switch {
	case text == "":
		return "", invalidCrateBriefing("the " + field + " is empty")
	case utf8.RuneCountInString(text) > limit:
		return "", invalidCrateBriefing(fmt.Sprintf("the %s exceeds %d characters", field, limit))
	case strings.ContainsAny(text, crateBriefingLineBreaks):
		return "", invalidCrateBriefing("the " + field + " contains a line break")
	case headlineHeading.MatchString(text):
		return "", invalidCrateBriefing("the " + field + " contains a Markdown heading")
	case strings.Contains(text, "```") || strings.Contains(text, "~~~"):
		return "", invalidCrateBriefing("the " + field + " contains a fenced block")
	}
	return text, nil
}
