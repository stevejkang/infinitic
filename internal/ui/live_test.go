package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func quits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestALiveViewWaitsForTheTerminalsAnswersBeforeQuitting(t *testing.T) {
	m := &liveModel{cancel: func() {}}
	if _, cmd := m.Update(doneMsg{}); quits(cmd) {
		t.Fatal("quit before the terminal answered the mode queries")
	}
	if _, cmd := m.Update(tea.ModeReportMsg{Mode: ansi.ModeSynchronizedOutput}); quits(cmd) {
		t.Fatal("quit after one answer of two")
	}
	if _, cmd := m.Update(tea.ModeReportMsg{Mode: ansi.ModeUnicodeCore}); !quits(cmd) {
		t.Fatal("did not quit once both answers were in")
	}

	answered := &liveModel{cancel: func() {}, replies: 2}
	if _, cmd := answered.Update(doneMsg{}); !quits(cmd) {
		t.Fatal("did not quit at once when the answers came in first")
	}
	if _, cmd := (&liveModel{cancel: func() {}}).Update(quitMsg{}); !quits(cmd) {
		t.Fatal("the wait does not end the view")
	}
}
