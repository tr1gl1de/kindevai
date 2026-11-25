package main

import (
	"errors"
	"log/slog"
	"testing"
)

func TestCLIAgentsUsesTheCLIsOnThePath(t *testing.T) {
	t.Parallel()
	look := func(name string) (string, error) {
		switch name {
		case "claude":
			return "/usr/local/bin/claude", nil
		case "codex":
			return "codex", nil // relative (PATH contains "."): never used
		}
		return "", errors.New("not found")
	}
	g := cliAgents("/home/runner", look, slog.New(slog.DiscardHandler))
	if a := g.Adapters["claude"]; a == nil || a.Config.Binary != "/usr/local/bin/claude" || a.Config.Home != "/home/runner" {
		t.Fatalf("claude adapter %+v", a)
	}
	if _, ok := g.Adapters["codex"]; ok {
		t.Fatal("a relative CLI path was configured")
	}
}
