package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/Starchaser1024/LoomCode/internal/tools"
)

func init() {
	builtinToolFactories["diff_summary"] = func(schema ToolSchema) tools.Tool {
		return &diffSummaryTool{schema: schema}
	}
}

// diffSummaryTool is the compiled-in implementation of the diff_summary tool
// declared in builtins/review/tool.json. It parses unified git diff output and
// returns structured per-file statistics, hunk context headers, and heuristic
// regression risk signals so the review SOP can prioritize high-risk changes.
type diffSummaryTool struct {
	schema ToolSchema
}

func (t *diffSummaryTool) Name() string                 { return t.schema.Name }
func (t *diffSummaryTool) Description() string          { return t.schema.Description }
func (t *diffSummaryTool) Category() tools.ToolCategory { return tools.CategoryRead }

func (t *diffSummaryTool) Schema() map[string]any {
	return map[string]any{
		"name":         t.schema.Name,
		"description":  t.schema.Description,
		"input_schema": t.schema.InputSchema,
	}
}

func (t *diffSummaryTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	rawDiff, _ := args["diff"].(string)
	if strings.TrimSpace(rawDiff) == "" {
		workDir, _ := args["work_dir"].(string)
		if workDir == "" {
			workDir = "."
		}
		target, _ := args["target"].(string)
		gitArgs := []string{"-C", workDir, "diff"}
		if strings.TrimSpace(target) != "" {
			gitArgs = append(gitArgs, strings.Fields(target)...)
		}
		out, err := exec.CommandContext(ctx, "git", gitArgs...).CombinedOutput()
		if err != nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("git diff failed: %v (%s)", err, strings.TrimSpace(string(out))),
				IsError: true,
			}
		}
		rawDiff = string(out)
	}

	report := summarizeUnifiedDiff(rawDiff)
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return tools.ToolResult{Output: fmt.Sprintf("marshal diff summary: %v", err), IsError: true}
	}
	return tools.ToolResult{Output: string(encoded)}
}

// DiffSummaryReport is the JSON payload emitted by diff_summary.
type DiffSummaryReport struct {
	FilesChanged    int              `json:"files_changed"`
	TotalInsertions int              `json:"total_insertions"`
	TotalDeletions  int              `json:"total_deletions"`
	Files           []DiffFileStat   `json:"files"`
	ModifiedSymbols []string         `json:"modified_symbols,omitempty"`
	RiskSignals     []string         `json:"risk_signals,omitempty"`
}

// DiffFileStat records line-level churn for a single path in a unified diff.
type DiffFileStat struct {
	Path       string `json:"path"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	Hunks      int    `json:"hunks"`
}

var (
	hunkHeaderRe = regexp.MustCompile(`^@@ -[0-9,]+ \+[0-9,]+ @@\s*(.*)$`)
	riskRules    = []struct {
		label   string
		pattern *regexp.Regexp
	}{
		{"concurrency primitive modified (goroutine/mutex/channel)", regexp.MustCompile(`\b(go\s+[a-zA-Z_]|sync\.(Mutex|RWMutex|WaitGroup)|chan\s+|<-\s*[a-zA-Z_])`)},
		{"process execution or shell invocation touched", regexp.MustCompile(`\b(exec\.Command|exec\.CommandContext|os\.StartProcess)\b`)},
		{"security/auth/permission logic modified", regexp.MustCompile(`(?i)\b(permission|sandbox|token|secret|apikey|api_key|auth|unsafe\.)\b`)},
		{"database/query surface modified", regexp.MustCompile(`(?i)\b(SELECT\s+|INSERT\s+INTO|UPDATE\s+|DELETE\s+FROM|sql\.DB|ExecContext|QueryContext)\b`)},
	}
)

func summarizeUnifiedDiff(raw string) DiffSummaryReport {
	var report DiffSummaryReport
	var current *DiffFileStat
	seenSymbols := make(map[string]bool)
	seenSignals := make(map[string]bool)

	flush := func() {
		if current != nil {
			report.Files = append(report.Files, *current)
			report.FilesChanged++
			report.TotalInsertions += current.Insertions
			report.TotalDeletions += current.Deletions
			current = nil
		}
	}

	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			parts := strings.Fields(line)
			filePath := ""
			if len(parts) >= 4 {
				filePath = strings.TrimPrefix(parts[3], "b/")
			}
			current = &DiffFileStat{Path: filePath}
		case strings.HasPrefix(line, "deleted file mode") && current != nil:
			if strings.HasSuffix(current.Path, "_test.go") || strings.Contains(current.Path, ".test.") {
				sig := "test file deleted: " + current.Path
				if !seenSignals[sig] {
					seenSignals[sig] = true
					report.RiskSignals = append(report.RiskSignals, sig)
				}
			}
		case strings.HasPrefix(line, "@@ ") && current != nil:
			current.Hunks++
			if m := hunkHeaderRe.FindStringSubmatch(line); len(m) == 2 {
				sym := strings.TrimSpace(m[1])
				if sym != "" && !seenSymbols[sym] && len(report.ModifiedSymbols) < 25 {
					seenSymbols[sym] = true
					report.ModifiedSymbols = append(report.ModifiedSymbols, sym)
				}
			}
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") && current != nil:
			current.Insertions++
			added := line[1:]
			for _, rule := range riskRules {
				if !seenSignals[rule.label] && rule.pattern.MatchString(added) {
					seenSignals[rule.label] = true
					report.RiskSignals = append(report.RiskSignals, rule.label)
				}
			}
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") && current != nil:
			current.Deletions++
		}
	}
	flush()

	return report
}
