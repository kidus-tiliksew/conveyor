package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

const (
	cliLabelWidth          = 28
	harnessDetailLimit     = 600
	harnessFallbackLimit   = 320
	harnessCommandLimit    = 180
	presentationElisionTag = " … [elided]"
)

type cliPalette struct {
	title, accent, success, warning, muted, label lipgloss.Style
}

func newCLIPalette(output io.Writer) cliPalette {
	renderer := lipgloss.NewRenderer(output)
	return cliPalette{
		title:   renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
		accent:  renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		success: renderer.NewStyle().Foreground(lipgloss.Color("10")),
		warning: renderer.NewStyle().Foreground(lipgloss.Color("11")),
		muted:   renderer.NewStyle().Foreground(lipgloss.Color("8")),
		label:   renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("7")),
	}
}

func renderCLIConfigRow(output io.Writer, styled bool, key, value, source string) error {
	if !styled {
		if source == "" {
			_, err := fmt.Fprintf(output, "%s\t%s\n", key, value)
			return err
		}
		_, err := fmt.Fprintf(output, "%s\t%s\t%s\n", key, value, source)
		return err
	}
	palette := newCLIPalette(output)
	line := fmt.Sprintf("%-*s %s", cliLabelWidth, key, value)
	if source != "" {
		line += "  " + palette.muted.Render("source: "+source)
	}
	_, err := fmt.Fprintln(output, palette.label.Render(line[:cliLabelWidth])+line[cliLabelWidth:])
	return err
}

func renderCLIStatusRows(output io.Writer, styled bool, rows ...[2]string) error {
	palette := newCLIPalette(output)
	for _, row := range rows {
		if !styled {
			if _, err := fmt.Fprintf(output, "%s: %s\n", row[0], row[1]); err != nil {
				return err
			}
			continue
		}
		label := fmt.Sprintf("%-12s", row[0])
		if _, err := fmt.Fprintf(output, "%s %s\n", palette.label.Render(label), row[1]); err != nil {
			return err
		}
	}
	return nil
}

type harnessEventRenderer struct {
	output  io.Writer
	palette cliPalette
	pending bytes.Buffer
}

type runOutputPresentation struct {
	output        io.Writer
	presentEvents bool
	styled        bool
	notice        func(string)
}

func presentRunChildReapNotice(stdout io.Writer, presentation *runOutputPresentation, stage, state string) error {
	message := fmt.Sprintf("work order is %s; ending lingering %s session so the run can advance", state, stage)
	if presentation == nil {
		_, err := fmt.Fprintln(stdout, "! "+message)
		return err
	}
	if presentation.notice != nil {
		presentation.notice("! " + message)
		return nil
	}
	if !presentation.styled {
		_, err := fmt.Fprintln(presentation.output, "! "+message)
		return err
	}
	_, err := fmt.Fprintln(presentation.output, newCLIPalette(presentation.output).warning.Render("! "+message))
	return err
}

func harnessStdoutFanout(stdout, failureTail, usage io.Writer, item workerservice.DispatchOrder, presentation *runOutputPresentation) (io.Writer, *harnessEventRenderer) {
	stdoutDestination := stdout
	var renderer *harnessEventRenderer
	if presentation != nil && presentation.presentEvents && item.Dispatch == "run" {
		renderer = newHarnessEventRenderer(presentation.output)
		stdoutDestination = renderer
	}
	destinations := []io.Writer{stdoutDestination, failureTail}
	if usage != nil {
		destinations = append(destinations, usage)
	}
	return io.MultiWriter(destinations...), renderer
}

func newHarnessEventRenderer(output io.Writer) *harnessEventRenderer {
	return &harnessEventRenderer{output: output, palette: newCLIPalette(output)}
}

func (r *harnessEventRenderer) Write(p []byte) (int, error) {
	written := len(p)
	if _, err := r.pending.Write(p); err != nil {
		return 0, err
	}
	for {
		line, err := r.pending.ReadString('\n')
		if err != nil {
			r.pending.WriteString(line)
			break
		}
		if renderErr := r.renderLine(strings.TrimSuffix(line, "\n")); renderErr != nil {
			return 0, renderErr
		}
	}
	return written, nil
}

func (r *harnessEventRenderer) Flush() error {
	if r.pending.Len() == 0 {
		return nil
	}
	line := r.pending.String()
	r.pending.Reset()
	return r.renderLine(line)
}

func (r *harnessEventRenderer) renderLine(line string) error {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}
	var event struct {
		Type     string          `json:"type"`
		ThreadID string          `json:"thread_id"`
		Message  string          `json:"message"`
		Error    json.RawMessage `json:"error"`
		Usage    struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
		Item struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Command  string `json:"command"`
			Tool     string `json:"tool"`
			Name     string `json:"name"`
			Status   string `json:"status"`
			ExitCode *int   `json:"exit_code"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil || event.Type == "" {
		_, writeErr := fmt.Fprintln(r.output, r.palette.muted.Render(boundText(trimmed, harnessFallbackLimit)))
		return writeErr
	}

	var rendered string
	switch event.Type {
	case "thread.started":
		rendered = r.palette.muted.Render("session started " + boundText(event.ThreadID, 24))
	case "turn.started":
		rendered = r.palette.muted.Render("agent started")
	case "item.started", "item.completed":
		rendered = r.renderItem(event.Type, event.Item.Type, event.Item.Text, event.Item.Command, firstNonEmpty(event.Item.Tool, event.Item.Name), event.Item.Status, event.Item.ExitCode)
		if rendered == "" {
			rendered = r.palette.muted.Render(boundText(trimmed, harnessFallbackLimit))
		}
	case "turn.completed":
		detail := "agent turn completed"
		if event.Usage.InputTokens > 0 || event.Usage.OutputTokens > 0 {
			detail += fmt.Sprintf(" · tokens in %d, out %d", event.Usage.InputTokens, event.Usage.OutputTokens)
		}
		rendered = r.palette.success.Render("✓ " + detail)
	case "error":
		message := event.Message
		if message == "" && len(event.Error) > 0 {
			message = string(event.Error)
		}
		rendered = r.palette.warning.Render("! " + boundText(message, harnessDetailLimit))
	default:
		rendered = r.palette.muted.Render(boundText(trimmed, harnessFallbackLimit))
	}
	if rendered == "" {
		return nil
	}
	_, err := fmt.Fprintln(r.output, rendered)
	return err
}

func (r *harnessEventRenderer) renderItem(eventType, itemType, text, command, tool, status string, exitCode *int) string {
	switch itemType {
	case "agent_message":
		if eventType == "item.completed" && strings.TrimSpace(text) != "" {
			return boundText(text, harnessDetailLimit)
		}
	case "command_execution":
		command = boundText(command, harnessCommandLimit)
		if command == "" {
			command = "command"
		}
		if eventType == "item.started" {
			return r.palette.accent.Render("› ") + command + r.palette.muted.Render(" · running")
		}
		result := strings.TrimSpace(status)
		if result == "" {
			result = "completed"
		}
		if exitCode != nil {
			result += fmt.Sprintf(" (exit %d)", *exitCode)
		}
		style := r.palette.success
		mark := "✓ "
		if (exitCode != nil && *exitCode != 0) || strings.Contains(strings.ToLower(result), "fail") {
			style, mark = r.palette.warning, "! "
		}
		return style.Render(mark) + command + r.palette.muted.Render(" · "+result)
	case "reasoning":
		if eventType == "item.started" {
			return r.palette.muted.Render("thinking…")
		}
	case "mcp_tool_call":
		tool = boundText(tool, harnessCommandLimit)
		if tool == "" {
			tool = "MCP tool"
		}
		result := strings.TrimSpace(status)
		if result == "" {
			if eventType == "item.started" {
				result = "running"
			} else {
				result = "completed"
			}
		}
		style, mark := r.palette.accent, "› "
		if eventType == "item.completed" {
			style, mark = r.palette.success, "✓ "
		}
		if strings.Contains(strings.ToLower(result), "fail") || strings.Contains(strings.ToLower(result), "error") {
			style, mark = r.palette.warning, "! "
		}
		return style.Render(mark) + tool + r.palette.muted.Render(" · "+result)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func boundText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	keep := limit - utf8.RuneCountInString(presentationElisionTag)
	if keep < 1 {
		keep = 1
	}
	return string(runes[:keep]) + presentationElisionTag
}
