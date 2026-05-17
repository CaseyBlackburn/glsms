package tui

import (
	"strings"
	"testing"

	"github.com/CaseyBlackburn/glsms/glsms"
	tea "github.com/charmbracelet/bubbletea"
)

func sampleModel() Model {
	m := New(nil)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = mm.(Model)
	mm, _ = m.Update(loadedMsg{
		status: glsms.ModemStatus{SIM: glsms.SIM{PhoneNumber: "15550001000"}},
		msgs: []glsms.Message{
			{Name: "GMS1.aaa", Direction: glsms.Received, PhoneNumber: "15551234567", Body: "Hey", Status: glsms.StatusReceivedUnread},
			{Name: "GMS_bbb", Direction: glsms.Sent, PhoneNumber: "+15551234567", Body: "Hello from glsms", Status: glsms.StatusSent},
			{Name: "GMS_ccc", Direction: glsms.Sent, PhoneNumber: "+15551234567", Body: "Second", Status: glsms.StatusSent},
		},
	})
	return mm.(Model)
}

func key(m Model, s string) Model {
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return mm.(Model)
}

func TestSplitInboxOutbox(t *testing.T) {
	m := sampleModel()
	if len(m.inbox) != 1 || m.inbox[0].Name != "GMS1.aaa" {
		t.Fatalf("inbox = %+v, want 1 received", m.inbox)
	}
	if len(m.outbox) != 2 {
		t.Fatalf("outbox = %+v, want 2 sent", m.outbox)
	}
	v := m.View()
	if !strings.Contains(v, "Inbox (1)") || !strings.Contains(v, "Outbox (2)") {
		t.Fatalf("view missing pane headers:\n%s", v)
	}
}

func TestReplyFromInboxPrefillsRecipient(t *testing.T) {
	m := sampleModel() // pane 0 (inbox), cursor on the received "Hey"
	m = key(m, "r")    // reply
	if m.scr != screenCompose {
		t.Fatalf("scr = %v, want compose", m.scr)
	}
	if got := m.to.Value(); got != "15551234567" {
		t.Fatalf("reply To = %q, want sender number", got)
	}
	if m.cFoc != 1 {
		t.Fatalf("compose focus = %d, want body (1)", m.cFoc)
	}
	if m.replyTo == "" || !strings.Contains(m.viewCompose(), "Reply to 15551234567") {
		t.Fatalf("compose should show reply heading; replyTo=%q", m.replyTo)
	}
}

func TestReplyIgnoredInOutbox(t *testing.T) {
	m := sampleModel()
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight}) // switch to outbox
	m = mm.(Model)
	if m.pane != 1 {
		t.Fatalf("pane = %d, want outbox (1)", m.pane)
	}
	m = key(m, "r") // reply should be a no-op in outbox
	if m.scr == screenCompose {
		t.Fatalf("reply must not open compose from outbox")
	}
}

func TestAutoRefreshDefaultsOnAndToggles(t *testing.T) {
	m := sampleModel()
	if !m.autoRefresh {
		t.Fatal("auto-refresh should default on")
	}
	if !strings.Contains(m.View(), "auto:on") {
		t.Fatal("header should show auto:on")
	}
	m = key(m, "a") // toggle off
	if m.autoRefresh {
		t.Fatal("'a' should toggle auto-refresh off")
	}
	if !strings.Contains(m.View(), "auto:off") {
		t.Fatal("header should show auto:off")
	}
	m = key(m, "a") // back on
	if !m.autoRefresh {
		t.Fatal("'a' should toggle auto-refresh back on")
	}
}

func TestTickReArmsAndRespectsToggle(t *testing.T) {
	m := sampleModel() // loading=false after loadedMsg, autoRefresh on
	// Tick while on and idle -> should re-arm AND issue a load (cmd non-nil).
	mm, cmd := m.Update(tickMsg{})
	m = mm.(Model)
	if !m.loading {
		t.Fatal("tick with auto-refresh on should start a (silent) load")
	}
	if cmd == nil {
		t.Fatal("tick must always return a command (re-arm)")
	}
	// While a load is in flight, another tick must not stack a second load.
	mm, _ = m.Update(tickMsg{})
	m = mm.(Model)
	if !m.loading { // still just the one in-flight load
		t.Fatal("loading flag should remain set")
	}
	// Finish load, turn auto off, tick should not start a load.
	mm, _ = m.Update(loadedMsg{})
	m = mm.(Model)
	m.autoRefresh = false
	mm, cmd = m.Update(tickMsg{})
	m = mm.(Model)
	if m.loading {
		t.Fatal("tick with auto-refresh off must not load")
	}
	if cmd == nil {
		t.Fatal("tick must still re-arm even when auto-refresh is off")
	}
}

func TestUnreadIndicator(t *testing.T) {
	m := sampleModel() // inbox "Hey" has Status 2 (unread)
	v := m.View()
	if !strings.Contains(v, "1 new") {
		t.Fatalf("inbox header should show unread count:\n%s", v)
	}
	if !strings.Contains(v, "●") {
		t.Fatalf("unread row should show the ● glyph:\n%s", v)
	}
	// Reload with that message marked read -> indicator gone.
	mm, _ := m.Update(loadedMsg{
		status: glsms.ModemStatus{},
		msgs: []glsms.Message{
			{Name: "GMS1.aaa", Direction: glsms.Received, PhoneNumber: "15551234567", Body: "Hey", Status: glsms.StatusReceivedRead},
		},
	})
	m = mm.(Model)
	if unreadCount(m.inbox) != 0 {
		t.Fatalf("unreadCount should be 0 after read, got %d", unreadCount(m.inbox))
	}
	if strings.Contains(m.View(), "1 new") {
		t.Fatalf("header should not show 'new' once read:\n%s", m.View())
	}
}

func TestEnterMarksReadThenToggleUnread(t *testing.T) {
	m := sampleModel() // inbox[0] "Hey" is unread (status 0)
	if !isUnread(m.inbox[0]) {
		t.Fatal("precondition: inbox[0] should start unread")
	}
	// Enter the message -> opens detail AND marks it read (optimistically).
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(Model)
	if m.scr != screenDetail {
		t.Fatalf("scr = %v, want detail", m.scr)
	}
	if isUnread(m.inbox[0]) || unreadCount(m.inbox) != 0 {
		t.Fatalf("opening a message should mark it read; inbox=%+v", m.inbox)
	}
	if !strings.Contains(m.viewDetail(), "received · read") {
		t.Fatalf("detail should show read state:\n%s", m.viewDetail())
	}
	// Toggle back to unread with 'u' (stays on the detail screen).
	m = key(m, "u")
	if m.scr != screenDetail {
		t.Fatalf("'u' must not leave detail; scr=%v", m.scr)
	}
	if !isUnread(m.inbox[0]) {
		t.Fatalf("'u' should mark the message unread again; inbox=%+v", m.inbox)
	}
	if !strings.Contains(m.viewDetail(), "UNREAD") {
		t.Fatalf("detail should show UNREAD after toggle:\n%s", m.viewDetail())
	}
}

func TestPaneSwitchAndCursorsIndependent(t *testing.T) {
	m := sampleModel()
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = mm.(Model)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown}) // move outbox cursor
	m = mm.(Model)
	if m.outCur != 1 || m.inCur != 0 {
		t.Fatalf("cursors not independent: inCur=%d outCur=%d", m.inCur, m.outCur)
	}
	cur, ok := m.current()
	if !ok || cur.Name != "GMS_ccc" {
		t.Fatalf("current() = %+v, want outbox[1]", cur)
	}
}
