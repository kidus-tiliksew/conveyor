package dispatch

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
)

// triageBudget separates text/history from raw attachment transport. Every file
// consumes raw bytes; only textual document bodies also consume text bytes.
// Neither total estimates model tokens (component-runtime; req-intake-and-triage REQ-6).
type triageBudget struct {
	TextBytes int
	RawBytes  int
}

type triageBudgetError struct {
	Budget         string `json:"budget"`
	Usage          int    `json:"usage_bytes"`
	Limit          int    `json:"limit_bytes"`
	AttachmentID   string `json:"attachment_id,omitempty"`
	AttachmentName string `json:"attachment_name,omitempty"`
}

func (e *triageBudgetError) Error() string {
	identity := ""
	if e.AttachmentID != "" || e.AttachmentName != "" {
		identity = fmt.Sprintf(" for attachment %q (%q)", e.AttachmentID, e.AttachmentName)
	}
	return fmt.Sprintf("triage %s budget exhausted%s: %d bytes exceeds %d-byte limit; reduce the identified attachment or task context before retrying", e.Budget, identity, e.Usage, e.Limit)
}

// Images and audio are binary. Documents use MIME first and extension fallback,
// as modelAttachmentKind does. RTF is textual; PDF and office containers are
// binary. Unknown document formats are conservatively charged as text.
func triageAttachmentText(kind inprocess.AttachmentKind, name, contentType string) bool {
	if kind == inprocess.AttachmentImage || kind == inprocess.AttachmentAudio {
		return false
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if strings.HasPrefix(mime, "text/") || mime == "application/json" || mime == "application/xml" || mime == "application/rtf" {
		return true
	}
	if mime == "application/pdf" {
		return false
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pdf", ".doc", ".docx", ".odt", ".ppt", ".pptx", ".xls", ".xlsx":
		return false
	default:
		return true
	}
}

func measureTriageInput(input inprocess.Input) triageBudget {
	budget := triageBudget{TextBytes: len(input.Prompt)}
	tools, _ := json.Marshal(input.Tools)
	budget.TextBytes += len(tools)
	for _, attachment := range input.Attachments {
		budget.RawBytes += len(attachment.Content)
		budget.TextBytes += len(attachment.ID) + len(attachment.Name) + len(attachment.ContentType) + len(attachment.Kind)
		if triageAttachmentText(attachment.Kind, attachment.Name, attachment.ContentType) {
			budget.TextBytes += len(attachment.Content)
		}
	}
	if input.Continuation != nil {
		for _, item := range input.Continuation.ResponseItems {
			budget.TextBytes += len(item)
		}
		for _, output := range input.Continuation.FunctionCallOutputs {
			budget.TextBytes += len(output.CallID) + len(output.Output)
		}
	}
	return budget
}

func triageAttachmentOverage(attachment inprocess.Attachment, size int, textLimit int) *triageBudgetError {
	budget, limit := "raw attachment per-file", maxModelAttachmentBytes
	if attachment.Kind == inprocess.AttachmentImage {
		budget, limit = "raw image", maxModelImageBytes
	}
	if size > limit {
		return &triageBudgetError{budget, size, limit, attachment.ID, attachment.Name}
	}
	if triageAttachmentText(attachment.Kind, attachment.Name, attachment.ContentType) && size > textLimit {
		return &triageBudgetError{"text/history", size, textLimit, attachment.ID, attachment.Name}
	}
	return nil
}

func checkTriageInput(input inprocess.Input, textLimit int) *triageBudgetError {
	for _, attachment := range input.Attachments {
		if err := triageAttachmentOverage(attachment, len(attachment.Content), textLimit); err != nil && err.Budget != "text/history" {
			return err
		}
	}
	budget := measureTriageInput(input)
	if budget.RawBytes > maxModelFileBytes {
		// Name the file that crosses the aggregate ceiling without exposing content.
		total := 0
		for _, attachment := range input.Attachments {
			total += len(attachment.Content)
			if total > maxModelFileBytes {
				return &triageBudgetError{"raw attachment combined", budget.RawBytes, maxModelFileBytes, attachment.ID, attachment.Name}
			}
		}
	}
	if budget.TextBytes > textLimit {
		ids, names := []string{}, []string{}
		for _, attachment := range input.Attachments {
			ids = append(ids, attachment.ID)
			names = append(names, attachment.Name)
		}
		return &triageBudgetError{"text/history", budget.TextBytes, textLimit, strings.Join(ids, ", "), strings.Join(names, ", ")}
	}
	return nil
}

// boundTriageInput preserves mandatory intent/authority and local inputs, then
// admits whole adjacent evidence bodies in the shared lineage selection order.
// Historical evidence cannot crowd out governing context (req-intake-and-triage
// REQ-6; req-260802-72fc68 REQ-2; component-lineage).
func boundTriageInput(input inprocess.Input, artifacts []core.Artifact, taskID string) (inprocess.Input, error) {
	local := map[string]bool{}
	for _, artifact := range artifacts {
		local[artifact.ID] = artifact.TaskID == taskID
	}
	candidates := input.Attachments
	input.Attachments = nil
	input.Prompt += "\n# Triage input allowance\n\nInput content is bounded in bytes, not measured model tokens. Adjacent artifact bodies may be omitted whole; their lineage metadata remains above. An omitted body has not been read and must not be cited as authority. Confirm governing documents through the corpus tools.\n"
	for _, attachment := range candidates {
		if local[attachment.ID] {
			input.Attachments = append(input.Attachments, attachment)
		}
	}
	if err := checkTriageInput(input, maxTriageInitialBytes); err != nil {
		return inprocess.Input{}, fmt.Errorf("mandatory input for task %s: %w; task intent, served authority, and local attachments were not truncated", taskID, err)
	}
	for _, attachment := range candidates {
		if local[attachment.ID] {
			continue
		}
		input.Attachments = append(input.Attachments, attachment)
		overage := checkTriageInput(input, maxTriageInitialBytes)
		if overage == nil {
			continue
		}
		input.Attachments = input.Attachments[:len(input.Attachments)-1]
		omitTriageAttachment(&input, attachment, overage)
	}
	// Omission diagnostics also consume text. If they displace an admitted
	// optional body, remove the lowest-priority body first; never truncate intent.
	for overage := checkTriageInput(input, maxTriageInitialBytes); overage != nil; overage = checkTriageInput(input, maxTriageInitialBytes) {
		index := len(input.Attachments) - 1
		for index >= 0 && local[input.Attachments[index].ID] {
			index--
		}
		if index < 0 {
			return inprocess.Input{}, fmt.Errorf("mandatory input metadata for task %s: %w", taskID, overage)
		}
		attachment := input.Attachments[index]
		input.Attachments = append(input.Attachments[:index], input.Attachments[index+1:]...)
		omitTriageAttachment(&input, attachment, overage)
	}

	return input, nil
}

func omitTriageAttachment(input *inprocess.Input, attachment inprocess.Attachment, overage *triageBudgetError) {
	supplied := fmt.Sprintf("Context artifact supplied as %s input: %s (%s, %d bytes, id %s)", attachment.Kind, attachment.Name, attachment.ContentType, len(attachment.Content), attachment.ID)
	omitted := fmt.Sprintf("Context artifact body omitted by triage byte budget: %s (%s, %d bytes, id %s); %s", attachment.Name, attachment.ContentType, len(attachment.Content), attachment.ID, overage)
	if strings.Contains(input.Prompt, supplied) {
		input.Prompt = strings.Replace(input.Prompt, supplied, omitted, 1)
	} else {
		input.Prompt += "\n" + omitted + "\n"
	}
}

// Preserve AC-6.3 fail-open behavior for incomplete corpus grounding while
// recording which allowance suppressed the continuation (DEC-17, DEC-25).
func triageBudgetFallback(aggregate inprocess.Result, transcripts []json.RawMessage, history []json.RawMessage, toolCalls int, overage *triageBudgetError) inprocess.Result {
	audit, _ := json.Marshal(map[string]any{"triage_context_budget": overage, "provider_call_skipped": true})
	transcripts = append(transcripts, audit)
	fallback := neutralTriageResult(aggregate, transcripts, history, toolCalls)
	reason, _ := json.Marshal("Corpus grounding was incomplete because the input byte allowance was exhausted; no oversized continuation was sent. " + overage.Error())
	fallback.Output = strings.Replace(fallback.Output, `"Corpus grounding was incomplete because the tool loop budget was exhausted."`, string(reason), 1)
	return fallback
}
