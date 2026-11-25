package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Starchaser1024/LoomCode/internal/commands"
	"github.com/Starchaser1024/LoomCode/internal/compact"
	"github.com/Starchaser1024/LoomCode/internal/conversation"
	"github.com/Starchaser1024/LoomCode/internal/filehistory"
	"github.com/Starchaser1024/LoomCode/internal/permissions"
	"github.com/Starchaser1024/LoomCode/internal/planfile"
	"github.com/Starchaser1024/LoomCode/internal/session"
	"github.com/Starchaser1024/LoomCode/internal/skills"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func (m Model) buildCommandContext(args string) *commands.Context {
	wd, _ := os.Getwd()
	modelName := ""
	if m.selectedProvider != nil {
		modelName = m.selectedProvider.Model
	}
	return &commands.Context{
		Args:    args,
		WorkDir: wd,
		Model:   modelName,
		MemoryList: func() []string {
			if m.memoryMgr == nil {
				return nil
			}
			return m.memoryMgr.GetMemories()
		},
		MemoryClear: func() {
			if m.memoryMgr != nil {
				m.memoryMgr.Clear()
			}
		},
		TokenCount: func() (int, int) {
			return m.totalInput, m.totalOutput
		},
		PermissionMode: func() string {
			if m.ag != nil && m.ag.Checker != nil {
				return string(m.ag.Checker.Mode)
			}
			return "default"
		},
		SetPermissionMode: func(mode string) error {
			if m.ag == nil || m.ag.Checker == nil {
				return fmt.Errorf("permission system not initialized")
			}
			target := permissions.PermissionMode(mode)
			switch target {
			case permissions.ModeDefault, permissions.ModeAcceptEdits, permissions.ModePlan, permissions.ModeBypass:
				m.ag.Checker.Mode = target
				return nil
			default:
				return fmt.Errorf("invalid mode: %s (expected: default|acceptEdits|plan|bypassPermissions)", mode)
			}
		},
		ToolCount: func() int {
			return len(m.registry.ListTools())
		},
		SessionInfo: func() string {
			return fmt.Sprintf("Current session: %d messages", len(m.chatMessages))
		},
		SkillList: func() []commands.SkillInfo {
			if m.skillCatalog == nil {
				return nil
			}
			var result []commands.SkillInfo
			for _, meta := range m.skillCatalog.List() {
				result = append(result, commands.SkillInfo{
					Name:        meta.Name,
					Description: meta.Description,
				})
			}
			return result
		},
		MCPInfo: func() string {
			return m.mcpServerInfo
		},
	}
}

func (m Model) executeCommand(name, args string) (tea.Model, tea.Cmd) {
	cmd := m.cmdRegistry.Find(name)
	if cmd == nil {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "error",
			content: fmt.Sprintf("Unknown command: /%s — type /help to see available commands", name),
		})
		m.updateViewport()
		return m, nil
	}

	if args == "" && cmd.ArgPrompt != "" {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "system",
			content: cmd.ArgPrompt,
		})
		m.updateViewport()
		return m, nil
	}

	ctx := m.buildCommandContext(args)

	switch cmd.Type {
	case commands.TypeLocalUI:
		switch name {
		case "clear":
			wd, _ := os.Getwd()
			m.chatMessages = nil
			m.committedUpTo = 0
			m.conversation = conversation.NewManager()
			m.sessionID = session.NewID()
			m.fileHistory = filehistory.New(wd, m.sessionID)
			m.defaultTools.EditFile.FileHistory = m.fileHistory
			m.defaultTools.WriteFile.FileHistory = m.fileHistory
			if m.defaultTools.FileStateCache != nil {
				m.defaultTools.FileStateCache.Clear()
			}
			if m.ag != nil {
				m.ag.ClearActiveSkills()
				m.ag.SetToolFilter(nil)
				m.ag.SetSessionID(m.sessionID)
				m.ag.FileHistory = m.fileHistory
			}
			m.updateViewport()
			return m, tea.Batch(
				func() tea.Msg { return tea.ClearScreen() },
				tea.Println(m.renderBanner()+"\n"),
			)
		case "plan":
			wd, _ := os.Getwd()
			if m.ag != nil && m.ag.Checker != nil {
				m.prePlanMode = m.ag.Checker.Mode
				m.ag.Checker.Mode = permissions.ModePlan
				planPath := planfile.GetOrCreatePlanPath(wd)
				m.ag.Checker.PlanFilePath = planPath
				m.chatMessages = append(m.chatMessages, chatMessage{
					role:    "system",
					content: fmt.Sprintf("Entered Plan mode. Plan file: %s\nExplore the codebase and design your approach.", planPath),
				})
			}
			if args != "" {
				m.updateViewport()
				return m.sendMessage(args)
			}
			m.updateViewport()
			return m, nil
		case "compact":
			if m.client == nil || m.conversation == nil {
				m.chatMessages = append(m.chatMessages, chatMessage{
					role: "error", content: "Compact requires an active provider.",
				})
				m.updateViewport()
				return m, nil
			}
			m.chatMessages = append(m.chatMessages, chatMessage{
				role: "system", content: "Compacting conversation…",
			})
			m.updateViewport()
			client := m.client
			conv := m.conversation
			window := 200000
			if m.selectedProvider != nil {
				window = m.selectedProvider.GetContextWindow()
			}
			var recovery *compact.RecoveryState
			var schemas []map[string]any
			if m.ag != nil {
				recovery = m.ag.RecoveryState
				schemas = m.ag.Registry.GetAllSchemas(m.ag.Protocol)
			}
			compactWD, _ := os.Getwd()
			compactSessionID := m.sessionID
			return m, func() tea.Msg {
				msg, err := compact.ForceCompact(context.Background(), conv, client, compactWD, compactSessionID, window, recovery, schemas)
				return compactDoneMsg{message: msg, err: err}
			}
		case "resume":
			return m.handleResume(args)
		case "rewind":
			return m.handleRewind(args)
		}

	case commands.TypePrompt:
		if cmd.Handler != nil {
			prompt := cmd.Handler(ctx)
			displayText := "/" + name
			if args != "" {
				displayText += " " + args
			}
			m.updateViewport()
			newModel, teaCmd := m.sendPromptCommand(displayText, prompt)
			if strings.HasSuffix(cmd.Description, "[skill]") {
				loadedLine := lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(
					fmt.Sprintf("skill(%s)\nSuccessfully loaded skill", name))
				return newModel, tea.Batch(tea.Println(loadedLine), teaCmd)
			}
			return newModel, teaCmd
		}

	case commands.TypeSkillFork:
		displayText := "/" + name
		if args != "" {
			displayText += " " + args
		}
		m.chatMessages = append(m.chatMessages, chatMessage{role: "user", content: displayText})
		userLine := promptStyle.Render("❯ ") + lipgloss.NewStyle().Foreground(brightText).Bold(true).Render(displayText)
		forkNotice := fmt.Sprintf("Forking skill %s in isolated sub-agent…", name)
		m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: forkNotice})
		m.committedUpTo = len(m.chatMessages)
		m.updateViewport()
		skillName := name
		skillArgs := args
		commitText := userLine + "\n" + lipgloss.NewStyle().Foreground(dimText).Render(forkNotice)
		return m, tea.Batch(
			tea.Println(commitText),
			func() tea.Msg {
				skill, err := m.skillCatalog.GetFull(skillName)
				if err != nil && skill == nil {
					return forkSkillDoneMsg{name: skillName, err: err}
				}
				if m.ag != nil {
					m.ag.RecoveryState.RecordSkillInvocation(skill.Meta.Name, skill.PromptBody)
				}
				result, runErr := skills.RunFork(context.Background(), skill, skillArgs, m)
				return forkSkillDoneMsg{name: skillName, result: result, err: runErr}
			},
		)

	case commands.TypeLocal:
		if cmd.Handler != nil {
			output := cmd.Handler(ctx)
			m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: output})
			m.updateViewport()
			return m, nil
		}
	}

	m.chatMessages = append(m.chatMessages, chatMessage{
		role:    "system",
		content: fmt.Sprintf("/%s — not yet implemented", name),
	})
	m.updateViewport()
	return m, nil
}
