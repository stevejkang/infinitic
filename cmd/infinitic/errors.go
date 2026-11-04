package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/stevejkang/infinitic/internal/ui"
)

// An errClass decides the exit code: 1 for a failure, 2 for a call that has to change (a usage
// error or missing input), 130 for a cancel.
type errClass int

const (
	classFailure errClass = iota
	classUsage
	classCanceled
)

var exitCodes = map[errClass]int{classFailure: 1, classUsage: 2, classCanceled: 130}

// cliError is an error as the user sees it: a headline, detail lines, and what to do next. reason
// is the stable code JSON output carries; the headline's wording may change, the reason does not.
type cliError struct {
	class          errClass
	reason         string
	msg            string
	details        []string
	next           []ui.Step
	actionRequired bool // blocked before any effect; input or approval lets it run
	data           map[string]any
	err            error
}

func (e *cliError) Error() string { return e.msg }
func (e *cliError) Unwrap() error { return e.err }

func usageErr(reason, msg string, next ...ui.Step) *cliError {
	return &cliError{class: classUsage, reason: reason, msg: msg, next: next}
}

func failErr(reason, msg string, next ...ui.Step) *cliError {
	return &cliError{class: classFailure, reason: reason, msg: msg, next: next}
}

// missingInput is a usage error for input a prompt would have asked for.
func missingInput(reason, msg string, next ...ui.Step) *cliError {
	e := usageErr(reason, msg, next...)
	e.actionRequired = true
	return e
}

func canceled(msg string) *cliError {
	return &cliError{class: classCanceled, reason: "canceled", msg: msg, err: ui.ErrCancelled}
}

func step(instruction, command string) ui.Step {
	return ui.Step{Instruction: instruction, Command: command}
}

// asCLIError gives every error a class: cliErrors as they are, a cancel as exit 130, and any other
// error as a failure whose message is made a sentence.
func asCLIError(err error) *cliError {
	var ce *cliError
	if errors.As(err, &ce) {
		return ce
	}
	if errors.Is(err, ui.ErrCancelled) {
		return canceled("Canceled. Nothing changed.")
	}
	return &cliError{class: classFailure, reason: "error", msg: sentence(err.Error()), err: err}
}

// sentence capitalizes msg and ends it with a period, for errors written as fragments deeper down.
func sentence(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return msg
	}
	r := []rune(msg)
	r[0] = unicode.ToUpper(r[0])
	msg = string(r)
	if last := r[len(r)-1]; unicode.IsLetter(last) || unicode.IsDigit(last) || strings.ContainsRune(")\"'`", last) {
		msg += "."
	}
	return msg
}

// report prints err for a person, or as one line of JSON when the command was asked for --json,
// and returns the exit code.
func report(err error, asJSON bool) int {
	ce := asCLIError(err)
	if asJSON {
		writeJSONError(ce)
		return exitCodes[ce.class]
	}
	p := ui.NewPrinter(os.Stderr)
	if ce.class == classCanceled {
		p.Info(ce.msg)
	} else {
		p.Fail(ce.msg, ce.details, ce.next)
	}
	return exitCodes[ce.class]
}

type jsonStep struct {
	Command string   `json:"command"`
	Argv    []string `json:"argv"`
}

func writeJSONError(ce *cliError) {
	status := "error"
	if ce.actionRequired {
		status = "action_required"
	}
	doc := map[string]any{"status": status, "reason": ce.reason, "message": ce.msg}
	var next []jsonStep
	for _, s := range ce.next {
		if s.Command != "" {
			next = append(next, jsonStep{Command: s.Command, Argv: strings.Fields(s.Command)})
		}
	}
	if len(next) > 0 {
		doc["next"] = next
	}
	if ce.data != nil {
		doc["data"] = ce.data
	}
	data, _ := json.Marshal(doc)
	fmt.Fprintln(os.Stderr, string(data))
}

// wantsJSON reports whether cmd was run with --json and promises JSON output.
func wantsJSON(cmd *cobra.Command) bool {
	if cmd == nil || cmd.Annotations[annotationJSON] == "" {
		return false
	}
	on, _ := cmd.Flags().GetBool("json")
	return on
}

// Argument validators that fail as usage errors.

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageErr("invalid_arguments", fmt.Sprintf("Can't use %q. %s takes no arguments.", args[0], cmd.CommandPath()),
			step("See its usage", cmd.CommandPath()+" --help"))
	}
	return nil
}

func maxOneArg(cmd *cobra.Command, args []string) error {
	if len(args) > 1 {
		return usageErr("invalid_arguments", fmt.Sprintf("Can't use %q. %s takes at most one argument.", args[1], cmd.CommandPath()),
			step("See its usage", cmd.CommandPath()+" --help"))
	}
	return nil
}

// subcommandArgs rejects an unknown subcommand of a group, suggesting the closest one.
func subcommandArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	e := usageErr("unknown_command", fmt.Sprintf("Unknown command %s for %s.", args[0], cmd.CommandPath()))
	if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
		e.next = []ui.Step{step("Did you mean", cmd.CommandPath()+" "+s[0])}
	} else {
		e.next = []ui.Step{step("See the commands", cmd.CommandPath()+" --help")}
	}
	return e
}
