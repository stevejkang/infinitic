package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stevejkang/infinitic/internal/state"
	"github.com/stevejkang/infinitic/internal/ui"
)

// Tests run without a terminal, so every prompt must fall back to its flag or argument.
func TestCommandsNeverPromptWithoutATerminal(t *testing.T) {
	t.Setenv("INFINITIC_HOME", t.TempDir())
	devnull, _ := os.Open(os.DevNull)
	t.Cleanup(func() { devnull.Close() })
	for args, want := range map[string]string{
		"rm":                          "Can't remove an account without its email in non-interactive mode",
		"rm --no-input":               "Can't remove an account without its email in non-interactive mode",
		"rm --non-interactive":        "Can't remove an account without its email in non-interactive mode",
		"switch b@example.com --best": "Can't use an account with --best",
		"rm a b":                      "takes at most one argument",
		"status extra":                "takes no arguments",
	} {
		err := run(context.Background(), ui.NewPrinter(devnull), strings.Fields(args))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("infinitic %s: err %v, want %q", args, err, want)
		}
	}
}

func TestPromptsAreRefusedWithNoInput(t *testing.T) {
	p := ui.NewPrinter(os.Stderr)
	p.NoInput = true
	if _, err := p.Confirm("?"); !errors.Is(err, ui.ErrNoAnswer) {
		t.Fatalf("Confirm: %v", err)
	}
	if _, err := p.Choose("?", []ui.Choice{{Label: "x", Value: "x"}}); !errors.Is(err, ui.ErrNoAnswer) {
		t.Fatalf("Choose: %v", err)
	}
}

func TestPromptRowsKeepTheLikelyPickFirst(t *testing.T) {
	acc := func(email string) *state.Account {
		return &state.Account{UUID: email, Email: email, Plan: "Team Premium"}
	}
	rows := []promptRow{
		{acc: acc("late@x"), value: "late", order: 1},
		{acc: acc("unranked@x"), value: "unranked", order: 1},
		{acc: acc("best@x"), value: "best", order: 0, tag: "recommended"},
		{acc: acc("dead@x"), value: "dead", order: 4, tag: "needs reauth"},
		{acc: acc("early@x"), value: "early", order: 1},
	}
	got, err := choices([]ui.Choice{{Label: "a new account", Value: "new"}}, rows, map[string]int{"best@x": 1, "early@x": 2, "late@x": 3})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range got {
		order = append(order, c.Value)
	}
	want := []string{"new", "best", "early", "late", "unranked", "dead"}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Fatalf("order %v, want %v", order, want)
	}
	if got[1].Label != "best@x      Team Premium  recommended" || got[2].Label != "early@x     Team Premium" {
		t.Fatalf("labels not aligned: %q / %q", got[1].Label, got[2].Label)
	}
}
