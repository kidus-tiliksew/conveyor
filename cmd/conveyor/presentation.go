package main

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// The CLI palette and the aligned rows that conveyor config list and
// conveyor auth status print (component-runtime).

const cliLabelWidth = 28

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

// outputIsTerminal reports whether output is a terminal, so styled rows are
// used only for an interactive reader.
func outputIsTerminal(output io.Writer) bool {
	file, ok := output.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}
