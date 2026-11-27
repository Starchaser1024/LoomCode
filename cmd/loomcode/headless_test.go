package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/agent"
	"github.com/Starchaser1024/LoomCode/internal/config"
	"github.com/Starchaser1024/LoomCode/internal/conversation"
	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/session"
)

type scriptedLLMClient struct {
	turns   []func(conv *conversation.Manager, schemas []map[string]any) ([]llm.StreamEvent, error)
	callIdx int
}

func (s *scriptedLLMClient) Stream(
	_ context.Context,
	conv *conversation.Manager,
	schemas []map[string]any,
) (<-chan llm.StreamEvent, <-chan error) {
	evCh := make(chan llm.StreamEvent, 64)
	errCh := make(chan error, 1)
	idx := s.callIdx
	s.callIdx++

	go func() {
		defer close(evCh)
		defer close(errCh)
		if idx >= len(s.turns) {
			evCh <- llm.TextDelta{Text: "done"}
			evCh <- llm.StreamEnd{StopReason: "end_turn"}
			return
		}
		evs, err := s.turns[idx](conv, schemas)
		for _, ev := range evs {
			evCh <- ev
		}
		if err != nil {
			errCh <- err
		}
	}()
	return evCh, errCh
}

func testAppConfig() *config.AppConfig {
	return &config.AppConfig{
		Providers: []config.ProviderConfig{
			{
				Name:     "primary-anthropic",
				Protocol: "anthropic",
				BaseURL:  "http://127.0.0.1:9",
				Model:    "claude-sonnet-4-6",
				APIKey:   "test-key",
			},
			{
				Name:     "secondary-openai",
				Protocol: "openai-compat",
				BaseURL:  "http://127.0.0.1:9/v1",
				Model:    "gpt-4.1",
				APIKey:   "test-key-2",
			},
		},
	}
}

func TestCLIHelpAndVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := executeCLI(context.Background(), []string{"--help"}, nil, &stdout, &stderr, headlessDeps{}, false)
	if code != 0 {
		t.Fatalf("--help exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	for _, want := range []string{"-p, --prompt", "--output-format", "--permission-mode", "--cwd", "--provider", "--version"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help output missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = executeCLI(context.Background(), []string{"--version"}, nil, &stdout, &stderr, headlessDeps{}, false)
	if code != 0 {
		t.Fatalf("--version exit code = %d, want 0", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout.String()), "loomcode v") {
		t.Errorf("unexpected version output: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = executeCLI(context.Background(), []string{"-p", "hi", "--output-format", "xml"}, nil, &stdout, &stderr, headlessDeps{}, false)
	if code != 1 {
		t.Fatalf("invalid --output-format exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid --output-format") {
		t.Errorf("expected invalid --output-format error, got: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = executeCLI(context.Background(), []string{"-p", "", "--output-format", "json"}, nil, &stdout, &stderr, headlessDeps{}, false)
	if code != 1 {
		t.Fatalf("empty prompt exit code = %d, want 1", code)
	}
	var errPayload HeadlessJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &errPayload); err != nil {
		t.Fatalf("expected valid JSON error payload on stdout, got err=%v raw=%q", err, stdout.String())
	}
	if !strings.Contains(errPayload.Error, "non-empty") {
		t.Errorf("unexpected JSON error field: %q", errPayload.Error)
	}
}

func TestHeadlessTextModeInstructionsSkillsHistoryAndSession(t *testing.T) {
	workDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(workDir, "LOOMCODE.md"), []byte("Always enforce PROJECT_RULE_ALPHA_99.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(workDir, ".loomcode", "skills", "audit-tag")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillBody := "---\nname: audit-tag\ndescription: Tag files during audit\n---\nFollow AUDIT_SOP_MARKER_42 strictly.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillBody), 0o644); err != nil {
		t.Fatal(err)
	}
	helloPath := filepath.Join(workDir, "hello.txt")
	if err := os.WriteFile(helloPath, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var capturedSysPrompt string
	var sawInstructions bool
	var sawPinnedSkill bool

	mock := &scriptedLLMClient{
		turns: []func(conv *conversation.Manager, schemas []map[string]any) ([]llm.StreamEvent, error){
			func(conv *conversation.Manager, _ []map[string]any) ([]llm.StreamEvent, error) {
				for _, m := range conv.GetMessages() {
					if strings.Contains(m.Content, "PROJECT_RULE_ALPHA_99") {
						sawInstructions = true
					}
				}
				return []llm.StreamEvent{
					llm.TextDelta{Text: "Inspecting workspace..."},
					llm.ToolCallComplete{
						ToolID:    "tc_read",
						ToolName:  "ReadFile",
						Arguments: map[string]any{"file_path": filepath.Join(workDir, "hello.txt")},
					},
					llm.ToolCallComplete{
						ToolID:    "tc_skill",
						ToolName:  "LoadSkill",
						Arguments: map[string]any{"name": "audit-tag"},
					},
					llm.StreamEnd{
						StopReason: "tool_use",
						Usage:      llm.UsageInfo{InputTokens: 100, OutputTokens: 25},
					},
				}, nil
			},
			func(conv *conversation.Manager, _ []map[string]any) ([]llm.StreamEvent, error) {
				for _, m := range conv.GetMessages() {
					if strings.Contains(m.Content, "AUDIT_SOP_MARKER_42") {
						sawPinnedSkill = true
					}
				}
				return []llm.StreamEvent{
					llm.ToolCallComplete{
						ToolID:   "tc_edit",
						ToolName: "EditFile",
						Arguments: map[string]any{
							"file_path":  filepath.Join(workDir, "hello.txt"),
							"old_string": "line two",
							"new_string": "line two updated",
						},
					},
					llm.ToolCallComplete{
						ToolID:   "tc_write",
						ToolName: "WriteFile",
						Arguments: map[string]any{
							"file_path": filepath.Join(workDir, "created.txt"),
							"content":   "fresh file\n",
						},
					},
					llm.StreamEnd{
						StopReason: "tool_use",
						Usage:      llm.UsageInfo{InputTokens: 140, OutputTokens: 30},
					},
				}, nil
			},
			func(_ *conversation.Manager, _ []map[string]any) ([]llm.StreamEvent, error) {
				return []llm.StreamEvent{
					llm.TextDelta{Text: "Completed workspace updates."},
					llm.StreamEnd{
						StopReason: "end_turn",
						Usage:      llm.UsageInfo{InputTokens: 160, OutputTokens: 15},
					},
				}, nil
			},
		},
	}

	deps := headlessDeps{
		Config: testAppConfig(),
		ClientFactory: func(_ *config.ProviderConfig, systemPrompt string) (llm.Client, error) {
			capturedSysPrompt = systemPrompt
			return mock, nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := executeCLI(
		context.Background(),
		[]string{"-p", "Update hello.txt and add created.txt", "--cwd", workDir},
		nil,
		&stdout,
		&stderr,
		deps,
		false,
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}

	if !strings.Contains(capturedSysPrompt, "/audit-tag:") {
		t.Errorf("expected SKILL.md catalog in system prompt, got:\n%s", capturedSysPrompt)
	}
	if !sawInstructions {
		t.Error("expected LOOMCODE.md instructions injected into conversation")
	}
	if !sawPinnedSkill {
		t.Error("expected LoadSkill SOP pinned into subsequent turn system reminder")
	}

	wantStdout := "Inspecting workspace...\nCompleted workspace updates.\n"
	if stdout.String() != wantStdout {
		t.Errorf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	if !strings.Contains(stderr.String(), "Execution Summary") || !strings.Contains(stderr.String(), "created.txt") {
		t.Errorf("expected Execution Summary with touched files on stderr, got:\n%s", stderr.String())
	}

	editedData, err := os.ReadFile(helloPath)
	if err != nil || string(editedData) != "line one\nline two updated\n" {
		t.Fatalf("unexpected hello.txt content: %q (err=%v)", string(editedData), err)
	}

	sessions := session.ListSessions(workDir)
	if len(sessions) != 1 {
		t.Fatalf("expected 1 persisted session, got %d", len(sessions))
	}
	msgs := session.LoadSession(workDir, sessions[0].ID)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Errorf("unexpected persisted session messages: %+v", msgs)
	}

	snapshotsPath := filepath.Join(workDir, ".loomcode", "file-history", sessions[0].ID, "snapshots.json")
	if _, err := os.Stat(snapshotsPath); err != nil {
		t.Errorf("expected filehistory snapshots persisted at %s: %v", snapshotsPath, err)
	}
}

func TestHeadlessJSONOutputStdinAndProviderSelect(t *testing.T) {
	workDir := t.TempDir()
	var selectedProviderName string

	mock := &scriptedLLMClient{
		turns: []func(conv *conversation.Manager, schemas []map[string]any) ([]llm.StreamEvent, error){
			func(_ *conversation.Manager, _ []map[string]any) ([]llm.StreamEvent, error) {
				return []llm.StreamEvent{
					llm.ToolCallComplete{
						ToolID:   "tc_w1",
						ToolName: "WriteFile",
						Arguments: map[string]any{
							"file_path": filepath.Join(workDir, "notes.txt"),
							"content":   "alpha\nbeta\n",
						},
					},
					llm.StreamEnd{
						StopReason: "tool_use",
						Usage:      llm.UsageInfo{InputTokens: 50, OutputTokens: 20},
					},
				}, nil
			},
			func(_ *conversation.Manager, _ []map[string]any) ([]llm.StreamEvent, error) {
				return []llm.StreamEvent{
					llm.TextDelta{Text: "Created notes.txt."},
					llm.StreamEnd{
						StopReason: "end_turn",
						Usage:      llm.UsageInfo{InputTokens: 70, OutputTokens: 10},
					},
				}, nil
			},
		},
	}

	deps := headlessDeps{
		Config: testAppConfig(),
		ClientFactory: func(cfg *config.ProviderConfig, _ string) (llm.Client, error) {
			selectedProviderName = cfg.Name
			return mock, nil
		},
	}

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("Create notes.txt with two lines\n")
	code := executeCLI(
		context.Background(),
		[]string{
			"-p", "-",
			"--output-format", "json",
			"--provider", "secondary-openai",
			"--cwd", workDir,
		},
		stdin,
		&stdout,
		&stderr,
		deps,
		false,
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s, stdout: %s)", code, stderr.String(), stdout.String())
	}
	if selectedProviderName != "secondary-openai" {
		t.Errorf("selected provider = %q, want secondary-openai", selectedProviderName)
	}

	var out HeadlessJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON on stdout: %v\nraw:\n%s", err, stdout.String())
	}
	if out.SessionID == "" {
		t.Error("expected non-empty session_id")
	}
	if out.AssistantText != "Created notes.txt." {
		t.Errorf("assistant_text = %q, want %q", out.AssistantText, "Created notes.txt.")
	}
	if out.Error != "" {
		t.Errorf("unexpected error field: %q", out.Error)
	}
	if out.Receipt.Turns != 1 || out.Receipt.Steps != 2 {
		t.Errorf("receipt turns/steps = %d/%d, want 1/2", out.Receipt.Turns, out.Receipt.Steps)
	}
	if out.Receipt.Tokens.InputTokens != 120 || out.Receipt.Tokens.OutputTokens != 30 {
		t.Errorf("unexpected receipt tokens: %+v", out.Receipt.Tokens)
	}
	if len(out.Receipt.TouchedFiles) != 1 || out.Receipt.TouchedFiles[0].Path != "notes.txt" || out.Receipt.TouchedFiles[0].Action != agent.FileCreated {
		t.Errorf("unexpected touched_files: %+v", out.Receipt.TouchedFiles)
	}
}

func TestHeadlessDefaultPermissionModeRejectsInteractiveWithoutHanging(t *testing.T) {
	workDir := t.TempDir()
	blockedFile := filepath.Join(workDir, "blocked.txt")
	var toolErrorSeen string

	mock := &scriptedLLMClient{
		turns: []func(conv *conversation.Manager, schemas []map[string]any) ([]llm.StreamEvent, error){
			func(_ *conversation.Manager, _ []map[string]any) ([]llm.StreamEvent, error) {
				return []llm.StreamEvent{
					llm.ToolCallComplete{
						ToolID:   "tc_blocked_write",
						ToolName: "WriteFile",
						Arguments: map[string]any{
							"file_path": blockedFile,
							"content":   "should not be written",
						},
					},
					llm.StreamEnd{
						StopReason: "tool_use",
						Usage:      llm.UsageInfo{InputTokens: 40, OutputTokens: 12},
					},
				}, nil
			},
			func(conv *conversation.Manager, _ []map[string]any) ([]llm.StreamEvent, error) {
				msgs := conv.GetMessages()
				for i := len(msgs) - 1; i >= 0; i-- {
					if len(msgs[i].ToolResults) > 0 {
						toolErrorSeen = msgs[i].ToolResults[0].Content
						break
					}
				}
				return []llm.StreamEvent{
					llm.TextDelta{Text: "Write requires elevated permission mode."},
					llm.StreamEnd{
						StopReason: "end_turn",
						Usage:      llm.UsageInfo{InputTokens: 55, OutputTokens: 10},
					},
				}, nil
			},
		},
	}

	deps := headlessDeps{
		Config: testAppConfig(),
		ClientFactory: func(_ *config.ProviderConfig, _ string) (llm.Client, error) {
			return mock, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	code := executeCLI(
		ctx,
		[]string{
			"--prompt", "Try writing blocked.txt",
			"--permission-mode", "default",
			"--output-format", "json",
			"--cwd", workDir,
		},
		nil,
		&stdout,
		&stderr,
		deps,
		false,
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}

	if _, err := os.Stat(blockedFile); !os.IsNotExist(err) {
		t.Fatalf("blocked.txt should not have been created in default mode")
	}
	if !strings.Contains(toolErrorSeen, "Permission denied in headless mode") || !strings.Contains(toolErrorSeen, `"default"`) {
		t.Errorf("expected descriptive headless permission error, got: %q", toolErrorSeen)
	}

	var out HeadlessJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal JSON: %v", err)
	}
	if out.Receipt.HumanInterventions != 0 {
		t.Errorf("HumanInterventions = %d, want 0 in headless mode", out.Receipt.HumanInterventions)
	}
	if len(out.Receipt.FailuresRecoveries) == 0 {
		t.Error("expected rejected tool call recorded in receipt failures_and_recoveries")
	}
}

func TestHeadlessLLMErrorExitCodeOne(t *testing.T) {
	workDir := t.TempDir()
	mock := &scriptedLLMClient{
		turns: []func(conv *conversation.Manager, schemas []map[string]any) ([]llm.StreamEvent, error){
			func(_ *conversation.Manager, _ []map[string]any) ([]llm.StreamEvent, error) {
				return nil, &llm.AuthenticationError{Message: "invalid api key"}
			},
		},
	}
	deps := headlessDeps{
		Config: testAppConfig(),
		ClientFactory: func(_ *config.ProviderConfig, _ string) (llm.Client, error) {
			return mock, nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := executeCLI(
		context.Background(),
		[]string{"-p", "ping", "--output-format", "json", "--cwd", workDir},
		nil,
		&stdout,
		&stderr,
		deps,
		false,
	)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	var out HeadlessJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("expected valid JSON payload on error: %v\nraw: %s", err, stdout.String())
	}
	if !strings.Contains(out.Error, "invalid api key") {
		t.Errorf("expected auth error in JSON payload, got %q", out.Error)
	}
	if out.SessionID == "" {
		t.Error("expected session_id even on runtime LLM error")
	}
}

func TestHeadlessEndToEndAnthropicHTTPServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/models/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"claude-sonnet-4-6","max_input_tokens":200000}`)
			return
		}
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"id":"msg_e2e","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":85,"output_tokens":1,"cache_read_input_tokens":300,"cache_creation_input_tokens":0}}}`+"\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\n")
		_, _ = io.WriteString(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Headless HTTP OK"}}`+"\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\n")
		_, _ = io.WriteString(w, `data: {"type":"content_block_stop","index":0}`+"\n\n")
		_, _ = io.WriteString(w, "event: message_delta\n")
		_, _ = io.WriteString(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":18}}`+"\n\n")
		_, _ = io.WriteString(w, "event: message_stop\n")
		_, _ = io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()

	workDir := t.TempDir()
	cfgDir := filepath.Join(workDir, ".loomcode")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgYAML := "permission_mode: acceptEdits\nproviders:\n  - name: local-mock\n    protocol: anthropic\n    base_url: " + srv.URL + "\n    model: claude-sonnet-4-6\n    api_key: test-key\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := executeCLI(
		context.Background(),
		[]string{"-p", "Verify HTTP SSE pipeline", "--cwd", workDir, "--output-format", "json"},
		nil,
		&stdout,
		&stderr,
		headlessDeps{},
		false,
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s, stdout: %s)", code, stderr.String(), stdout.String())
	}

	var out HeadlessJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal JSON: %v\nraw: %s", err, stdout.String())
	}
	if out.AssistantText != "Headless HTTP OK" {
		t.Errorf("assistant_text = %q, want %q", out.AssistantText, "Headless HTTP OK")
	}
	if out.Receipt.Tokens.InputTokens != 85 || out.Receipt.Tokens.OutputTokens != 18 || out.Receipt.Tokens.CacheReadTokens != 300 {
		t.Errorf("unexpected tokens: %+v", out.Receipt.Tokens)
	}
}
