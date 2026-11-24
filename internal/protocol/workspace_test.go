package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestWorkspaceEventsAreChannelEvents(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{EventWorkspacePrepared, EventWorkspaceChanges} {
		e := Event{Epoch: "e", Seq: 1, Step: "s", Fence: 1, AttemptSeq: 1, Kind: kind}
		if err := e.Validate(); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
		if TerminalEvent(kind) {
			t.Errorf("%s must not end the attempt", kind)
		}
	}
}

// The token travels inside the TLS session only: it is on the wire, but never in a formatted value (logs).
func TestGitCredentialIsRedactedWhenFormatted(t *testing.T) {
	t.Parallel()
	a := Assign{Step: "s", Base: "dev", Credential: &GitCredential{Token: "ghs_secret"}}
	for _, s := range []string{fmt.Sprint(a), fmt.Sprintf("%+v", a), fmt.Sprintf("%#v", a), fmt.Sprint(a.Credential)} {
		if strings.Contains(s, "ghs_secret") {
			t.Fatalf("token formatted in clear: %s", s)
		}
	}
	b, err := json.Marshal(a)
	if err != nil || !strings.Contains(string(b), `"credential":{"token":"ghs_secret"}`) {
		t.Fatalf("wire form: %s %v", b, err)
	}
	var back Assign
	if err := json.Unmarshal(b, &back); err != nil || back.Credential == nil || back.Credential.Token != "ghs_secret" {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}
