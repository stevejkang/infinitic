package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/stevejkang/infinitic/internal/quota"
	"github.com/stevejkang/infinitic/internal/rotator"
	"github.com/stevejkang/infinitic/internal/service"
	"github.com/stevejkang/infinitic/internal/state"
	"github.com/stevejkang/infinitic/internal/ui"
)

// app holds what commands share; tests replace the service manager, the prompt and the wait.
type app struct {
	p     *ui.Printer
	mgr   service.Manager // nil: the current OS's
	ask   func(string) (bool, error)
	wait  time.Duration
	upd   updater
	check *updateCheck
}

func newApp(p *ui.Printer) *app {
	return &app{p: p, ask: p.Confirm, wait: startWait, upd: newUpdater()}
}

func (a *app) manager() (service.Manager, error) {
	if a.mgr != nil {
		return a.mgr, nil
	}
	return service.ForCurrentOS()
}

const (
	groupAccounts = "accounts"
	groupCore     = "core"
	groupService  = "service"

	annotationJSON      = "infinitic/json"      // the command prints JSON with --json
	annotationArguments = "infinitic/arguments" // "<NAME>\tdescription" lines for help
	annotationHint      = "infinitic/hint"      // a flag's value hint, such as PERCENT

	learnMore     = "https://github.com/stevejkang/infinitic"
	accountArgDoc = "<ACCOUNT>\tAn account's email or UUID, exactly as `infinitic status` shows it"
)

// hint sets the value hint help shows for a flag: --session <PERCENT>.
func hint(fs *pflag.FlagSet, name, value string) {
	_ = fs.SetAnnotation(name, annotationHint, []string{value})
}

func jsonFlag(cmd *cobra.Command, usage string) *bool {
	cmd.Annotations = map[string]string{annotationJSON: "true"}
	return cmd.Flags().Bool("json", false, usage)
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "infinitic",
		Short: "Rotate your Claude Code login across accounts before you hit a limit",
		Long: `Commands that need an account ask for one on a terminal when none is given. Every prompt has an
argument or option that skips it, and none is shown with --non-interactive, --json, in CI, or to a
coding agent.`,
		Example: `# Register the account Claude Code is logged in as
infinitic import
# Add another account to the pool
infinitic add
# Switch automatically, in the background
infinitic service start`,
		SilenceUsage:               true,
		SilenceErrors:              true,
		SuggestionsMinimumDistance: 2,
		Args:                       subcommandArgs,
		RunE:                       func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
		PersistentPreRunE:          a.before,
	}
	pf := root.PersistentFlags()
	pf.Bool("non-interactive", false, "Fail instead of prompting")
	pf.Bool("no-input", false, "")
	_ = pf.MarkHidden("no-input")
	pf.Bool("no-color", false, "Turn off color")
	pf.BoolP("debug", "d", false, "Print diagnostics to stderr")
	pf.Bool("json", false, "")
	_ = pf.MarkHidden("json") // read by --help --json; commands that print JSON declare their own
	root.Version = version
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetHelpFunc(showHelp)
	root.SetFlagErrorFunc(flagError)
	root.AddGroup(
		&cobra.Group{ID: groupAccounts, Title: "Account Commands"},
		&cobra.Group{ID: groupCore, Title: "Core Commands"},
		&cobra.Group{ID: groupService, Title: "Background Service"},
	)
	root.AddCommand(a.add(), a.reauth(), a.importCmd(), a.rm(), a.status(), a.switchCmd(), a.watch(), a.service(), a.update(), a.version())
	a.setupHelpAndCompletion(root)
	return root
}

// before resolves the run's mode once: prompts, color and diagnostics. Commands read it from the
// printer and never look at the terminal or the environment themselves.
func (a *app) before(cmd *cobra.Command, _ []string) error {
	fs := cmd.Flags()
	if fs.Changed("json") && cmd.Annotations[annotationJSON] == "" {
		return usageErr("unknown_option", "Unknown option --json for "+cmd.CommandPath()+".",
			step("See its options", cmd.CommandPath()+" --help"))
	}
	nonInteractive, _ := fs.GetBool("non-interactive")
	noInput, _ := fs.GetBool("no-input")
	explicitOff := fs.Changed("non-interactive") && !nonInteractive
	a.p.NoInput = !ui.Interactive(nonInteractive || noInput, explicitOff, wantsJSON(cmd))
	if off, _ := fs.GetBool("no-color"); off {
		a.p.NoColor()
	}
	if on, _ := fs.GetBool("debug"); on {
		enableDebug()
	}
	if noInput {
		ui.NewPrinter(os.Stderr).Warn("--no-input is deprecated. Use --non-interactive.")
	}
	a.startUpdateCheck(cmd)
	return nil
}

// flagError turns pflag's parse errors into usage errors that name the command.
func flagError(c *cobra.Command, err error) error {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "unknown flag: "):
		msg = fmt.Sprintf("Unknown option %s for %s.", strings.TrimPrefix(msg, "unknown flag: "), c.CommandPath())
	case strings.HasPrefix(msg, "unknown shorthand flag: "):
		name, _, _ := strings.Cut(strings.TrimPrefix(msg, "unknown shorthand flag: "), " in ")
		msg = fmt.Sprintf("Unknown option -%s for %s.", strings.Trim(name, "'"), c.CommandPath())
	default:
		msg = sentence(msg)
	}
	return usageErr("invalid_arguments", msg, step("See its options", c.CommandPath()+" --help"))
}

// setupHelpAndCompletion replaces cobra's help and completion commands with ones that fail on an
// unknown topic or shell (by default both print help and exit 0). help also lists every command
// and searches them.
func (a *app) setupHelpAndCompletion(root *cobra.Command) {
	root.InitDefaultCompletionCmd()
	for _, c := range root.Commands() {
		if c.Name() == "completion" {
			c.Short = "Print the completion script for bash, zsh, fish, or powershell"
			c.Args = subcommandArgs
			c.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }
		}
	}
	var all bool
	help := &cobra.Command{
		Use:   "help [<COMMAND>]",
		Short: "Show help for a command, every command, or the ones matching a search",
		Long:  "help search <QUERY> matches command names, summaries, and examples, offline.",
		Example: `# Show help for a command
infinitic help service start
# List every command as JSON
infinitic help --all --json
# Find commands about logs
infinitic help search logs`,
		Annotations: map[string]string{annotationJSON: "true"},
		RunE: func(c *cobra.Command, args []string) error {
			asJSON, _ := c.Flags().GetBool("json")
			if len(args) > 0 && args[0] == "search" {
				if len(args) < 2 {
					return missingInput("missing_query", "Can't search without a query.", step("Search with", "infinitic help search <QUERY>"))
				}
				return helpSearch(c.OutOrStdout(), c.Root(), strings.Join(args[1:], " "), asJSON)
			}
			if all {
				return helpAll(c.OutOrStdout(), c.Root(), asJSON)
			}
			target, rest, err := c.Root().Find(args)
			if err != nil || len(rest) > 0 {
				return usageErr("unknown_command", fmt.Sprintf("Unknown help topic %q.", strings.Join(args, " ")),
					step("See the commands", "infinitic --help"))
			}
			if asJSON {
				return writeHelpJSON(c.OutOrStdout(), target)
			}
			return target.Help()
		},
	}
	help.Flags().BoolVar(&all, "all", false, "List every command")
	root.SetHelpCommand(help)
}

func (a *app) status() *cobra.Command {
	var o statusOptions
	var flagBars *quota.Bars
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "Show usage for each account",
		Long:    "● marks the active account and ○ a parked one. --json prints the document its $schema names.",
		GroupID: groupCore,
		Args:    noArgs,
		Example: `# Show live usage for every account
infinitic status
# Read the last saved usage as JSON, without the network
infinitic status --json --cached`,
	}
	cmd.Flags().BoolVar(&o.refresh, "refresh", false, "Read usage again even if the saved sample is fresh")
	cmd.Flags().BoolVar(&o.cached, "cached", false, "Show the saved state only, without the network, keychain, or lock")
	asJSON := jsonFlag(cmd, "Output the status as JSON")
	flagBars = barFlags(cmd.Flags())
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		o.json = *asJSON
		return cmdStatus(cmd.Context(), cmd, a.p, o, *flagBars)
	}
	return cmd
}

const newAccount = "\x00new" // see bestAccount

func (a *app) reauth() *cobra.Command {
	var fresh bool
	cmd := &cobra.Command{
		Use:   "reauth [<ACCOUNT>]",
		Short: "Renew an account's login, or add a new account",
		Long: `Opens an isolated claude session for /login and stores the login where its account lives.
Without an account, a terminal asks which one to renew (or a new one); --new, or no terminal,
takes a login as any account.`,
		GroupID:           groupAccounts,
		Args:              maxOneArg,
		ValidArgsFunction: completeAccounts,
		Annotations:       map[string]string{annotationArguments: accountArgDoc},
		Example: `# Renew an account's login
infinitic reauth <ACCOUNT>
# Log in as any account, without the question
infinitic reauth --new`,
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := first(args)
			if selector != "" && fresh {
				return usageErr("invalid_arguments", "Can't use an account with --new.",
					step("Renew that account", "infinitic reauth "+selector))
			}
			if selector == "" && !fresh && a.p.CanPrompt() {
				picked, err := a.pickReauthTarget()
				if err != nil {
					return err
				}
				if picked != newAccount {
					selector = picked
				}
			}
			return cmdReauth(cmd.Context(), a.p, selector)
		},
	}
	cmd.Flags().BoolVar(&fresh, "new", false, "Take a login as any account, without asking (same as add)")
	return cmd
}

// add is reauth without the question: no account logs in as a new one, an account renews that one.
func (a *app) add() *cobra.Command {
	return &cobra.Command{
		Use:   "add [<EMAIL>]",
		Short: "Add a Claude account to the pool",
		Long: `Opens an isolated claude session for /login. Without an email the login is taken as a new
account (same as reauth --new); with one it must be that account's login (same as reauth <ACCOUNT>).`,
		GroupID:           groupAccounts,
		Args:              maxOneArg,
		ValidArgsFunction: completeAccounts,
		Example: `# Add whichever account you log in as
infinitic add
# Add or renew a given account
infinitic add <EMAIL>`,
		RunE: func(cmd *cobra.Command, args []string) error { return cmdReauth(cmd.Context(), a.p, first(args)) },
	}
}

func (a *app) importCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "import",
		Short:   "Import the account Claude Code is logged in as",
		Long:    "Reads the login in ~/.claude, or the macOS keychain. Run it once, first.",
		GroupID: groupAccounts,
		Args:    noArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmdImport(cmd.Context(), a.p) },
	}
}

// Sentinel values a pick returns: a NUL cannot start an email or UUID, so they cannot collide
// with a selector. bestAccount means re-rank with fresh usage at switch time, so the switch can
// land on a different account than the one tagged "recommended".
const bestAccount = "\x00best"

func (a *app) switchCmd() *cobra.Command {
	var best bool
	var flagBars *quota.Bars
	cmd := &cobra.Command{
		Use:     "switch [<ACCOUNT>]",
		Aliases: []string{"use"},
		Short:   "Move the primary login to an account",
		Long: `Without an account, a terminal asks for one (the recommended one first); --best, or no terminal,
takes the best parked account under the bars.`,
		GroupID:           groupCore,
		Args:              maxOneArg,
		ValidArgsFunction: completeAccounts,
		Annotations:       map[string]string{annotationArguments: accountArgDoc},
		Example: `# Switch to a given account
infinitic switch <ACCOUNT>
# Switch to the recommended account, without asking
infinitic switch --best`,
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := first(args)
			if selector != "" && best {
				return usageErr("invalid_arguments", "Can't use an account with --best.",
					step("Switch to that account", "infinitic switch "+selector))
			}
			bars, _, err := barsFromSettings(cmd.Flags(), *flagBars)
			if err != nil {
				return err
			}
			if selector == "" && !best && a.p.CanPrompt() {
				picked, err := a.pickSwitchTarget(bars)
				if err != nil {
					return err
				}
				if picked != bestAccount {
					selector = picked
				}
			}
			return cmdSwitch(cmd.Context(), a.p, selector, bars)
		},
	}
	cmd.Flags().BoolVar(&best, "best", false, "Take the best parked account, without asking")
	flagBars = barFlags(cmd.Flags())
	return cmd
}

func (a *app) rm() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm [<ACCOUNT>]",
		Short: "Remove an account from the pool",
		Long: `Removes a parked account and deletes its stored login; adding it back takes a new /login.
It asks for confirmation on a terminal; elsewhere it needs --yes.`,
		GroupID:           groupAccounts,
		Args:              maxOneArg,
		ValidArgsFunction: completeAccounts,
		Annotations:       map[string]string{annotationArguments: accountArgDoc},
		Example: `# Remove an account, after confirming
infinitic rm <ACCOUNT>
# Remove an account in a script
infinitic rm <ACCOUNT> --yes`,
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := first(args)
			if selector == "" {
				if !a.p.CanPrompt() {
					return missingInput("missing_account", "Can't remove an account without its email in non-interactive mode.",
						step("Name the account", "infinitic rm <ACCOUNT> --yes"))
				}
				picked, err := a.pickRemoveTarget()
				if err != nil {
					return err
				}
				selector = picked
			}
			return a.remove(cmd.Context(), selector, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Remove without asking for confirmation")
	return cmd
}

// remove previews what removing an account deletes and asks before doing it; --yes is the consent
// a run without a terminal needs.
func (a *app) remove(ctx context.Context, selector string, yes bool) error {
	st, err := loadPoolState()
	if err != nil {
		return err
	}
	acc, err := resolveAccount(st, selector, "infinitic rm %s")
	if err != nil {
		return err
	}
	if acc.UUID == st.ActiveUUID {
		return activeRemoveErr(acc)
	}
	label := acc.Label()
	if !yes {
		if !a.p.CanPrompt() {
			e := missingInput("confirmation_required", fmt.Sprintf("Can't remove %s without confirmation in non-interactive mode.", label),
				step("Confirm with", "infinitic rm "+label+" --yes"))
			e.data = map[string]any{"effects": []string{"deletes the stored login of " + label}}
			return e
		}
		a.p.Facts([]ui.Fact{{Label: "Account", Value: label}, {Label: "Effect", Value: "Deletes its stored login. Adding it back takes a new /login."}})
		ok, err := a.ask(fmt.Sprintf("Remove %s from the pool?", label))
		if err != nil {
			return err
		}
		if !ok {
			return canceled("Canceled. Nothing changed.")
		}
	}
	return cmdRemove(ctx, a.p, label)
}

func (a *app) watch() *cobra.Command {
	var f watchFlags
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Switch accounts when usage reaches a bar",
		Long: `Checks the active account every interval and switches once it reaches a bar. Settings come
from ~/.config/infinitic/config.yml when it exists; options override it.`,
		GroupID: groupCore,
		Args:    noArgs,
		Example: `# Watch in this terminal
infinitic watch
# Run a single check and exit
infinitic watch --once`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmdWatch(cmd.Context(), a.p, cmd.Flags(), f)
		},
	}
	f = addWatchFlags(cmd.Flags())
	return cmd
}

func (a *app) service() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "service",
		Short:   "Run watch in the background (launchd, systemd)",
		GroupID: groupService,
		Args:    subcommandArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	var f watchFlags
	var overwrite bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Start or restart the background watch",
		Long: `Registers watch --config <FILE> with the service manager and (re)starts it. The file is the one
given with --config, else the one the running watch uses, else ~/.config/infinitic/config.yml;
a missing file is written from the options given and the defaults.

Without settings options the file is used as it is. When the options differ from an existing
file, a terminal asks whether to overwrite it: yes stores them in the file, no keeps the file and
applies them to this background watch only (the next start without them drops them).
--overwrite stores them without asking; without a terminal the answer is no.`,
		Args: noArgs,
		Example: `# Start the background watch, or restart it
infinitic service start
# Start it with a different 7-day bar and store it in the settings file
infinitic service start --weekly 95 --overwrite`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := a.manager()
			if err != nil {
				return err
			}
			return serviceStart(cmd.Context(), a.p, mgr, cmd.Flags(), f, overwrite, a.ask, a.wait)
		},
	}
	watchSet := pflag.NewFlagSet("start", pflag.ContinueOnError)
	f = addWatchFlags(watchSet)
	watchSet.Lookup("config").Usage = "Settings file (default: the one the running watch uses)"
	watchSet.Lookup("config").DefValue = ""
	for _, name := range []string{"interval", "session", "weekly", "config"} {
		start.Flags().AddFlag(watchSet.Lookup(name))
	}
	start.Flags().BoolVar(&overwrite, "overwrite", false, "Store the options in the settings file without asking")

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop and remove the background watch",
		Args:  noArgs,
		RunE: func(*cobra.Command, []string) error {
			mgr, err := a.manager()
			if err != nil {
				return err
			}
			if err := mgr.Uninstall(); err != nil {
				return err
			}
			a.p.Receipt("Stopped", "background watch")
			return nil
		},
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the background watch's state",
		Args:  noArgs,
	}
	asJSON := jsonFlag(status, "Output the service status as JSON")
	status.RunE = func(*cobra.Command, []string) error {
		mgr, err := a.manager()
		if err != nil {
			return err
		}
		return serviceStatus(a.p, mgr, *asJSON)
	}

	var limit int
	var follow bool
	logs := &cobra.Command{
		Use:   "logs",
		Short: "Show the background watch's logs",
		Args:  noArgs,
		Example: `# Show the last 50 lines
infinitic service logs
# Keep printing new lines
infinitic service logs --follow`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := a.manager()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("lines") {
				ui.NewPrinter(os.Stderr).Warn("--lines is deprecated. Use --limit.")
			}
			return serviceLogs(cmd.Context(), a.p, mgr, limit, follow)
		},
	}
	logs.Flags().IntVarP(&limit, "limit", "n", 50, "Number of lines to show")
	hint(logs.Flags(), "limit", "NUMBER")
	logs.Flags().IntVar(&limit, "lines", 50, "")
	_ = logs.Flags().MarkHidden("lines")
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing new lines")

	cmd.AddCommand(start, stop, status, logs)
	return cmd
}

func (a *app) version() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  noArgs,
	}
	asJSON := jsonFlag(cmd, "Output the version as JSON")
	cmd.Run = func(*cobra.Command, []string) {
		if *asJSON {
			_ = ui.WriteJSON(os.Stdout, map[string]string{"version": version})
			return
		}
		a.p.Block(version)
	}
	return cmd
}

// resolveAccount names the account selector means exactly. The error suggests the account a
// near miss meant, as the full command (format has one %s for it).
func resolveAccount(st *state.State, selector, format string) (*state.Account, error) {
	acc, err := st.Resolve(selector)
	var nf *state.NotFoundError
	if !errors.As(err, &nf) {
		return acc, err
	}
	e := failErr("account_not_found", fmt.Sprintf("Couldn't find account %s in the pool.", selector))
	if nf.Suggestion != "" {
		e.next = []ui.Step{step("Did you mean", fmt.Sprintf(format, nf.Suggestion))}
	} else {
		e.next = []ui.Step{step("See the accounts in the pool", "infinitic status --cached")}
	}
	e.err = err
	return nil, e
}

func activeRemoveErr(acc *state.Account) error {
	return failErr("conflict", fmt.Sprintf("Can't remove %s. It's the active login.", acc.Label()),
		step("Switch to another account first", "infinitic switch --best"))
}

// Account prompts (switch, reauth, rm) share one shape: rows "email  plan  tag" in aligned
// columns, a tag only where it changes the choice, the rows ordered so the likely pick comes
// first. Which tags matter depends on the command.

type promptRow struct {
	acc   *state.Account
	tag   string // styled; "" for none
	value string // what a pick returns
	order int    // lower first
}

// choices lays rows out as "email  plan  tag" in aligned columns, extra first. Within an order
// group ranked rows come first, best first; a row with no rank sorts last.
func choices(extra []ui.Choice, rows []promptRow, rank map[string]int) ([]ui.Choice, error) {
	width, plan := 0, 0
	for _, r := range rows {
		width, plan = max(width, len(r.acc.Label())), max(plan, len(r.acc.Plan))
	}
	pos := func(r promptRow) int {
		if n, ok := rank[r.acc.UUID]; ok {
			return n
		}
		return math.MaxInt
	}
	slices.SortStableFunc(rows, func(x, y promptRow) int {
		if c := cmp.Compare(x.order, y.order); c != 0 {
			return c
		}
		return cmp.Compare(pos(x), pos(y))
	})
	out := slices.Clone(extra)
	for _, r := range rows {
		parts := []string{fmt.Sprintf("%-*s", width, r.acc.Label()), fmt.Sprintf("%-*s", plan, r.acc.Plan), r.tag}
		out = append(out, ui.Choice{Label: strings.TrimRight(strings.Join(parts, "  "), " "), Value: r.value})
	}
	if len(out) == 0 {
		return nil, failErr("empty_pool", "No accounts in the pool.", step("Add one", "infinitic add"))
	}
	return out, nil
}

// ranking is rotator.Rank on the stored usage: position by UUID (1 = best) and the best UUID,
// "" if none. Unranked accounts are absent from the map.
func ranking(st *state.State, bars quota.Bars) (map[string]int, string) {
	ranked, _ := rotator.Rank(st, bars, time.Now())
	rank := map[string]int{}
	for i, a := range ranked {
		rank[a.UUID] = i + 1
	}
	if len(ranked) == 0 {
		return rank, ""
	}
	return rank, ranked[0].UUID
}

// pickSwitchTarget asks which account to switch to. The pick infinitic would make is first and
// tagged recommended; then the other usable ones best first, the active one, the exhausted
// ones, those needing reauth. Choosing the recommendation returns bestAccount, so the switch
// re-ranks with fresh usage.
func (a *app) pickSwitchTarget(bars quota.Bars) (string, error) {
	st, err := loadPoolState()
	if err != nil {
		return "", err
	}
	rank, best := ranking(st, bars)
	var rows []promptRow
	for _, acc := range st.Accounts {
		r := promptRow{acc: acc, value: acc.Label(), order: 2}
		switch {
		case acc.UUID == best:
			r.tag, r.value, r.order = ui.TagGood.Render("recommended"), bestAccount, 0
		case acc.NeedsReauth != "":
			r.tag, r.order = ui.TagBad.Render("needs reauth"), 4
		case acc.UUID == st.ActiveUUID:
			r.tag = "active now" // even when over a bar: that it is active matters more
		case bars.Over(acc.Usage):
			r.tag, r.order = ui.TagWarn.Render("exhausted"), 3
		case rank[acc.UUID] > 0:
			r.order = 1
		}
		rows = append(rows, r)
	}
	cs, err := choices(nil, rows, rank)
	if err != nil {
		return "", err
	}
	return a.p.Choose("Switch to which account?", cs)
}

// pickReauthTarget asks which account to log in as: a new account first, then the ones whose
// login is dead or about to expire (tagged), then the rest with the active one last.
func (a *app) pickReauthTarget() (string, error) {
	st, err := loadPoolState()
	if err != nil {
		return "", err
	}
	now := time.Now()
	var rows []promptRow
	for _, acc := range st.Accounts {
		r := promptRow{acc: acc, value: acc.Label(), order: 2}
		switch notice := acc.LoginNotice(now); {
		case acc.NeedsReauth != "":
			r.tag, r.order = ui.TagBad.Render("needs reauth"), 0
		case notice != "":
			r.tag, r.order = ui.TagWarn.Render(notice), 1
		case acc.UUID == st.ActiveUUID:
			r.tag, r.order = "active now", 3
		}
		rows = append(rows, r)
	}
	cs, err := choices([]ui.Choice{{Label: "a new account", Value: newAccount}}, rows, nil)
	if err != nil {
		return "", err
	}
	return a.p.Choose("Log in as which account?", cs)
}

// pickRemoveTarget asks which parked account to forget: plain "email  plan" rows in pool order,
// without the active account.
func (a *app) pickRemoveTarget() (string, error) {
	st, err := loadPoolState()
	if err != nil {
		return "", err
	}
	var rows []promptRow
	for _, acc := range st.Accounts {
		if acc.UUID != st.ActiveUUID {
			rows = append(rows, promptRow{acc: acc, value: acc.Label()})
		}
	}
	cs, err := choices(nil, rows, nil)
	if err != nil {
		return "", err
	}
	return a.p.Choose("Remove which account?", cs)
}

// No network or lock: completion must be instant.
func completeAccounts(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	st, err := loadPoolState()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, acc := range st.Accounts {
		out = append(out, acc.Label()+"\t"+acc.Plan)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// loadPoolState reads state.json without the lock, for prompts and completion.
func loadPoolState() (*state.State, error) { return state.Load(loadConfig().statePath()) }
