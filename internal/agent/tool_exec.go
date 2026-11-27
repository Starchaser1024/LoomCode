package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/compact"
	"github.com/Starchaser1024/LoomCode/internal/conversation"
	"github.com/Starchaser1024/LoomCode/internal/hooks"
	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/permissions"
	"github.com/Starchaser1024/LoomCode/internal/tools"
)

const maxTransientStreamRetries = 3

// handleStreamError returns (retry, compacted): retry signals the caller to
// re-run the turn; compacted signals that a ForceCompact rewrote the
// conversation, so the caller must drop its usage anchor (its AnchorCount no
// longer maps to the new transcript).
func (a *Agent) handleStreamError(ctx context.Context, ch chan AgentEvent, conv *conversation.Manager, err error, transientRetries *int) (retry, compacted bool) {
	if ctx.Err() != nil {
		return false, false
	}

	var ctxErr *llm.ContextTooLongError
	if errors.As(err, &ctxErr) {
		msg, compactErr := compact.ForceCompact(ctx, conv, a.Client, a.WorkDir, a.SessionID, a.ContextWindow, a.RecoveryState, a.currentToolSchemas())
		if compactErr == nil && msg != "" {
			a.receipt().recordCompact(msg)
			ch <- CompactEvent{Message: "Auto-compacted due to context length: " + msg}
			return true, true
		}
		return false, false
	}

	var rlErr *llm.RateLimitError
	if errors.As(err, &rlErr) {
		if transientRetries != nil && *transientRetries >= maxTransientStreamRetries {
			return false, false
		}
		if transientRetries != nil {
			*transientRetries++
		}
		wait := a.retryWait(rlErr.RetryAfter, transientRetries)
		reason := fmt.Sprintf("rate limited (429): %s", rlErr.Message)
		a.receipt().recordRetry(reason)
		ch <- RetryEvent{Reason: "rate limited", Wait: wait}
		select {
		case <-time.After(wait):
			return true, false
		case <-ctx.Done():
			return false, false
		}
	}

	var llmErr *llm.LLMError
	if errors.As(err, &llmErr) && llmErr.IsTransient() {
		if transientRetries != nil && *transientRetries >= maxTransientStreamRetries {
			return false, false
		}
		if transientRetries != nil {
			*transientRetries++
		}
		wait := a.retryWait("", transientRetries)
		reason := fmt.Sprintf("transient upstream error: %s", llmErr.Message)
		a.receipt().recordRetry(reason)
		ch <- RetryEvent{Reason: reason, Wait: wait}
		select {
		case <-time.After(wait):
			return true, false
		case <-ctx.Done():
			return false, false
		}
	}

	var netErr *llm.NetworkError
	if errors.As(err, &netErr) {
		if transientRetries != nil && *transientRetries >= maxTransientStreamRetries {
			return false, false
		}
		if transientRetries != nil {
			*transientRetries++
		}
		wait := a.retryWait("", transientRetries)
		reason := fmt.Sprintf("transient network error: %s", netErr.Message)
		a.receipt().recordRetry(reason)
		ch <- RetryEvent{Reason: reason, Wait: wait}
		select {
		case <-time.After(wait):
			return true, false
		case <-ctx.Done():
			return false, false
		}
	}

	return false, false
}

func (a *Agent) retryWait(retryAfterHeader string, attempt *int) time.Duration {
	if retryAfterHeader != "" {
		if secs, err := strconv.Atoi(retryAfterHeader); err == nil {
			if secs <= 0 {
				return 5 * time.Millisecond
			}
			return time.Duration(secs) * time.Second
		}
	}
	base := 5 * time.Second
	if a.RetryBaseDelay > 0 {
		base = a.RetryBaseDelay
	}
	n := 1
	if attempt != nil && *attempt > 1 {
		n = 1 << (*attempt - 1)
	}
	return base * time.Duration(n)
}

type toolExecResult struct {
	toolID    string
	toolName  string
	output    string
	isError   bool
	elapsed   time.Duration
	isUnknown bool
}

// extractFilePath pulls a representative path from common tool argument keys so hooks can do path-
// glob matching (`file_path =* "**/*.go"`).
func extractFilePath(args map[string]any) string {
	for _, key := range []string{"file_path", "path", "pattern", "target"} {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (a *Agent) executeSingleTool(ctx context.Context, eventCh chan AgentEvent, tc llm.ToolCallComplete) toolExecResult {
	tool := a.Registry.Get(tc.ToolName)
	start := time.Now()

	if tool == nil {
		res := toolExecResult{
			toolID:    tc.ToolID,
			toolName:  tc.ToolName,
			output:    fmt.Sprintf("Error: unknown tool '%s'", tc.ToolName),
			isError:   true,
			elapsed:   time.Since(start),
			isUnknown: true,
		}
		a.receipt().recordToolExecution(tc.ToolName, tc.Arguments, res.output, true, res.elapsed)
		return res
	}

	if a.Checker != nil {
		decision := a.Checker.Check(tool, tc.Arguments)
		if decision.Effect == permissions.Deny {
			out := fmt.Sprintf("Permission denied: %s", decision.Reason)
			elapsed := time.Since(start)
			a.receipt().recordToolExecution(tc.ToolName, tc.Arguments, out, true, elapsed)
			return toolExecResult{
				toolID:   tc.ToolID,
				toolName: tc.ToolName,
				output:   out,
				isError:  true,
				elapsed:  elapsed,
			}
		}
		if decision.Effect == permissions.Ask {
			desc := permissions.ExtractContent(tc.ToolName, tc.Arguments)
			if desc == "" {
				desc = formatToolArgs(tc.Arguments)
			}
			if a.NonInteractive {
				modeName := string(a.Checker.Mode)
				if modeName == "" {
					modeName = string(permissions.ModeDefault)
				}
				out := fmt.Sprintf(
					"Permission denied in headless mode: %s (%s) requires interactive approval under permission mode %q; re-run with --permission-mode acceptEdits or --permission-mode bypassPermissions, or add an allow rule",
					tc.ToolName, desc, modeName,
				)
				elapsed := time.Since(start)
				a.receipt().recordToolExecution(tc.ToolName, tc.Arguments, out, true, elapsed)
				return toolExecResult{
					toolID:   tc.ToolID,
					toolName: tc.ToolName,
					output:   out,
					isError:  true,
					elapsed:  elapsed,
				}
			}
			a.receipt().recordHumanIntervention()
			respCh := make(chan PermissionResponse, 1)
			eventCh <- PermissionRequestEvent{
				ToolName:   tc.ToolName,
				Desc:       desc,
				ResponseCh: respCh,
			}
			resp := <-respCh
			if resp == PermDeny {
				out := "Permission denied by user"
				elapsed := time.Since(start)
				a.receipt().recordToolExecution(tc.ToolName, tc.Arguments, out, true, elapsed)
				return toolExecResult{
					toolID:   tc.ToolID,
					toolName: tc.ToolName,
					output:   out,
					isError:  true,
					elapsed:  elapsed,
				}
			}
			if resp == PermAllowAlways && a.Checker.RuleEngine != nil {
				content := permissions.ExtractContent(tc.ToolName, tc.Arguments)
				pattern := content + "*"
				if len(content) > 60 {
					pattern = content[:60] + "*"
				}
				a.Checker.RuleEngine.AppendLocalRule(permissions.Rule{
					ToolName: tc.ToolName,
					Pattern:  pattern,
					Effect:   permissions.RuleAllow,
				})
			}
		}
	}

	if a.Hooks != nil {
		hctx := hooks.HookContext{
			EventName: hooks.EventPreToolUse,
			ToolName:  tc.ToolName,
			ToolArgs:  tc.Arguments,
			FilePath:  extractFilePath(tc.Arguments),
		}
		if rejected, msg := a.Hooks.RunPreToolHooks(hctx); rejected {
			out := "Blocked by hook: " + msg
			elapsed := time.Since(start)
			a.receipt().recordToolExecution(tc.ToolName, tc.Arguments, out, true, elapsed)
			return toolExecResult{
				toolID:   tc.ToolID,
				toolName: tc.ToolName,
				output:   out,
				isError:  true,
				elapsed:  elapsed,
			}
		}
	}

	var writeTarget string
	if tool.Category() == tools.CategoryWrite {
		if targetPath := extractFilePath(tc.Arguments); targetPath != "" {
			writeTarget = targetPath
			if !filepath.IsAbs(writeTarget) && a.WorkDir != "" {
				if _, err := os.Stat(writeTarget); err != nil {
					writeTarget = filepath.Join(a.WorkDir, writeTarget)
				}
			}
			a.receipt().capturePreFileTouch(writeTarget)
		}
	}

	result := tool.Execute(ctx, tc.Arguments)
	elapsed := time.Since(start)
	if !result.IsError && writeTarget != "" {
		a.receipt().capturePostFileTouch(writeTarget)
	}
	a.receipt().recordToolExecution(tc.ToolName, tc.Arguments, result.Output, result.IsError, elapsed)

	// Snapshot what ReadFile just handed to the model so the compact
	// recovery block can replay it after a Layer 2 summary wipes the
	// transcript.
	if !result.IsError && tc.ToolName == "ReadFile" {
		if p, _ := tc.Arguments["file_path"].(string); p != "" {
			if data, err := os.ReadFile(p); err == nil {
				a.RecoveryState.RecordFileRead(p, string(data))
			}
		}
	}

	if a.Hooks != nil {
		a.Hooks.RunHooks(hooks.HookContext{
			EventName: hooks.EventPostToolUse,
			ToolName:  tc.ToolName,
			ToolArgs:  tc.Arguments,
			FilePath:  extractFilePath(tc.Arguments),
			Message:   result.Output,
		})
	}

	return toolExecResult{
		toolID:   tc.ToolID,
		toolName: tc.ToolName,
		output:   result.Output,
		isError:  result.IsError,
		elapsed:  time.Since(start),
	}
}

func formatToolArgs(args map[string]any) string {
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", k, s))
	}
	return strings.Join(parts, ", ")
}
