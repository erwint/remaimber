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
// it fails the moment someone copies it.
func TestExamplesOnlyUseRealFlags(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			for _, line := range strings.Split(sub.Example, "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "remaimber "+sub.Name()) {
					continue
				}
				for _, tok := range strings.Fields(line) {
					if !strings.HasPrefix(tok, "--") {
						continue
					}
					name := strings.TrimPrefix(strings.SplitN(tok, "=", 2)[0], "--")
					if name == "" {
						continue
					}
					if sub.Flags().Lookup(name) == nil && sub.InheritedFlags().Lookup(name) == nil &&
						c.PersistentFlags().Lookup(name) == nil {
						t.Errorf("%s: example uses undefined flag --%s\n  %s", sub.CommandPath(), name, line)
					}
				}
			}
			walk(sub)
		}
	}
	walk(newRootCmd())
}
