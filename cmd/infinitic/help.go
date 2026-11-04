package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/stevejkang/infinitic/internal/snapshot"
	"github.com/stevejkang/infinitic/internal/ui"
)

// Help follows the Vercel CLI guidelines: the usage line and a one-line summary, then Commands,
// Arguments, Options, Global Options, Examples, Notes and Learn more, empty sections left out.
// Headings are bold magenta with a colon, command and option names bold, the summary faint, and
// everything else the normal foreground. Example commands are cyan unless they hold a placeholder.

const helpWidth = 80

var (
	heading  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Magenta)
	name     = lipgloss.NewStyle().Bold(true)
	summary  = lipgloss.NewStyle().Faint(true)
	runnable = lipgloss.NewStyle().Foreground(lipgloss.Cyan)
)

func showHelp(cmd *cobra.Command, _ []string) {
	if on, _ := cmd.Flags().GetBool("json"); on {
		_ = writeHelpJSON(cmd.OutOrStdout(), cmd)
		return
	}
	w := colorprofile.NewWriter(cmd.OutOrStdout(), os.Environ())
	path, rest := usageParts(cmd)
	fmt.Fprintln(w, strings.TrimSpace(name.Render(path)+" "+rest))
	if cmd.Short != "" {
		fmt.Fprintln(w, summary.Render(cmd.Short))
	}
	if len(cmd.Aliases) > 0 {
		fmt.Fprintln(w, "Also: "+name.Render(strings.Join(aliasPaths(cmd), ", ")))
	}
	fmt.Fprintln(w)
	if cmd.HasAvailableSubCommands() {
		commandSections(w, cmd)
	}
	if args := argumentRows(cmd); len(args) > 0 {
		section(w, "Arguments", table(args, false))
	}
	if fl := optionRows(localFlags(cmd)); len(fl) > 0 {
		section(w, "Options", table(fl, true))
	}
	if cmd.HasParent() {
		fmt.Fprintln(w, heading.Render("Global Options:")+" See "+name.Render("infinitic --help"))
		fmt.Fprintln(w)
	} else {
		section(w, "Global Options", table(optionRows(globalFlags(cmd)), true))
	}
	if ex := examples(cmd); len(ex) > 0 {
		var b strings.Builder
		for i, e := range ex {
			if i > 0 {
				b.WriteString("\n")
			}
			line := e.Command
			if !strings.Contains(line, "<") {
				line = runnable.Render(line)
			}
			if e.Description != "" {
				fmt.Fprintf(&b, "  %s\n", e.Description)
			}
			fmt.Fprintf(&b, "    %s\n", line)
		}
		section(w, "Examples", strings.TrimRight(b.String(), "\n"))
	}
	if notes := strings.TrimSpace(cmd.Long); notes != "" {
		var b strings.Builder
		for line := range strings.Lines(notes) {
			fmt.Fprintf(&b, "  %s", line)
		}
		section(w, "Notes", strings.TrimRight(b.String(), "\n"))
	}
	fmt.Fprintln(w, heading.Render("Learn more:")+" "+learnMore)
}

func section(w io.Writer, title, body string) {
	fmt.Fprintln(w, heading.Render(title+":"))
	fmt.Fprintln(w, body)
	fmt.Fprintln(w)
}

// usageParts is the command's full path and what follows it: infinitic rm, [<ACCOUNT>] [options].
func usageParts(cmd *cobra.Command) (string, string) {
	var rest []string
	if args := useArgs(cmd); args != "" {
		rest = append(rest, args)
	}
	if cmd.HasAvailableSubCommands() {
		rest = append(rest, "<command>")
	}
	if len(optionRows(localFlags(cmd))) > 0 {
		rest = append(rest, "[options]")
	}
	return cmd.CommandPath(), strings.Join(rest, " ")
}

func useArgs(cmd *cobra.Command) string {
	return strings.TrimSpace(strings.TrimPrefix(cmd.Use, cmd.Name()))
}

func aliasPaths(cmd *cobra.Command) []string {
	var out []string
	for _, a := range cmd.Aliases {
		out = append(out, cmd.Parent().CommandPath()+" "+a)
	}
	return out
}

// argumentRows reads the arguments annotation: one "NAME\tdescription" per line.
func argumentRows(cmd *cobra.Command) [][2]string {
	var rows [][2]string
	for line := range strings.Lines(cmd.Annotations[annotationArguments]) {
		if n, d, _ := strings.Cut(strings.TrimRight(line, "\n"), "\t"); n != "" {
			rows = append(rows, [2]string{n, d})
		}
	}
	return rows
}

// table lays rows out in two columns. When a row's name and description do not fit in 80
// columns side by side, the description goes on the next line; names are never cut.
func table(rows [][2]string, bold bool) string {
	width := 0
	for _, r := range rows {
		width = max(width, lipgloss.Width(r[0]))
	}
	var b strings.Builder
	for _, r := range rows {
		n := r[0]
		if bold {
			n = boldNames(n)
		}
		switch {
		case r[1] == "":
			fmt.Fprintf(&b, "  %s\n", n)
		case 2+width+2+lipgloss.Width(r[1]) <= helpWidth:
			fmt.Fprintf(&b, "  %s%s  %s\n", n, strings.Repeat(" ", width-lipgloss.Width(r[0])), r[1])
		default:
			fmt.Fprintf(&b, "  %s\n      %s\n", n, r[1])
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// boldNames bolds the command or option words of a row, leaving arguments and value hints as they are.
func boldNames(row string) string {
	words := strings.Split(row, " ")
	for i, wd := range words {
		if wd != "" && !strings.ContainsAny(wd[:1], "[<") {
			words[i] = name.Render(wd)
		}
	}
	return strings.Join(words, " ")
}

// commandSections lists cmd's subcommands under their group headings. A grouped command with
// subcommands of its own (service) is listed as its subcommands, and shortcuts follow the name.
func commandSections(w io.Writer, cmd *cobra.Command) {
	byGroup := map[string][][2]string{}
	add := func(group string, c *cobra.Command) {
		use := c.CommandPath()
		for _, a := range c.Aliases {
			use += ", " + a
		}
		if args := useArgs(c); args != "" {
			use += " " + args
		}
		if c.Name() == "completion" {
			use = c.CommandPath() + " <shell>"
		}
		byGroup[group] = append(byGroup[group], [2]string{use, c.Short})
	}
	for _, c := range cmd.Commands() {
		if !c.IsAvailableCommand() || c.Name() == "help" {
			continue
		}
		if c.GroupID != "" && c.HasAvailableSubCommands() {
			for _, sub := range c.Commands() {
				if sub.IsAvailableCommand() {
					add(c.GroupID, sub)
				}
			}
			continue
		}
		add(c.GroupID, c)
	}
	for _, g := range cmd.Groups() {
		if rows := byGroup[g.ID]; len(rows) > 0 {
			section(w, g.Title, table(rows, true))
		}
	}
	title := "Other Commands"
	if len(cmd.Groups()) == 0 {
		title = "Commands"
	}
	if rows := byGroup[""]; len(rows) > 0 {
		section(w, title, table(rows, true))
	}
}

// localFlags are cmd's own visible flags; --help and --version are listed with the global ones.
func localFlags(cmd *cobra.Command) *pflag.FlagSet {
	fs := pflag.NewFlagSet("local", pflag.ContinueOnError)
	cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name != "help" && f.Name != "version" && !f.Hidden {
			fs.AddFlag(f)
		}
	})
	return fs
}

// globalFlags are the visible persistent flags every command takes, plus --help and --version.
func globalFlags(cmd *cobra.Command) *pflag.FlagSet {
	fs := pflag.NewFlagSet("global", pflag.ContinueOnError)
	cmd.Root().PersistentFlags().VisitAll(func(f *pflag.Flag) {
		if !f.Hidden {
			fs.AddFlag(f)
		}
	})
	for _, n := range []string{"help", "version"} {
		if f := cmd.Flags().Lookup(n); f != nil {
			fs.AddFlag(f)
		}
	}
	return fs
}

// valueHint is how help names an option's value: its annotation, else one from its type.
func valueHint(f *pflag.Flag) string {
	if h := f.Annotations[annotationHint]; len(h) > 0 {
		return h[0]
	}
	return map[string]string{"duration": "DURATION", "int": "NUMBER", "float64": "NUMBER", "string": "VALUE"}[f.Value.Type()]
}

func defaultValue(f *pflag.Flag) string {
	d := f.DefValue
	switch {
	case d == "" || d == "false" || d == "0" || d == "[]":
		return ""
	case f.Value.Type() == "duration":
		if dur, err := time.ParseDuration(d); err == nil {
			return shortDuration(dur)
		}
	}
	return d
}

func optionUsage(f *pflag.Flag) string {
	switch f.Name {
	case "help":
		return "Show help for a command"
	case "version":
		return "Print the version"
	}
	return f.Usage
}

func optionRows(fs *pflag.FlagSet) [][2]string {
	var rows [][2]string
	fs.VisitAll(func(f *pflag.Flag) {
		n := "    --" + f.Name
		if f.Shorthand != "" {
			n = "-" + f.Shorthand + ", --" + f.Name
		}
		if h := valueHint(f); h != "" {
			n += " <" + h + ">"
		}
		usage := optionUsage(f)
		if d := defaultValue(f); d != "" {
			usage += " (default: " + d + ")"
		}
		rows = append(rows, [2]string{n, usage})
	})
	return rows
}

type example struct {
	Description string `json:"description"`
	Command     string `json:"command"`
}

// examples reads cmd.Example: each "# description" line followed by the command it describes.
func examples(cmd *cobra.Command) []example {
	var out []example
	var desc string
	for line := range strings.Lines(cmd.Example) {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "#"):
			desc = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		default:
			out = append(out, example{Description: desc, Command: line})
			desc = ""
		}
	}
	return out
}

// Machine-readable help: `<command> --help --json`, `help --all --json` and `help search --json`.

type helpDoc struct {
	Path          string          `json:"path"`
	Summary       string          `json:"summary"`
	Usage         string          `json:"usage"`
	Aliases       []string        `json:"aliases"`
	Arguments     []argumentDoc   `json:"arguments"`
	Options       []optionDoc     `json:"options"`
	GlobalOptions []optionDoc     `json:"globalOptions,omitempty"`
	Subcommands   []commandRef    `json:"subcommands"`
	Examples      []example       `json:"examples"`
	Notes         string          `json:"notes"`
	ResultSchema  json.RawMessage `json:"resultSchema"`
}

type argumentDoc struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

type optionDoc struct {
	Name        string  `json:"name"`
	Short       *string `json:"short"`
	Type        string  `json:"type"`
	ValueHint   *string `json:"valueHint"`
	Default     *string `json:"default"`
	Description string  `json:"description"`
}

type commandRef struct {
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

// resultSchemas are the JSON results of the commands that have --json.
var resultSchemas = map[string]string{
	"infinitic status":         `{"$ref":"` + snapshot.SchemaURL + `"}`,
	"infinitic version":        `{"type":"object","properties":{"version":{"type":"string"}},"required":["version"]}`,
	"infinitic service status": `{"type":"object","properties":{"service":{"type":"object","properties":{"installed":{"type":"boolean"},"running":{"type":"boolean"},"pid":{"type":["integer","null"]},"startedAt":{"type":["string","null"],"format":"date-time"},"version":{"type":["string","null"]},"executable":{"type":["string","null"]},"config":{"type":["string","null"]},"settings":{"type":["object","null"]},"unit":{"type":"string"},"logs":{"type":"string"},"attention":{"type":"array","items":{"type":"string"}}}}},"required":["service"]}`,
}

func optionDocs(fs *pflag.FlagSet) []optionDoc {
	out := []optionDoc{}
	ptr := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	fs.VisitAll(func(f *pflag.Flag) {
		out = append(out, optionDoc{
			Name: f.Name, Short: ptr(f.Shorthand), Type: f.Value.Type(), ValueHint: ptr(valueHint(f)),
			Default: ptr(defaultValue(f)), Description: optionUsage(f),
		})
	})
	return out
}

func newHelpDoc(cmd *cobra.Command) helpDoc {
	path, rest := usageParts(cmd)
	doc := helpDoc{
		Path: cmd.CommandPath(), Summary: cmd.Short, Usage: strings.TrimSpace(path + " " + rest),
		Aliases: append([]string{}, aliasPaths(cmd)...), Arguments: []argumentDoc{},
		Options: optionDocs(localFlags(cmd)), Subcommands: []commandRef{},
		Examples: append([]example{}, examples(cmd)...), Notes: strings.TrimSpace(cmd.Long),
		ResultSchema: json.RawMessage("null"),
	}
	if !cmd.HasParent() {
		doc.GlobalOptions = optionDocs(globalFlags(cmd))
	}
	if s, ok := resultSchemas[cmd.CommandPath()]; ok {
		doc.ResultSchema = json.RawMessage(s)
	}
	for _, r := range argumentRows(cmd) {
		doc.Arguments = append(doc.Arguments, argumentDoc{Name: r[0], Description: r[1], Required: !strings.Contains(useArgs(cmd), "["+r[0]+"]")})
	}
	for _, c := range cmd.Commands() {
		if c.IsAvailableCommand() {
			doc.Subcommands = append(doc.Subcommands, commandRef{c.CommandPath(), c.Short})
		}
	}
	return doc
}

func writeJSONTo(out io.Writer, v any) error {
	if f, ok := out.(*os.File); ok {
		return ui.WriteJSON(f, v)
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeHelpJSON(out io.Writer, cmd *cobra.Command) error {
	return writeJSONTo(out, newHelpDoc(cmd))
}

// everyCommand is every runnable, visible command under root, depth first.
func everyCommand(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if !sub.IsAvailableCommand() || sub.Name() == "help" {
				continue
			}
			if sub.Runnable() {
				out = append(out, sub)
			}
			walk(sub)
		}
	}
	walk(root)
	return out
}

func helpAll(out io.Writer, root *cobra.Command, asJSON bool) error {
	cmds := everyCommand(root)
	if asJSON {
		docs := []helpDoc{newHelpDoc(root)}
		for _, c := range cmds {
			docs = append(docs, newHelpDoc(c))
		}
		return writeJSONTo(out, map[string]any{"commands": docs})
	}
	var rows [][2]string
	for _, c := range cmds {
		rows = append(rows, [2]string{c.CommandPath(), c.Short})
	}
	fmt.Fprintln(colorprofile.NewWriter(out, os.Environ()), table(rows, true))
	return nil
}

// helpSearch prints up to 10 commands whose path, aliases, summary, notes or examples hold every
// word of query, offline.
func helpSearch(out io.Writer, root *cobra.Command, query string, asJSON bool) error {
	words := strings.Fields(strings.ToLower(query))
	found := []commandRef{}
	for _, c := range everyCommand(root) {
		text := strings.ToLower(strings.Join(append([]string{c.CommandPath(), c.Short, c.Long, c.Example}, c.Aliases...), " "))
		if !slices.ContainsFunc(words, func(wd string) bool { return !strings.Contains(text, wd) }) {
			found = append(found, commandRef{c.CommandPath(), c.Short})
		}
		if len(found) == 10 {
			break
		}
	}
	if asJSON {
		return writeJSONTo(out, map[string]any{"commands": found})
	}
	if len(found) == 0 {
		fmt.Fprintf(out, "No commands match %q.\n", query)
		return nil
	}
	rows := make([][2]string, len(found))
	for i, f := range found {
		rows[i] = [2]string{f.Path, f.Summary}
	}
	fmt.Fprintln(colorprofile.NewWriter(out, os.Environ()), table(rows, true))
	return nil
}
