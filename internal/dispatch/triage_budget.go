package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
)

// triageInputBytes measures input content, including tool declarations and
// continuation history. This is a byte allowance, not a tokenizer or a claim
// about any provider's context window. Binary file expansion is provider-owned.
func triageInputBytes(input inprocess.Input) int {
	size := len(input.Prompt)
	tools, _ := json.Marshal(input.Tools)
	size += len(tools)
	for _, attachment := range input.Attachments {
		size += len(attachment.Content) + len(attachment.ID) + len(attachment.Name) + len(attachment.ContentType)
	}
	if input.Continuation != nil {
		for _, item := range input.Continuation.ResponseItems {
			size += len(item)
		}
		for _, output := range input.Continuation.FunctionCallOutputs {
			size += len(output.CallID) + len(output.Output)
		}
	}
	return size
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
	if size := triageInputBytes(input); size > maxTriageInitialBytes {
		return inprocess.Input{}, fmt.Errorf("triage mandatory input for task %s is %d bytes, exceeding the %d-byte initial input allowance; task intent, served authority, and local attachments were not truncated; reduce local attachments or split the task before retrying", taskID, size, maxTriageInitialBytes)
	}
	for _, attachment := range candidates {
		if local[attachment.ID] {
			continue
		}
		input.Attachments = append(input.Attachments, attachment)
		if triageInputBytes(input) <= maxTriageInitialBytes {
			continue
		}
		input.Attachments = input.Attachments[:len(input.Attachments)-1]
		supplied := fmt.Sprintf("Context artifact supplied as %s input: %s (%s, %d bytes, id %s)", attachment.Kind, attachment.Name, attachment.ContentType, len(attachment.Content), attachment.ID)
		omitted := fmt.Sprintf("Context artifact body omitted by triage byte budget: %s (%s, %d bytes, id %s)", attachment.Name, attachment.ContentType, len(attachment.Content), attachment.ID)
		input.Prompt = strings.Replace(input.Prompt, supplied, omitted, 1)
	}
	// Omission annotations can be longer than the original supply annotations.
	if size := triageInputBytes(input); size > maxTriageInitialBytes {
		return inprocess.Input{}, fmt.Errorf("triage input metadata for task %s is %d bytes, exceeding the %d-byte initial input allowance; reduce task context before retrying", taskID, size, maxTriageInitialBytes)
	}
	return input, nil
}
