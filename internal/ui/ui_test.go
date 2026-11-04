package ui

import (
	"os"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/stevejkang/infinitic/internal/quota"
	"github.com/stevejkang/infinitic/internal/state"
)

func pool(t *testing.T) *state.State {
	t.Helper()
	now := time.Now()
	reset := now.Add(2 * time.Hour)
	soon := now.Add(2 * 24 * time.Hour)
	usage := func(session, weekly float64) *quota.Usage {
		return &quota.Usage{At: now, Windows: []quota.Window{
			{Label: quota.SessionLabel, Percent: session, ResetsAt: &reset},
			{Label: quota.WeeklyLabel, Percent: weekly, ResetsAt: &reset},
			{Label: "Fable", Percent: 5, ResetsAt: &reset},
		}}
	}
	return &state.State{ActiveUUID: "a", Accounts: []*state.Account{
		{UUID: "b", Email: "b@example.com", Plan: "Team Premium", LoginExpiresAt: &soon, Usage: usage(62, 40)},
		{UUID: "a", Email: "a@example.com", Plan: "Ultra", Usage: usage(91, 20)},
		{UUID: "c", Email: "c@example.com", Plan: "Pro", NeedsReauth: "token refresh: invalid_grant: the refresh token has been revoked"},
	}}
}

func TestRenderPutsTheActiveAccountFirstAndFlagsTheRest(t *testing.T) {
	out := ansi.Strip(Render(pool(t), quota.DefaultBars, 0, Fact{Label: "bars", Value: "5h 90% · 7d 98%"}))
	lines := strings.Split(out, "\n")
	for i, want := range []string{"╭─ Accounts", "Account", "● a@example.com", "! b@example.com", "✗ c@example.com"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %q, want %q", i, lines[i], want)
		}
	}
	for _, want := range []string{"91%▲", "Fable", "╭─ Attention", "login expires in 2 days", "infinitic reauth b@example.com", "login revoked: token refresh: invalid_grant", "infinitic reauth c@example.com", "bars  5h 90%", "Checked", "just now"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestRenderSaysWhenAPlanHasNoSessionLimit(t *testing.T) {
	st := pool(t)
	st.Accounts[1].Usage.Windows = st.Accounts[1].Usage.Windows[1:] // a has no 5h window
	out := ansi.Strip(Render(st, quota.DefaultBars, 0))
	if !strings.Contains(out, "no limit") || strings.Contains(out, "91%") {
		t.Fatalf("a missing 5h window must read \"no limit\":\n%s", out)
	}
}

func TestRenderFitsTheTerminalWidth(t *testing.T) {
	for _, width := range []int{60, 80, 100} {
		out := Render(pool(t), quota.DefaultBars, width, Fact{Label: "bars", Value: "5h 90% · 7d 98%"}, Fact{Label: "watch", Value: "running in the background"})
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if w := lipgloss.Width(line); w > width {
				t.Errorf("width %d: line is %d wide: %q", width, w, line)
			}
		}
		if width < 70 && strings.Contains(out, "Reset") {
			t.Errorf("width %d: RESET columns were not dropped", width)
		}
	}
}

func TestSupportingLinesGoUnderTheBlockAfterOneBlankLine(t *testing.T) {
	var out strings.Builder
	p := &Printer{Out: &out, file: os.Stdout} // a terminal: only there are blank lines added
	var held []string
	p.Hold(&held)
	p.Warn("checking b failed")
	p.Warn("checking c failed")
	p.Hold(nil)
	p.Facts([]Fact{{Label: "bars", Value: "5h 90%"}})
	p.Attention([]Note{{Subject: "watch", Reason: "changed"}})
	p.Release(held)
	p.Info("a newer release is out")

	lines := strings.Split(ansi.Strip(out.String()), "\n")
	var shape []string
	for _, l := range lines[:len(lines)-1] {
		switch {
		case l == "":
			shape = append(shape, "blank")
		case strings.Contains(l, "bars"), strings.ContainsAny(l, "╭│╰"):
			shape = append(shape, "block")
		default:
			shape = append(shape, "line")
		}
	}
	want := "block block block block blank line line line"
	if got := strings.Join(shape, " "); got != want {
		t.Fatalf("output shape %q, want %q:\n%s", got, want, out.String())
	}

	var piped strings.Builder
	q := &Printer{Out: &piped}
	q.Block("0.4.0")
	q.Info("a newer release is out")
	if strings.Contains(piped.String(), "\n\n") {
		t.Fatalf("blank line added outside a terminal:\n%s", piped.String())
	}
}

func TestALineIsNamedInALogAndInWordsOnAPlainTerminal(t *testing.T) {
	var log strings.Builder
	l := NewLogPrinter(&log)
	l.Info("i")
	l.Warn("w")
	l.Success("s")
	l.Error("e")
	var term strings.Builder
	p := &Printer{Out: &term}
	p.Info("i")
	p.Warn("w")
	p.Success("s")
	p.Error("e")
	for _, name := range []string{"INFO", "WARN", "DONE", "FAIL"} {
		if !strings.Contains(log.String(), "level="+name+" ") {
			t.Errorf("log has no level=%s:\n%s", name, log.String())
		}
	}
	// Without color a warning and an error still say what they are, in words.
	if got, want := ansi.Strip(term.String()), "i\nWarning: w\ns\nError: e\n"; got != want {
		t.Errorf("terminal %q, want %q", got, want)
	}
}
