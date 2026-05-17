// Package tui is an optional interactive terminal UI for glsms, built on
// Bubble Tea. It reuses the same *glsms.SMS library the CLI and REST server
// use, so behaviour (auth, the two-step delete, direction detection) is
// identical.
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/CaseyBlackburn/glsms/glsms"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type screen int

const (
	screenList screen = iota
	screenDetail
	screenCompose
	screenConfirmDelete
)

const opTimeout = 90 * time.Second

// ---- styles ----

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("63")).Padding(0, 1)
	headerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	footerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	noticeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	selStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("238"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	labelStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	readStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	// Unread (new) received messages: bold, bright yellow so they pop.
	unreadStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	// Selected + unread: keep the selection background but stay bold/bright.
	selUnreadStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220")).Background(lipgloss.Color("238"))
	newBadgeStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("16")).Background(lipgloss.Color("220")).Padding(0, 1)
)

func isUnread(m glsms.Message) bool {
	return m.Direction == glsms.Received && m.Status == glsms.StatusReceivedUnread
}

func statusText(m glsms.Message) string {
	switch m.Status {
	case glsms.StatusReceivedUnread:
		return "received · UNREAD"
	case glsms.StatusReceivedRead:
		return "received · read"
	case glsms.StatusSent:
		return "sent"
	default:
		return fmt.Sprintf("status %d", m.Status)
	}
}

func unreadCount(ms []glsms.Message) int {
	n := 0
	for _, m := range ms {
		if isUnread(m) {
			n++
		}
	}
	return n
}

// ---- async messages ----

type loadedMsg struct {
	status glsms.ModemStatus
	msgs   []glsms.Message
	err    error
}
type actionMsg struct {
	verb string
	err  error
}

// markedMsg reports a read/unread change without forcing a screen change.
type markedMsg struct {
	read bool
	err  error
}

// tickMsg drives the optional auto-refresh.
type tickMsg struct{}

const autoRefreshEvery = 2 * time.Second

func tickCmd() tea.Cmd {
	return tea.Tick(autoRefreshEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

// ---- model ----

// Model is the Bubble Tea model for the glsms TUI.
type Model struct {
	sms *glsms.SMS

	scr   screen
	spin  spinner.Model
	to    textinput.Model
	body  textarea.Model
	vp    viewport.Model
	cFoc  int // compose focus: 0=to, 1=body
	w, h  int
	ready bool
	busy  bool // user-initiated op in flight (shows spinner)

	loading     bool // any load in flight (guards auto-refresh overlap)
	autoRefresh bool // periodic silent reload, default on

	status glsms.ModemStatus
	all    []glsms.Message
	inbox  []glsms.Message // Direction received (and unknown)
	outbox []glsms.Message // Direction sent
	inCur  int             // cursor within inbox
	outCur int             // cursor within outbox
	pane   int             // active pane: 0 = inbox, 1 = outbox

	replyTo string // non-empty while composing a reply

	notice string
	err    error
}

// New builds the initial model.
func New(sms *glsms.SMS) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot

	to := textinput.New()
	to.Placeholder = "+15551234567"
	to.Prompt = "To:   "
	to.CharLimit = 24

	ta := textarea.New()
	ta.Placeholder = "Message…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 1000

	return Model{
		sms:         sms,
		scr:         screenList,
		spin:        sp,
		to:          to,
		body:        ta,
		busy:        true,
		loading:     true,
		autoRefresh: true,
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.load(), tickCmd())
}

// ---- commands ----

func (m Model) load() tea.Cmd {
	sms := m.sms
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		st, err := sms.Status(ctx)
		if err != nil {
			return loadedMsg{err: err}
		}
		ms, err := sms.List(ctx)
		return loadedMsg{status: st, msgs: ms, err: err}
	}
}

func (m Model) send(to, body string) tea.Cmd {
	sms := m.sms
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout+60*time.Second)
		defer cancel()
		return actionMsg{verb: "sent", err: sms.Send(ctx, to, body, 60)}
	}
}

func (m Model) del(name string) tea.Cmd {
	sms := m.sms
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_, err := sms.Delete(ctx, name)
		return actionMsg{verb: "deleted", err: err}
	}
}

// mark changes a message's read state. Unlike send/delete it reports via
// markedMsg, which does NOT navigate away from the current screen.
func (m Model) mark(name string, read bool) tea.Cmd {
	sms := m.sms
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		var err error
		if read {
			err = sms.MarkRead(ctx, name)
		} else {
			err = sms.MarkUnread(ctx, name)
		}
		return markedMsg{read: read, err: err}
	}
}

// ---- update ----

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.ready = true
		m.resize()
		return m, nil

	case spinner.TickMsg:
		if m.busy {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			return m, cmd
		}
		return m, nil

	case loadedMsg:
		m.busy = false
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.status = msg.status
		m.all = msg.msgs
		m.split()
		return m, nil

	case actionMsg:
		m.busy = false
		if msg.err != nil {
			m.loading = false
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.notice = "✓ " + msg.verb
		m.scr = screenList
		m.busy = true
		m.loading = true
		return m, tea.Batch(m.spin.Tick, m.load())

	case markedMsg:
		// Stay on the current screen; just reconcile with a silent reload.
		if msg.err != nil {
			m.loading = false
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		if msg.read {
			m.notice = "✓ marked read"
		} else {
			m.notice = "✓ marked unread"
		}
		if !m.loading {
			m.loading = true
			return m, m.load()
		}
		return m, nil

	case tickMsg:
		// Always re-arm so toggling auto-refresh back on resumes it.
		cmds := []tea.Cmd{tickCmd()}
		if m.autoRefresh && !m.loading && !m.busy {
			m.loading = true // silent: no busy spinner for background refresh
			cmds = append(cmds, m.load())
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}

	// Forward to focused sub-component on the compose screen.
	if m.scr == screenCompose {
		var cmds []tea.Cmd
		var cmd tea.Cmd
		if m.cFoc == 0 {
			m.to, cmd = m.to.Update(msg)
		} else {
			m.body, cmd = m.body.Update(msg)
		}
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	// Global quit (not while typing in compose).
	if m.scr != screenCompose {
		switch msg.String() {
		case "q", "ctrl+c":
			return tea.Quit
		}
	} else if msg.String() == "ctrl+c" {
		return tea.Quit
	}

	switch m.scr {
	case screenList:
		return m.keyList(msg)
	case screenDetail:
		return m.keyDetail(msg)
	case screenCompose:
		return m.keyCompose(msg)
	case screenConfirmDelete:
		return m.keyConfirm(msg)
	}
	return nil
}

func (m *Model) keyList(msg tea.KeyMsg) tea.Cmd {
	cur := m.paneSlice()
	cidx := m.paneCursor()
	switch msg.String() {
	case "left", "h", "shift+tab":
		m.pane = 0
	case "right", "l", "tab":
		m.pane = 1
	case "up", "k":
		if *cidx > 0 {
			*cidx--
		}
	case "down", "j":
		if *cidx < len(cur)-1 {
			*cidx++
		}
	case "g", "home":
		*cidx = 0
	case "G", "end":
		*cidx = max(0, len(cur)-1)
	case "ctrl+r", "f5":
		m.notice, m.err, m.busy, m.loading = "", nil, true, true
		return tea.Batch(m.spin.Tick, m.load())
	case "a":
		m.autoRefresh = !m.autoRefresh
	case "enter":
		if c, ok := m.current(); ok {
			m.scr = screenDetail
			var cmd tea.Cmd
			// Opening a message marks it read.
			if c.Direction == glsms.Received && c.Status == glsms.StatusReceivedUnread {
				cmd = m.doMark(c.Name, true)
			}
			if cc, ok := m.current(); ok {
				m.vp.SetContent(m.detailText(cc))
			}
			m.vp.GotoTop()
			return cmd
		}
	case "c":
		m.openCompose("")
	case "r":
		// Reply: only meaningful for an inbox message.
		if m.pane == 0 {
			if c, ok := m.current(); ok {
				m.openCompose(c.PhoneNumber)
			}
		}
	case "m":
		if c, ok := m.current(); ok && c.Direction == glsms.Received &&
			c.Status != glsms.StatusReceivedRead {
			return m.doMark(c.Name, true)
		}
	case "u":
		if c, ok := m.current(); ok && c.Direction == glsms.Received &&
			c.Status != glsms.StatusReceivedUnread {
			return m.doMark(c.Name, false)
		}
	case "d":
		if _, ok := m.current(); ok {
			m.scr = screenConfirmDelete
		}
	}
	return nil
}

func (m *Model) keyDetail(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "backspace", "b", "left", "h":
		m.scr = screenList
		return nil
	case "d":
		m.scr = screenConfirmDelete
		return nil
	case "m":
		if cur, ok := m.current(); ok && cur.Direction == glsms.Received &&
			cur.Status != glsms.StatusReceivedRead {
			return m.doMark(cur.Name, true)
		}
	case "u":
		if cur, ok := m.current(); ok && cur.Direction == glsms.Received &&
			cur.Status != glsms.StatusReceivedUnread {
			return m.doMark(cur.Name, false)
		}
	case "r":
		if cur, ok := m.current(); ok && cur.Direction == glsms.Received {
			m.openCompose(cur.PhoneNumber)
			return nil
		}
	case "a":
		m.autoRefresh = !m.autoRefresh
		return nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return cmd
}

func (m *Model) keyCompose(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.scr = screenList
		m.to.Blur()
		m.body.Blur()
		m.replyTo = ""
		return nil
	case "tab", "shift+tab":
		m.cFoc = 1 - m.cFoc
		m.syncComposeFocus()
		return nil
	case "ctrl+s":
		to := strings.TrimSpace(m.to.Value())
		body := m.body.Value()
		if to == "" || strings.TrimSpace(body) == "" {
			m.err = fmt.Errorf("recipient and message are both required")
			return nil
		}
		m.err = nil
		m.busy = true
		m.scr = screenList
		m.replyTo = ""
		return tea.Batch(m.spin.Tick, m.send(to, body))
	}
	var cmd tea.Cmd
	if m.cFoc == 0 {
		m.to, cmd = m.to.Update(msg)
	} else {
		m.body, cmd = m.body.Update(msg)
	}
	return cmd
}

func (m *Model) keyConfirm(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "y", "Y":
		cur, ok := m.current()
		m.scr = screenList
		if !ok {
			return nil
		}
		m.busy = true
		return tea.Batch(m.spin.Tick, m.del(cur.Name))
	case "n", "N", "esc":
		m.scr = screenList
	}
	return nil
}

// ---- helpers ----

// openCompose enters the compose screen. If prefillTo is non-empty the
// recipient is pre-filled (a reply) and focus starts in the message body.
func (m *Model) openCompose(prefillTo string) {
	m.to.SetValue(prefillTo)
	m.body.SetValue("")
	m.replyTo = prefillTo
	if prefillTo == "" {
		m.cFoc = 0
	} else {
		m.cFoc = 1
	}
	m.syncComposeFocus()
	m.scr = screenCompose
}

func (m *Model) syncComposeFocus() {
	if m.cFoc == 0 {
		m.to.Focus()
		m.body.Blur()
	} else {
		m.to.Blur()
		m.body.Focus()
	}
}

// split partitions m.all into the inbox (received + unknown) and outbox
// (sent), clamping each pane's cursor.
func (m *Model) split() {
	m.inbox = m.inbox[:0]
	m.outbox = m.outbox[:0]
	for _, msg := range m.all {
		if msg.Direction == glsms.Sent {
			m.outbox = append(m.outbox, msg)
		} else {
			m.inbox = append(m.inbox, msg)
		}
	}
	if m.inCur >= len(m.inbox) {
		m.inCur = max(0, len(m.inbox)-1)
	}
	if m.outCur >= len(m.outbox) {
		m.outCur = max(0, len(m.outbox)-1)
	}
}

func (m *Model) paneSlice() []glsms.Message {
	if m.pane == 0 {
		return m.inbox
	}
	return m.outbox
}

func (m *Model) paneCursor() *int {
	if m.pane == 0 {
		return &m.inCur
	}
	return &m.outCur
}

func (m *Model) current() (glsms.Message, bool) {
	s := m.paneSlice()
	i := *m.paneCursor()
	if i >= 0 && i < len(s) {
		return s[i], true
	}
	return glsms.Message{}, false
}

// setLocalStatus optimistically updates a message's status in-memory so the UI
// reflects a read/unread change immediately; a background reload reconciles.
func (m *Model) setLocalStatus(name string, status int) {
	for i := range m.all {
		if m.all[i].Name == name {
			m.all[i].Status = status
		}
	}
	m.split()
}

// doMark flips a message's read state: optimistic local update, refresh the
// detail pane if open, and return the background mark command.
func (m *Model) doMark(name string, read bool) tea.Cmd {
	st := glsms.StatusReceivedUnread
	if read {
		st = glsms.StatusReceivedRead
	}
	m.setLocalStatus(name, st)
	if m.scr == screenDetail {
		if c, ok := m.current(); ok {
			m.vp.SetContent(m.detailText(c))
		}
	}
	return m.mark(name, read)
}

func (m *Model) resize() {
	bodyH := m.h - 7
	if bodyH < 3 {
		bodyH = 3
	}
	m.vp = viewport.New(m.w-2, bodyH)
	m.body.SetWidth(min(m.w-6, 70))
	m.body.SetHeight(min(bodyH-4, 8))
	m.to.Width = min(m.w-10, 30)
}

// ---- view ----

// View implements tea.Model.
func (m Model) View() string {
	if !m.ready {
		return "loading…"
	}
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	switch m.scr {
	case screenList:
		b.WriteString(m.viewList())
	case screenDetail:
		b.WriteString(m.viewDetail())
	case screenCompose:
		b.WriteString(m.viewCompose())
	case screenConfirmDelete:
		b.WriteString(m.viewConfirm())
	}
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

func (m Model) header() string {
	title := titleStyle.Render(" glsms ")
	sim := m.status.SIM
	auto := "auto:off"
	if m.autoRefresh {
		auto = noticeStyle.Render("auto:on")
	} else {
		auto = dimStyle.Render(auto)
	}
	info := fmt.Sprintf("%s · %s · %s %s · new:%d · ",
		orDash(sim.PhoneNumber), orDash(sim.Carrier),
		orDash(sim.NetworkType), bars(sim.SignalBars), m.status.NewSMSCount)
	right := headerStyle.Render(info) + auto
	gap := m.w - lipgloss.Width(title) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return title + strings.Repeat(" ", gap) + right
}

func (m Model) viewList() string {
	if m.busy && len(m.all) == 0 {
		return "  " + m.spin.View() + " loading messages…"
	}
	rows := m.h - 8
	if rows < 1 {
		rows = 1
	}
	colW := (m.w - 9) / 2
	if colW < 14 {
		colW = 14
	}
	left := m.renderPane("Inbox", m.inbox, m.inCur, m.pane == 0, rows, colW)
	right := m.renderPane("Outbox", m.outbox, m.outCur, m.pane == 1, rows, colW)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
}

func (m Model) renderPane(title string, msgs []glsms.Message, cur int, active bool, rows, w int) string {
	prefix := "  "
	if active {
		prefix = "▌ "
	}
	head := prefix + labelStyle.Render(fmt.Sprintf("%s (%d)", title, len(msgs)))
	if u := unreadCount(msgs); u > 0 {
		head += " " + newBadgeStyle.Render(fmt.Sprintf("%d new", u))
	}
	var b strings.Builder
	b.WriteString(head + "\n")
	if len(msgs) == 0 {
		b.WriteString(dimStyle.Render("  (empty)"))
	} else {
		start := 0
		if cur >= rows {
			start = cur - rows + 1
		}
		for i := start; i < len(msgs) && i < start+rows; i++ {
			msg := msgs[i]
			unread := isUnread(msg)
			glyph := " "
			if unread {
				glyph = "●"
			}
			rowPrefix := "  "
			if i == cur {
				rowPrefix = "▶ "
			}
			content := fmt.Sprintf("%s%s %-14s %s",
				rowPrefix, glyph, trunc(msg.PhoneNumber, 14),
				trunc(oneLine(msg.Body), max(6, w-20)))
			var line string
			switch {
			case i == cur && active && unread:
				line = selUnreadStyle.Render(content)
			case i == cur && active:
				line = selStyle.Render(content)
			case i == cur:
				line = dimStyle.Render(content)
			case unread:
				line = unreadStyle.Render(content)
			default:
				line = readStyle.Render(content)
			}
			b.WriteString(line + "\n")
		}
	}
	bs := boxStyle.Width(w).Height(rows + 1)
	if active {
		bs = bs.BorderForeground(lipgloss.Color("63"))
	} else {
		bs = bs.BorderForeground(lipgloss.Color("240"))
	}
	return bs.Render(b.String())
}

func (m Model) viewDetail() string {
	cur, ok := m.current()
	if !ok {
		return "  (nothing selected)"
	}
	_ = cur
	return boxStyle.Width(m.w - 4).Render(m.vp.View())
}

func (m Model) detailText(cur glsms.Message) string {
	dir := "Received from"
	if cur.Direction == glsms.Sent {
		dir = "Sent to"
	} else if cur.Direction == glsms.UnknownDirection {
		dir = "Unknown party"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", labelStyle.Render(dir), cur.PhoneNumber)
	fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("Date:"), cur.DateRaw)
	state := statusText(cur)
	if isUnread(cur) {
		state = unreadStyle.Render(state)
	}
	fmt.Fprintf(&b, "%s %s  %s %s\n", labelStyle.Render("Name:"), cur.Name,
		labelStyle.Render("State:"), state)
	b.WriteString(strings.Repeat("─", min(40, max(10, m.w-6))) + "\n\n")
	b.WriteString(cur.Body)
	return b.String()
}

func (m Model) viewCompose() string {
	var b strings.Builder
	heading := "  Compose message"
	if m.replyTo != "" {
		heading = "  Reply to " + m.replyTo
	}
	b.WriteString(labelStyle.Render(heading) + "\n\n")
	b.WriteString("  " + m.to.View() + "\n\n")
	b.WriteString("  " + labelStyle.Render("Message:") + "\n")
	b.WriteString(m.body.View() + "\n")
	return b.String()
}

func (m Model) viewConfirm() string {
	cur, _ := m.current()
	q := fmt.Sprintf("Delete this message to/from %s?\n\n  %q\n\n[y] yes   [n] no",
		cur.PhoneNumber, trunc(oneLine(cur.Body), 60))
	return "\n" + boxStyle.BorderForeground(lipgloss.Color("203")).Render(q)
}

func (m Model) footer() string {
	var hint string
	switch m.scr {
	case screenList:
		hint = "←/→ pane · ↑/↓ move · enter view (marks read) · r reply · c compose · m read · u unread · d delete · a auto · ctrl+r refresh · q quit"
	case screenDetail:
		hint = "↑/↓ scroll · m read · u unread · r reply · d delete · a auto · esc back · q quit"
	case screenCompose:
		hint = "tab switch field · ctrl+s send · esc cancel"
	case screenConfirmDelete:
		hint = "y confirm · n cancel"
	}
	line := footerStyle.Render(hint)
	switch {
	case m.err != nil:
		line = errStyle.Render("✗ "+oneLine(m.err.Error())) + "\n" + line
	case m.busy:
		line = m.spin.View() + " working…\n" + line
	case m.notice != "":
		line = noticeStyle.Render(m.notice) + "\n" + line
	}
	return line
}

// ---- small utils ----

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func bars(n int) string {
	if n < 0 {
		n = 0
	}
	if n > 5 {
		n = 5
	}
	return strings.Repeat("▮", n) + dimStyle.Render(strings.Repeat("▯", 5-n))
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// Run starts the TUI program. It returns an error (without hanging) if
// stdin/stdout is not an interactive terminal.
func Run(sms *glsms.SMS) error {
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return fmt.Errorf("stdin/stdout is not an interactive terminal; " +
			"run `glsms tui` directly in a terminal (use the CLI subcommands when scripting)")
	}
	p := tea.NewProgram(New(sms), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func isTerminal(f *os.File) bool {
	fd := f.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}
