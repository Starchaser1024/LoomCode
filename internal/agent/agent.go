// Package agent implements LoomCode's core streaming ReAct loop, concurrent tool execution,
// context-window management, permission gating, and lifecycle hook dispatch.
package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/compact"
	"github.com/Starchaser1024/LoomCode/internal/conversation"
	"github.com/Starchaser1024/LoomCode/internal/filehistory"
	"github.com/Starchaser1024/LoomCode/internal/hooks"
	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/permissions"
	"github.com/Starchaser1024/LoomCode/internal/planfile"
	"github.com/Starchaser1024/LoomCode/internal/prompt"
	"github.com/Starchaser1024/LoomCode/internal/toolresult"
	"github.com/Starchaser1024/LoomCode/internal/tools"
)

const (
	maxTokensCeiling          = 64000
	maxOutputTokensRecoveries = 3
)

type Agent struct {
	Client        llm.Client
	Registry      *tools.Registry
	Protocol      string
	WorkDir       string
	MaxIterations int
	ContextWindow int
	// MaxOutputTokens is the model's max output budget; used by Layer 2 to
	// compute the effective window for the compaction threshold. Zero falls
	// back to the summaryOutputReserve default inside compact.
	MaxOutputTokens int
	Checker         *permissions.Checker
	Hooks           *hooks.Engine
	// SessionID identifies the on-disk session log this agent appends to. When
	// set, Layer 2 compaction writes a compact_boundary record into that session
	// so a later resume can rebuild the compacted state instead of replaying the
	// full pre-compaction transcript. Empty disables boundary persistence (tests,
	// one-shot callers).
	SessionID      string
	NotificationFn func() []string
	// ToolNameFilter, when non-nil, drops any tool whose Name returns false from the schemas sent to
	// the LLM. The filter is consulted at the top of every iteration so callers can flip Coordinator
	// Mode on or off (e.g., when a team is created/torn down) without restarting the agent.
	Instructions   string
	MemoryContent  string
	ToolNameFilter func(name string) bool
	// OnLoopComplete, when non-nil, is invoked fire-and-forget after the agent reaches LoopComplete
	// (final assistant message, no tool calls remaining). Used by background memory extraction.
	// Failures are silent and must not block the main loop. The callback receives the live
	// conversation — do not mutate it from another goroutine.
	OnLoopComplete  func(conv *conversation.Manager)
	FileHistory     *filehistory.History
	compactTracking compact.AutoCompactTrackingState
	// ReplacementState carries the per-conversation-thread tool-result
	// decision log across iterations. Lazily created on first Run; forks
	// inherit a Clone() so they share the parent's frozen decisions but do
	// not write back into the parent's maps. Exposed so external code (e.g.
	// AgentTool fork-spawn) can pre-populate from a parent state.
	ReplacementState *toolresult.ContentReplacementState
	// RecoveryState holds the snapshots needed to rebuild working context
	// after Layer 2 collapses the conversation into a summary: most-recent
	// file reads and skill invocations. The struct is concurrency-safe so
	// the streaming executor can write to it from multiple goroutines.
	RecoveryState *compact.RecoveryState
	// Receipt aggregates structured execution telemetry (touched files with diff stats,
	// verification commands, retries/self-repairs, human interventions, tokens) for /summary.
	Receipt *ReceiptTracker
	// NonInteractive disables interactive TUI approval prompts (PermissionRequestEvent).
	// When true, any tool decision that requires interactive confirmation (permissions.Ask)
	// is immediately rejected with a descriptive tool error instead of blocking on a dialog.
	NonInteractive bool
	// RetryBaseDelay overrides the default backoff base duration for transient 429/5xx retries
	// when non-zero (used by deterministic integration tests).
	RetryBaseDelay time.Duration
	eventCh        chan AgentEvent
	// activeSkills holds Skill SOPs that have been pinned to the environment context. Each Agent Loop
	// iteration re-injects all entries as system-reminders, so the model sees them at the most
	// prominent position regardless of how long the conversation grows. Populated by ActivateSkill
	// (via LoadSkillTool or explicit /skill invocations), cleared by ClearActiveSkills (called from
	// /clear).
	activeSkills map[string]string
}

// ActivateSkill pins a skill's SOP body to the environment context. The body is emitted as a
// system-reminder at the top of every subsequent Agent Loop iteration so the model sees it before
// each decision. Calling ActivateSkill twice with the same name overwrites the previous body (hot
// reload case).
func (a *Agent) ActivateSkill(name, body string) {
	if a.activeSkills == nil {
		a.activeSkills = make(map[string]string)
	}
	a.activeSkills[name] = body
}

// ClearActiveSkills drops every pinned SOP. Called by /clear so a fresh conversation doesn't carry
// over SOPs from a prior task. Safe to call when no skills were ever activated.
func (a *Agent) ClearActiveSkills() {
	a.activeSkills = nil
}

// GetActiveSkills returns a copy of the currently-pinned SOPs (name → body). Used by tests and by
// /skills to surface what's active.
func (a *Agent) GetActiveSkills() map[string]string {
	out := make(map[string]string, len(a.activeSkills))
	for k, v := range a.activeSkills {
		out[k] = v
	}
	return out
}

// SetToolFilter installs a tool visibility filter for the current conversation. The filter is
// consulted at the top of every iteration so callers can flip skill-imposed allowedTools on or off
// without restarting the agent. Passing nil clears any previous filter. Implements one half of the
// skills.SkillHost interface.
func (a *Agent) SetToolFilter(allow func(name string) bool) {
	a.ToolNameFilter = allow
}

// ToolRegistry returns the live tool registry. Named ToolRegistry (not just Registry, even though
// that would match the field name) to avoid the method/field collision Go disallows. Matches the
// skills.SkillHost contract.
func (a *Agent) ToolRegistry() *tools.Registry {
	return a.Registry
}

func New(client llm.Client, registry *tools.Registry, protocol string) *Agent {
	wd, _ := os.Getwd()
	return &Agent{
		Client:           client,
		Registry:         registry,
		Protocol:         protocol,
		WorkDir:          wd,
		MaxIterations:    0,
		ContextWindow:    200000,
		ReplacementState: toolresult.New(),
		RecoveryState:    compact.NewRecoveryState(),
		Receipt:          NewReceiptTracker(),
	}
}

func (a *Agent) receipt() *ReceiptTracker {
	if a.Receipt == nil {
		a.Receipt = NewReceiptTracker()
	}
	return a.Receipt
}

// GetReceipt returns an immutable ExecutionReceipt snapshot for the current session/task.
func (a *Agent) GetReceipt() ExecutionReceipt {
	return a.receipt().Snapshot(a.SessionID, a.WorkDir)
}

// ResetReceipt clears accumulated execution telemetry (e.g. on /clear).
func (a *Agent) ResetReceipt() {
	if a.Receipt != nil {
		a.Receipt.Reset()
	}
}

// SetSessionID wires the on-disk session log id onto the agent so Layer 2
// compaction can persist a compact_boundary record into the same session the TUI
// is appending plain messages to. Called from the TUI right after the agent is
// constructed (and again after a resume switches sessions).
func (a *Agent) SetSessionID(id string) { a.SessionID = id }

// currentToolSchemas builds the schema list the next API call will use,
// honouring any active skill's allowed-tools filter. Shared between the
// recovery attachment (which lists what's still available after compact)
// and the actual Stream call so both views stay consistent.
func (a *Agent) currentToolSchemas() []map[string]any {
	schemas := a.Registry.GetAllSchemas(a.Protocol)
	if a.ToolNameFilter == nil {
		return schemas
	}
	allow := func(name string) bool {
		if t := a.Registry.Get(name); t != nil && tools.IsSystemTool(t) {
			return true
		}
		return a.ToolNameFilter(name)
	}
	return filterSchemasByName(schemas, allow)
}

func (a *Agent) Run(ctx context.Context, conv *conversation.Manager) <-chan AgentEvent {
	ch := make(chan AgentEvent, 32)

	go func() {
		runStart := a.receipt().markStart()
		defer func() { a.receipt().markFinish(runStart) }()
		defer close(ch)
		defer a.emitHook(hooks.EventSessionEnd, "", nil)

		a.emitHook(hooks.EventSessionStart, "", nil)

		conv.InjectLongTermMemory(a.Instructions, a.MemoryContent)

		if a.FileHistory != nil {
			msgs := conv.GetMessages()
			preTurnIdx := len(msgs)
			userText := ""
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Role == "user" && !strings.HasPrefix(msgs[i].Content, "<system-reminder>") {
					preTurnIdx = i
					userText = msgs[i].Content
					break
				}
			}
			a.FileHistory.BeginTurn(preTurnIdx, userText)
		}

		var totalInput, totalOutput int
		consecutiveUnknown := 0
		transientRetries := 0
		maxTokensEscalated := false
		outputRecoveries := 0
		// usageAnchor pins the last real API usage to the conversation length at
		// the moment it was reported, so the compaction check can use
		// baseline + incremental estimate instead of estimating every message.
		// Zero-value (HasUsage false) on the first iteration → full estimate fallback.
		var usageAnchor compact.UsageAnchor

		for iteration := 1; ; iteration++ {
			if a.MaxIterations > 0 && iteration > a.MaxIterations {
				ch <- ErrorEvent{Message: fmt.Sprintf("Agent reached maximum iterations (%d)", a.MaxIterations)}
				return
			}

			if ctx.Err() != nil {
				a.receipt().recordInterrupt(ctx.Err().Error())
				return
			}

			// Compute the tool schema list once per iteration so the recovery
			// attachment (when compact fires) and the actual Stream call below
			// agree on what's wired up. Skill filters can only change between
			// iterations, never within one.
			toolSchemas := a.currentToolSchemas()

			// Two-layer context management: spill+snip always, autocompact when needed.
			if msg, err := compact.ManageContext(ctx, conv, a.Client, a.WorkDir, a.SessionID, a.ContextWindow, a.MaxOutputTokens, &a.compactTracking, a.RecoveryState, toolSchemas, usageAnchor); err == nil && msg != "" {
				a.receipt().recordCompact(msg)
				ch <- CompactEvent{Message: msg}
				// A compaction rewrote the conversation, so the prior anchor's
				// AnchorCount no longer maps to the new transcript. Drop it and
				// fall back to a full estimate until the next real usage lands.
				usageAnchor = compact.UsageAnchor{}
			}

			// Plan mode: inject structured workflow reminder.
			if a.Checker != nil && a.Checker.Mode == permissions.ModePlan {
				planPath := planfile.GetOrCreatePlanPath(a.WorkDir)
				// Sync PlanFilePath onto the Checker on every turn so the Layer 0 plan-file write exception
				// works regardless of how Plan Mode was entered (Shift+Tab, SetPermissionMode, /plan).
				a.Checker.PlanFilePath = planPath
				planExists := planfile.PlanExists(a.WorkDir)
				reminder := prompt.BuildPlanModeReminder(planPath, planExists, iteration)
				conv.AddSystemReminder(reminder)
			}

			if a.NotificationFn != nil {
				for _, note := range a.NotificationFn() {
					conv.AddSystemReminder(note)
				}
			}

			a.emitHook(hooks.EventTurnStart, "", nil)

			// Re-inject active-skill SOPs as system-reminder on every turn so the model sees them at the
			// most prominent position regardless of how long the conversation grows.
			if reminder := buildActiveSkillsReminder(a.activeSkills); reminder != "" {
				conv.AddSystemReminder(reminder)
			}

			// Inject deferred tool names into system-reminder so the model knows what's available via
			// ToolSearch.
			if deferredNames := a.Registry.GetDeferredToolNames(); len(deferredNames) > 0 {
				reminder := "The following deferred tools are available via ToolSearch. Their schemas are NOT loaded - use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:\n" + strings.Join(deferredNames, "\n")
				conv.AddSystemReminder(reminder)
			}

			a.emitHook(hooks.EventPreSend, "", nil)

			// Layer 1: apply tool-result budget against ReplacementState.
			// Returns a fresh *conversation.Manager with replacements
			// baked in; `conv` is never mutated.
			apiConv, newRecords, _ := toolresult.Apply(conv, a.WorkDir, a.ReplacementState)
			if len(newRecords) > 0 {
				_ = toolresult.AppendRecords(a.WorkDir, newRecords)
			}

			events, errs := a.Client.Stream(ctx, apiConv, toolSchemas)

			var text string
			var toolCalls []llm.ToolCallComplete
			var thinkingBlocks []conversation.ThinkingBlock
			var stopReason string
			var usage llm.UsageInfo

			executor := NewStreamingExecutor(a.Registry, a.Checker, ch)

			for ev := range events {
				switch e := ev.(type) {
				case llm.ThinkingDelta:
					ch <- ThinkingText{Text: e.Text}
				case llm.ThinkingComplete:
					thinkingBlocks = append(thinkingBlocks, conversation.ThinkingBlock{
						Thinking:  e.Thinking,
						Signature: e.Signature,
					})
				case llm.TextDelta:
					text += e.Text
					ch <- StreamText{Text: e.Text}
				case llm.ToolCallStart:
					ch <- ToolUseEvent{ToolID: e.ToolID, ToolName: e.ToolName}
				case llm.ToolCallDelta:
					// ignore
				case llm.ToolCallComplete:
					toolCalls = append(toolCalls, e)
					ch <- ToolUseEvent{
						ToolID:   e.ToolID,
						ToolName: e.ToolName,
						Args:     e.Arguments,
					}
					// Start executing immediately while LLM continues streaming.
					executor.Submit(ctx, a, e)
				case llm.StreamEnd:
					stopReason = e.StopReason
					usage = e.Usage
				}
			}
			a.emitHook(hooks.EventPostReceive, text, nil)

			// Handle stream errors.
			select {
			case err := <-errs:
				if err != nil {
					hadCompletedTools := len(toolCalls) > 0
					if hadCompletedTools {
						var partialUses []conversation.ToolUseBlock
						for _, tc := range toolCalls {
							partialUses = append(partialUses, conversation.ToolUseBlock{
								ToolUseID: tc.ToolID,
								ToolName:  tc.ToolName,
								Arguments: tc.Arguments,
							})
						}
						conv.AddAssistantFull(text, thinkingBlocks, partialUses)
						partialResults := executor.CollectResults()
						var partialBlocks []conversation.ToolResultBlock
						for _, r := range partialResults {
							ch <- ToolResultEvent{
								ToolID:   r.toolID,
								ToolName: r.toolName,
								Output:   r.output,
								IsError:  r.isError,
								Elapsed:  r.elapsed,
							}
							truncated := r.output
							if len(truncated) > tools.MaxOutputChars {
								truncated = truncated[:tools.MaxOutputChars] + "\n… (output truncated)"
							}
							partialBlocks = append(partialBlocks, conversation.ToolResultBlock{
								ToolUseID: r.toolID,
								Content:   truncated,
								IsError:   r.isError,
							})
						}
						conv.AddToolResultsMessage(partialBlocks)
					}
					if ctx.Err() != nil {
						a.receipt().recordInterrupt(ctx.Err().Error())
						return
					}
					if retry, compacted := a.handleStreamError(ctx, ch, conv, err, &transientRetries); retry {
						if compacted {
							usageAnchor = compact.UsageAnchor{}
						}
						if !hadCompletedTools {
							iteration--
						}
						continue
					}
					a.receipt().recordUnresolvedRetryFailure(err.Error())
					ch <- ErrorEvent{Message: err.Error()}
					return
				}
			default:
			}

			if ctx.Err() != nil && stopReason == "" && len(toolCalls) == 0 && text == "" {
				a.receipt().recordInterrupt(ctx.Err().Error())
				return
			}

			transientRetries = 0
			a.receipt().recordStep()
			a.receipt().recordUsage(usage)
			totalInput += usage.InputTokens
			totalOutput += usage.OutputTokens
			ch <- UsageEvent{InputTokens: totalInput, OutputTokens: totalOutput}

			anchorBaseline := compact.BaselineFromUsage(usage)
			anchorAfterAssistant := func() {
				if anchorBaseline > 0 {
					usageAnchor = compact.UsageAnchor{
						BaselineTokens: anchorBaseline,
						AnchorCount:    conv.Len(),
						HasUsage:       true,
					}
				}
			}

			// Handle max_tokens stop reason.
			if stopReason == "max_tokens" {
				if !maxTokensEscalated {
					if setter, ok := a.Client.(llm.MaxTokensSetter); ok {
						setter.SetMaxOutputTokens(maxTokensCeiling)
						maxTokensEscalated = true
					}
					if text != "" {
						conv.AddAssistantFull(text, thinkingBlocks, nil)
						anchorAfterAssistant()
						conv.AddUserMessage("Output token limit hit. Continue directly from where you stopped. Do not apologize or repeat previous content. Pick up mid-thought if needed.")
					}
					ch <- RetryEvent{Reason: "max_tokens escalation", Wait: 0}
					continue
				} else if outputRecoveries < maxOutputTokensRecoveries {
					outputRecoveries++
					conv.AddAssistantFull(text, thinkingBlocks, nil)
					anchorAfterAssistant()
					conv.AddUserMessage("Output token limit hit. Continue directly from where you stopped. Break remaining work into smaller pieces.")
					ch <- RetryEvent{Reason: fmt.Sprintf("max_tokens recovery %d/%d", outputRecoveries, maxOutputTokensRecoveries), Wait: 0}
					continue
				}
			} else {
				outputRecoveries = 0
			}

			if len(toolCalls) == 0 {
				conv.AddAssistantFull(text, thinkingBlocks, nil)
				if a.FileHistory != nil {
					summary := text
					if len(summary) > 60 {
						summary = summary[:60] + "..."
					}
					a.FileHistory.MakeSnapshot(conv.Len(), summary)
				}
				ch <- LoopComplete{TotalTurns: iteration}
				if a.OnLoopComplete != nil {
					go a.OnLoopComplete(conv)
				}
				return
			}

			var toolUses []conversation.ToolUseBlock
			for _, tc := range toolCalls {
				toolUses = append(toolUses, conversation.ToolUseBlock{
					ToolUseID: tc.ToolID,
					ToolName:  tc.ToolName,
					Arguments: tc.Arguments,
				})
			}
			conv.AddAssistantFull(text, thinkingBlocks, toolUses)
			anchorAfterAssistant()

			// Collect results from streaming executor (tools already started during LLM streaming).
			results := executor.CollectResults()

			var toolResults []conversation.ToolResultBlock
			for _, r := range results {
				if r.isUnknown {
					consecutiveUnknown++
				} else {
					consecutiveUnknown = 0
				}

				ch <- ToolResultEvent{
					ToolID:   r.toolID,
					ToolName: r.toolName,
					Output:   r.output,
					IsError:  r.isError,
					Elapsed:  r.elapsed,
				}

				truncated := r.output
				if len(truncated) > tools.MaxOutputChars {
					truncated = truncated[:tools.MaxOutputChars] + "\n… (output truncated)"
				}
				toolResults = append(toolResults, conversation.ToolResultBlock{
					ToolUseID: r.toolID,
					Content:   truncated,
					IsError:   r.isError,
				})
			}

			if consecutiveUnknown >= 3 {
				ch <- ErrorEvent{Message: "Too many consecutive unknown tool calls"}
				return
			}

			exitPlanCalled := false
			for _, tc := range toolCalls {
				if tc.ToolName == "ExitPlanMode" {
					exitPlanCalled = true
					break
				}
			}
			conv.AddToolResultsMessage(toolResults)
			if exitPlanCalled {
				ch <- TurnComplete{Turn: iteration}
				ch <- LoopComplete{TotalTurns: iteration}
				return
			}
			ch <- TurnComplete{Turn: iteration}
			a.emitHook(hooks.EventTurnEnd, "", nil)
		}
	}()

	return ch
}

// emitHook fires a hook event when an Engine is configured. Failures are non-fatal and surface via
// the hook notification queue (drained into the next turn's system reminders).
func (a *Agent) emitHook(event hooks.EventName, message string, args map[string]any) {
	if a.Hooks == nil {
		return
	}
	a.Hooks.RunHooks(hooks.HookContext{
		EventName: event,
		ToolArgs:  args,
		Message:   message,
	})
}

// buildActiveSkillsReminder renders all pinned skill SOPs into a single system-reminder string.
// Returns "" if no skills are active so callers can short-circuit.
func buildActiveSkillsReminder(active map[string]string) string {
	if len(active) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("# Active Skills\n\nThe following Skill SOPs are pinned to the environment context. Follow each SOP when its triggering condition applies.\n\n")
	for name, body := range active {
		sb.WriteString("## Active Skill: ")
		sb.WriteString(name)
		sb.WriteString("\n\n")
		sb.WriteString(body)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// filterSchemasByName keeps only the tool schemas whose "name" passes the allow predicate. Used by
// Coordinator Mode to restrict a Lead agent to coordination-only tools while teammates do the
// actual work.
func filterSchemasByName(schemas []map[string]any, allow func(name string) bool) []map[string]any {
	out := make([]map[string]any, 0, len(schemas))
	for _, s := range schemas {
		name, _ := s["name"].(string)
		if allow(name) {
			out = append(out, s)
		}
	}
	return out
}
