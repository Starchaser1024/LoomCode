package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/commands"
	"github.com/Starchaser1024/LoomCode/internal/conversation"
	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/permissions"
	"github.com/Starchaser1024/LoomCode/internal/planfile"
	"github.com/Starchaser1024/LoomCode/internal/tools"
)

type scriptedStep struct {
	events []llm.StreamEvent
	err    error
}

type scriptedStreamClient struct {
	steps []scriptedStep
	idx   int
}

func (s *scriptedStreamClient) Stream(_ context.Context, _ *conversation.Manager, _ []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	evCh := make(chan llm.StreamEvent, 32)
	errCh := make(chan error, 1)
	stepIdx := s.idx
	s.idx++
	go func() {
		defer close(evCh)
		defer close(errCh)
		if stepIdx >= len(s.steps) {
			evCh <- llm.TextDelta{Text: "done"}
			evCh <- llm.StreamEnd{StopReason: "end_turn"}
			return
		}
		st := s.steps[stepIdx]
		for _, ev := range st.events {
			evCh <- ev
		}
		if st.err != nil {
			errCh <- st.err
		}
	}()
	return evCh, errCh
}

type stubBashTool struct {
	outputs []tools.ToolResult
	idx     int
}

func (t *stubBashTool) Name() string                 { return "Bash" }
func (t *stubBashTool) Description() string          { return "stub bash" }
func (t *stubBashTool) Category() tools.ToolCategory { return tools.CategoryCommand }
func (t *stubBashTool) Schema() map[string]any {
	return map[string]any{
		"name":        "Bash",
		"description": "stub bash",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
		},
	}
}
func (t *stubBashTool) Execute(_ context.Context, args map[string]any) tools.ToolResult {
	i := t.idx
	t.idx++
	if i < len(t.outputs) {
		return t.outputs[i]
	}
	cmd, _ := args["command"].(string)
	return tools.ToolResult{Output: "$ " + cmd + "\n(exit code 0)"}
}

func TestExecutionReceiptTracksFilesDiffsRetriesAndSelfRepair(t *testing.T) {
	dir := t.TempDir()
	planfile.ResetPlanPath()
	defer planfile.ResetPlanPath()
	if err := planfile.SavePlan(dir, "# Fix Plan\n- [ ] Fix boundary condition in calc.go\n- [ ] Add regression test\n"); err != nil {
		t.Fatal(err)
	}

	calcPath := filepath.Join(dir, "calc.go")
	helperPath := filepath.Join(dir, "calc_helper.go")
	if err := os.WriteFile(calcPath, []byte("package calc\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dt := tools.CreateDefaultTools()
	bashStub := &stubBashTool{
		outputs: []tools.ToolResult{
			{Output: "$ go test ./...\n--- FAIL: TestAdd\n(exit code 1)", IsError: true},
			{Output: "$ go test ./...\nok  \texample/calc\t0.003s\n(exit code 0)", IsError: false},
		},
	}
	dt.Registry.Register(bashStub)

	client := &scriptedStreamClient{
		steps: []scriptedStep{
			// 1. Transient 502 error before stream succeeds.
			{err: &llm.LLMError{StatusCode: 502, Message: "API error (502): bad gateway"}},
			// 2. Read calc.go and run failing go test.
			{events: []llm.StreamEvent{
				llm.ToolCallComplete{ToolID: "c1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": calcPath}},
				llm.ToolCallComplete{ToolID: "c2", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
				llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 120, OutputTokens: 40, CacheReadTokens: 80}},
			}},
			// 3. Edit calc.go, create calc_helper.go, and re-run go test (now passes).
			{events: []llm.StreamEvent{
				llm.ToolCallComplete{
					ToolID:   "c3",
					ToolName: "EditFile",
					Arguments: map[string]any{
						"file_path":  calcPath,
						"old_string": "return a - b",
						"new_string": "return addImpl(a, b)",
					},
				},
				llm.ToolCallComplete{
					ToolID:   "c4",
					ToolName: "WriteFile",
					Arguments: map[string]any{
						"file_path": helperPath,
						"content":   "package calc\n\nfunc addImpl(a, b int) int {\n\treturn a + b\n}\n",
					},
				},
				llm.ToolCallComplete{ToolID: "c5", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
				llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 180, OutputTokens: 65, CacheReadTokens: 100}},
			}},
			// 4. Final response.
			{events: []llm.StreamEvent{
				llm.TextDelta{Text: "Fixed Add and verified tests."},
				llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 50, OutputTokens: 15, CacheReadTokens: 150}},
			}},
		},
	}

	ag := New(client, dt.Registry, "anthropic")
	ag.WorkDir = dir
	ag.SessionID = "sess-receipt-1"
	ag.RetryBaseDelay = time.Millisecond
	// ModeDefault triggers Ask for Write/Command tools -> exercises human intervention counter.
	ag.Checker = permissions.NewChecker(permissions.NewPathSandbox(dir), nil, permissions.ModeDefault)

	conv := conversation.NewManager()
	conv.AddUserMessage("fix Add in calc.go")
	_ = collectEvents(ag.Run(context.Background(), conv))

	receipt := ag.GetReceipt()
	if receipt.Turns != 1 || receipt.Steps != 3 {
		t.Fatalf("expected 1 turn / 3 steps, got %d turns / %d steps", receipt.Turns, receipt.Steps)
	}
	if receipt.Tokens.InputTokens != 350 || receipt.Tokens.OutputTokens != 120 || receipt.Tokens.CacheReadTokens != 330 {
		t.Fatalf("unexpected token totals: %+v", receipt.Tokens)
	}
	if receipt.HumanInterventions != 4 {
		t.Fatalf("expected 4 human approval interventions (2 Bash + EditFile + WriteFile), got %d", receipt.HumanInterventions)
	}
	if len(receipt.ActivePlanSteps) != 2 {
		t.Fatalf("expected 2 plan steps, got %v", receipt.ActivePlanSteps)
	}

	if len(receipt.TouchedFiles) != 2 {
		t.Fatalf("expected 2 touched files, got %+v", receipt.TouchedFiles)
	}
	fileByPath := map[string]FileChangeStat{}
	for _, f := range receipt.TouchedFiles {
		fileByPath[f.Path] = f
	}
	if mod, ok := fileByPath["calc.go"]; !ok || mod.Action != FileModified || mod.LinesAdded != 1 || mod.LinesDeleted != 1 {
		t.Fatalf("unexpected calc.go diff stat: %+v", mod)
	}
	if created, ok := fileByPath["calc_helper.go"]; !ok || created.Action != FileCreated || created.LinesAdded != 5 {
		t.Fatalf("unexpected calc_helper.go diff stat: %+v", created)
	}

	if len(receipt.Commands) != 2 || receipt.Commands[0].Passed || !receipt.Commands[1].Passed {
		t.Fatalf("expected [FAIL, PASS] verification commands, got %+v", receipt.Commands)
	}

	foundRetry := false
	foundSelfRepair := false
	for _, rec := range receipt.FailuresRecoveries {
		if rec.Kind == "api_retry" && rec.Outcome == "recovered" {
			foundRetry = true
		}
		if rec.Kind == "test_self_repair" && rec.Outcome == "recovered" {
			foundSelfRepair = true
		}
	}
	if !foundRetry || !foundSelfRepair {
		t.Fatalf("expected recovered api_retry and test_self_repair, got %+v", receipt.FailuresRecoveries)
	}

	formatted := receipt.FormatText()
	for _, mustContain := range []string{
		"Execution Summary",
		"[modified] calc.go (+1/-1)",
		"[created] calc_helper.go (+5/-0)",
		"[recovered] api_retry",
		"[recovered] test_self_repair",
	} {
		if !strings.Contains(formatted, mustContain) {
			t.Errorf("FormatText() missing %q:\n%s", mustContain, formatted)
		}
	}

	exportPath := filepath.Join(dir, "receipt.json")
	if err := receipt.ExportJSONFile(exportPath); err != nil {
		t.Fatalf("ExportJSONFile failed: %v", err)
	}
	raw, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ExecutionReceipt
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("invalid exported JSON: %v", err)
	}
	if decoded.SessionID != "sess-receipt-1" || len(decoded.TouchedFiles) != 2 {
		t.Fatalf("unexpected decoded receipt: %+v", decoded)
	}
}

func TestSummaryAndInspectSlashCommandRegistered(t *testing.T) {
	reg := commands.CreateDefaultRegistry()
	cmd := reg.Find("summary")
	if cmd == nil {
		t.Fatal("expected /summary command in default registry")
	}
	alias := reg.Find("inspect")
	if alias == nil || alias.Name != "summary" {
		t.Fatalf("expected /inspect alias to resolve to /summary, got %+v", alias)
	}
	out := cmd.Handler(&commands.Context{
		Args: "json",
		ExecutionSummary: func(args string) string {
			return `{"mode":"` + args + `"}`
		},
	})
	if out != `{"mode":"json"}` {
		t.Fatalf("unexpected handler output: %s", out)
	}
}
