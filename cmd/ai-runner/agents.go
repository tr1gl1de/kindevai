package main

import (
	"log/slog"
	"path/filepath"

	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/cliagent"
)

// cliAgents wires the AI CLI adapters (KIN-32/KIN-79): each CLI found on the runner's PATH (absolute paths only),
// run with the runner user's HOME, where the CLI keeps its local login. An assignment for a CLI the runner does not
// have fails with a reason.
func cliAgents(home string, lookPath func(string) (string, error), logger *slog.Logger) *cliagent.Agents {
	g := &cliagent.Agents{Adapters: map[string]*cliagent.Adapter{}}
	for _, d := range []cliagent.Dialect{cliagent.Claude{}, cliagent.Codex{}} {
		bin, err := lookPath(d.Name())
		if err != nil || !filepath.IsAbs(bin) || !filepath.IsAbs(home) {
			continue
		}
		g.Adapters[d.Name()] = &cliagent.Adapter{Dialect: d, Config: cliagent.Config{Binary: bin, Home: home}}
		logger.Info("AI CLI available", "agent", d.Name(), "path", bin)
	}
	return g
}
