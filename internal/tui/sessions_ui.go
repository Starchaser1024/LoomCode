package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/conversation"
	"github.com/Starchaser1024/LoomCode/internal/filehistory"
	"github.com/Starchaser1024/LoomCode/internal/session"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func (m Model) handleResume(args string) (tea.Model, tea.Cmd) {
	wd, _ := os.Getwd()
	sessions := session.ListSessions(wd)

	if args != "" {
		return m.doResumeSession(wd, args, sessions)
	}

	if len(sessions) == 0 {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role: "system", content: "No previous sessions found.",
		})
		m.updateViewport()
		return m, nil
	}

	m.resumeSessions = sessions
	m.resumeFiltered = sessions
	m.resumeCursor = 0
	m.resumeSearch = ""
	m.resumeScrollTop = 0
	m.state = stateResume
	return m, nil
}

func (m Model) doResumeSession(wd, targetID string, sessions []session.SessionInfo) (tea.Model, tea.Cmd) {
	var idx int
	if n, _ := fmt.Sscanf(targetID, "%d", &idx); n == 1 && idx >= 1 && idx <= len(sessions) {
		targetID = sessions[idx-1].ID
	}

	msgs := session.LoadSession(wd, strings.TrimSpace(targetID))
	if len(msgs) == 0 {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role: "error", content: fmt.Sprintf("Session '%s' not found or empty.", targetID),
		})
		m.updateViewport()
		return m, nil
	}

	m.chatMessages = nil
	m.committedUpTo = 0
	m.conversation = conversation.NewManager()
	m.sessionID = strings.TrimSpace(targetID)
	m.fileHistory = filehistory.New(wd, m.sessionID)
	m.defaultTools.EditFile.FileHistory = m.fileHistory
	m.defaultTools.WriteFile.FileHistory = m.fileHistory
	if m.defaultTools.FileStateCache != nil {
		m.defaultTools.FileStateCache.Clear()
	}
	if m.ag != nil {
		m.ag.SetSessionID(m.sessionID)
		m.ag.FileHistory = m.fileHistory
	}

	// Compaction-aware rebuild: if the session contains a compact_boundary, rebuild
	// from [summary] + kept tail + newer messages appended after the boundary.
	boundary, after, compacted := session.FindLastCompactBoundary(msgs)
	var replay []session.Message
	if compacted {
		summary := "本次会话延续自之前的对话，因上下文空间不足进行了压缩。以下是早期对话的摘要：\n\n" + boundary.Summary
		if len(boundary.Keep) > 0 {
			summary += "\n\n近期消息已原样保留。"
		}
		replay = append(replay, session.Message{Role: "user", Content: summary})
		for _, k := range boundary.Keep {
			replay = append(replay, session.Message{Role: k.Role, Content: k.Content})
		}
		replay = append(replay, after...)
	} else {
		replay = msgs
	}

	for _, msg := range replay {
		m.chatMessages = append(m.chatMessages, chatMessage{role: msg.Role, content: msg.Content})
		switch msg.Role {
		case "user":
			m.conversation.AddUserMessage(msg.Content)
		case "assistant":
			m.conversation.AddAssistantMessage(msg.Content)
		}
	}

	restored := fmt.Sprintf("Session %s restored (%d messages).", strings.TrimSpace(targetID), len(replay))
	if compacted {
		restored = fmt.Sprintf("Session %s restored from compacted state (summary + %d kept + %d newer messages).",
			strings.TrimSpace(targetID), len(boundary.Keep), len(after))
	}
	m.chatMessages = append(m.chatMessages, chatMessage{
		role:    "system",
		content: restored,
	})
	commitText := m.renderMessagesRange(0, len(m.chatMessages))
	m.committedUpTo = len(m.chatMessages)
	m.updateViewport()
	if commitText != "" {
		return m, tea.Println(commitText)
	}
	return m, nil
}

func (m Model) handleResumeKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "escape":
		m.state = stateChat
		m.textarea.Focus()
		return m, nil
	case "enter":
		if m.resumeCursor < len(m.resumeFiltered) {
			selected := m.resumeFiltered[m.resumeCursor]
			m.state = stateChat
			m.textarea.Focus()
			wd, _ := os.Getwd()
			return m.doResumeSession(wd, selected.ID, m.resumeSessions)
		}
		return m, nil
	case "up":
		if m.resumeCursor > 0 {
			m.resumeCursor--
			if m.resumeCursor < m.resumeScrollTop {
				m.resumeScrollTop = m.resumeCursor
			}
		}
		return m, nil
	case "down":
		if m.resumeCursor < len(m.resumeFiltered)-1 {
			m.resumeCursor++
			maxVisible := m.resumeVisibleCount()
			if m.resumeCursor >= m.resumeScrollTop+maxVisible {
				m.resumeScrollTop = m.resumeCursor - maxVisible + 1
			}
		}
		return m, nil
	case "backspace":
		if len(m.resumeSearch) > 0 {
			m.resumeSearch = m.resumeSearch[:len(m.resumeSearch)-1]
			m.resumeFilterSessions()
		}
		return m, nil
	default:
		if len(msg.String()) == 1 && msg.String() >= " " {
			m.resumeSearch += msg.String()
			m.resumeFilterSessions()
			return m, nil
		}
	}
	return m, nil
}

func (m *Model) resumeFilterSessions() {
	m.resumeFiltered = nil
	for _, s := range m.resumeSessions {
		if session.MatchesSearch(s, m.resumeSearch) {
			m.resumeFiltered = append(m.resumeFiltered, s)
		}
	}
	m.resumeCursor = 0
	m.resumeScrollTop = 0
}

func (m Model) resumeVisibleCount() int {
	available := m.height - 8
	perItem := 2
	if available < perItem {
		return 1
	}
	return available / perItem
}

func (m Model) renderResumeView() string {
	var sb strings.Builder

	total := len(m.resumeFiltered)
	current := 0
	if total > 0 {
		current = m.resumeCursor + 1
	}

	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(
		fmt.Sprintf("Resume session (%d of %d)", current, total),
	))
	sb.WriteString("\n")

	searchText := m.resumeSearch
	if searchText == "" {
		searchText = lipgloss.NewStyle().Foreground(dimText).Render("⌕ Search…")
	} else {
		searchText = "⌕ " + searchText
	}
	boxWidth := m.width - 6
	if boxWidth < 20 {
		boxWidth = 20
	}
	border := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Width(boxWidth).
		PaddingLeft(1)
	sb.WriteString(lipgloss.NewStyle().PaddingLeft(2).Render(border.Render(searchText)))
	sb.WriteString("\n")

	wd, _ := os.Getwd()
	projectName := filepath.Base(wd)
	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(4).Render(projectName))
	sb.WriteString("\n\n")

	maxVisible := m.resumeVisibleCount()
	if maxVisible > total {
		maxVisible = total
	}

	for i := m.resumeScrollTop; i < m.resumeScrollTop+maxVisible && i < total; i++ {
		s := m.resumeFiltered[i]

		title := s.FirstMessage
		if title == "" {
			title = "(empty session)"
		}
		maxTitleLen := m.width - 8
		if maxTitleLen > 0 && len(title) > maxTitleLen {
			title = title[:maxTitleLen] + "…"
		}

		prefix := "  "
		if i == m.resumeCursor {
			prefix = "❯ "
			title = lipgloss.NewStyle().Foreground(cyanText).Bold(true).Render(title)
		} else {
			title = lipgloss.NewStyle().Foreground(normalText).Render(title)
		}
		sb.WriteString(lipgloss.NewStyle().PaddingLeft(2).Render(prefix + title))
		sb.WriteString("\n")

		var meta []string
		meta = append(meta, session.FormatRelativeTime(s.ModTime))
		if s.GitBranch != "" {
			meta = append(meta, s.GitBranch)
		}
		meta = append(meta, session.FormatFileSize(s.FileSize))
		metaStr := strings.Join(meta, " · ")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(6).Render(metaStr))
		sb.WriteString("\n")

		if i < m.resumeScrollTop+maxVisible-1 && i < total-1 {
			sb.WriteString("\n")
		}
	}

	if total > maxVisible && m.resumeScrollTop+maxVisible < total {
		sb.WriteString("\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(
			fmt.Sprintf("  ↓ %d more session(s)", total-m.resumeScrollTop-maxVisible),
		))
	}

	rendered := strings.Count(sb.String(), "\n") + 1
	footerHeight := 2
	pad := m.height - rendered - footerHeight
	if pad > 0 {
		sb.WriteString(strings.Repeat("\n", pad))
	}

	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(4).Render(
		"Type to search · Enter to select · Esc to cancel",
	))

	return sb.String()
}

var rewindOptions = []string{
	"Restore code and conversation",
	"Restore conversation only",
	"Restore code only",
	"Force restore code and conversation (overwrite manual edits)",
	"Never mind",
}

func formatRewindSummary(checkpointNum int, res *filehistory.RewindResult, includeConv bool) string {
	var sb strings.Builder
	totalChanged := len(res.Restored) + len(res.Deleted)
	if includeConv {
		sb.WriteString(fmt.Sprintf("⟲ Rewound to checkpoint %d. Updated %d file(s) and conversation.", checkpointNum, totalChanged))
	} else {
		sb.WriteString(fmt.Sprintf("⟲ Restored %d file(s) to checkpoint %d. Conversation unchanged.", totalChanged, checkpointNum))
	}
	for _, f := range res.Restored {
		sb.WriteString("\n  • restored: " + f)
	}
	for _, f := range res.Deleted {
		sb.WriteString("\n  • deleted (agent-created): " + f)
	}
	if len(res.SkippedUserModified) > 0 {
		sb.WriteString(fmt.Sprintf("\n  ⚠ preserved %d file(s) modified externally by user (use --force to overwrite):", len(res.SkippedUserModified)))
		for _, f := range res.SkippedUserModified {
			sb.WriteString("\n    - " + f)
		}
	}
	return sb.String()
}

func (m Model) handleRewind(args string) (tea.Model, tea.Cmd) {
	if m.fileHistory == nil {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role: "system", content: "No file history available (fileHistory is nil).",
		})
		m.updateViewport()
		return m, nil
	}
	if !m.fileHistory.HasSnapshots() {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role: "system", content: "No checkpoints to rewind to (0 snapshots).",
		})
		m.updateViewport()
		return m, nil
	}
	m.rewindSnapshots = m.fileHistory.GetSnapshots()

	if strings.TrimSpace(args) != "" {
		fields := strings.Fields(args)
		force := false
		targetIdx := -1
		for _, tok := range fields {
			if tok == "--force" || tok == "-f" {
				force = true
				continue
			}
			if n, err := strconv.Atoi(tok); err == nil {
				targetIdx = n - 1
			}
		}
		if targetIdx < 0 || targetIdx >= len(m.rewindSnapshots) {
			m.chatMessages = append(m.chatMessages, chatMessage{
				role:    "error",
				content: fmt.Sprintf("Usage: /rewind [1..%d] [--force]", len(m.rewindSnapshots)),
			})
			m.updateViewport()
			return m, nil
		}
		res, err := m.fileHistory.RewindSafe(targetIdx, force)
		if err != nil {
			m.chatMessages = append(m.chatMessages, chatMessage{
				role: "error", content: fmt.Sprintf("Rewind failed: %s", err),
			})
			m.updateViewport()
			return m, nil
		}
		if m.defaultTools.FileStateCache != nil {
			m.defaultTools.FileStateCache.Clear()
		}
		snap := m.rewindSnapshots[targetIdx]
		m.conversation.TruncateTo(snap.MessageIndex)
		summary := formatRewindSummary(targetIdx+1, res, true)
		m.chatMessages = []chatMessage{{role: "system", content: summary}}
		m.committedUpTo = 0
		m.updateViewport()
		return m, nil
	}

	m.rewindCursor = len(m.rewindSnapshots) - 1
	m.rewindPhase = 0
	m.rewindOptionCursor = 0
	m.rewindDialog = true
	m.updateViewport()
	return m, nil
}

func (m Model) handleRewindKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.rewindPhase == 0 {
		return m.handleRewindPhase0(msg)
	}
	return m.handleRewindPhase1(msg)
}

func (m Model) handleRewindPhase0(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "escape":
		m.rewindDialog = false
		m.textarea.Focus()
		m.updateViewport()
		return m, nil
	case "up":
		if m.rewindCursor > 0 {
			m.rewindCursor--
			m.updateViewport()
		}
		return m, nil
	case "down":
		if m.rewindCursor < len(m.rewindSnapshots)-1 {
			m.rewindCursor++
			m.updateViewport()
		}
		return m, nil
	case "enter":
		if m.rewindCursor < len(m.rewindSnapshots) {
			m.rewindPhase = 1
			m.rewindOptionCursor = 0
			m.updateViewport()
		}
		return m, nil
	}
	return m, nil
}

func (m Model) handleRewindPhase1(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "escape":
		m.rewindPhase = 0
		m.updateViewport()
		return m, nil
	case "up":
		if m.rewindOptionCursor > 0 {
			m.rewindOptionCursor--
			m.updateViewport()
		}
		return m, nil
	case "down":
		if m.rewindOptionCursor < len(rewindOptions)-1 {
			m.rewindOptionCursor++
			m.updateViewport()
		}
		return m, nil
	case "enter":
		return m.executeRewindOption()
	}
	return m, nil
}

func (m Model) executeRewindOption() (tea.Model, tea.Cmd) {
	snap := m.rewindSnapshots[m.rewindCursor]
	var summary string

	switch m.rewindOptionCursor {
	case 0, 3:
		force := m.rewindOptionCursor == 3
		res, err := m.fileHistory.RewindSafe(m.rewindCursor, force)
		if err != nil {
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: fmt.Sprintf("Rewind failed: %s", err)})
			m.rewindDialog = false
			m.textarea.Focus()
			m.updateViewport()
			return m, nil
		}
		if m.defaultTools.FileStateCache != nil {
			m.defaultTools.FileStateCache.Clear()
		}
		m.conversation.TruncateTo(snap.MessageIndex)
		summary = formatRewindSummary(m.rewindCursor+1, res, true)
		m.chatMessages = m.chatMessages[:0]
		m.committedUpTo = 0

	case 1:
		m.conversation.TruncateTo(snap.MessageIndex)
		summary = fmt.Sprintf("⟲ Rewound conversation to checkpoint %d. Files unchanged.", m.rewindCursor+1)
		m.chatMessages = m.chatMessages[:0]
		m.committedUpTo = 0

	case 2:
		res, err := m.fileHistory.RewindSafe(m.rewindCursor, false)
		if err != nil {
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: fmt.Sprintf("Rewind failed: %s", err)})
			m.rewindDialog = false
			m.textarea.Focus()
			m.updateViewport()
			return m, nil
		}
		if m.defaultTools.FileStateCache != nil {
			m.defaultTools.FileStateCache.Clear()
		}
		summary = formatRewindSummary(m.rewindCursor+1, res, false)

	case 4:
		m.rewindDialog = false
		m.rewindPhase = 0
		m.textarea.Focus()
		m.updateViewport()
		return m, nil
	}

	m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: summary})
	m.rewindDialog = false
	m.rewindPhase = 0
	m.textarea.Focus()
	m.updateViewport()

	if m.rewindOptionCursor <= 1 || m.rewindOptionCursor == 3 {
		return m, tea.Batch(
			func() tea.Msg { return tea.ClearScreen() },
			tea.Println(m.renderBanner()+"\n"+lipgloss.NewStyle().Foreground(dimText).Render(summary)),
		)
	}
	commitText := m.renderMessagesRange(m.committedUpTo-1, len(m.chatMessages))
	m.committedUpTo = len(m.chatMessages)
	if commitText != "" {
		return m, tea.Println(commitText)
	}
	return m, nil
}

func (m Model) renderRewindDialog() string {
	if m.rewindPhase == 1 {
		return m.renderRewindOptionsDialog()
	}
	return m.renderRewindSnapshotList()
}

func (m Model) renderRewindSnapshotList() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render("  ⟲ Rewind to checkpoint"))
	sb.WriteString("\n\n")

	maxVisible := 8
	start := 0
	if m.rewindCursor >= maxVisible {
		start = m.rewindCursor - maxVisible + 1
	}
	end := start + maxVisible
	if end > len(m.rewindSnapshots) {
		end = len(m.rewindSnapshots)
	}

	for i := start; i < end; i++ {
		snap := m.rewindSnapshots[i]
		prefix := "   "
		style := lipgloss.NewStyle().Foreground(dimText)
		if i == m.rewindCursor {
			prefix = " ❯ "
			style = lipgloss.NewStyle().Foreground(cyanText)
		}
		ago := time.Since(snap.Timestamp).Truncate(time.Second)
		label := snap.UserText
		if len(label) > 50 {
			label = label[:50] + "…"
		}
		files := len(snap.Backups)
		line := fmt.Sprintf("%s[%d] %s (%s ago, %d file(s))", prefix, i+1, label, ago, files)
		sb.WriteString(style.Render(line) + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render("↑/↓ navigate · enter select · esc cancel"))
	sb.WriteString("\n")
	return sb.String()
}

func (m Model) renderRewindOptionsDialog() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render("  ⟲ Rewind to checkpoint"))
	sb.WriteString("\n\n")

	snap := m.rewindSnapshots[m.rewindCursor]
	ago := time.Since(snap.Timestamp).Truncate(time.Second)
	label := snap.UserText
	if len(label) > 50 {
		label = label[:50] + "…"
	}
	selected := fmt.Sprintf("  Selected: [%d] %s (%s ago, %d file(s))", m.rewindCursor+1, label, ago, len(snap.Backups))
	sb.WriteString(lipgloss.NewStyle().Foreground(normalText).Render(selected))
	sb.WriteString("\n\n")

	for i, opt := range rewindOptions {
		prefix := "   "
		style := lipgloss.NewStyle().Foreground(dimText)
		if i == m.rewindOptionCursor {
			prefix = " ❯ "
			style = lipgloss.NewStyle().Foreground(cyanText)
		}
		sb.WriteString(style.Render(prefix+opt) + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render("↑/↓ navigate · enter select · esc back"))
	sb.WriteString("\n")
	return sb.String()
}
