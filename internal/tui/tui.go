// Package tui implements LoomCode's interactive Bubble Tea terminal user interface.
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
	"github.com/Starchaser1024/LoomCode/internal/filehistory"
	"github.com/Starchaser1024/LoomCode/internal/history"
	"github.com/Starchaser1024/LoomCode/internal/hooks"
	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/mcp"
	"github.com/Starchaser1024/LoomCode/internal/memory"
	"github.com/Starchaser1024/LoomCode/internal/memory/extractor"
	"github.com/Starchaser1024/LoomCode/internal/permissions"
	"github.com/Starchaser1024/LoomCode/internal/session"
	"github.com/Starchaser1024/LoomCode/internal/skills"
	"github.com/Starchaser1024/LoomCode/internal/teams"
	"github.com/Starchaser1024/LoomCode/internal/todo"
	"github.com/Starchaser1024/LoomCode/internal/tools"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type appState int

const (
	stateProviderSelect appState = iota
	stateChat
	stateResume
)

type chatMessage struct {
	role          string
	content       string
	toolGroup     []toolBlockInfo
	subAgentBlock *subAgentBlock
	expanded      bool
}

type subAgentBlock struct {
	desc      string
	agentType string
	toolUses  []toolBlockInfo
	done      bool
	toolCount int
	totalTime float64
}

type toolBlockInfo struct {
	toolName  string
	args      map[string]any
	output    string
	isError   bool
	elapsed   float64
	collapsed bool
	loading   bool
}

type agentEventMsg struct {
	event agent.AgentEvent
	ch    <-chan agent.AgentEvent
}
type agentDoneMsg struct {
	ch <-chan agent.AgentEvent
}
type agentErrMsg struct{ err error }
type agentReadyMsg struct {
	ch <-chan agent.AgentEvent
}
type mailboxPollMsg struct{}
type mcpReadyMsg struct{ result mcp.ConnectResult }
type compactDoneMsg struct {
	message string
	err     error
}
type initSingleProviderMsg struct{}
type askUserMsg struct {
	req tools.AskUserRequest
}
type subAgentProgressMsg struct {
	progress agents.SubAgentProgress
}

// forkSkillDoneMsg is dispatched when a fork-mode Skill's sub-agent has
// reached LoopComplete (or failed).
type forkSkillDoneMsg struct {
	name   string
	result string
	err    error
}

type Model struct {
	providers        []config.ProviderConfig
	selectedProvider *config.ProviderConfig
	client           llm.Client
	registry         *tools.Registry
	ag               *agent.Agent

	state     appState
	streaming bool

	textarea textarea.Model
	viewport viewport.Model
	width    int
	height   int
	ready    bool

	providerCursor int

	conversation *conversation.Manager

	chatMessages []chatMessage
	toolBlocks   []toolBlockInfo
	streamBuf    string
	agentCh      <-chan agent.AgentEvent
	cancelStream context.CancelFunc

	totalInput  int
	totalOutput int

	permDialog   bool
	permToolName string
	permDesc     string
	permRespCh   chan<- agent.PermissionResponse
	permCursor   int

	cmdRegistry   *commands.Registry
	slashMenuOpen bool
	slashMatches  []*commands.Command
	slashCursor   int

	userScrolled  bool
	committedUpTo int
	bannerPrinted bool

	atMenuOpen bool
	atMatches  []string
	atCursor   int
	atPrefix   string

	spinner       spinner.Model
	thinking      bool
	thinkingStart time.Time
	thinkingDone  float64
	thinkingVerb  string

	instructionsContent string
	memoryContent       string

	mcpConfigs        []config.MCPServerConfig
	mcpMgr            *mcp.Manager
	mcpConnecting     bool
	mcpInstructions   string
	mcpInstructionsOK bool
	mcpServerInfo     string
	hookConfigs       []hooks.Hook

	historyEntries []string
	historyIndex   int
	historyDraft   string

	sessionID    string
	fileHistory  *filehistory.History
	defaultTools tools.DefaultTools
	prePlanMode  permissions.PermissionMode

	planApprovalDialog bool
	planApprovalCursor int
	planApprovalInput  string

	rewindDialog       bool
	rewindSnapshots    []filehistory.Snapshot
	rewindCursor       int
	rewindPhase        int // 0=select checkpoint, 1=select restore option
	rewindOptionCursor int

	askUserCh          chan tools.AskUserRequest
	subAgentProgressCh chan agents.SubAgentProgress
	activeSubAgent     *subAgentBlock
	askUserDialog      bool
	askUserQuestions   []tools.Question
	askUserCursors     []int
	askUserSelected    []map[int]bool
	askUserOther       []string
	askUserQIdx        int
	askUserRespCh      chan tools.QuestionResponse
	askUserAnswered    map[int]string
	askUserOnSubmit    bool
	askUserSubmitIdx   int
	skillCatalog       *skills.Catalog
	taskMgr            *agents.TaskManager
	todoList           *todo.TaskList
	memoryMgr          *memory.Manager
	memoryExtractor    *extractor.Extractor
	teamMgr            *teams.TeamManager

	resumeSessions  []session.SessionInfo
	resumeFiltered  []session.SessionInfo
	resumeCursor    int
	resumeSearch    string
	resumeScrollTop int
}

func New(providers []config.ProviderConfig, mcpConfigs []config.MCPServerConfig, hookConfigs []hooks.Hook) Model {
	ta := textarea.New()
	ta.Placeholder = "Send a message..."
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.ShowLineNumbers = false
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.BlurredStyle.Base = lipgloss.NewStyle()
	ta.SetHeight(1)

	sp := spinner.New()
	sp.Spinner = spinner.Spinner{
		Frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		FPS:    80 * time.Millisecond,
	}
	sp.Style = lipgloss.NewStyle().Foreground(brandPurple)

	askCh := make(chan tools.AskUserRequest, 1)
	subProgressCh := make(chan agents.SubAgentProgress, 32)
	dt := tools.CreateDefaultTools()
	reg := dt.Registry
	reg.Register(&tools.AskUserQuestionTool{RequestCh: askCh})

	m := Model{
		providers:          providers,
		mcpConfigs:         mcpConfigs,
		hookConfigs:        hookConfigs,
		state:              stateProviderSelect,
		textarea:           ta,
		conversation:       conversation.NewManager(),
		registry:           reg,
		defaultTools:       dt,
		cmdRegistry:        commands.CreateDefaultRegistry(),
		spinner:            sp,
		askUserCh:          askCh,
		subAgentProgressCh: subProgressCh,
	}

	if len(providers) == 1 {
		m.state = stateChat
	}

	return m
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink}
	if len(m.providers) == 1 {
		cmds = append(cmds, func() tea.Msg { return initSingleProviderMsg{} })
	}
	return tea.Batch(cmds...)
}

func (m *Model) initProviderSession(p *config.ProviderConfig) error {
	m.selectedProvider = p
	wd, _ := os.Getwd()
	systemPrompt := m.loadSkillsAndBuildPrompt(wd)
	client, err := llm.NewClient(p, systemPrompt)
	if err != nil {
		return err
	}
	m.client = client
	m.sessionID = session.NewID()
	m.fileHistory = filehistory.New(wd, m.sessionID)
	m.defaultTools.EditFile.FileHistory = m.fileHistory
	m.defaultTools.WriteFile.FileHistory = m.fileHistory
	m.registerAgentTools(client, p, p.Protocol, wd)

	llm.ResolveContextWindow(context.Background(), p)
	ag := agent.New(client, m.registry, p.Protocol)
	ag.ContextWindow = p.GetContextWindow()
	ag.MaxOutputTokens = p.GetMaxOutputTokens()
	ag.Instructions = m.instructionsContent
	ag.MemoryContent = m.memoryContent
	ag.FileHistory = m.fileHistory
	ag.SetSessionID(m.sessionID)

	sandboxAllow := []string{memory.GetAutoMemPath(wd)}
	if userMem := memory.GetUserAutoMemPath(); userMem != "" {
		sandboxAllow = append(sandboxAllow, userMem)
	}
	ag.Checker = permissions.NewChecker(
		permissions.NewPathSandbox(wd, sandboxAllow...),
		&permissions.RuleEngine{
			LocalPath: filepath.Join(wd, ".loomcode", "permissions.local.yaml"),
		},
		permissions.ModeDefault,
	)
	ag.NotificationFn = m.drainTaskNotifications
	ag.ToolNameFilter = coordinatorToolFilter(m.teamMgr)
	if len(m.hookConfigs) > 0 {
		eng := hooks.NewEngine()
		eng.LoadHooks(m.hookConfigs)
		eng.AgentRunner = newAgentHookRunner(client)
		ag.Hooks = eng
	}
	m.ag = ag
	if at, ok := m.registry.Get("Agent").(*agents.AgentTool); ok {
		at.ParentChecker = ag.Checker
		at.ParentReplacementState = ag.ReplacementState
	}
	m.wireSkillsToAgent()
	m.memoryExtractor = m.installMemoryExtractor(ag, wd, p.Protocol)
	m.historyEntries = history.Load(wd)
	return nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.textarea.SetWidth(msg.Width - 4)

		statusHeight := 1
		sepHeight := 2
		inputHeight := m.textarea.Height() + 1
		vpHeight := msg.Height - statusHeight - sepHeight - inputHeight - 1
		if vpHeight < 1 {
			vpHeight = 1
		}

		if !m.ready {
			m.viewport = viewport.New(msg.Width, vpHeight)
			m.viewport.MouseWheelEnabled = false
			m.ready = true
			m.bannerPrinted = true
			m.updateViewport()
			return m, tea.Println(m.renderBanner() + "\n")
		}
		m.viewport.Width = msg.Width
		m.viewport.Height = vpHeight
		m.updateViewport()
		return m, nil

	case subAgentProgressMsg:
		p := msg.progress
		if p.Done {
			if m.activeSubAgent != nil {
				m.activeSubAgent.done = true
				m.activeSubAgent.toolCount = p.ToolCount
				m.activeSubAgent.totalTime = p.TotalTime
			}
		} else {
			if m.activeSubAgent == nil || m.activeSubAgent.done {
				m.activeSubAgent = &subAgentBlock{
					desc:      p.AgentDesc,
					agentType: p.AgentType,
				}
			}
			m.activeSubAgent.toolUses = append(m.activeSubAgent.toolUses, toolBlockInfo{
				toolName: p.ToolName,
				args:     p.ToolArgs,
				elapsed:  p.Elapsed,
				isError:  p.IsError,
			})
		}
		m.updateViewport()
		return m, m.listenForSubAgentProgress()

	case askUserMsg:
		m.askUserDialog = true
		m.askUserQuestions = msg.req.Questions
		m.askUserRespCh = msg.req.ResponseCh
		m.askUserQIdx = 0
		m.askUserCursors = make([]int, len(msg.req.Questions))
		m.askUserSelected = make([]map[int]bool, len(msg.req.Questions))
		m.askUserOther = make([]string, len(msg.req.Questions))
		m.askUserAnswered = make(map[int]string)
		m.askUserOnSubmit = false
		m.askUserSubmitIdx = 0
		for i := range msg.req.Questions {
			m.askUserSelected[i] = make(map[int]bool)
		}
		m.updateViewport()
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.streaming && m.cancelStream != nil {
				m.cancelStream()
				m.savePartialResponse()
				m.finishStreaming()
				return m, nil
			}
			if m.cancelStream != nil {
				m.cancelStream()
			}
			if m.mcpMgr != nil {
				m.mcpMgr.Shutdown()
			}
			if m.memoryExtractor != nil {
				_ = m.memoryExtractor.Drain(5000)
			}
			return m, tea.Quit
		}

		if m.permDialog {
			return m.handlePermDialog(msg)
		}

		switch m.state {
		case stateProviderSelect:
			return m.handleProviderSelect(msg)
		case stateChat:
			return m.handleChat(msg)
		case stateResume:
			return m.handleResumeKeys(msg)
		}

	case initSingleProviderMsg:
		if err := m.initProviderSession(&m.providers[0]); err != nil {
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: err.Error()})
			m.updateViewport()
			return m, nil
		}
		m.textarea.Focus()
		m.updateViewport()
		return m, m.initMCPServersCmd()

	case forkSkillDoneMsg:
		var commit string
		if msg.err != nil {
			line := fmt.Sprintf("Skill %s (fork) failed: %v", msg.name, msg.err)
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: line})
			commit = errorStyle.Render("✖ " + line)
		} else {
			result := strings.TrimSpace(msg.result)
			if result == "" {
				result = fmt.Sprintf("(Skill %s returned no output)", msg.name)
			}
			m.chatMessages = append(m.chatMessages, chatMessage{role: "assistant", content: result})
			commit = m.renderMessagesRange(len(m.chatMessages)-1, len(m.chatMessages))
		}
		m.committedUpTo = len(m.chatMessages)
		m.updateViewport()
		if commit != "" {
			return m, tea.Println(commit)
		}
		return m, nil

	case compactDoneMsg:
		switch {
		case msg.err != nil:
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: "Compact failed: " + msg.err.Error()})
		case msg.message == "":
			m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: "Compact: no changes."})
		default:
			m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: "Compact: " + msg.message})
		}
		m.updateViewport()
		return m, nil

	case mcpReadyMsg:
		return m.handleMCPReady(msg)

	case spinner.TickMsg:
		if m.streaming {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			m.updateViewport()
			return m, cmd
		}

	case agentReadyMsg:
		m.agentCh = msg.ch
		return m, m.listenForAgentEvents()

	case agentEventMsg:
		if msg.ch != m.agentCh {
			return m, nil
		}
		return m.handleAgentEvent(msg.event)

	case agentDoneMsg:
		if msg.ch != m.agentCh {
			return m, nil
		}
		m.finishStreaming()
		return m, nil

	case agentErrMsg:
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "error",
			content: msg.err.Error(),
		})
		m.finishStreaming()
		return m, nil

	case mailboxPollMsg:
		return m.handleMailboxPoll()
	}

	return m, nil
}

func (m *Model) savePartialResponse() {
	if m.streamBuf != "" {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "assistant",
			content: m.streamBuf,
		})
		m.conversation.AddAssistantMessage(m.streamBuf)
		wd, _ := os.Getwd()
		session.SaveMessage(wd, m.sessionID, session.Message{
			Role: "assistant", Content: m.streamBuf, Ts: time.Now().Unix(),
		})
		m.streamBuf = ""
	}
	m.toolBlocks = nil

	msgs := m.conversation.GetMessages()
	if len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		if last.Role == "assistant" && len(last.ToolUses) > 0 {
			var results []conversation.ToolResultBlock
			for _, tu := range last.ToolUses {
				results = append(results, conversation.ToolResultBlock{
					ToolUseID: tu.ToolUseID,
					Content:   "Tool execution was interrupted by user.",
					IsError:   true,
				})
			}
			m.conversation.AddToolResultsMessage(results)
		}
	}

	m.chatMessages = append(m.chatMessages, chatMessage{
		role:    "system",
		content: "(response interrupted)",
	})
	m.updateViewport()
}

func (m *Model) finishStreaming() {
	if m.thinking {
		m.thinkingDone = time.Since(m.thinkingStart).Seconds()
		m.thinking = false
	}
	m.streaming = false
	m.cancelStream = nil
	m.agentCh = nil
	m.updateViewport()
}
