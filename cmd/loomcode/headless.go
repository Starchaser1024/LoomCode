package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/agent"
	"github.com/Starchaser1024/LoomCode/internal/agents"
	"github.com/Starchaser1024/LoomCode/internal/config"
	"github.com/Starchaser1024/LoomCode/internal/conversation"
	"github.com/Starchaser1024/LoomCode/internal/filehistory"
	"github.com/Starchaser1024/LoomCode/internal/hooks"
	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/mcp"
	"github.com/Starchaser1024/LoomCode/internal/memory"
	"github.com/Starchaser1024/LoomCode/internal/permissions"
	"github.com/Starchaser1024/LoomCode/internal/planfile"
	"github.com/Starchaser1024/LoomCode/internal/prompt"
	"github.com/Starchaser1024/LoomCode/internal/session"
	"github.com/Starchaser1024/LoomCode/internal/skills"
	"github.com/Starchaser1024/LoomCode/internal/teams"
	"github.com/Starchaser1024/LoomCode/internal/todo"
	"github.com/Starchaser1024/LoomCode/internal/tools"
	"github.com/Starchaser1024/LoomCode/internal/worktree"
)

var version = "0.1.0"

// cliOptions holds parsed CLI flags for interactive and headless invocations.
type cliOptions struct {
	Prompt         string
	HasPrompt      bool
	OutputFormat   string
	PermissionMode string
	Cwd            string
	Provider       string
	ShowVersion    bool
	ShowHelp       bool
}

// HeadlessJSONOutput is the structured payload emitted to stdout when running
// with -p/--prompt and --output-format json.
type HeadlessJSONOutput struct {
	SessionID     string                 `json:"session_id"`
	AssistantText string                 `json:"assistant_text"`
	Receipt       agent.ExecutionReceipt `json:"receipt"`
	Error         string                 `json:"error,omitempty"`
}

// headlessDeps allows unit tests to inject configuration and a mock LLM client
// without requiring live API keys or external network calls.
type headlessDeps struct {
	Config        *config.AppConfig
	ClientFactory func(cfg *config.ProviderConfig, systemPrompt string) (llm.Client, error)
}

func parseCLIFlags(args []string) (cliOptions, error) {
	var opts cliOptions
	fs := flag.NewFlagSet("loomcode", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&opts.Prompt, "p", "", "Run a single task in non-interactive headless mode (pass '-' to read stdin)")
	fs.StringVar(&opts.Prompt, "prompt", "", "Run a single task in non-interactive headless mode (pass '-' to read stdin)")
	fs.StringVar(&opts.OutputFormat, "output-format", "text", "Headless output format: text or json")
	fs.StringVar(&opts.PermissionMode, "permission-mode", "", "Permission mode: default, acceptEdits, plan, or bypassPermissions")
	fs.StringVar(&opts.Cwd, "cwd", "", "Working directory to execute in")
	fs.StringVar(&opts.Provider, "provider", "", "Configured provider name to select")
	fs.BoolVar(&opts.ShowVersion, "version", false, "Print version and exit")
	fs.BoolVar(&opts.ShowVersion, "v", false, "Print version and exit")
	fs.BoolVar(&opts.ShowHelp, "help", false, "Print CLI usage and exit")
	fs.BoolVar(&opts.ShowHelp, "h", false, "Print CLI usage and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			opts.ShowHelp = true
			return opts, nil
		}
		return opts, err
	}

	fs.Visit(func(f *flag.Flag) {
		if f.Name == "p" || f.Name == "prompt" {
			opts.HasPrompt = true
		}
	})

	if opts.ShowHelp || opts.ShowVersion {
		return opts, nil
	}

	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected argument %q (use -p/--prompt to run a non-interactive task)", fs.Arg(0))
	}

	if opts.OutputFormat == "" {
		opts.OutputFormat = "text"
	}
	if opts.OutputFormat != "text" && opts.OutputFormat != "json" {
		return opts, fmt.Errorf("invalid --output-format %q: must be \"text\" or \"json\"", opts.OutputFormat)
	}

	if opts.PermissionMode != "" {
		if _, err := parsePermissionMode(opts.PermissionMode); err != nil {
			return opts, err
		}
	}

	return opts, nil
}

func wantsJSONOutput(args []string) bool {
	for i := 0; i < len(args); i++ {
		if args[i] == "--output-format=json" || args[i] == "-output-format=json" {
			return true
		}
		if (args[i] == "--output-format" || args[i] == "-output-format") && i+1 < len(args) && args[i+1] == "json" {
			return true
		}
	}
	return false
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `LoomCode v%s — Multi-Agent Terminal Coding Harness

Usage:
  loomcode [flags]
  loomcode -p "<task>" [flags]
  echo "<task>" | loomcode -p - [flags]

Flags:
  -p, --prompt <text>         Run a non-interactive headless task ('-' reads piped stdin)
      --output-format <fmt>   Headless output format: text (default) or json
      --permission-mode <m>   Permission mode: default, acceptEdits, plan, bypassPermissions
      --cwd <dir>             Working directory to operate in
      --provider <name>       Provider name (from .loomcode/config.yaml) to use
  -v, --version               Print version and exit
  -h, --help                  Show this help message and exit
`, version)
}

func parsePermissionMode(raw string) (permissions.PermissionMode, error) {
	mode := permissions.PermissionMode(strings.TrimSpace(raw))
	switch mode {
	case permissions.ModeDefault, permissions.ModeAcceptEdits, permissions.ModePlan, permissions.ModeBypass:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid permission mode %q: must be one of default, acceptEdits, plan, bypassPermissions", raw)
	}
}

func resolveHeadlessPermissionMode(cliMode, configMode string) (permissions.PermissionMode, error) {
	if strings.TrimSpace(cliMode) != "" {
		return parsePermissionMode(cliMode)
	}
	if strings.TrimSpace(configMode) != "" {
		return parsePermissionMode(configMode)
	}
	return permissions.ModeAcceptEdits, nil
}

func resolvePromptInput(raw string, stdin io.Reader) (string, error) {
	if raw == "-" {
		if stdin == nil {
			return "", fmt.Errorf("cannot read prompt from '-': stdin is nil")
		}
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read prompt from stdin: %w", err)
		}
		trimmed := strings.TrimRight(string(data), "\r\n")
		if strings.TrimSpace(trimmed) == "" {
			return "", fmt.Errorf("prompt from stdin is empty")
		}
		return trimmed, nil
	}
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("--prompt requires a non-empty task")
	}
	return raw, nil
}

func selectProvider(providers []config.ProviderConfig, selector string) (*config.ProviderConfig, error) {
	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers configured")
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		cp := providers[0]
		return &cp, nil
	}
	for i := range providers {
		if providers[i].Name == selector {
			cp := providers[i]
			return &cp, nil
		}
	}
	for i := range providers {
		if strings.EqualFold(providers[i].Name, selector) {
			cp := providers[i]
			return &cp, nil
		}
	}
	for i := range providers {
		if strings.EqualFold(providers[i].Protocol, selector) || strings.EqualFold(providers[i].Model, selector) {
			cp := providers[i]
			return &cp, nil
		}
	}
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Name)
	}
	return nil, fmt.Errorf("provider %q not found (configured: %s)", selector, strings.Join(names, ", "))
}

func changeWorkingDir(cwd string) (restore func(), workDir string, err error) {
	if strings.TrimSpace(cwd) == "" {
		wd, wdErr := os.Getwd()
		if wdErr != nil {
			return func() {}, "", fmt.Errorf("resolve working directory: %w", wdErr)
		}
		return func() {}, wd, nil
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return func() {}, "", fmt.Errorf("resolve --cwd %q: %w", cwd, err)
	}
	info, err := os.Stat(absCwd)
	if err != nil {
		return func() {}, "", fmt.Errorf("invalid --cwd %q: %w", cwd, err)
	}
	if !info.IsDir() {
		return func() {}, "", fmt.Errorf("invalid --cwd %q: not a directory", cwd)
	}
	origWd, origErr := os.Getwd()
	if err := os.Chdir(absCwd); err != nil {
		return func() {}, "", fmt.Errorf("chdir %q: %w", absCwd, err)
	}
	restoreFn := func() {
		if origErr == nil && origWd != "" {
			_ = os.Chdir(origWd)
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		wd = absCwd
	}
	return restoreFn, wd, nil
}

func buildHeadlessSkillSection(workDir string, catalog *skills.Catalog) string {
	if catalog == nil {
		return ""
	}
	metas := catalog.List()
	if len(metas) == 0 {
		return ""
	}
	skillsDir := filepath.Join(workDir, ".loomcode", "skills")
	var sb strings.Builder
	sb.WriteString("## Available Skills\n\n")
	sb.WriteString(fmt.Sprintf("Skills are installed at: %s\n", skillsDir))
	sb.WriteString("When creating new skills, always place them under this directory as <skill-name>/SKILL.md.\n\n")
	sb.WriteString("Only Skill names and one-line descriptions are listed below. To activate a Skill on demand call the LoadSkill tool with {name: \"<skill-name>\"}. After activation the Skill's full SOP gets pinned to the environment context, and any tools the Skill declares get registered. Users can also invoke a Skill directly with /<name>.\n\n")
	sb.WriteString("If the user pastes a Skill URL (skills.sh, github.com tree URL, or raw SKILL.md URL) and asks to install / add / get it, call the InstallSkill tool with {url: \"<url>\"} — the new Skill becomes available immediately afterwards.\n\n")
	for _, meta := range metas {
		desc := meta.Description
		if len(desc) > 200 {
			desc = desc[:200] + "…"
		}
		sb.WriteString(fmt.Sprintf("- /%s: %s\n", meta.Name, desc))
	}
	return sb.String()
}

func formatMCPInstructions(res mcp.ConnectResult) string {
	if len(res.Servers) == 0 {
		return ""
	}
	toolsByServer := make(map[string][]string)
	for _, t := range res.Tools {
		toolName := t.Name()
		for _, srv := range res.Servers {
			if strings.HasPrefix(toolName, "mcp__"+mcp.SanitizeName(srv.Name)+"__") {
				toolsByServer[srv.Name] = append(toolsByServer[srv.Name], toolName)
				break
			}
		}
	}
	var mcpParts []string
	for _, srv := range res.Servers {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("## %s\n", srv.Name))
		if srv.Instructions != "" {
			sb.WriteString(srv.Instructions + "\n")
		}
		if toolNames := toolsByServer[srv.Name]; len(toolNames) > 0 {
			sb.WriteString("\nAvailable tools: " + strings.Join(toolNames, ", "))
		}
		mcpParts = append(mcpParts, sb.String())
	}
	return "# MCP Server Instructions\n\nThe following MCP servers are connected. Use their tools when the user asks.\n\n" + strings.Join(mcpParts, "\n\n")
}

var headlessAtRefRE = regexp.MustCompile(`@([\w./_-]+)`)

func expandHeadlessAtRefs(workDir, text string) string {
	return headlessAtRefRE.ReplaceAllStringFunc(text, func(match string) string {
		rel := strings.TrimPrefix(match, "@")
		rel = strings.TrimSuffix(rel, "/")
		target := rel
		if !filepath.IsAbs(target) && workDir != "" {
			target = filepath.Join(workDir, target)
		}
		info, err := os.Stat(target)
		if err != nil || info.IsDir() {
			return match
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return match
		}
		content := string(data)
		if len(content) > 10000 {
			content = content[:10000] + "\n… (truncated)"
		}
		return fmt.Sprintf("[File: %s]\n```\n%s\n```", rel, content)
	})
}

func newHeadlessHookRunner(client llm.Client) func(promptText string, hctx hooks.HookContext) (string, error) {
	return func(promptText string, _ hooks.HookContext) (string, error) {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		conv := conversation.NewManager()
		conv.AddUserMessage(promptText)
		events, errs := client.Stream(c, conv, nil)
		var sb strings.Builder
		for ev := range events {
			if td, ok := ev.(llm.TextDelta); ok {
				sb.WriteString(td.Text)
			}
		}
		select {
		case err := <-errs:
			if err != nil {
				return "", err
			}
		default:
		}
		return sb.String(), nil
	}
}

func emitHeadlessError(
	format string,
	stdout, stderr io.Writer,
	sessionID, assistantText string,
	receipt agent.ExecutionReceipt,
	err error,
) int {
	if format == "json" {
		payload := HeadlessJSONOutput{
			SessionID:     sessionID,
			AssistantText: assistantText,
			Receipt:       receipt,
			Error:         err.Error(),
		}
		data, mErr := json.MarshalIndent(payload, "", "  ")
		if mErr != nil {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(data))
		return 1
	}

	if assistantText != "" && !strings.HasSuffix(assistantText, "\n") {
		fmt.Fprintln(stdout)
	}
	if receipt.Turns > 0 {
		fmt.Fprintln(stderr, receipt.FormatText())
	}
	fmt.Fprintf(stderr, "Error: %s\n", err)
	return 1
}

// runHeadless executes a single non-interactive agent run and returns a process exit code (0 or 1).
func runHeadless(
	parentCtx context.Context,
	opts cliOptions,
	stdin io.Reader,
	stdout, stderr io.Writer,
	deps headlessDeps,
) int {
	promptText, err := resolvePromptInput(opts.Prompt, stdin)
	if err != nil {
		return emitHeadlessError(opts.OutputFormat, stdout, stderr, "", "", agent.ExecutionReceipt{}, err)
	}

	restoreDir, workDir, err := changeWorkingDir(opts.Cwd)
	if err != nil {
		return emitHeadlessError(opts.OutputFormat, stdout, stderr, "", "", agent.ExecutionReceipt{}, err)
	}
	defer restoreDir()

	cfg := deps.Config
	if cfg == nil {
		cfg, err = config.LoadConfig("")
		if err != nil {
			return emitHeadlessError(opts.OutputFormat, stdout, stderr, "", "", agent.ExecutionReceipt{WorkDir: workDir}, err)
		}
	}

	provider, err := selectProvider(cfg.Providers, opts.Provider)
	if err != nil {
		return emitHeadlessError(opts.OutputFormat, stdout, stderr, "", "", agent.ExecutionReceipt{WorkDir: workDir}, err)
	}

	permMode, err := resolveHeadlessPermissionMode(opts.PermissionMode, cfg.PermissionMode)
	if err != nil {
		return emitHeadlessError(opts.OutputFormat, stdout, stderr, "", "", agent.ExecutionReceipt{WorkDir: workDir}, err)
	}

	ctx, stopSignals := signal.NotifyContext(parentCtx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	catalog := skills.LoadCatalog(workDir)
	instructionsContent := memory.LoadInstructions(workDir)
	memoryContent := memory.LoadAutoMemoryPrompt(workDir)

	env := prompt.DetectEnvironment(workDir)
	env.Model = provider.Model
	systemPrompt := prompt.BuildSystemPrompt(env, prompt.BuildOptions{
		SkillSection: buildHeadlessSkillSection(workDir, catalog),
	})

	var client llm.Client
	if deps.ClientFactory != nil {
		client, err = deps.ClientFactory(provider, systemPrompt)
	} else {
		client, err = llm.NewClient(provider, systemPrompt)
		if err == nil {
			llm.ResolveContextWindow(ctx, provider)
		}
	}
	if err != nil {
		return emitHeadlessError(opts.OutputFormat, stdout, stderr, "", "", agent.ExecutionReceipt{WorkDir: workDir}, err)
	}

	sessionID := session.NewID()
	fh := filehistory.New(workDir, sessionID)
	conv := conversation.NewManager()

	dt := tools.CreateDefaultTools()
	registry := dt.Registry
	dt.WriteFile.FileHistory = fh
	dt.EditFile.FileHistory = fh
	registry.Register(&tools.BashTool{WorkDir: workDir})
	registry.Register(&tools.AskUserQuestionTool{RequestCh: nil})

	taskMgr := agents.NewTaskManager()
	todoStore := todo.NewStore(workDir, sessionID)
	todoList := todo.NewTaskList(todoStore)
	loader := agents.NewAgentLoader(workDir)
	loader.LoadAll()
	teamMgr := teams.NewTeamManager()
	gitRoot := worktree.FindCanonicalGitRoot(workDir)

	registry.Register(&tools.EnterWorktreeTool{
		SessionID: sessionID,
		RepoRoot:  gitRoot,
	})
	registry.Register(&tools.ExitWorktreeTool{
		RepoRoot: gitRoot,
	})
	registry.Register(&todo.TaskCreateTool{List: todoList})
	registry.Register(&todo.TaskGetTool{List: todoList})
	registry.Register(&todo.TaskListTool{List: todoList})
	registry.Register(&todo.TaskUpdateTool{List: todoList})
	registry.Register(&tools.ToolSearchTool{Registry: registry, Protocol: provider.Protocol})
	registry.Register(&teams.TeamCreateTool{TeamMgr: teamMgr})
	registry.Register(&teams.TeamDeleteTool{TeamMgr: teamMgr})
	registry.Register(&teams.SendMessageTool{TeamMgr: teamMgr, SenderName: "lead"})

	ag := agent.New(client, registry, provider.Protocol)
	ag.WorkDir = workDir
	ag.ContextWindow = provider.GetContextWindow()
	ag.MaxOutputTokens = provider.GetMaxOutputTokens()
	ag.Instructions = instructionsContent
	ag.MemoryContent = memoryContent
	ag.FileHistory = fh
	ag.SetSessionID(sessionID)
	ag.NonInteractive = true

	registry.Register(&tools.ExitPlanModeTool{
		IsPlanMode: func() bool {
			return ag.Checker != nil && ag.Checker.Mode == permissions.ModePlan
		},
		PlanExists: func() bool {
			return planfile.PlanExists(workDir)
		},
	})

	sandboxAllow := []string{memory.GetAutoMemPath(workDir)}
	if userMem := memory.GetUserAutoMemPath(); userMem != "" {
		sandboxAllow = append(sandboxAllow, userMem)
	}
	ag.Checker = permissions.NewChecker(
		permissions.NewPathSandbox(workDir, sandboxAllow...),
		&permissions.RuleEngine{
			LocalPath: filepath.Join(workDir, ".loomcode", "permissions.local.yaml"),
		},
		permMode,
	)

	var hookEngine *hooks.Engine
	if len(cfg.Hooks) > 0 {
		if hErr := hooks.Validate(cfg.Hooks); hErr != nil {
			fmt.Fprintf(stderr, "Warning: hook configuration is invalid, running without hooks:\n%s\n", hErr)
		} else {
			hookEngine = hooks.NewEngine()
			hookEngine.LoadHooks(cfg.Hooks)
			hookEngine.AgentRunner = newHeadlessHookRunner(client)
			ag.Hooks = hookEngine
		}
	}

	ag.NotificationFn = func() []string {
		var notes []string
		for _, n := range taskMgr.DrainNotifications() {
			notes = append(notes, fmt.Sprintf("<task-notification>\n<task_id>%s</task_id>\n<status>%s</status>\n<summary>Agent %q %s</summary>\n<result>%s</result>\n</task-notification>",
				n.TaskID, n.Status, n.Name, n.Status, n.Output))
		}
		notes = append(notes, teams.DrainLeadMailbox(teamMgr)...)
		if hookEngine != nil {
			for _, r := range hookEngine.DrainNotifications() {
				if r.Output == "" || r.Output == "(async)" {
					continue
				}
				notes = append(notes, fmt.Sprintf("<hook-notification id=%q>\n%s\n</hook-notification>", r.HookID, r.Output))
			}
		}
		return notes
	}
	ag.ToolNameFilter = func(name string) bool {
		if len(teamMgr.ListTeams()) == 0 {
			return true
		}
		return teams.IsCoordinatorTool(name)
	}

	agentTool := &agents.AgentTool{
		Client:                 client,
		ModelResolver:          llm.NewModelResolver(*provider),
		Registry:               registry,
		Protocol:               provider.Protocol,
		TaskMgr:                taskMgr,
		Loader:                 loader,
		Conversation:           conv,
		TeamMgr:                teamMgr,
		ParentChecker:          ag.Checker,
		ParentReplacementState: ag.ReplacementState,
	}
	registry.Register(agentTool)

	if catalog != nil {
		for _, meta := range catalog.List() {
			if skill := catalog.Get(meta.Name); skill != nil && skill.IsDirectory {
				_, _ = skills.RegisterDirectoryTools(skill, registry)
			}
		}
		registry.Register(&skills.LoadSkillTool{
			Catalog: catalog,
			Host:    ag,
		})
		registry.Register(&skills.InstallSkillTool{
			Catalog: catalog,
			OnInstalled: func(name string) {
				if skill := catalog.Get(name); skill != nil && skill.IsDirectory {
					_, _ = skills.RegisterDirectoryTools(skill, registry)
				}
			},
		})
	}

	var mcpInstructions string
	if len(cfg.MCPServers) > 0 {
		mcpMgr := mcp.NewManager()
		serverConfigs := make([]mcp.ServerConfig, 0, len(cfg.MCPServers))
		for _, c := range cfg.MCPServers {
			serverConfigs = append(serverConfigs, mcp.ServerConfig{
				Name:       c.Name,
				Command:    c.Command,
				Args:       c.Args,
				URL:        c.URL,
				Transport:  c.Transport,
				Headers:    c.Headers,
				Env:        c.Env,
				TimeoutSec: c.TimeoutSec,
			})
		}
		mcpMgr.LoadConfigs(serverConfigs)
		defer mcpMgr.Close()

		mcpRes := mcpMgr.ConnectAll(ctx)
		for _, t := range mcpRes.Tools {
			registry.Register(t)
		}
		for _, errMsg := range mcpRes.Errors {
			fmt.Fprintf(stderr, "Warning: %s\n", errMsg)
		}
		mcpInstructions = formatMCPInstructions(mcpRes)
	}

	session.SaveMessage(workDir, sessionID, session.Message{
		Role:    "user",
		Content: promptText,
		Ts:      time.Now().Unix(),
	})

	conv.AddUserMessage(expandHeadlessAtRefs(workDir, promptText))
	if mcpInstructions != "" {
		conv.AddSystemReminder(mcpInstructions)
	}

	var assistantBuf strings.Builder
	var runErr error
	needTurnNewline := false

	for ev := range ag.Run(ctx, conv) {
		switch e := ev.(type) {
		case agent.StreamText:
			if needTurnNewline && e.Text != "" {
				assistantBuf.WriteByte('\n')
				if opts.OutputFormat == "text" {
					fmt.Fprint(stdout, "\n")
				}
				needTurnNewline = false
			}
			assistantBuf.WriteString(e.Text)
			if opts.OutputFormat == "text" {
				fmt.Fprint(stdout, e.Text)
			}
		case agent.TurnComplete:
			cur := assistantBuf.String()
			if cur != "" && !strings.HasSuffix(cur, "\n") {
				needTurnNewline = true
			}
		case agent.PermissionRequestEvent:
			select {
			case e.ResponseCh <- agent.PermDeny:
			default:
			}
		case agent.ErrorEvent:
			runErr = errors.New(e.Message)
		}
	}

	if runErr == nil && ctx.Err() != nil {
		runErr = ctx.Err()
	}

	assistantText := assistantBuf.String()
	if assistantText != "" {
		session.SaveMessage(workDir, sessionID, session.Message{
			Role:    "assistant",
			Content: assistantText,
			Ts:      time.Now().Unix(),
		})
	}

	receipt := ag.GetReceipt()
	if runErr != nil {
		return emitHeadlessError(opts.OutputFormat, stdout, stderr, sessionID, assistantText, receipt, runErr)
	}

	if opts.OutputFormat == "json" {
		payload := HeadlessJSONOutput{
			SessionID:     sessionID,
			AssistantText: assistantText,
			Receipt:       receipt,
		}
		data, mErr := json.MarshalIndent(payload, "", "  ")
		if mErr != nil {
			fmt.Fprintf(stderr, "Error marshaling JSON output: %s\n", mErr)
			return 1
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}

	if assistantText != "" && !strings.HasSuffix(assistantText, "\n") {
		fmt.Fprintln(stdout)
	}
	fmt.Fprintln(stderr, receipt.FormatText())
	return 0
}
