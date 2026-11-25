package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/planfile"
)

// FileTouchKind classifies how an agent session modified a workspace file.
type FileTouchKind string

const (
	FileModified FileTouchKind = "modified"
	FileCreated  FileTouchKind = "created"
	FileDeleted  FileTouchKind = "deleted"
)

// FileChangeStat summarizes net changes to a single file across the task/session.
type FileChangeStat struct {
	Path         string        `json:"path"`
	Action       FileTouchKind `json:"action"`
	LinesAdded   int           `json:"lines_added"`
	LinesDeleted int           `json:"lines_deleted"`
}

// CommandExecution records a shell or verification command run by the agent.
type CommandExecution struct {
	Command        string `json:"command"`
	ExitCode       int    `json:"exit_code"`
	DurationMs     int64  `json:"duration_ms"`
	IsVerification bool   `json:"is_verification"`
	Passed         bool   `json:"passed"`
}

// RecoveryRecord captures a failure and its recovery outcome during execution.
type RecoveryRecord struct {
	Kind    string `json:"kind"` // api_retry, stream_interrupt, tool_error, test_self_repair, context_compact
	Detail  string `json:"detail"`
	Outcome string `json:"outcome"` // recovered, retrying, unresolved
}

// TokenSummary aggregates LLM token consumption across turns.
type TokenSummary struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
}

// ExecutionReceipt provides a structured, user-inspectable summary of agent work.
type ExecutionReceipt struct {
	SessionID          string             `json:"session_id,omitempty"`
	WorkDir            string             `json:"work_dir,omitempty"`
	Turns              int                `json:"turns"`
	Steps              int                `json:"steps"`
	WallClockMs        int64              `json:"wall_clock_ms"`
	ActivePlanSteps    []string           `json:"active_plan_steps,omitempty"`
	TouchedFiles       []FileChangeStat   `json:"touched_files,omitempty"`
	Commands           []CommandExecution `json:"commands,omitempty"`
	FailuresRecoveries []RecoveryRecord   `json:"failures_and_recoveries,omitempty"`
	HumanInterventions int                `json:"human_interventions"`
	Tokens             TokenSummary       `json:"tokens"`
	StartedAt          time.Time          `json:"started_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

type fileBaseline struct {
	existed bool
	content string
}

type pendingTestFailure struct {
	command  string
	exitCode int
	edited   bool
	recIdx   int
}

// ReceiptTracker collects execution telemetry concurrently across Agent.Run turns.
type ReceiptTracker struct {
	mu                 sync.Mutex
	startedAt          time.Time
	updatedAt          time.Time
	turns              int
	steps              int
	wallClock          time.Duration
	humanInterventions int
	tokens             TokenSummary
	fileBaselines      map[string]fileBaseline
	fileCurrent        map[string]*fileBaseline
	commands           []CommandExecution
	recoveries         []RecoveryRecord
	todoSteps          []string
	pendingRetryIdx    []int
	lastFailedVerify   *pendingTestFailure
}

// NewReceiptTracker creates an empty ReceiptTracker.
func NewReceiptTracker() *ReceiptTracker {
	return &ReceiptTracker{
		fileBaselines: make(map[string]fileBaseline),
		fileCurrent:   make(map[string]*fileBaseline),
	}
}

// Reset clears accumulated telemetry (e.g. on /clear).
func (rt *ReceiptTracker) Reset() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.startedAt = time.Time{}
	rt.updatedAt = time.Time{}
	rt.turns = 0
	rt.steps = 0
	rt.wallClock = 0
	rt.humanInterventions = 0
	rt.tokens = TokenSummary{}
	rt.fileBaselines = make(map[string]fileBaseline)
	rt.fileCurrent = make(map[string]*fileBaseline)
	rt.commands = nil
	rt.recoveries = nil
	rt.todoSteps = nil
	rt.pendingRetryIdx = nil
	rt.lastFailedVerify = nil
}

func (rt *ReceiptTracker) markStart() time.Time {
	now := time.Now()
	rt.mu.Lock()
	if rt.startedAt.IsZero() {
		rt.startedAt = now
	}
	rt.turns++
	rt.updatedAt = now
	rt.mu.Unlock()
	return now
}

func (rt *ReceiptTracker) markFinish(runStart time.Time) {
	now := time.Now()
	rt.mu.Lock()
	rt.wallClock += now.Sub(runStart)
	rt.updatedAt = now
	rt.mu.Unlock()
}

func (rt *ReceiptTracker) recordStep() {
	rt.mu.Lock()
	rt.steps++
	rt.updatedAt = time.Now()
	rt.mu.Unlock()
}

func (rt *ReceiptTracker) recordUsage(u llm.UsageInfo) {
	rt.mu.Lock()
	rt.tokens.InputTokens += u.InputTokens
	rt.tokens.OutputTokens += u.OutputTokens
	rt.tokens.CacheReadTokens += u.CacheReadTokens
	rt.tokens.CacheCreationTokens += u.CacheCreationTokens
	for _, idx := range rt.pendingRetryIdx {
		if idx >= 0 && idx < len(rt.recoveries) && rt.recoveries[idx].Outcome == "retrying" {
			rt.recoveries[idx].Outcome = "recovered"
		}
	}
	rt.pendingRetryIdx = nil
	rt.updatedAt = time.Now()
	rt.mu.Unlock()
}

func (rt *ReceiptTracker) recordRetry(reason string) {
	rt.mu.Lock()
	idx := len(rt.recoveries)
	rt.recoveries = append(rt.recoveries, RecoveryRecord{
		Kind:    "api_retry",
		Detail:  reason,
		Outcome: "retrying",
	})
	rt.pendingRetryIdx = append(rt.pendingRetryIdx, idx)
	rt.updatedAt = time.Now()
	rt.mu.Unlock()
}

func (rt *ReceiptTracker) recordUnresolvedRetryFailure(errMsg string) {
	rt.mu.Lock()
	if len(rt.pendingRetryIdx) > 0 {
		for _, idx := range rt.pendingRetryIdx {
			if idx >= 0 && idx < len(rt.recoveries) {
				rt.recoveries[idx].Outcome = "unresolved"
			}
		}
		rt.pendingRetryIdx = nil
	} else {
		rt.recoveries = append(rt.recoveries, RecoveryRecord{
			Kind:    "api_error",
			Detail:  errMsg,
			Outcome: "unresolved",
		})
	}
	rt.updatedAt = time.Now()
	rt.mu.Unlock()
}

func (rt *ReceiptTracker) recordInterrupt(detail string) {
	rt.mu.Lock()
	rt.recoveries = append(rt.recoveries, RecoveryRecord{
		Kind:    "stream_interrupt",
		Detail:  detail,
		Outcome: "unresolved",
	})
	rt.updatedAt = time.Now()
	rt.mu.Unlock()
}

func (rt *ReceiptTracker) recordCompact(msg string) {
	rt.mu.Lock()
	rt.recoveries = append(rt.recoveries, RecoveryRecord{
		Kind:    "context_compact",
		Detail:  msg,
		Outcome: "recovered",
	})
	rt.updatedAt = time.Now()
	rt.mu.Unlock()
}

func (rt *ReceiptTracker) recordHumanIntervention() {
	rt.mu.Lock()
	rt.humanInterventions++
	rt.updatedAt = time.Now()
	rt.mu.Unlock()
}

func (rt *ReceiptTracker) capturePreFileTouch(path string) {
	if path == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	rt.mu.Lock()
	_, exists := rt.fileBaselines[abs]
	rt.mu.Unlock()
	if exists {
		return
	}

	data, readErr := os.ReadFile(abs)
	base := fileBaseline{
		existed: readErr == nil,
		content: string(data),
	}
	rt.mu.Lock()
	if _, already := rt.fileBaselines[abs]; !already {
		rt.fileBaselines[abs] = base
	}
	rt.mu.Unlock()
}

func (rt *ReceiptTracker) capturePostFileTouch(path string) {
	if path == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	data, readErr := os.ReadFile(abs)
	cur := &fileBaseline{
		existed: readErr == nil,
		content: string(data),
	}
	rt.mu.Lock()
	rt.fileCurrent[abs] = cur
	if rt.lastFailedVerify != nil {
		rt.lastFailedVerify.edited = true
	}
	rt.updatedAt = time.Now()
	rt.mu.Unlock()
}

var bashExitCodeRE = regexp.MustCompile(`\(exit code (-?\d+)\)\s*$`)

func (rt *ReceiptTracker) recordToolExecution(toolName string, args map[string]any, output string, isError bool, elapsed time.Duration) {
	switch toolName {
	case "AskUserQuestion":
		rt.recordHumanIntervention()
	case "TaskCreate":
		if subj, _ := args["subject"].(string); strings.TrimSpace(subj) != "" {
			rt.mu.Lock()
			rt.todoSteps = append(rt.todoSteps, strings.TrimSpace(subj))
			rt.mu.Unlock()
		}
	case "Bash":
		cmd, _ := args["command"].(string)
		exitCode := 0
		if m := bashExitCodeRE.FindStringSubmatch(strings.TrimSpace(output)); len(m) == 2 {
			exitCode, _ = strconv.Atoi(m[1])
		} else if isError {
			exitCode = 1
		}
		isVerify := isVerificationCommand(cmd)
		passed := !isError && exitCode == 0

		rt.mu.Lock()
		rt.commands = append(rt.commands, CommandExecution{
			Command:        cmd,
			ExitCode:       exitCode,
			DurationMs:     elapsed.Milliseconds(),
			IsVerification: isVerify,
			Passed:         passed,
		})
		if isVerify {
			if !passed {
				recIdx := len(rt.recoveries)
				rt.recoveries = append(rt.recoveries, RecoveryRecord{
					Kind:    "test_self_repair",
					Detail:  fmt.Sprintf("%s failed with exit code %d", cmd, exitCode),
					Outcome: "unresolved",
				})
				rt.lastFailedVerify = &pendingTestFailure{
					command:  cmd,
					exitCode: exitCode,
					recIdx:   recIdx,
				}
			} else if rt.lastFailedVerify != nil {
				prev := rt.lastFailedVerify
				idx := prev.recIdx
				if idx >= 0 && idx < len(rt.recoveries) {
					action := "retried"
					if prev.edited {
						action = "patched and verified"
					}
					rt.recoveries[idx] = RecoveryRecord{
						Kind:    "test_self_repair",
						Detail:  fmt.Sprintf("%s failed (exit %d) -> %s via %s", prev.command, prev.exitCode, action, cmd),
						Outcome: "recovered",
					}
				}
				rt.lastFailedVerify = nil
			}
		} else if isError {
			rt.recoveries = append(rt.recoveries, RecoveryRecord{
				Kind:    "command_error",
				Detail:  fmt.Sprintf("%s (exit %d)", cmd, exitCode),
				Outcome: "unresolved",
			})
		}
		rt.updatedAt = time.Now()
		rt.mu.Unlock()
		return
	}

	if isError {
		summary := firstLine(output, 120)
		rt.mu.Lock()
		rt.recoveries = append(rt.recoveries, RecoveryRecord{
			Kind:    "tool_error",
			Detail:  fmt.Sprintf("%s: %s", toolName, summary),
			Outcome: "recovered",
		})
		rt.updatedAt = time.Now()
		rt.mu.Unlock()
	}
}

func isVerificationCommand(cmd string) bool {
	lower := strings.ToLower(strings.TrimSpace(cmd))
	verifyMarkers := []string{
		"go test", "go vet", "golangci-lint", "pytest", "python -m unittest",
		"npm test", "npm run test", "pnpm test", "yarn test", "vitest", "jest",
		"cargo test", "cargo check", "make test", "ctest", "mvn test", "gradle test",
	}
	for _, m := range verifyMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// Snapshot builds an immutable ExecutionReceipt, resolving live disk diffs for touched files
// and active plan steps from workDir.
func (rt *ReceiptTracker) Snapshot(sessionID, workDir string) ExecutionReceipt {
	if rt == nil {
		return ExecutionReceipt{SessionID: sessionID, WorkDir: workDir}
	}
	rt.mu.Lock()
	baselines := make(map[string]fileBaseline, len(rt.fileBaselines))
	for k, v := range rt.fileBaselines {
		baselines[k] = v
	}
	currents := make(map[string]*fileBaseline, len(rt.fileCurrent))
	for k, v := range rt.fileCurrent {
		cp := *v
		currents[k] = &cp
	}
	cmds := make([]CommandExecution, len(rt.commands))
	copy(cmds, rt.commands)
	recs := make([]RecoveryRecord, len(rt.recoveries))
	copy(recs, rt.recoveries)
	todoSteps := make([]string, len(rt.todoSteps))
	copy(todoSteps, rt.todoSteps)
	receipt := ExecutionReceipt{
		SessionID:          sessionID,
		WorkDir:            workDir,
		Turns:              rt.turns,
		Steps:              rt.steps,
		WallClockMs:        rt.wallClock.Milliseconds(),
		Commands:           cmds,
		FailuresRecoveries: recs,
		HumanInterventions: rt.humanInterventions,
		Tokens:             rt.tokens,
		StartedAt:          rt.startedAt,
		UpdatedAt:          rt.updatedAt,
	}
	rt.mu.Unlock()

	paths := make([]string, 0, len(baselines))
	for p := range baselines {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	absWorkDir, _ := filepath.Abs(workDir)
	for _, absPath := range paths {
		base := baselines[absPath]
		var afterExists bool
		var afterContent string
		if data, err := os.ReadFile(absPath); err == nil {
			afterExists = true
			afterContent = string(data)
		} else if cached, ok := currents[absPath]; ok && os.IsNotExist(err) {
			_ = cached
			afterExists = false
		}

		displayPath := absPath
		if absWorkDir != "" {
			if rel, err := filepath.Rel(absWorkDir, absPath); err == nil && !strings.HasPrefix(rel, "..") {
				displayPath = filepath.ToSlash(rel)
			}
		}

		switch {
		case !base.existed && afterExists:
			receipt.TouchedFiles = append(receipt.TouchedFiles, FileChangeStat{
				Path:       displayPath,
				Action:     FileCreated,
				LinesAdded: countLines(afterContent),
			})
		case base.existed && !afterExists:
			receipt.TouchedFiles = append(receipt.TouchedFiles, FileChangeStat{
				Path:         displayPath,
				Action:       FileDeleted,
				LinesDeleted: countLines(base.content),
			})
		case base.existed && afterExists && base.content != afterContent:
			added, deleted := computeLineDiffStats(base.content, afterContent)
			receipt.TouchedFiles = append(receipt.TouchedFiles, FileChangeStat{
				Path:         displayPath,
				Action:       FileModified,
				LinesAdded:   added,
				LinesDeleted: deleted,
			})
		}
	}

	planSteps := extractPlanSteps(workDir)
	planSteps = append(planSteps, todoSteps...)
	if len(planSteps) > 0 {
		receipt.ActivePlanSteps = planSteps
	}

	return receipt
}

// FormatText renders a concise, scannable terminal execution receipt.
func (r ExecutionReceipt) FormatText() string {
	var sb strings.Builder
	sb.WriteString("Execution Summary\n")
	sb.WriteString("─────────────────\n")
	if r.SessionID != "" {
		sb.WriteString(fmt.Sprintf("  Session:        %s\n", r.SessionID))
	}
	if r.WorkDir != "" {
		sb.WriteString(fmt.Sprintf("  Workspace:      %s\n", r.WorkDir))
	}
	sb.WriteString(fmt.Sprintf("  Turns / Steps:  %d turn(s), %d step(s)\n", r.Turns, r.Steps))
	sb.WriteString(fmt.Sprintf("  Duration:       %.2fs (%dms)\n", float64(r.WallClockMs)/1000.0, r.WallClockMs))
	sb.WriteString(fmt.Sprintf("  Interventions:  %d human approval(s)/prompt(s)\n", r.HumanInterventions))
	sb.WriteString(fmt.Sprintf("  Tokens:         %d in / %d out (cache read: %d, cache write: %d)\n",
		r.Tokens.InputTokens, r.Tokens.OutputTokens, r.Tokens.CacheReadTokens, r.Tokens.CacheCreationTokens))

	if len(r.ActivePlanSteps) > 0 {
		sb.WriteString("\nActive Plan Steps:\n")
		for i, step := range r.ActivePlanSteps {
			sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, step))
		}
	}

	sb.WriteString("\nTouched Files:\n")
	if len(r.TouchedFiles) == 0 {
		sb.WriteString("  (none)\n")
	} else {
		for _, f := range r.TouchedFiles {
			sb.WriteString(fmt.Sprintf("  [%s] %s (+%d/-%d)\n", f.Action, f.Path, f.LinesAdded, f.LinesDeleted))
		}
	}

	sb.WriteString("\nVerification & Commands:\n")
	if len(r.Commands) == 0 {
		sb.WriteString("  (none)\n")
	} else {
		for _, c := range r.Commands {
			status := "PASS"
			if !c.Passed {
				status = "FAIL"
			}
			tag := "cmd"
			if c.IsVerification {
				tag = "verify"
			}
			sb.WriteString(fmt.Sprintf("  [%s] (%s, exit=%d, %dms) $ %s\n",
				status, tag, c.ExitCode, c.DurationMs, c.Command))
		}
	}

	sb.WriteString("\nFailures & Recovery:\n")
	if len(r.FailuresRecoveries) == 0 {
		sb.WriteString("  (none)\n")
	} else {
		for _, rec := range r.FailuresRecoveries {
			sb.WriteString(fmt.Sprintf("  [%s] %s: %s\n", rec.Outcome, rec.Kind, rec.Detail))
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// ToJSON serializes the receipt as indented JSON.
func (r ExecutionReceipt) ToJSON() (string, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ExportJSONFile writes the JSON-serialized receipt to path.
func (r ExecutionReceipt) ExportJSONFile(path string) error {
	raw, err := r.ToJSON()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(raw+"\n"), 0o644)
}

func splitNormalizedLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{""}
	}
	return strings.Split(s, "\n")
}

func countLines(s string) int {
	return len(splitNormalizedLines(s))
}

// computeLineDiffStats computes (+added, -deleted) line counts via LCS for typical source
// files and falls back to O(N) multiset frequency counting for large files (>2000 lines).
func computeLineDiffStats(before, after string) (added, deleted int) {
	a := splitNormalizedLines(before)
	b := splitNormalizedLines(after)
	if len(a) == 0 {
		return len(b), 0
	}
	if len(b) == 0 {
		return 0, len(a)
	}

	const maxLCSLines = 2000
	if len(a) <= maxLCSLines && len(b) <= maxLCSLines {
		prev := make([]int, len(b)+1)
		curr := make([]int, len(b)+1)
		for i := 1; i <= len(a); i++ {
			for j := 1; j <= len(b); j++ {
				if a[i-1] == b[j-1] {
					curr[j] = prev[j-1] + 1
				} else if prev[j] >= curr[j-1] {
					curr[j] = prev[j]
				} else {
					curr[j] = curr[j-1]
				}
			}
			prev, curr = curr, prev
			for j := range curr {
				curr[j] = 0
			}
		}
		lcs := prev[len(b)]
		return len(b) - lcs, len(a) - lcs
	}

	freq := make(map[string]int, len(a))
	for _, line := range a {
		freq[line]++
	}
	common := 0
	for _, line := range b {
		if freq[line] > 0 {
			freq[line]--
			common++
		}
	}
	return len(b) - common, len(a) - common
}

var planStepLineRE = regexp.MustCompile(`^\s*(?:[-*]\s*(?:\[[ xX]\]\s*)?|\d+\.\s+)(.+)$`)

func extractPlanSteps(workDir string) []string {
	if workDir == "" {
		return nil
	}
	content, err := planfile.LoadPlan(workDir)
	if err != nil || strings.TrimSpace(content) == "" {
		return nil
	}
	var steps []string
	for _, line := range strings.Split(content, "\n") {
		m := planStepLineRE.FindStringSubmatch(line)
		if len(m) == 2 {
			step := strings.TrimSpace(m[1])
			if step != "" {
				steps = append(steps, step)
				if len(steps) >= 20 {
					break
				}
			}
		}
	}
	return steps
}

func firstLine(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	if len(s) > maxChars {
		return s[:maxChars] + "…"
	}
	return s
}
