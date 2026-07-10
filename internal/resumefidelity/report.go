package resumefidelity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func writeArtifacts(artifactDir string, result Result) (string, string, error) {
	jsonPath := filepath.Join(artifactDir, "result.json")
	markdownPath := filepath.Join(artifactDir, "report.md")
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", "", err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(jsonPath, encoded, 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(markdownPath, []byte(markdownReport(result)), 0o644); err != nil {
		return "", "", err
	}
	return jsonPath, markdownPath, nil
}

func markdownReport(result Result) string {
	var report strings.Builder
	fmt.Fprintf(&report, "# Codex resume-fidelity experiment %s\n\n", result.RunID)
	fmt.Fprintf(&report, "Run: %s to %s  \n", result.StartedAt.Format("2006-01-02 15:04:05Z"), result.FinishedAt.Format("2006-01-02 15:04:05Z"))
	fmt.Fprintf(&report, "Versions: `%s` (`%s`) → `%s` (`%s`)\n\n", result.BaseVersion, result.BaseImage, result.BumpVersion, result.BumpImage)
	fmt.Fprintf(&report, "Host boundary: %s\n\n", result.HostBoundary)

	report.WriteString("## Results\n\n")
	report.WriteString("| Scenario | Resume recall | Cold recall | Resume effective tokens | Cold effective tokens | Ratio | Gate |\n")
	report.WriteString("|---|---:|---:|---:|---:|---:|---|\n")
	for _, scenario := range result.Scenarios {
		gate := "cold"
		if scenario.Comparison.ResumeQualified {
			gate = "resume"
		}
		if scenario.Error != "" {
			gate = "error"
		}
		fmt.Fprintf(&report, "| %s | %d/5 | %d/5 | %d | %d | %.2f | %s |\n",
			scenario.Name,
			scenario.Resume.Score.Extended,
			scenario.Cold.Score.Extended,
			scenario.Resume.Usage.EffectiveTokens,
			scenario.Cold.Usage.EffectiveTokens,
			scenario.Comparison.ResumeToColdRatio,
			gate,
		)
	}

	report.WriteString("\nEffective tokens are `max(input - cached_input, 0) + output`; this is a token-cost proxy, not a dollar estimate for subscription-auth runs. Resume qualifies only when it preserves core recall, recalls the native-only marker, and costs no more than 125% of cold start.\n\n")
	for _, scenario := range result.Scenarios {
		fmt.Fprintf(&report, "### %s\n\n", scenario.Name)
		fmt.Fprintf(&report, "%s\n\n", scenario.Description)
		fmt.Fprintf(&report, "- Crash observed: `%t`\n", scenario.CrashObserved)
		fmt.Fprintf(&report, "- Resume answer: decision `%s`; marker `%s`; core `%d/4`; duration `%s`\n",
			inline(scenario.Resume.Answer.Decision), inline(scenario.Resume.Answer.ContinuityMarker), scenario.Resume.Score.Core, scenario.Resume.Duration.Round(100_000_000))
		fmt.Fprintf(&report, "- Cold answer: decision `%s`; marker `%s`; core `%d/4`; duration `%s`\n",
			inline(scenario.Cold.Answer.Decision), inline(scenario.Cold.Answer.ContinuityMarker), scenario.Cold.Score.Core, scenario.Cold.Duration.Round(100_000_000))
		fmt.Fprintf(&report, "- Resume tokens: input `%d` (`%d` cached), output `%d`, effective `%d`\n",
			scenario.Resume.Usage.InputTokens, scenario.Resume.Usage.CachedInputTokens, scenario.Resume.Usage.OutputTokens, scenario.Resume.Usage.EffectiveTokens)
		fmt.Fprintf(&report, "- Cold tokens: input `%d` (`%d` cached), output `%d`, effective `%d`\n",
			scenario.Cold.Usage.InputTokens, scenario.Cold.Usage.CachedInputTokens, scenario.Cold.Usage.OutputTokens, scenario.Cold.Usage.EffectiveTokens)
		fmt.Fprintf(&report, "- Recommendation: %s\n", scenario.Recommendation)
		fmt.Fprintf(&report, "- Evidence: `%s`, `%s`, `%s`, `%s`\n",
			scenario.SeedEvents, scenario.CrashEvents, scenario.Resume.Events, scenario.Cold.Events)
		if scenario.Error != "" {
			fmt.Fprintf(&report, "- Error: `%s`\n", inline(scenario.Error))
		}
		if scenario.Resume.CLIError != "" || scenario.Resume.ParseError != "" {
			fmt.Fprintf(&report, "- Resume error: `%s %s`\n", inline(scenario.Resume.CLIError), inline(scenario.Resume.ParseError))
		}
		if scenario.Cold.CLIError != "" || scenario.Cold.ParseError != "" {
			fmt.Fprintf(&report, "- Cold error: `%s %s`\n", inline(scenario.Cold.CLIError), inline(scenario.Cold.ParseError))
		}
		report.WriteByte('\n')
	}

	report.WriteString("## Calibrated routing default\n\n")
	fmt.Fprintf(&report, "- Matching CLI version: `%s`\n", result.RoutingDefault.MatchingVersion)
	fmt.Fprintf(&report, "- Cross-version restore: `%s`\n", result.RoutingDefault.CrossVersion)
	fmt.Fprintf(&report, "- Snapshot fallback: `%s`\n", result.RoutingDefault.SnapshotFallback)
	fmt.Fprintf(&report, "- Basis: %s\n", result.RoutingDefault.Rationale)
	report.WriteString("\nThis is a one-run calibration, not a latency or cache distribution. Rerun after a pinned CLI or model change. Large cache-ratio swings remain visible in the raw counts above rather than being averaged away.\n")
	return report.String()
}

func inline(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "`", "'")
	return strings.TrimSpace(value)
}
