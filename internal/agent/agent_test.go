package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/conversation"
	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/prompt"
	"github.com/Starchaser1024/LoomCode/internal/skills"
	"github.com/Starchaser1024/LoomCode/internal/tools"
)

// --- Mock infrastructure ---

// mockClient returns scripted responses in order.
type mockClient struct {
	responses [][]llm.StreamEvent
	callIdx   int
}

func (m *mockClient) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	ch := make(chan llm.StreamEvent, 64)
	errCh := make(chan error, 1)
	go func() {
		defer close(ch)
		defer close(errCh)
		if m.callIdx >= len(m.responses) {
			ch <- llm.TextDelta{Text: "[mock: no more scripted responses]"}
			ch <- llm.StreamEnd{StopReason: "end_turn"}
			return
		}
		for _, ev := range m.responses[m.callIdx] {
			ch <- ev
		}
		m.callIdx++
	}()
	return ch, errCh
}

// dynamicMock inspects the conversation to decide what to respond.
// Each handler receives the current conversation and returns events.
type dynamicMock struct {
	handlers []func(msgs []conversation.Message) []llm.StreamEvent
	callIdx  int
}

func (m *dynamicMock) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	ch := make(chan llm.StreamEvent, 64)
	errCh := make(chan error, 1)
	msgs := conv.GetMessages()
	go func() {
		defer close(ch)
		defer close(errCh)
		if m.callIdx >= len(m.handlers) {
			ch <- llm.TextDelta{Text: "[dynamic mock: no more handlers]"}
			ch <- llm.StreamEnd{StopReason: "end_turn"}
			return
		}
		events := m.handlers[m.callIdx](msgs)
		for _, ev := range events {
			ch <- ev
		}
		m.callIdx++
	}()
	return ch, errCh
}

// mockTool returns a fixed result.
type mockTool struct {
	name   string
	result string
}

func (t *mockTool) Name() string                 { return t.name }
func (t *mockTool) Description() string          { return "mock tool" }
func (t *mockTool) Category() tools.ToolCategory { return tools.CategoryRead }
func (t *mockTool) Schema() map[string]any {
	return map[string]any{
		"name": t.name, "description": "mock",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
	}
}
func (t *mockTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	return tools.ToolResult{Output: t.result}
}

// --- Helpers ---

func collectEvents(ch <-chan AgentEvent) []AgentEvent {
	var events []AgentEvent
	for ev := range ch {
		if perm, ok := ev.(PermissionRequestEvent); ok {
			perm.ResponseCh <- PermAllow
			continue
		}
		events = append(events, ev)
	}
	return events
}

func getStreamText(events []AgentEvent) string {
	var sb strings.Builder
	for _, ev := range events {
		if st, ok := ev.(StreamText); ok {
			sb.WriteString(st.Text)
		}
	}
	return sb.String()
}

func getToolResults(events []AgentEvent) []ToolResultEvent {
	var results []ToolResultEvent
	for _, ev := range events {
		if tr, ok := ev.(ToolResultEvent); ok {
			results = append(results, tr)
		}
	}
	return results
}

func buildSkillSystemPrompt(skillsDir string, catalog *skills.Catalog) string {
	var sb strings.Builder
	sb.WriteString("## Available Skills\n\n")
	sb.WriteString(fmt.Sprintf("Skills are installed at: %s\n", skillsDir))
	sb.WriteString("When creating new skills, always place them under this directory as <skill-name>/SKILL.md.\n\n")
	sb.WriteString("The following skills are available. When the user invokes /<name>, follow that skill's instructions.\n\n")
	for _, meta := range catalog.List() {
		desc := meta.Description
		if len(desc) > 200 {
			desc = desc[:200] + "…"
		}
		sb.WriteString(fmt.Sprintf("- /%s: %s\n", meta.Name, desc))
	}
	env := prompt.DetectEnvironment(".")
	return prompt.BuildSystemPrompt(env, prompt.BuildOptions{SkillSection: sb.String()})
}

func runConversationRound(ag *Agent, conv *conversation.Manager, userMsg string) (string, []AgentEvent) {
	conv.AddUserMessage(userMsg)
	events := collectEvents(ag.Run(context.Background(), conv))
	return getStreamText(events), events
}

// --- Basic agent tests ---

func TestAgentSimpleResponse(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{{
		llm.TextDelta{Text: "Hello, "},
		llm.TextDelta{Text: "world!"},
		llm.StreamEnd{StopReason: "end_turn"},
	}}}
	ag := New(client, tools.NewRegistry(), "anthropic")
	conv := conversation.NewManager()
	text, events := runConversationRound(ag, conv, "hi")
	if text != "Hello, world!" {
		t.Errorf("got %q, want %q", text, "Hello, world!")
	}
	hasComplete := false
	for _, ev := range events {
		if _, ok := ev.(LoopComplete); ok {
			hasComplete = true
		}
	}
	if !hasComplete {
		t.Error("missing LoopComplete event")
	}
}

func TestAgentToolCallLoop(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{
		{
			llm.TextDelta{Text: "Let me read that."},
			llm.ToolCallStart{ToolName: "ReadFile", ToolID: "t1"},
			llm.ToolCallComplete{ToolID: "t1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": "/tmp/x"}},
			llm.StreamEnd{StopReason: "tool_use"},
		},
		{
			llm.TextDelta{Text: "File says: hello from mock"},
			llm.StreamEnd{StopReason: "end_turn"},
		},
	}}
	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "ReadFile", result: "hello from mock"})
	ag := New(client, reg, "anthropic")
	conv := conversation.NewManager()
	text, events := runConversationRound(ag, conv, "read it")
	if !strings.Contains(text, "hello from mock") {
		t.Errorf("response %q should mention tool output", text)
	}
	trs := getToolResults(events)
	if len(trs) != 1 || trs[0].Output != "hello from mock" {
		t.Errorf("unexpected tool results: %+v", trs)
	}
}

func TestAgentMaxIterations(t *testing.T) {
	loop := []llm.StreamEvent{
		llm.ToolCallStart{ToolName: "Glob", ToolID: "t"},
		llm.ToolCallComplete{ToolID: "t", ToolName: "Glob", Arguments: map[string]any{"pattern": "*"}},
		llm.StreamEnd{StopReason: "tool_use"},
	}
	responses := make([][]llm.StreamEvent, 10)
	for i := range responses {
		responses[i] = loop
	}
	client := &mockClient{responses: responses}
	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "Glob", result: "f.txt"})
	ag := New(client, reg, "anthropic")
	ag.MaxIterations = 3
	conv := conversation.NewManager()
	_, events := runConversationRound(ag, conv, "loop")
	found := false
	for _, ev := range events {
		if e, ok := ev.(ErrorEvent); ok && strings.Contains(e.Message, "maximum iterations") {
			found = true
		}
	}
	if !found {
		t.Error("expected max iterations error")
	}
}

func TestAgentWithThinking(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{{
		llm.ThinkingDelta{Text: "hmm..."},
		llm.ThinkingComplete{Thinking: "hmm...", Signature: "sig_1"},
		llm.TextDelta{Text: "My answer."},
		llm.StreamEnd{StopReason: "end_turn"},
	}}}
	ag := New(client, tools.NewRegistry(), "anthropic")
	conv := conversation.NewManager()
	runConversationRound(ag, conv, "think")
	msgs := conv.GetMessages()
	last := msgs[len(msgs)-1]
	if len(last.ThinkingBlocks) == 0 || last.ThinkingBlocks[0].Signature != "sig_1" {
		t.Error("thinking block not stored correctly")
	}
}

// --- Multi-round conversation tests ---

func TestMultiRoundConversation(t *testing.T) {
	client := &dynamicMock{handlers: []func([]conversation.Message) []llm.StreamEvent{
		func(msgs []conversation.Message) []llm.StreamEvent {
			last := msgs[len(msgs)-1]
			if !strings.Contains(last.Content, "refactor") {
				t.Errorf("round 1: expected user msg about refactor, got %q", last.Content)
			}
			return []llm.StreamEvent{
				llm.TextDelta{Text: "Which file do you want me to refactor? And what style — extract functions, simplify conditionals, or both?"},
				llm.StreamEnd{StopReason: "end_turn"},
			}
		},
		func(msgs []conversation.Message) []llm.StreamEvent {
			last := msgs[len(msgs)-1]
			if !strings.Contains(last.Content, "main.go") {
				t.Errorf("round 2: expected user msg about main.go, got %q", last.Content)
			}
			return []llm.StreamEvent{
				llm.TextDelta{Text: "I'll read main.go first."},
				llm.ToolCallStart{ToolName: "ReadFile", ToolID: "r1"},
				llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": "main.go"}},
				llm.StreamEnd{StopReason: "tool_use"},
			}
		},
		func(msgs []conversation.Message) []llm.StreamEvent {
			found := false
			for _, m := range msgs {
				for _, tr := range m.ToolResults {
					if strings.Contains(tr.Content, "func main") {
						found = true
					}
				}
			}
			if !found {
				t.Error("round 2 continued: tool result not found in conversation")
			}
			return []llm.StreamEvent{
				llm.TextDelta{Text: "Here's the refactored version:\n```go\nfunc main() {\n    run()\n}\n```\nI extracted the logic into a `run()` function."},
				llm.StreamEnd{StopReason: "end_turn"},
			}
		},
	}}

	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "ReadFile", result: "package main\n\nfunc main() {\n    // lots of code\n    fmt.Println(\"hello\")\n}"})

	ag := New(client, reg, "anthropic")
	conv := conversation.NewManager()

	text1, _ := runConversationRound(ag, conv, "please refactor this code")
	if !strings.Contains(text1, "Which file") {
		t.Errorf("round 1: agent should ask clarification, got: %s", text1)
	}

	text2, events2 := runConversationRound(ag, conv, "main.go, extract functions please")
	if !strings.Contains(text2, "refactored") {
		t.Errorf("round 2: agent should produce refactored code, got: %s", text2)
	}

	trs := getToolResults(events2)
	if len(trs) != 1 || trs[0].ToolName != "ReadFile" {
		t.Errorf("expected ReadFile tool call, got: %+v", trs)
	}

	msgs := conv.GetMessages()
	if len(msgs) < 6 {
		t.Fatalf("expected 6+ messages, got %d", len(msgs))
	}
	if msgs[0].Content != "please refactor this code" {
		t.Error("msg[0] should be first user message")
	}
	if msgs[2].Content != "main.go, extract functions please" {
		t.Error("msg[2] should be second user message")
	}
}

func TestAgentOnLoopCompleteFiresOnFinalTurn(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{{
		llm.TextDelta{Text: "done"},
		llm.StreamEnd{StopReason: "end_turn"},
	}}}
	ag := New(client, tools.NewRegistry(), "anthropic")

	got := make(chan *conversation.Manager, 1)
	ag.OnLoopComplete = func(conv *conversation.Manager) {
		got <- conv
	}

	conv := conversation.NewManager()
	runConversationRound(ag, conv, "hi")

	select {
	case received := <-got:
		if received != conv {
			t.Error("callback should receive the same conv pointer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnLoopComplete was not called within 2s")
	}
}

func TestAgentOnLoopCompleteSkippedOnError(t *testing.T) {
	loop := []llm.StreamEvent{
		llm.ToolCallStart{ToolName: "Glob", ToolID: "t"},
		llm.ToolCallComplete{ToolID: "t", ToolName: "Glob", Arguments: map[string]any{"pattern": "*"}},
		llm.StreamEnd{StopReason: "tool_use"},
	}
	responses := make([][]llm.StreamEvent, 5)
	for i := range responses {
		responses[i] = loop
	}
	client := &mockClient{responses: responses}
	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "Glob", result: "f.txt"})
	ag := New(client, reg, "anthropic")
	ag.MaxIterations = 2

	called := make(chan struct{}, 1)
	ag.OnLoopComplete = func(*conversation.Manager) { called <- struct{}{} }

	conv := conversation.NewManager()
	runConversationRound(ag, conv, "spin")

	select {
	case <-called:
		t.Error("OnLoopComplete must not fire when loop exits via error")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestFilterSchemasByName(t *testing.T) {
	schemas := []map[string]any{
		{"name": "Agent", "x": 1},
		{"name": "Bash", "x": 2},
		{"name": "ReadFile", "x": 3},
	}
	allow := func(name string) bool { return name == "Agent" || name == "ReadFile" }
	got := filterSchemasByName(schemas, allow)
	if len(got) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(got))
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s["name"].(string)] = true
	}
	if !names["Agent"] || !names["ReadFile"] || names["Bash"] {
		t.Errorf("filter kept the wrong set: %v", names)
	}
}

func TestFilterSchemasByNameEmptyInput(t *testing.T) {
	got := filterSchemasByName(nil, func(string) bool { return true })
	if len(got) != 0 {
		t.Errorf("expected empty result, got %d entries", len(got))
	}
}
