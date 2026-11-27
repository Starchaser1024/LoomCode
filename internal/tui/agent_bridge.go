package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/agent"
	"github.com/Starchaser1024/LoomCode/internal/agents"
	"github.com/Starchaser1024/LoomCode/internal/commands"
	"github.com/Starchaser1024/LoomCode/internal/config"
	"github.com/Starchaser1024/LoomCode/internal/conversation"
	"github.com/Starchaser1024/LoomCode/internal/hooks"
	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/mcp"
	"github.com/Starchaser1024/LoomCode/internal/memory"
	"github.com/Starchaser1024/LoomCode/internal/memory/extractor"
	"github.com/Starchaser1024/LoomCode/internal/permissions"
	"github.com/Starchaser1024/LoomCode/internal/planfile"
	"github.com/Starchaser1024/LoomCode/internal/prompt"
	"github.com/Starchaser1024/LoomCode/internal/skills"
	"github.com/Starchaser1024/LoomCode/internal/teams"
	"github.com/Starchaser1024/LoomCode/internal/todo"
	"github.com/Starchaser1024/LoomCode/internal/tools"
	"github.com/Starchaser1024/LoomCode/internal/worktree"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) drainTaskNotifications() []string {
	var messages []string
	if m.taskMgr != nil {
		for _, n := range m.taskMgr.DrainNotifications() {
			msg := fmt.Sprintf("<task-notification>\n<task_id>%s</task_id>\n<status>%s</status>\n<summary>Agent \"%s\" %s</summary>\n<result>%s</result>\n</task-notification>",
				n.TaskID, n.Status, n.Name, n.Status, n.Output)
			messages = append(messages, msg)
		}
	}
	messages = append(messages, teams.DrainLeadMailbox(m.teamMgr)...)
	if m.ag != nil && m.ag.Hooks != nil {
		for _, r := range m.ag.Hooks.DrainNotifications() {
			if r.Output == "" || r.Output == "(async)" {
				continue
			}
			messages = append(messages, fmt.Sprintf("<hook-notification id=%q>\n%s\n</hook-notification>", r.HookID, r.Output))
		}
	}
	return messages
}

func newAgentHookRunner(client llm.Client) func(prompt string, ctx hooks.HookContext) (string, error) {
	return func(prompt string, _ hooks.HookContext) (string, error) {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		conv := conversation.NewManager()
		conv.AddUserMessage(prompt)
		events, errs := client.Stream(c, conv, nil)
		var text string
		for ev := range events {
			if td, ok := ev.(llm.TextDelta); ok {
				text += td.Text
			}
		}
		select {
		case err := <-errs:
			if err != nil {
				return "", err
			}
		default:
		}
		return text, nil
	}
}

func coordinatorToolFilter(teamMgr *teams.TeamManager) func(name string) bool {
	if teamMgr == nil {
		return nil
	}
	return func(name string) bool {
		if len(teamMgr.ListTeams()) == 0 {
			return true
		}
		return teams.IsCoordinatorTool(name)
	}
}

func (m *Model) registerAgentTools(client llm.Client, providerCfg *config.ProviderConfig, protocol, wd string) {
	m.taskMgr = agents.NewTaskManager()

	store := todo.NewStore(wd, m.sessionID)
	m.todoList = todo.NewTaskList(store)

	m.memoryMgr = memory.NewManager(wd)

	loader := agents.NewAgentLoader(wd)
	loader.LoadAll()

	teamMgr := teams.NewTeamManager()
	m.teamMgr = teamMgr

	gitRoot := worktree.FindCanonicalGitRoot(wd)
	m.registry.Register(&tools.EnterWorktreeTool{
		SessionID: m.sessionID,
		RepoRoot:  gitRoot,
	})
	m.registry.Register(&tools.ExitWorktreeTool{
		RepoRoot: gitRoot,
	})

	if gitRoot != "" {
		if savedSession, err := worktree.LoadWorktreeSession(gitRoot); err == nil && savedSession != nil {
			if info, err := os.Stat(savedSession.WorktreePath); err == nil && info.IsDir() {
				worktree.RestoreWorktreeSession(savedSession)
			}
		}
	}

	worktree.StartCleanupLoop(context.Background())

	m.registry.Register(&tools.ExitPlanModeTool{
		IsPlanMode: func() bool {
			return m.ag != nil && m.ag.Checker != nil && m.ag.Checker.Mode == permissions.ModePlan
		},
		PlanExists: func() bool {
			wd, _ := os.Getwd()
			return planfile.PlanExists(wd)
		},
	})
	m.registry.Register(&todo.TaskCreateTool{List: m.todoList})
	m.registry.Register(&todo.TaskGetTool{List: m.todoList})
	m.registry.Register(&todo.TaskListTool{List: m.todoList})
	m.registry.Register(&todo.TaskUpdateTool{List: m.todoList})
	m.registry.Register(&tools.ToolSearchTool{Registry: m.registry, Protocol: protocol})
	m.registry.Register(&teams.TeamCreateTool{TeamMgr: teamMgr})
	m.registry.Register(&teams.TeamDeleteTool{TeamMgr: teamMgr})
	m.registry.Register(&teams.SendMessageTool{TeamMgr: teamMgr, SenderName: "lead"})
	m.registry.Register(&agents.AgentTool{
		Client:        client,
		ModelResolver: llm.NewModelResolver(*providerCfg),
		Registry:      m.registry,
		Protocol:      protocol,
		TaskMgr:       m.taskMgr,
		ProgressCh:    m.subAgentProgressCh,
		Loader:        loader,
		Conversation:  m.conversation,
		TeamMgr:       teamMgr,
	})
}

func (m *Model) initMCPServersCmd() tea.Cmd {
	if len(m.mcpConfigs) == 0 {
		return nil
	}

	m.mcpConnecting = true
	configs := m.mcpConfigs

	return func() tea.Msg {
		mgr := mcp.NewManager()
		var serverConfigs []mcp.ServerConfig
		for _, c := range configs {
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
		mgr.LoadConfigs(serverConfigs)
		return mcpReadyMsg{result: mgr.ConnectAll(context.Background())}
	}
}

func (m Model) handleMCPReady(msg mcpReadyMsg) (tea.Model, tea.Cmd) {
	m.mcpConnecting = false
	m.mcpMgr = msg.result.Mgr
	for _, t := range msg.result.Tools {
		m.registry.Register(t)
	}
	var mcpPrintLines []string
	for _, errMsg := range msg.result.Errors {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "error",
			content: errMsg,
		})
		mcpPrintLines = append(mcpPrintLines, errorStyle.Render("✖ "+errMsg))
	}
	registered := len(msg.result.Tools)
	if registered > 0 {
		m.mcpServerInfo = fmt.Sprintf("Connected to %d MCP server(s), %d tools registered", len(m.mcpConfigs)-len(msg.result.Errors), registered)
	}
	m.committedUpTo = len(m.chatMessages)
	if len(msg.result.Servers) > 0 {
		toolsByServer := make(map[string][]string)
		for _, t := range msg.result.Tools {
			toolName := t.Name()
			for _, srv := range msg.result.Servers {
				if strings.HasPrefix(toolName, "mcp__"+mcp.SanitizeName(srv.Name)+"__") {
					toolsByServer[srv.Name] = append(toolsByServer[srv.Name], toolName)
					break
				}
			}
		}

		var mcpParts []string
		for _, srv := range msg.result.Servers {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("## %s\n", srv.Name))
			if srv.Instructions != "" {
				sb.WriteString(srv.Instructions + "\n")
			}
			if toolNames, ok := toolsByServer[srv.Name]; ok && len(toolNames) > 0 {
				sb.WriteString("\nAvailable tools: " + strings.Join(toolNames, ", "))
			}
			mcpParts = append(mcpParts, sb.String())
		}
		m.mcpInstructions = "# MCP Server Instructions\n\nThe following MCP servers are connected. Use their tools when the user asks.\n\n" + strings.Join(mcpParts, "\n\n")
	}
	m.updateViewport()
	if len(mcpPrintLines) > 0 {
		return m, tea.Println(strings.Join(mcpPrintLines, "\n"))
	}
	return m, nil
}

func (m *Model) loadSkillsAndBuildPrompt(wd string) string {
	catalog := skills.LoadCatalog(wd)
	m.skillCatalog = catalog

	skillsDir := filepath.Join(wd, ".loomcode", "skills")
	skillSection := ""
	if catalog != nil {
		metas := catalog.List()
		if len(metas) > 0 {
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
			skillSection = sb.String()
		}
	}

	for _, cmd := range commands.LoadUserCommands(wd) {
		if m.cmdRegistry.HasConflict(cmd) {
			continue
		}
		m.cmdRegistry.Register(cmd)
	}

	m.instructionsContent = m.loadCustomInstructions(wd)
	m.memoryContent = memory.LoadAutoMemoryPrompt(wd)

	env := prompt.DetectEnvironment(wd)
	if m.selectedProvider != nil {
		env.Model = m.selectedProvider.Model
	}

	return prompt.BuildSystemPrompt(env, prompt.BuildOptions{
		SkillSection: skillSection,
	})
}

func (m *Model) wireSkillsToAgent() {
	if m.skillCatalog == nil || m.ag == nil {
		return
	}
	for _, meta := range m.skillCatalog.List() {
		m.registerSkillCommand(meta.Name)
		if skill := m.skillCatalog.Get(meta.Name); skill != nil && skill.IsDirectory {
			_, _ = skills.RegisterDirectoryTools(skill, m.registry)
		}
	}
	m.registry.Register(&skills.LoadSkillTool{
		Catalog: m.skillCatalog,
		Host:    m,
	})
	m.registry.Register(&skills.InstallSkillTool{
		Catalog: m.skillCatalog,
		OnInstalled: func(name string) {
			m.registerSkillCommand(name)
			if skill := m.skillCatalog.Get(name); skill != nil && skill.IsDirectory {
				_, _ = skills.RegisterDirectoryTools(skill, m.registry)
			}
		},
	})
}

func (m *Model) registerSkillCommand(name string) {
	if m.skillCatalog == nil || m.cmdRegistry == nil {
		return
	}
	if m.cmdRegistry.Find(name) != nil {
		return
	}
	meta := m.skillCatalog.Get(name)
	if meta == nil {
		return
	}
	cmd := &commands.Command{
		Name:        name,
		Description: meta.Meta.Description + " [skill]",
	}
	if meta.Meta.IsFork() {
		cmd.Type = commands.TypeSkillFork
		cmd.Handler = func(ctx *commands.Context) string { return "" }
	} else {
		cmd.Type = commands.TypePrompt
		captured := name
		cmd.Handler = func(ctx *commands.Context) string {
			skill, err := m.skillCatalog.GetFull(captured)
			if err != nil && skill == nil {
				return fmt.Sprintf("[skill error] %v", err)
			}
			body, runErr := skills.RunInline(context.Background(), skill, ctx.Args, m)
			if runErr != nil {
				return fmt.Sprintf("[skill error] %v", runErr)
			}
			if m.ag != nil {
				m.ag.RecoveryState.RecordSkillInvocation(skill.Meta.Name, body)
			}
			return body
		}
	}
	m.cmdRegistry.Register(cmd)
}

// ----- SkillForkHost implementation on Model -----

func (m Model) ActivateSkill(name, body string) {
	if m.ag != nil {
		m.ag.ActivateSkill(name, body)
	}
}

func (m Model) SetToolFilter(allow func(name string) bool) {
	if m.ag == nil {
		return
	}
	m.ag.SetToolFilter(allow)
}

func (m Model) ToolRegistry() *tools.Registry {
	return m.registry
}

func (m Model) SnapshotParentMessages() []conversation.Message {
	if m.conversation == nil {
		return nil
	}
	src := m.conversation.GetMessages()
	out := make([]conversation.Message, len(src))
	copy(out, src)
	return out
}

func (m Model) RunSubAgent(ctx context.Context, body string, seed []conversation.Message, allowedTools []string, _ string) (string, error) {
	if m.client == nil {
		return "", fmt.Errorf("RunSubAgent: no llm client (provider not selected)")
	}
	subConv := conversation.NewManager()
	for _, msg := range seed {
		switch msg.Role {
		case "user":
			subConv.AddUserMessage(msg.Content)
		case "assistant":
			subConv.AddAssistantMessage(msg.Content)
		}
	}
	subConv.AddUserMessage(body)

	subAgent := agent.New(m.client, m.registry, "")
	if m.selectedProvider != nil {
		subAgent.Protocol = m.selectedProvider.Protocol
		subAgent.ContextWindow = m.selectedProvider.GetContextWindow()
		subAgent.MaxOutputTokens = m.selectedProvider.GetMaxOutputTokens()
	}
	subAgent.MaxIterations = 50
	if len(allowedTools) > 0 {
		allowed := make(map[string]bool, len(allowedTools))
		for _, name := range allowedTools {
			allowed[name] = true
		}
		subAgent.ToolNameFilter = func(name string) bool {
			if t := m.registry.Get(name); t != nil && tools.IsSystemTool(t) {
				return true
			}
			return allowed[name]
		}
	}

	var output strings.Builder
	ch := subAgent.Run(ctx, subConv)
	for ev := range ch {
		switch e := ev.(type) {
		case agent.StreamText:
			output.WriteString(e.Text)
		case agent.ErrorEvent:
			return output.String(), fmt.Errorf("%s", e.Message)
		}
	}
	return output.String(), nil
}

func (m *Model) loadCustomInstructions(wd string) string {
	return memory.LoadInstructions(wd)
}

// installMemoryExtractor wires background memory extraction onto the given agent via OnLoopComplete.
func (m *Model) installMemoryExtractor(ag *agent.Agent, wd, protocol string) *extractor.Extractor {
	if m.client == nil || m.conversation == nil {
		return nil
	}
	conv := m.conversation
	extr := extractor.InitExtractMemories(extractor.Deps{
		MemoryDir:     memory.GetAutoMemPath(wd),
		UserMemoryDir: memory.GetUserAutoMemPath(),
		ProjectRoot:   wd,
		Client:        m.client,
		ToolRegistry:  m.registry,
		Protocol:      protocol,
		Conversation:  conv,
		AppendSystem:  func(s string) { conv.AddSystemReminder(s) },
	})
	ag.OnLoopComplete = func(_ *conversation.Manager) {
		_ = extr.Execute(context.Background())
	}
	return extr
}

func (m *Model) prefetchRelevantMemories(query string) <-chan string {
	out := make(chan string, 1)
	if m.memoryMgr == nil || m.selectedProvider == nil {
		out <- ""
		return out
	}
	provider := m.selectedProvider
	memDir := m.memoryMgr.Dir()
	userMemDir := m.memoryMgr.UserDir()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		selector := func(ctx context.Context, sys, user string) (string, error) {
			sideClient, err := llm.NewClient(provider, sys)
			if err != nil {
				return "", err
			}
			miniConv := conversation.NewManager()
			miniConv.AddUserMessage(user)
			events, errs := sideClient.Stream(ctx, miniConv, nil)
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
		results, _ := memory.FindRelevantMemories(ctx, query, userMemDir, memDir, nil, nil, selector)
		out <- renderRelevantMemoriesReminder(results)
	}()
	return out
}

func collectPrefetchedRecall(conv *conversation.Manager, prefetchCh <-chan string, timeout time.Duration) {
	if conv == nil || prefetchCh == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case reminder := <-prefetchCh:
		if reminder != "" {
			conv.AddSystemReminder(reminder)
		}
	case <-timer.C:
	}
}

func renderRelevantMemoriesReminder(memories []memory.RelevantMemory) string {
	if len(memories) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("The following relevant memories from prior conversations may help:\n\n")
	for _, mem := range memories {
		data, err := os.ReadFile(mem.Path)
		if err != nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("## Memory: %s (saved %s)\n\n", filepath.Base(mem.Path), memory.MemoryAge(mem.MtimeMs)))
		if note := memory.MemoryFreshnessText(mem.MtimeMs); note != "" {
			sb.WriteString(note)
			sb.WriteString("\n\n")
		}
		sb.Write(data)
		sb.WriteString("\n\n---\n\n")
	}
	return sb.String()
}
