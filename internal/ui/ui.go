// Package ui renders progress lines, for a terminal or as structured log records, the status
// table, and the prompts.
package ui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"github.com/stevejkang/infinitic/internal/quota"
	"github.com/stevejkang/infinitic/internal/state"
)

type Printer struct {
	Out        io.Writer
	NoInput    bool     // never prompt, as if there were no terminal; see Interactive
	Timestamps bool     // start terminal lines with the time, for long-running output (watch)
	file       *os.File // Out's terminal, when it is one
	structured *slog.Logger
	live       *tea.Program  // set in a live view: lines go above the pinned panel
	width      *atomic.Int32 // the live view's terminal width
	held       *[]string
	last       kind // what went out last, to keep a blank line between lines and blocks
}

// A command's output is blocks (panels, facts, a result) and timestamped lines; on a terminal a
// blank line separates the two kinds, so a supporting line never sticks to what it supports.
type kind int

const (
	nothing kind = iota
	lineKind
	blockKind
)

func (p *Printer) gap(next kind) {
	if p.file != nil && p.last != nothing && p.last != next {
		fmt.Fprintln(p.Out)
	}
	p.last = next
}

// Hold keeps the lines printed from now on in lines instead, so Release can print them under the
// main output.
func (p *Printer) Hold(lines *[]string) { p.held = lines }

func (p *Printer) Release(lines []string) {
	for _, line := range lines {
		p.gap(lineKind)
		fmt.Fprintln(p.Out, line)
	}
}

// Block prints a command's result, such as a version.
func (p *Printer) Block(text string) {
	p.gap(blockKind)
	fmt.Fprintln(p.Out, text)
}

// Shown records that a block reached the terminal some other way, such as a live view's last frame.
func (p *Printer) Shown() { p.last = blockKind }

// NewPrinter writes styled lines; colors out cannot show (a pipe, a file, NO_COLOR, TERM=dumb)
// are dropped.
func NewPrinter(out *os.File) *Printer {
	p := &Printer{Out: colorprofile.NewWriter(out, os.Environ())}
	if IsTerminal(out) {
		p.file = out
	}
	return p
}

// NewLogPrinter writes progress lines as slog text records: RFC 3339 time, level, key=value fields.
func NewLogPrinter(out io.Writer) *Printer {
	h := slog.NewTextHandler(out, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.LevelKey {
			a.Value = slog.StringValue(levelName(a.Value.Any().(slog.Level)))
		}
		return a
	}})
	return &Printer{Out: out, structured: slog.New(h)}
}

// A line's kind, named the same on a terminal and in a log: four letters each, so messages line up.
// levelDone sits between info and warning, so a log filtered at warning drops it with info.
const levelDone = slog.LevelInfo + 2

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "FAIL"
	case l >= slog.LevelWarn:
		return "WARN"
	case l >= levelDone:
		return "DONE"
	default:
		return "INFO"
	}
}

func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// agentEnv are variables coding agents set in the commands they run. An agent cannot answer a
// prompt it cannot see, so its runs are non-interactive.
var agentEnv = []string{"CLAUDECODE", "OPENCODE", "CURSOR_AGENT", "GEMINI_CLI", "CODEX_SANDBOX"}

// Automated reports whether the run belongs to CI or a coding agent.
func Automated() bool {
	if ci := os.Getenv("CI"); ci != "" && ci != "0" && ci != "false" {
		return true
	}
	for _, name := range agentEnv {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// Interactive resolves whether this run may prompt: never with --non-interactive or --json, in
// CI, for an agent, or without a terminal on stdin and stderr. For an agent at a terminal,
// --non-interactive=false (explicitOff) brings prompts back. Commands read the result through
// Printer.NoInput and never check terminals or the environment themselves.
func Interactive(nonInteractive, explicitOff, json bool) bool {
	switch {
	case nonInteractive, json:
		return false
	case !IsTerminal(os.Stdin) || !IsTerminal(os.Stderr):
		return false
	case Automated():
		return explicitOff && os.Getenv("CI") == ""
	}
	return true
}

// NoColor turns color off on every stream from now on, as NO_COLOR does.
func (p *Printer) NoColor() {
	if w, ok := p.Out.(*colorprofile.Writer); ok {
		w.Profile = colorprofile.Ascii
	}
	_ = os.Setenv("NO_COLOR", "1") // prompts, help and live views read it
}

// CanPrompt reports whether a prompt can be shown: it reads stdin and draws on stderr.
func (p *Printer) CanPrompt() bool {
	return !p.NoInput && IsTerminal(os.Stdin) && IsTerminal(os.Stderr)
}

// ErrNoAnswer means a prompt could not be shown: there is no terminal to ask on.
var ErrNoAnswer = errors.New("no answer")

// Confirm asks a yes/no question. Cancelling it (esc, ctrl+c) answers no.
func (p *Printer) Confirm(question string) (bool, error) {
	if !p.CanPrompt() {
		return false, ErrNoAnswer
	}
	var yes bool
	err := prompt(huh.NewConfirm().Title(question).Affirmative("Yes").Negative("No").Value(&yes), "←→ choose  ·  enter confirm  ·  esc cancel")
	if errors.Is(err, huh.ErrUserAborted) {
		return false, nil
	}
	return yes, err
}

type Choice struct{ Label, Value string }

// ErrCancelled means a prompt was cancelled or declined: a normal end, nothing changed. main
// reports it quietly and exits 0.
var ErrCancelled = errors.New("nothing changed")

// Choose asks for one of choices and returns its Value; esc or ctrl+c returns ErrCancelled.
func (p *Printer) Choose(title string, choices []Choice) (string, error) {
	if !p.CanPrompt() {
		return "", ErrNoAnswer
	}
	opts := make([]huh.Option[string], len(choices))
	for i, c := range choices {
		opts[i] = huh.NewOption(c.Label, c.Value)
	}
	var v string
	err := prompt(huh.NewSelect[string]().Title(title).Options(opts...).Value(&v), "↑↓ move  ·  enter choose  ·  esc cancel")
	if errors.Is(err, huh.ErrUserAborted) {
		return "", ErrCancelled
	}
	return v, err
}

// prompt runs one field with keys as its grey footer, in place of huh's own help line, so every
// wait in infinitic says the same way how to get out of it (see Keys).
func prompt(f huh.Field, keys string) error {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"))
	footer := huh.NewNote().Description(keys) // a Note takes no focus: enter on f submits the form
	return huh.NewForm(huh.NewGroup(f, footer)).WithKeyMap(km).WithTheme(huh.ThemeFunc(chalkTheme)).
		WithOutput(os.Stderr).WithShowHelp(false).Run()
}

// Keys styles a key hint footer, "a  ·  b" in grey, as the live view and the prompts show it.
func Keys(hint string) string { return "  " + faint.Render(hint) }

// Hint prints a key hint footer under what a person is waiting on, only on a terminal: a log
// or a pipe has nobody to press the keys.
func (p *Printer) Hint(hint string) {
	if p.structured != nil || p.file == nil {
		return
	}
	fmt.Fprintln(p.Out, Keys(hint))
}

// chalkTheme styles prompts with the palette below: a grey bar, a green cursor. The row under
// the cursor is plain text and every other row is grey, so the current choice stands out
// without recoloring it; the buttons work the same way.
func chalkTheme(isDark bool) *huh.Styles {
	t := huh.ThemeBase(isDark)
	f := &t.Focused
	f.Base = f.Base.BorderForeground(faint.GetForeground())
	f.Title = f.Title.Bold(true)
	f.Description = faint
	f.SelectSelector = f.SelectSelector.Foreground(green.GetForeground())
	f.SelectedOption = lipgloss.NewStyle()
	f.UnselectedOption = faint
	f.Option = faint
	f.NextIndicator = f.NextIndicator.Inherit(faint)
	f.PrevIndicator = f.PrevIndicator.Inherit(faint)
	f.FocusedButton = f.FocusedButton.Foreground(ink).Background(green.GetForeground())
	f.BlurredButton = f.BlurredButton.Foreground(ink).Background(faint.GetForeground())
	f.ErrorIndicator = red
	f.ErrorMessage = red
	t.Blurred = t.Focused
	t.Blurred.Base = t.Blurred.Base.BorderStyle(lipgloss.HiddenBorder())
	return t
}

// A chalk palette: light tones that are still clearly green, yellow and red. Terminals with
// fewer colors get the nearest ones. Grey is the terminal's own bright black, so the user's
// theme decides its contrast.
var (
	plain  = lipgloss.NewStyle()
	faint  = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("#F28B8B"))
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8FD69A"))
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("#F2C76B"))
	ink    = lipgloss.Color("#1E2029") // text on a colored button

	// Muted is the palette's grey, for text that supports the main text; TagGood, TagWarn and
	// TagBad are the palette's green, sand and rose, for tags in prompts.
	Muted, TagGood, TagWarn, TagBad = faint, green, yellow, red
)

// Info, Warn, Success and Error take slog key/value pairs, which only structured output records.
// On a terminal a line starts with a glyph in a 2-column gutter (✓ done, ! warning, ✗ failure);
// in a pipe or a file the glyph becomes a word, so the line reads without color.
func (p *Printer) Info(msg string, fields ...any)    { p.emit(slog.LevelInfo, plain, msg, fields) }
func (p *Printer) Warn(msg string, fields ...any)    { p.emit(slog.LevelWarn, yellow, msg, fields) }
func (p *Printer) Success(msg string, fields ...any) { p.emit(levelDone, green, msg, fields) }
func (p *Printer) Error(msg string, fields ...any)   { p.emit(slog.LevelError, red, msg, fields) }

// WriteJSON writes v as one JSON document: indented for a person at a terminal, one line for a
// program reading a pipe.
func WriteJSON(out *os.File, v any) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	if IsTerminal(out) {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}

// rich reports whether Out takes glyphs and styling: a terminal whose TERM is not dumb.
func (p *Printer) rich() bool {
	return (p.file != nil || p.live != nil) && os.Getenv("TERM") != "dumb"
}

func (p *Printer) prefix(level slog.Level, style lipgloss.Style) string {
	glyphs := map[slog.Level][2]string{
		slog.LevelInfo:  {"  ", ""},
		levelDone:       {"✓ ", ""},
		slog.LevelWarn:  {"! ", "Warning: "},
		slog.LevelError: {"✗ ", "Error: "},
	}[level]
	if p.rich() {
		return style.Render(glyphs[0])
	}
	return glyphs[1]
}

func (p *Printer) emit(level slog.Level, style lipgloss.Style, msg string, fields []any) {
	if p.structured != nil {
		p.structured.Log(context.Background(), level, msg, fields...)
		return
	}
	p.line(p.prefix(level, style) + msg)
}

// line prints one line of output, above the pinned panel in a live view.
func (p *Printer) line(line string) {
	if p.Timestamps {
		line = faint.Render(time.Now().Format("15:04:05")) + " " + line
	}
	if p.held != nil {
		*p.held = append(*p.held, line)
		return
	}
	if p.live != nil {
		p.live.Send(tea.Println(line)()) // the Cmd's message prints above the pinned panel
		p.live.Send(printedMsg{})
		return
	}
	p.gap(lineKind)
	fmt.Fprintln(p.Out, line)
}

// A Fact is a footer line: a label, its value, and an optional note shown dim after the value,
// e.g. "bars", "5h 90% · 7d 98%", "from ~/.config/infinitic/config.yml".
type Fact struct{ Label, Value, Note string }

// Status prints the status panels (see Render), fitted to the terminal when there is one.
func (p *Printer) Status(s *state.State, bars quota.Bars, footer ...Fact) {
	p.gap(blockKind)
	fmt.Fprint(p.Out, Render(s, bars, p.Width(), footer...))
}

// Width is the terminal's width, 0 when Out is not one.
func (p *Printer) Width() int {
	if p.width != nil {
		return int(p.width.Load())
	}
	if p.file == nil {
		return 0
	}
	w, _, _ := term.GetSize(p.file.Fd())
	return w
}

// Terminal reports whether lines go to a person's terminal, not a pipe, a file or a log.
func (p *Printer) Terminal() bool { return p.structured == nil && p.file != nil }

// CanLive reports whether a live view can be drawn: it draws on stdout and reads keys from stdin.
func (p *Printer) CanLive() bool {
	return !p.NoInput && p.file != nil && IsTerminal(os.Stdin)
}

// Render draws the accounts panel, the active account first, and below it an attention panel with
// what to run for each account that needs it, then footer dim at the bottom. width > 0 is the
// terminal width: the panels fit in it, dropping the OTHER and RESET columns if they must.
func Render(s *state.State, bars quota.Bars, width int, footer ...Fact) string {
	accounts := slices.Clone(s.Accounts)
	slices.SortStableFunc(accounts, func(x, y *state.Account) int {
		return cmp.Compare(boolInt(y.UUID == s.ActiveUUID), boolInt(x.UUID == s.ActiveUUID))
	})
	others := otherLabels(accounts)
	now := time.Now()
	var notes []note
	for _, a := range accounts {
		notes = append(notes, attention(a, now)...)
	}
	if s.ActiveUUID == "" {
		notes = append(notes, note{yellow.Render("!"), "no primary login", "", faint.Render("claude /login, or infinitic switch <account>")})
	}

	body := accountRows(accounts, s.ActiveUUID, bars, others, now, 2)
	for drop := 1; width > 0 && lipgloss.Width(body)+4 > width && drop <= 2; drop++ {
		body = accountRows(accounts, s.ActiveUUID, bars, others, now, 2-drop)
	}
	// The attention panel may be wider than the accounts panel when the terminal allows it.
	limit := width - 4
	if width <= 0 {
		limit = 0
	}
	notesBody := attentionRows(notes, limit)
	inner := max(lipgloss.Width(body), lipgloss.Width(notesBody))
	if limit > 0 {
		inner = min(inner, limit)
	}
	var b strings.Builder
	b.WriteString(panel("Accounts", body, inner) + "\n")
	if notesBody != "" {
		b.WriteString(panel("Attention", notesBody, inner) + "\n")
	}
	b.WriteString(renderFacts(footer))
	return b.String()
}

// otherLabels are the windows beyond 5h and 7d (e.g. a per-model weekly cap) any account reports,
// in the order first seen; each gets its own columns.
func otherLabels(accounts []*state.Account) []string {
	var labels []string
	for _, a := range accounts {
		if a.Usage == nil {
			continue
		}
		for _, w := range a.Usage.Others() {
			if !slices.Contains(labels, w.Label) {
				labels = append(labels, w.Label)
			}
		}
	}
	return labels
}

// accountRows renders the accounts table. extra is how many optional column groups to keep:
// 2 = Reset columns and the other windows, 1 = Reset columns only, 0 = neither. A thin rule
// separates the account from its usage groups (5h, 7d, each other window) and the checked time.
func accountRows(accounts []*state.Account, activeUUID string, bars quota.Bars, others []string, now time.Time, extra int) string {
	rule := faint.Render("│") + " "
	headers := []string{"", "Account", "Plan", rule + "5h"}
	if extra >= 1 {
		headers = append(headers, "Reset")
	}
	headers = append(headers, rule+"7d")
	if extra >= 1 {
		headers = append(headers, "Reset")
	}
	if extra >= 2 {
		for _, label := range others {
			headers = append(headers, rule+label, "Reset")
		}
	}
	headers = append(headers, rule+"Checked")
	t := table.New().Headers(headers...).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderColumn(false).BorderHeader(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			st := lipgloss.NewStyle().PaddingRight(2)
			if col == 0 {
				st = st.PaddingRight(1)
			}
			if col == len(headers)-1 {
				st = st.UnsetPaddingRight()
			}
			if row == table.HeaderRow {
				return st.Inherit(faint)
			}
			return st
		})
	for _, a := range accounts {
		session, weekly := a.Usage.Window(quota.SessionLabel), a.Usage.Window(quota.WeeklyLabel)
		sessionMeter := meter(session, bars.Session)
		if quota.NoSessionLimit(a.Usage) {
			sessionMeter = faint.Render("no limit") + strings.Repeat(" ", meterCells+6-len("no limit"))
		}
		row := []string{describe(a, a.UUID == activeUUID, bars, now), a.Label(), faint.Render(a.Plan), rule + sessionMeter}
		if extra >= 1 {
			row = append(row, faint.Render(resetIn(session, now)))
		}
		row = append(row, rule+meter(weekly, bars.Weekly))
		if extra >= 1 {
			row = append(row, faint.Render(resetIn(weekly, now)))
		}
		if extra >= 2 {
			for _, label := range others {
				w := a.Usage.Window(label)
				row = append(row, rule+meter(w, bars.Weekly), faint.Render(resetIn(w, now))) // other windows are weekly caps
			}
		}
		t.Row(append(row, rule+checked(a.Usage, now))...)
	}
	return t.Render()
}

func (p *Printer) Facts(facts []Fact) {
	p.gap(blockKind)
	fmt.Fprint(p.Out, renderFacts(facts))
}

// A Step is what to do next: an instruction ending in a colon and the command to run, or a
// plain sentence when there is no command.
type Step struct{ Instruction, Command string }

var command = lipgloss.NewStyle().Foreground(lipgloss.Cyan)

// Steps prints next steps after a blank line, each command on its own line, indented 4.
func (p *Printer) Steps(steps ...Step) {
	if len(steps) == 0 || p.structured != nil {
		return
	}
	var b strings.Builder
	for _, s := range steps {
		if s.Command == "" {
			fmt.Fprintf(&b, "  %s\n", s.Instruction)
			continue
		}
		cmd := s.Command
		if p.rich() {
			cmd = command.Render(cmd)
		}
		fmt.Fprintf(&b, "  %s:\n    %s\n", s.Instruction, cmd)
	}
	fmt.Fprintln(p.Out)
	fmt.Fprint(p.Out, b.String())
	p.last = blockKind
}

// Fail prints an error: the headline after ✗ (Error: outside a terminal), its details indented
// under it, then what to do about it.
func (p *Printer) Fail(headline string, details []string, steps []Step) {
	p.Error(headline)
	for _, d := range details {
		fmt.Fprintln(p.Out, "  "+d)
	}
	p.Steps(steps...)
}

// Receipt reports a completed change as aligned rows: ✓, the past-tense verb and its target,
// then supporting facts. Labels take 16 columns, so values line up at column 18.
func (p *Printer) Receipt(verb, target string, rows ...Fact) {
	if p.structured != nil {
		p.Success(verb + " " + target)
		return
	}
	gutter := ""
	if p.rich() {
		gutter = "  "
	}
	p.line(p.prefix(levelDone, green) + fmt.Sprintf("%-16s%s", verb, target))
	for _, r := range rows {
		p.line(gutter + fmt.Sprintf("%-16s%s", r.Label, r.Value))
	}
}

func renderFacts(facts []Fact) string {
	labels := 0
	for _, f := range facts {
		labels = max(labels, lipgloss.Width(f.Label))
	}
	var b strings.Builder
	for _, f := range facts {
		line := fmt.Sprintf("  %s%s  %s", faint.Render(f.Label), strings.Repeat(" ", labels-lipgloss.Width(f.Label)), f.Value)
		if f.Note != "" {
			line += "  " + faint.Render(f.Note)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// A Note is one thing to do: what about, why, and the command to run. It is laid out like the
// status panel's attention rows.
type Note struct{ Subject, Reason, Fix string }

// Attention prints notes as a warning panel sized to the terminal; nothing when there are none.
func (p *Printer) Attention(notes []Note) {
	if len(notes) == 0 {
		return
	}
	p.gap(blockKind)
	rows := make([]note, len(notes))
	for i, n := range notes {
		rows[i] = note{yellow.Render("!"), n.Subject, yellow.Render(n.Reason), faint.Render(n.Fix)}
	}
	limit := 0
	if w := p.Width(); w > 0 {
		limit = w - 4
	}
	body := attentionRows(rows, limit)
	inner := lipgloss.Width(body)
	if limit > 0 {
		inner = min(inner, limit)
	}
	fmt.Fprintln(p.Out, panel("Attention", body, inner))
}

// panel boxes body in a rounded border with title set into the top edge. width is the body's;
// the box adds 2 of border and 2 of padding.
func panel(title, body string, width int) string {
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder(), false, true, true, true).
		BorderForeground(faint.GetForeground()).Padding(0, 1).Width(width + 4)
	inner := box.Render(body)
	edge := faint.Render
	fill := lipgloss.Width(inner) - lipgloss.Width(title) - 5 // "╭─ " + " " + "╮"
	return edge("╭─ ") + title + edge(" "+strings.Repeat("─", max(fill, 0))+"╮") + "\n" + inner
}

// note is one line of the attention panel: icon, account, reason, and what to run.
type note struct{ icon, account, reason, fix string }

// attentionRows lays notes out in columns; when the fix does not fit beside the reason within
// width, it goes on its own line under it, and the reason is clipped to fit.
func attentionRows(notes []note, width int) string {
	if len(notes) == 0 {
		return ""
	}
	account := 0
	for _, n := range notes {
		account = max(account, lipgloss.Width(n.account))
	}
	lead := 2 + account + 2
	reason, fix := 0, 0
	for _, n := range notes {
		reason = max(reason, lipgloss.Width(n.reason))
		fix = max(fix, lipgloss.Width(n.fix))
	}
	inline := width <= 0 || lead+reason+2+fix <= width
	var b strings.Builder
	for i, n := range notes {
		if i > 0 {
			b.WriteString("\n")
		}
		text := n.reason
		if room := width - lead; width > 0 && lipgloss.Width(text) > room {
			text = clipStyled(text, room)
		}
		b.WriteString(n.icon + " " + n.account + strings.Repeat(" ", account-lipgloss.Width(n.account)) + "  " + text)
		switch {
		case n.fix == "":
		case inline:
			b.WriteString(strings.Repeat(" ", reason-lipgloss.Width(n.reason)+2) + n.fix)
		default:
			b.WriteString("\n" + strings.Repeat(" ", lead) + n.fix)
		}
	}
	return b.String()
}

func checked(u *quota.Usage, now time.Time) string {
	if u == nil {
		return ""
	}
	d := now.Sub(u.At)
	switch {
	case d < time.Minute:
		return faint.Render("just now")
	case d < time.Hour:
		return faint.Render(fmt.Sprintf("%dm ago", int(d.Minutes())))
	default:
		return faint.Render(fmt.Sprintf("%dh ago", int(d.Hours())))
	}
}

func clipStyled(s string, n int) string {
	if n < 2 {
		return "" // no room even for the ellipsis
	}
	return ansi.Truncate(s, n-1, "…")
}

func attention(a *state.Account, now time.Time) []note {
	fix := faint.Render("infinitic reauth " + a.Label())
	var out []note
	switch {
	case a.NeedsReauth != "":
		out = append(out, note{red.Render("✗"), a.Label(), red.Render("login revoked: " + oneLine(a.NeedsReauth)), fix})
	case a.Problem() != "" && a.BackingOff(now):
		out = append(out, note{yellow.Render("!"), a.Label(), yellow.Render(oneLine(a.Problem())), faint.Render("retrying at " + a.RetryAt.Local().Format("15:04"))})
	case a.Problem() != "":
		out = append(out, note{yellow.Render("!"), a.Label(), yellow.Render(oneLine(a.Problem())), ""})
	}
	if notice := a.LoginNotice(now); notice != "" && a.NeedsReauth == "" {
		out = append(out, note{yellow.Render("!"), a.Label(), yellow.Render(notice), fix})
	}
	return out
}

// describe is the row's icon; its shape alone says the state (● active, ○ parked, ! needs a look,
// ✗ needs reauth) and color only repeats it. The attention panel says why.
func describe(a *state.Account, active bool, bars quota.Bars, now time.Time) string {
	issue := a.Problem() != "" || a.LoginNotice(now) != ""
	switch {
	case a.NeedsReauth != "":
		return red.Render("✗")
	case active && issue:
		return yellow.Render("●")
	case active:
		return green.Render("●")
	case issue:
		return yellow.Render("!")
	case bars.Over(a.Usage):
		return faint.Render("○")
	default:
		return "○"
	}
}

const meterCells = 8

// meter draws usage as ▰▰▰▱▱▱▱▱  34%; at or past the bar it turns red and gets a ▲. Unknown
// usage is the word, padded to the meter's width.
func meter(w *quota.Window, bar float64) string {
	if w == nil {
		return faint.Render("unknown") + strings.Repeat(" ", meterCells+6-len("unknown"))
	}
	filled := int(math.Round(math.Min(w.Percent, 100) / 100 * meterCells))
	cells := strings.Repeat("▰", filled)
	value := fmt.Sprintf(" %3.0f%%", w.Percent)
	if w.Percent >= bar {
		return red.Render(cells) + faint.Render(strings.Repeat("▱", meterCells-filled)) + red.Render(value+"▲")
	}
	return cells + faint.Render(strings.Repeat("▱", meterCells-filled)) + value + " " // as wide as "▲"
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func resetIn(w *quota.Window, now time.Time) string {
	if w == nil || w.ResetsAt == nil {
		return ""
	}
	d := w.ResetsAt.Sub(now)
	switch {
	case d <= 0:
		return "now"
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func oneLine(s string) string { return strings.ReplaceAll(s, "\n", " ") }
