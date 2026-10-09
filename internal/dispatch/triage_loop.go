package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/corpus"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
)

// runTriageLoop retains the ordinary single-call Agent boundary while giving
// triage a bounded native function-tool exchange. Corpus failures are returned
// in-band, so absent grounding cannot fail or park intake by itself
// (req-intake-and-triage REQ-6/AC-6.3; DEC-25).
func (d *Dispatcher) runTriageLoop(ctx context.Context, model string, input inprocess.Input) (inprocess.Result, error) {
	executor := corpus.Executor{Store: d.Store}
	toolCalls := 0
	var aggregate inprocess.Result
	transcripts := make([]json.RawMessage, 0, maxTriageIterations)
	history := make([]json.RawMessage, 0)
	pendingOutputs := make([]inprocess.FunctionCallOutput, 0)
	for iteration := 0; iteration < maxTriageIterations; iteration++ {
		if iteration == maxTriageIterations-1 {
			input.Prompt += "\n\n# Tool loop closed\n\nThe bounded corpus-tool phase is over. Return a complete final conveyor:triage verdict now using available evidence; do not request another tool.\n"
		}
		if len(history) > 0 || len(pendingOutputs) > 0 {
			input.Continuation = &inprocess.Continuation{ResponseItems: append([]json.RawMessage(nil), history...), FunctionCallOutputs: append([]inprocess.FunctionCallOutput(nil), pendingOutputs...)}
		}
		textLimit := maxTriageInputBytes
		if iteration == 0 {
			textLimit = maxTriageInitialBytes
		}
		if overage := checkTriageInput(input, textLimit); overage != nil {
			if iteration == 0 {
				return aggregate, overage
			}
			return triageBudgetFallback(aggregate, transcripts, history, toolCalls, overage), nil
		}
		preparedTextBytes := measureTriageInput(input).TextBytes
		input.CheckPreparedText = func(attachment inprocess.Attachment, size int) error {
			preparedTextBytes += size
			if preparedTextBytes > textLimit {
				return &triageBudgetError{"text/history", preparedTextBytes, textLimit, attachment.ID, attachment.Name}
			}
			return nil
		}
		result, err := d.Agent.Run(ctx, model, input)
		aggregate.Model = result.Model
		aggregate.TokensIn += result.TokensIn
		aggregate.TokensOut += result.TokensOut
		aggregate.Redactions.Add(result.Redactions)
		aggregate.Diagnostic = result.Diagnostic
		if len(result.Transcript) > 0 {
			if json.Valid(result.Transcript) {
				transcripts = append(transcripts, append(json.RawMessage(nil), result.Transcript...))
			}
		}
		if err != nil {
			var overage *triageBudgetError
			if iteration > 0 && errors.As(err, &overage) {
				return triageBudgetFallback(aggregate, transcripts, history, toolCalls, overage), nil
			}
			if len(transcripts) > 0 {
				aggregate.Transcript, _ = json.Marshal(transcripts)
			}
			return aggregate, err
		}
		for _, output := range pendingOutputs {
			encoded, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": output.CallID, "output": output.Output})
			history = append(history, encoded)
		}
		pendingOutputs = pendingOutputs[:0]
		history = append(history, result.ResponseItems...)
		if len(result.FunctionCalls) == 0 {
			aggregate.Output = result.Output
			aggregate.ResponseItems = append([]json.RawMessage(nil), history...)
			aggregate.ToolCallsExecuted = toolCalls
			if len(transcripts) > 0 {
				aggregate.Transcript, _ = json.Marshal(transcripts)
			}
			return aggregate, nil
		}
		if iteration == maxTriageIterations-1 {
			break
		}
		seen := map[string]bool{}
		for index, call := range result.FunctionCalls {
			callID := strings.TrimSpace(call.CallID)
			if callID == "" || seen[callID] {
				return neutralTriageResult(aggregate, transcripts, history, toolCalls), nil
			}
			seen[callID] = true
			entry := map[string]any{"id": callID, "name": call.Name}
			switch {
			case index >= maxTriageRequestsPerTurn:
				entry["error"] = fmt.Sprintf("tool request limit exceeded after %d calls in one response", maxTriageRequestsPerTurn)
			case !corpus.IsTool(call.Name):
				entry["error"] = fmt.Sprintf("triage tool %q is unavailable", call.Name)
			case len(call.ArgumentsJSON) > maxTriageToolArgumentBytes:
				entry["error"] = fmt.Sprintf("triage tool arguments exceed %d bytes", maxTriageToolArgumentBytes)
			case toolCalls >= maxTriageToolCalls:
				entry["error"] = fmt.Sprintf("tool call budget exhausted after %d calls", maxTriageToolCalls)
			default:
				toolCalls++
				output, executeErr := executor.Execute(ctx, call.Name, call.ArgumentsJSON)
				if executeErr != nil {
					entry["error"] = executeErr.Error()
				} else {
					entry["output"] = boundedTriageToolOutput(output)
				}
			}
			encoded, _ := json.Marshal(map[string]any{"result": entry, "untrusted_data": true})
			output := inprocess.FunctionCallOutput{CallID: callID, Output: boundedTriagePromptJSON(encoded)}
			candidate := input
			candidate.Continuation = &inprocess.Continuation{ResponseItems: history, FunctionCallOutputs: append(append([]inprocess.FunctionCallOutput(nil), pendingOutputs...), output)}
			// Reserve room for bounded refusal results and the final instruction.
			if overage := checkTriageInput(candidate, maxTriageInputBytes-(8<<10)-(preparedTextBytes-measureTriageInput(input).TextBytes)); overage != nil {
				refusal, _ := json.Marshal(map[string]any{"untrusted_data": true, "result": map[string]any{"id": callID, "name": call.Name, "error": overage.Error() + "; body not supplied or read; finish with available evidence"}})
				output.Output = string(refusal)
			}
			pendingOutputs = append(pendingOutputs, output)
		}
	}
	return neutralTriageResult(aggregate, transcripts, history, toolCalls), nil
}

func neutralTriageResult(aggregate inprocess.Result, transcripts []json.RawMessage, history []json.RawMessage, toolCalls int) inprocess.Result {
	// Preserve fail-open intake when the provider ignores finalization or emits
	// an unusable native call sequence.
	aggregate.Output = "```conveyor:triage\n{\"class\":\"chore\",\"route\":\"proceed\",\"summary\":\"Triage completed with available task context after the bounded corpus-tool phase.\",\"brief\":{\"questions\":[],\"affected_areas\":[],\"risks\":[\"Corpus grounding was incomplete because the tool loop budget was exhausted.\"]},\"requirement_proposals\":[],\"system_design_proposals\":[]}\n```"
	aggregate.ResponseItems = append([]json.RawMessage(nil), history...)
	aggregate.ToolCallsExecuted = toolCalls
	if len(transcripts) > 0 {
		aggregate.Transcript, _ = json.Marshal(transcripts)
	}
	return aggregate
}

func boundedTriagePromptJSON(encoded []byte) string {
	if len(encoded) <= maxTriageToolResultBytes {
		return string(encoded)
	}
	bounded, _ := json.Marshal(map[string]any{"error": "corpus result exceeds the byte budget; body not supplied or read; do not cite this result as authority", "original_bytes": len(encoded)})
	return string(bounded)
}

func boundedTriageToolOutput(output any) any {
	encoded, err := json.Marshal(output)
	if err != nil {
		return map[string]any{"error": "tool output could not be encoded"}
	}
	if len(encoded) <= maxTriageToolResultBytes {
		return output
	}
	return map[string]any{"error": "corpus result exceeds the byte budget; body not supplied or read; do not cite this result as authority", "original_bytes": len(encoded)}
}

// recordTriageContextProposals uses the unified advisory lifecycle. Validation
// and deduplication live in Store.ProposeTaskContext; bad model references are
// logged and skipped because a context suggestion cannot fail intake
// (req-intake-and-triage REQ-7; DEC-25).
func (d *Dispatcher) recordTriageContextProposals(ctx context.Context, task core.Task, result pipeline.Triage) {
	record := func(kind core.TaskContextProposalTargetKind, proposal pipeline.ContextProposal) {
		id, justification := strings.TrimSpace(proposal.ID), strings.TrimSpace(proposal.Justification)
		if id == "" || justification == "" {
			log.Printf("[task %s] drop triage %s context proposal %q: id and justification are required", task.ID, kind, id)
			return
		}
		_, _, err := d.Store.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: task.ID, TargetKind: kind, TargetID: id, Source: core.TaskContextProposalTriage, Justification: justification})
		if err != nil {
			log.Printf("[task %s] drop triage %s context proposal %q: %v", task.ID, kind, id, err)
		}
	}
	for _, proposal := range result.RequirementProposals {
		record(core.TaskContextProposalRequirement, proposal)
	}
	for _, proposal := range result.SystemDesignProposals {
		record(core.TaskContextProposalSystemDesign, proposal)
	}
}
