package ui

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// replyWait is how long a finished view waits for the terminal to answer the two mode queries
// bubbletea sends at start. An answer that arrives after exit is left in the terminal's input and
// the shell prints it as typed text ("2026;0$y2027;0$y").
const replyWait = 100 * time.Millisecond

// A live view is inline (not full-screen): progress lines scroll above, and a panel stays pinned
// below them while work runs. status and watch use it on a terminal.

// RunLive runs work with a Printer whose lines appear above a pinned panel. work sets the panel
// with Printer.Pin; the last panel is left on screen when it returns. Ctrl+C cancels ctx.
func (parent *Printer) RunLive(ctx context.Context, title string, work func(ctx context.Context, p *Printer) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := &liveModel{title: title, spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(faint)), cancel: cancel}
	// Two messages end a view, in this order. stoppingMsg (ctx cancelled: ctrl+c, a signal) only
	// freezes the spinner, so lines work prints while winding down still land above the panel.
	// doneMsg (work returned) quits, with the last frame drawn. Send blocks until the event loop
	// runs and is a no-op after it quits, which is what makes the goroutine safe.
	program := tea.NewProgram(m, tea.WithOutput(os.Stdout), tea.WithoutSignalHandler())
	stop := context.AfterFunc(ctx, func() { program.Send(stoppingMsg{}) })
	defer stop()
	p := &Printer{Out: os.Stdout, live: program, width: &m.width, Timestamps: parent.Timestamps}

	result := make(chan error, 1)
	go func() {
		err := work(ctx, p)
		program.Send(doneMsg{})
		result <- err
	}()
	if _, err := program.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) && !errors.Is(err, tea.ErrInterrupted) {
		cancel()
		return errors.Join(err, <-result) // wait: work may hold the lock or be mid-save
	}
	return <-result // a keyboard stop cancels ctx; work decides what that means
}

// Pin replaces the pinned panel. Outside a live view there is none, and nothing is printed.
func (p *Printer) Pin(panel string) {
	if p.live != nil {
		p.live.Send(panelMsg(panel))
	}
}

// Busy notes what is going on next to the spinner; empty clears it.
func (p *Printer) Busy(what string) {
	if p.live != nil {
		p.live.Send(busyMsg(what))
	}
}

type (
	panelMsg    string
	busyMsg     string
	printedMsg  struct{}
	stoppingMsg struct{} // ctx was cancelled: stop the spinner while work winds down
	doneMsg     struct{}
	quitMsg     struct{} // replyWait passed without both answers
)

type liveModel struct {
	title   string
	spinner spinner.Model
	panel   string
	busy    string
	printed bool // lines went out above the panel: a blank line keeps them off it
	width   atomic.Int32
	done    bool
	cancel  context.CancelFunc
	replies int  // answers to the mode queries read so far
	ending  bool // work returned; quit once both answers are in
}

func (m *liveModel) Init() tea.Cmd { return m.spinner.Tick }

func (m *liveModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width.Store(int32(msg.Width))
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" || msg.String() == "esc" {
			m.done = true // stop drawing the spinner; work ends when it sees the cancelled context
			m.cancel()
		}
	case panelMsg:
		m.panel = string(msg)
	case busyMsg:
		m.busy = string(msg)
	case printedMsg:
		m.printed = true
	case tea.ModeReportMsg:
		if msg.Mode == ansi.ModeSynchronizedOutput || msg.Mode == ansi.ModeUnicodeCore {
			m.replies++
		}
		if m.ending && m.replies >= 2 {
			return m, tea.Quit
		}
	case doneMsg:
		m.done = true
		if m.replies >= 2 {
			return m, tea.Quit
		}
		m.ending = true
		return m, tea.Tick(replyWait, func(time.Time) tea.Msg { return quitMsg{} })
	case quitMsg:
		return m, tea.Quit
	case stoppingMsg:
		m.done = true
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

// View is the pinned panel with a spinner line under it; the last frame, the panel alone, stays
// on screen when the program exits.
func (m *liveModel) View() tea.View {
	top := ""
	if m.printed {
		top = "\n"
	}
	if m.done {
		return tea.NewView(top + m.panel)
	}
	var b strings.Builder
	b.WriteString(top)
	if m.panel != "" {
		b.WriteString(m.panel)
	}
	status := m.spinner.View() + " " + faint.Render(m.title)
	if m.busy != "" {
		status += faint.Render(": " + m.busy)
	}
	b.WriteString("  " + status + faint.Render("  ·  ctrl+c to stop") + "\n")
	return tea.NewView(b.String())
}
