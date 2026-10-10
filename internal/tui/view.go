package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lip "charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var titleStyle = lip.NewStyle().Bold(true).Foreground(lip.Color("#73C6D6"))
var mutedStyle = lip.NewStyle().Foreground(lip.Color("#8995A6"))
var selectedStyle = lip.NewStyle().Foreground(lip.Color("#73C6D6")).Bold(true)
var warningStyle = lip.NewStyle().Foreground(lip.Color("#F0BD71")).Bold(true)

// quote escapes control sequences, bidi controls and line breaks before any
// terminal formatting. Limit the input too: proposals can contain a full MiB.
func quote(s string) string {
	const limit = 65536
	truncated := len(s) > limit
	if truncated {
		s = s[:limit]
	}
	quoted := strconv.QuoteToGraphic(s)
	if truncated {
		quoted += " [display limited to 64 KiB]"
	}
	return quoted
}
func short(s string, width int) string { return ansi.Truncate(s, max(1, width), "…") }
func remaining(deadline, now time.Time) string {
	if !now.Before(deadline) {
		return "EXPIRED"
	}
	seconds := int64(math.Ceil(deadline.Sub(now).Seconds()))
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
}
func (m *model) detailHeight() int {
	if m.width >= 100 {
		return max(1, m.height-10)
	}
	return max(1, (m.height-10)/2)
}
func panel(title, body string, width, height int) string {
	style := lip.NewStyle().Border(lip.RoundedBorder()).BorderForeground(lip.Color("#44536A")).Width(max(1, width-2)).Height(height)
	lines := strings.Split(body, "\n")
	if len(lines) > height-1 {
		lines = lines[:max(0, height-1)]
	}
	content := titleStyle.Render(title) + "\n" + strings.Join(lines, "\n")
	return style.Render(content)
}
func (m *model) queue(width, height int) string {
	rows := max(1, (height-1)/2)
	selected := m.selectedIndex()
	start := max(0, selected-rows/2)
	start = min(start, max(0, len(m.items)-rows))
	if len(m.items) == 0 {
		return mutedStyle.Render("No pending requests.\nNew proposals will appear here.")
	}
	lines := []string{}
	for _, p := range m.items[start:min(len(m.items), start+rows)] {
		marker := "  "
		style := lip.NewStyle()
		if p.Request.RequestID == m.selected {
			marker = "› "
			style = selectedStyle
		}
		countdown := remaining(p.Deadline, m.now)
		text := marker + short(quote(p.Request.Agent)+" / "+quote(p.Request.ConversationID), width-ansi.StringWidth(countdown)-4) + "  " + countdown
		lines = append(lines, style.Render(short(text, width)), mutedStyle.Render(short("  "+p.Request.RequestID, width)))
	}
	return strings.Join(lines, "\n")
}
func (m *model) details(width, height int) string {
	p := m.selection()
	if p == nil {
		return mutedStyle.Render("Select a proposal to review its details.")
	}
	cwd := "unknown"
	if p.Request.CWD != "" {
		cwd = quote(p.Request.CWD)
	}
	roots := []string{}
	for i, r := range p.Request.WorkspaceRoots {
		if i == 1024 {
			roots = append(roots, fmt.Sprintf("… %d additional roots not displayed", len(p.Request.WorkspaceRoots)-i))
			break
		}
		roots = append(roots, quote(r))
	}
	rootsDisplay := strings.Join(roots, ", ")
	if len(roots) == 0 {
		rootsDisplay = "not supplied"
	}
	text := fmt.Sprintf("Command: %s\n\nRequest: %s\nAgent: %s  Version: %s\nConversation: %s\nCWD: %s\nWorkspace: %s\nDeadline: %s (%s)\n\nAuthorization only · execution unobserved\nShell analysis is not implemented.", quote(p.Request.Command), p.Request.RequestID, quote(p.Request.Agent), quote(p.Request.AgentVersion), quote(p.Request.ConversationID), cwd, rootsDisplay, p.Deadline.Local().Format("15:04:05.000 MST"), remaining(p.Deadline, m.now))
	lines := strings.Split(ansi.Hardwrap(text, max(1, width), true), "\n")
	// Keep the final page within reach after resizing or selecting a short item.
	offset := min(m.detailOffset, max(0, len(lines)-height))
	return strings.Join(lines[offset:min(len(lines), offset+height)], "\n")
}
func (m *model) View() tea.View {
	width := max(1, m.width)
	state := warningStyle.Render("CONNECTING · decisions disabled")
	if m.live() {
		state = titleStyle.Render("LIVE")
	} else if m.connected || m.retry > 0 {
		state = warningStyle.Render("DISCONNECTED · decisions disabled")
	}
	header := titleStyle.Render("AIRLOCK") + "  " + state + fmt.Sprintf("  ·  %d pending", len(m.items))
	intro := header + "\n" + mutedStyle.Render("Manual approval inbox · proposals are never executed by Airlock") + "\n" + mutedStyle.Render(short("Socket: "+quote(m.socket), width)) + "\n"
	hint := "↑/↓ j/k select · a allow · d deny · PgUp/PgDn details · r refresh · q quit"
	message := m.notice
	if m.confirm != nil && m.canDecide() {
		message = fmt.Sprintf("%s %s? Enter confirms · Esc cancels", strings.ToUpper(m.confirm.permission), m.confirm.id)
	}
	if !m.live() {
		message = "Waiting for a fresh queue snapshot. Last queue is shown for reference."
		if m.retry > 0 {
			message = m.connectionReason + ". Reconnecting automatically; cached queue."
		}
	}
	if m.width < 52 || m.height < 16 {
		return terminalView(header+"\n"+mutedStyle.Render(short("Socket: "+quote(m.socket), width))+"\nResize the terminal to at least 52 columns × 16 rows.\nDecisions are disabled. Press q to quit.", width, m.height)
	}
	height := m.detailHeight()
	body := ""
	if width >= 100 {
		left := max(36, width*2/5)
		right := width - left - 1
		body = lip.JoinHorizontal(lip.Top, panel("QUEUE", m.queue(left-2, height-1), left, height), " ", panel("PROPOSAL · PgUp/PgDn scroll", m.details(right-2, height-1), right, height))
	} else {
		body = panel("QUEUE", m.queue(width-2, height-1), width, height) + "\n" + panel("PROPOSAL · PgUp/PgDn scroll", m.details(width-2, height-1), width, height)
	}
	content := intro + body + "\n" + warningStyle.Render(short(message, width)) + "\n" + mutedStyle.Render(short(hint, width))
	if !m.live() && m.notice != "" {
		content = intro + body + "\n" + warningStyle.Render(short(m.notice, width)) + "\n" + mutedStyle.Render(short(message, width)) + "\n" + mutedStyle.Render(short(hint, width))
	}
	return terminalView(content, width, m.height)
}

func terminalView(content string, width, height int) tea.View {
	// Truncate each line using terminal cell widths, preserving trusted styling.
	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = short(lines[i], width)
	}
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}
