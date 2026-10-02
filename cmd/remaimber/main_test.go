package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Every command a user can reach must show at least one worked example. Flag
// lists say what exists; examples say what to type, and this is the only part of
// the help that survives being skimmed.
func TestEveryCommandHasAnExample(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			// Hidden commands are hook plumbing, invoked by Claude Code rather
			// than typed; "help" and "completion" are cobra's own.
			if !sub.Hidden && sub.Name() != "help" && sub.Name() != "completion" {
				if strings.TrimSpace(sub.Example) == "" {
					t.Errorf("command %q has no Example", sub.CommandPath())
				}
				walk(sub)
			}
		}
	}
	root := newRootCmd()
	if strings.TrimSpace(root.Example) == "" {
		t.Error("root command has no Example")
	}
	walk(root)
}

// An example that names a flag the command does not define is worse than none:
// it fails the moment someone copies it. Each line is checked against the
// command it actually runs, so a parent's examples of its subcommands (sync
// pull, sync push) are held to those subcommands' flags.
func TestExamplesOnlyUseRealFlags(t *testing.T) {
	root := newRootCmd()
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, line := range strings.Split(c.Example, "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) < 2 || fields[0] != "remaimber" {
				continue
			}
			target, _, err := root.Find(fields[1:])
			if err != nil || target == root {
				continue
			}
			for _, tok := range fields {
				if !strings.HasPrefix(tok, "--") {
					continue
				}
				name := strings.TrimPrefix(strings.SplitN(tok, "=", 2)[0], "--")
				if name == "" {
					continue
				}
				if target.Flags().Lookup(name) == nil && target.InheritedFlags().Lookup(name) == nil {
					t.Errorf("%s: example uses undefined flag --%s\n  %s", target.CommandPath(), name, line)
				}
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}
