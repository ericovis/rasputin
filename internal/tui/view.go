package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Adaptive colours: the same run has to read on a white terminal and a black
// one, so nothing here picks an absolute background.
var (
	subtleC = lipgloss.AdaptiveColor{Light: "#6c6c6c", Dark: "#949494"}
	accentC = lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fafd7"}
	okC     = lipgloss.AdaptiveColor{Light: "#2f6f2f", Dark: "#87d787"}
	errC    = lipgloss.AdaptiveColor{Light: "#af0000", Dark: "#ff8787"}
)

var (
	titleStyle   = lipgloss.NewStyle().Bold(true)
	dimStyle     = lipgloss.NewStyle().Foreground(subtleC)
	okStyle      = lipgloss.NewStyle().Foreground(okC)
	runningStyle = lipgloss.NewStyle().Foreground(accentC)
	failStyle    = lipgloss.NewStyle().Foreground(errC)
)

const (
	minWidth     = 40
	maxBarW      = 24
	phaseW       = 22
	narrowPhaseW = 14
	minLogRow    = 1
	maxLogRow    = 8
)

func (m *model) View() string {
	w := m.width
	if w < minWidth {
		w = minWidth
	}

	var rows []string
	rows = append(rows, m.headerLine(w), m.barLine(w), "")

	labelW, nameW := 6, 8
	for _, s := range m.steps {
		if n := len(string(s.ID)); n > labelW {
			labelW = n
		}
		for _, nd := range s.nodes {
			if n := len(nd.name); n > nameW {
				nameW = n
			}
		}
	}
	for _, s := range m.steps {
		rows = append(rows, m.stepLine(s, w, labelW))
		if s.status == statusRunning || s.status == statusFailed {
			for _, n := range s.nodes {
				rows = append(rows, m.nodeLine(n, w, nameW))
			}
		}
	}
	rows = append(rows, "")
	rows = append(rows, m.logBox(w, m.logRows(len(rows)))...)
	rows = append(rows, m.footer(w))
	for i, r := range rows {
		rows[i] = strings.TrimRight(r, " ")
	}
	return strings.Join(rows, "\n") + "\n"
}

// logRows is whatever vertical space is left once the steps have theirs.
func (m *model) logRows(used int) int {
	n := m.height - used - 4 // box borders and the footer
	if n < minLogRow {
		return minLogRow
	}
	if n > maxLogRow {
		return maxLogRow
	}
	return n
}

func (m *model) headerLine(w int) string {
	left := " " + m.title
	right := "elapsed " + clock(m.now.Sub(m.start))
	if left := leftText(m.remaining); left != "" && !m.done {
		right += " · " + left
	}
	right += " "
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return titleStyle.Render(left) + strings.Repeat(" ", gap) + dimStyle.Render(right)
}

func (m *model) barLine(w int) string {
	pct := fmt.Sprintf(" %3.0f%%", m.lastFrac*100)
	bar := m.overall
	bar.Width = w - lipgloss.Width(pct) - 2
	if bar.Width < 8 {
		bar.Width = 8
	}
	return " " + bar.ViewAs(m.lastFrac) + pct
}

// stepLine is one row of the step list: glyph, step id, what it is doing, and
// how long it took.
func (m *model) stepLine(s *stepState, w, labelW int) string {
	prefix := " " + m.glyph(s) + " " + pad(string(s.ID), labelW) + "  "
	right := m.stepDuration(s)
	detailW := w - lipgloss.Width(prefix) - lipgloss.Width(right) - 2
	detail := truncate(s.detail(), detailW)

	style := lipgloss.NewStyle()
	switch s.status {
	case statusPending, statusSkipped:
		style = dimStyle
	case statusFailed:
		style = failStyle
	}
	return prefix + style.Render(pad(detail, detailW)) + " " + dimStyle.Render(right)
}

func (m *model) stepDuration(s *stepState) string {
	switch s.status {
	case statusRunning:
		if s.started.IsZero() {
			return ""
		}
		return shortDur(m.now.Sub(s.started))
	case statusDone, statusFailed:
		if s.started.IsZero() || s.ended.Before(s.started) {
			return ""
		}
		return shortDur(s.ended.Sub(s.started))
	}
	return ""
}

func (m *model) glyph(s *stepState) string {
	switch s.status {
	case statusRunning:
		return m.spin.View()
	case statusDone:
		return okStyle.Render("✓")
	case statusSkipped:
		return dimStyle.Render("✓")
	case statusFailed:
		return failStyle.Render("✗")
	}
	return dimStyle.Render("○")
}

// detail is the sentence next to the step id.
func (s *stepState) detail() string {
	switch s.status {
	case statusSkipped:
		return "skipped · " + s.message
	case statusFailed:
		if s.message != "" {
			return "failed · " + s.message
		}
		return "failed"
	case statusDone:
		if s.message != "" {
			return s.message
		}
	case statusRunning:
		if s.phase != "" {
			return s.phase
		}
	default:
		if s.Skip {
			return "skipped · " + s.Reason
		}
		if s.Title == "" {
			return strings.Join(s.Nodes, " ")
		}
	}
	if s.Title != "" {
		return s.Title
	}
	return strings.Join(s.Nodes, " ")
}

// nodeLine is a node's own bar under a running step.
func (m *model) nodeLine(n *nodeState, w, nameW int) string {
	pw := phaseW
	if w < 90 {
		pw = narrowPhaseW
	}
	name := pad(n.name, nameW)
	switch n.state {
	case statusFailed:
		name = failStyle.Render(name)
	case statusDone, statusSkipped:
		name = dimStyle.Render(name)
	}
	head := "     " + name + "  " + pad(truncate(n.phase, pw), pw)

	if n.total <= 0 {
		if n.bytes > 0 {
			return truncate(head+"  "+join(humanBytes(n.bytes), humanRate(n.rate)), w)
		}
		return truncate(head, w)
	}

	counts := humanBytes(n.bytes) + "/" + humanBytes(n.total)
	frac := float64(n.bytes) / float64(n.total)
	if frac > 1 {
		frac = 1
	}
	// The bar earns its width first: at 80 columns the eta goes, then the
	// rate, and only then the bar.
	for _, info := range []string{
		join(counts, humanRate(n.rate), etaText(n)),
		join(counts, humanRate(n.rate)),
		counts,
	} {
		barW := min(w-lipgloss.Width(head)-lipgloss.Width(info)-4, maxBarW)
		if barW < 8 {
			continue
		}
		bar := n.bar
		bar.Width = barW
		return truncate(head+"  "+bar.ViewAs(frac)+"  "+info, w)
	}
	return truncate(head+"  "+join(counts, humanRate(n.rate), etaText(n)), w)
}

// etaText is how long this node's transfer still has at the current rate.
func etaText(n *nodeState) string {
	if n.rate <= 0 || n.total <= n.bytes {
		return ""
	}
	return clock(durationOf(float64(n.total-n.bytes) / n.rate))
}

func (m *model) logBox(w, rows int) []string {
	inner := w - 5
	top := " ┌ log " + strings.Repeat("─", max(w-8, 0)) + "┐"
	bottom := " └" + strings.Repeat("─", max(w-3, 0)) + "┘"

	tail := m.log
	if len(tail) > rows {
		tail = tail[len(tail)-rows:]
	}
	out := []string{dimStyle.Render(top)}
	for i := 0; i < rows; i++ {
		line := ""
		if i < len(tail) {
			line = tail[i]
		}
		out = append(out, dimStyle.Render(" │ ")+pad(truncate(line, inner), inner)+dimStyle.Render(" │"))
	}
	return append(out, dimStyle.Render(bottom))
}

func (m *model) footer(w int) string {
	msg := " q / ctrl-c aborts · nodes stay in the recovery agent, nothing is half-written"
	if m.aborted {
		msg = " aborting · waiting for the current step to unwind"
	}
	if m.err != nil {
		return failStyle.Render(truncate(" "+flatten(m.err.Error()), w))
	}
	return dimStyle.Render(truncate(msg, w))
}
