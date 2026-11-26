package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/configstore"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/datadir"
)

// The runner routes network commands to the egress agent and keeps everything else on the executor path.
func TestNetworkCommandsRouted(t *testing.T) {
	t.Parallel()
	d, err := datadir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var other []string
	cmds := withNetwork(networkAgent(d, slog.New(slog.DiscardHandler)), func(_ context.Context, c protocol.Command) error {
		other = append(other, c.Kind)
		return nil
	})
	body, _ := json.Marshal(protocol.ConfigUpdate{Revision: 1, Network: protocol.NetworkConfig{Default: protocol.NetworkProfile{Name: "corp"}}})
	if err := cmds(t.Context(), protocol.Command{ID: "c1", Kind: protocol.CommandConfigUpdate, Body: body}); err != nil {
		t.Fatal(err)
	}
	if err := cmds(t.Context(), protocol.Command{ID: "c2", Kind: protocol.CommandCancel}); err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0] != protocol.CommandCancel {
		t.Fatalf("other commands %v", other)
	}
	cu, ok, err := configstore.File{Path: networkConfigPath(d)}.Load()
	if !ok || err != nil || cu.Network.Default.Name != "corp" {
		t.Fatalf("stored %+v %v %v", cu, ok, err)
	}
}

// The local report never prints proxy credentials.
func TestPrintNetworkResultHidesCredentials(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	printNetworkResult(&b, protocol.NetworkProfile{Name: "corp", Enforcement: "strict", Proxy: &protocol.NetworkProxy{
		Scheme: "socks5", Host: "p.example", Port: 1080, Username: "alice", Password: "hunter2"}},
		protocol.NetworkTestResult{Code: "proxy_auth_failed", Checks: []protocol.NetworkCheck{{Name: "github", Host: "github.com", Code: "timeout"}}})
	out := b.String()
	if strings.Contains(out, "hunter2") || strings.Contains(out, "alice") || !strings.Contains(out, "FAILED proxy_auth_failed") {
		t.Fatalf("report:\n%s", out)
	}
}
