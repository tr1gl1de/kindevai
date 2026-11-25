package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestRuleset(t *testing.T) {
	t.Parallel()
	strict := Policy{Enforcement: EnforcementStrict, DNS: DNSProxy, HasProxy: true}
	tests := []struct {
		name string
		p    Policy
		err  error
	}{
		{"strict with proxy and remote DNS", strict, nil},
		{"monitor builds no sandbox", Policy{Enforcement: EnforcementMonitor, DNS: DNSProxy, HasProxy: true}, ErrNotStrict},
		{"off builds no sandbox", Policy{Enforcement: EnforcementOff}, ErrNotStrict},
		{"strict without proxy is refused", Policy{Enforcement: EnforcementStrict, DNS: DNSProxy}, ErrInvalidPolicy},
		{"strict with system DNS is refused", Policy{Enforcement: EnforcementStrict, DNS: DNSSystem, HasProxy: true}, ErrInvalidPolicy},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rs, err := Ruleset(tc.p)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if tc.err != nil {
				if rs != "" {
					t.Fatalf("ruleset on error: %q", rs)
				}
				return
			}
			for _, want := range []string{
				"table inet " + TableName, // inet: IPv4 and IPv6 alike
				"hook output priority 0; policy drop;",
				"th dport 53 counter reject", // DNS
				"oifname \"lo\" accept",      // only the namespace's own loopback
				"hook input priority 0; policy drop;",
				"hook forward priority 0; policy drop;",
			} {
				if !strings.Contains(rs, want) {
					t.Errorf("ruleset lacks %q:\n%s", want, rs)
				}
			}
			// DNS is rejected before the loopback is accepted.
			if strings.Index(rs, "dport 53") > strings.Index(rs, "oifname \"lo\" accept") {
				t.Errorf("DNS rule after the loopback rule:\n%s", rs)
			}
			if strings.Contains(rs, "ip daddr") || strings.Contains(rs, "masquerade") {
				t.Errorf("ruleset opens a destination:\n%s", rs)
			}
		})
	}
}

func TestAllowlist(t *testing.T) {
	t.Parallel()
	rules := []Rule{
		{Host: "api.anthropic.com", Ports: []int{443}},
		{Host: "*.github.com", Ports: []int{443, 22}},
		{CIDR: "140.82.112.0/20", Ports: []int{443}},
		{CIDR: "2001:db8::/32", Ports: []int{443}},
	}
	tests := []struct {
		name string
		a    Allowlist
		host string
		port int
		want bool
	}{
		{"unrestricted allows any host", Allowlist{}, "example.org", 80, true},
		{"unrestricted still checks the port", Allowlist{}, "example.org", 0, false},
		{"empty host", Allowlist{}, "", 443, false},
		{"restricted and empty allows nothing", Allowlist{Restricted: true}, "api.anthropic.com", 443, false},
		{"exact host", Allowlist{true, rules}, "api.anthropic.com", 443, true},
		{"exact host is case-insensitive, trailing dot", Allowlist{true, rules}, "API.Anthropic.com.", 443, true},
		{"exact host on another port", Allowlist{true, rules}, "api.anthropic.com", 80, false},
		{"exact host does not cover a subdomain", Allowlist{true, rules}, "x.api.anthropic.com", 443, false},
		{"wildcard covers a subdomain", Allowlist{true, rules}, "codeload.github.com", 22, true},
		{"wildcard does not cover the apex", Allowlist{true, rules}, "github.com", 443, false},
		{"wildcard does not cover a lookalike", Allowlist{true, rules}, "evilgithub.com", 443, false},
		{"CIDR covers a literal IP", Allowlist{true, rules}, "140.82.113.3", 443, true},
		{"CIDR does not cover an IP outside", Allowlist{true, rules}, "140.82.128.1", 443, false},
		{"CIDR covers an IPv6 literal", Allowlist{true, rules}, "2001:db8::1", 443, true},
		{"CIDR covers a v4-mapped literal", Allowlist{true, rules}, "::ffff:140.82.113.3", 443, true},
		{"a name is never matched to a CIDR", Allowlist{true, []Rule{{CIDR: "0.0.0.0/1", Ports: []int{443}}}}, "example.org", 443, false},
		{"an IP is never matched to a host rule", Allowlist{true, []Rule{{Host: "1.2.3.4", Ports: []int{443}}}}, "1.2.3.4", 443, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.a.Allows(tc.host, tc.port); got != tc.want {
				t.Fatalf("Allows(%q, %d) = %v, want %v", tc.host, tc.port, got, tc.want)
			}
		})
	}
}

func TestReserved(t *testing.T) {
	t.Parallel()
	for _, h := range []string{"localhost", "LOCALHOST.", "db.localhost", "127.0.0.1", "127.8.9.1", "::1",
		"::ffff:127.0.0.1", "169.254.169.254", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1",
		"127.1", "2130706433", "0x7f.1", "0177.0.0.1", "localhost6", "ip6-localhost", "metadata.google.internal"} {
		if !Reserved(h) {
			t.Errorf("Reserved(%q) = false", h)
		}
	}
	for _, h := range []string{"api.github.com", "10.1.2.3", "140.82.112.3", "2001:db8::1", "localhost.example.com", "x0.example.com",
		"1password.com"} {
		if Reserved(h) {
			t.Errorf("Reserved(%q) = true", h)
		}
	}
}

func allBlocked() []Leak {
	var ls []Leak
	for _, t := range LeakTests {
		ls = append(ls, Leak{Test: t, Blocked: true})
	}
	return ls
}

func TestVerdict(t *testing.T) {
	t.Parallel()
	leaked := allBlocked()
	leaked[2] = Leak{Test: LeakDNS, Blocked: false, Target: "1.1.1.1:53"}
	tests := []struct {
		name     string
		v        Verdict
		selfTest bool
		code     string // "" = verified
	}{
		{"all blocked, proxy verified", Verdict{Leaks: allBlocked(), ProxyChecked: true}, false, ""},
		{"self-test needs no proxy", Verdict{Leaks: allBlocked()}, true, ""},
		{"setup failed", Verdict{Setup: "nft failed", Leaks: allBlocked(), ProxyChecked: true}, false, CodeSetupFailed},
		{"a leak", Verdict{Leaks: leaked, ProxyChecked: true}, false, CodeLeak},
		{"a leak fails the self-test too", Verdict{Leaks: leaked}, true, CodeLeak},
		{"a missing leak test", Verdict{Leaks: allBlocked()[1:], ProxyChecked: true}, false, CodeLeak},
		{"proxy not checked", Verdict{Leaks: allBlocked()}, false, CodeProxy},
		{"egress IP mismatch", Verdict{Leaks: allBlocked(), ProxyChecked: true, Probe: "egress_ip_mismatch"}, false, CodeProxy},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.v.Verified(tc.selfTest)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("Verified = %v", err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tc.code) {
				t.Fatalf("Verified = %v, want code %s", err, tc.code)
			}
		})
	}
}

func TestSupport(t *testing.T) {
	t.Parallel()
	ok := Platform{GOOS: "linux", Namespace: true, NFT: true}
	tests := []struct {
		name string
		p    Platform
		want bool
		why  string
	}{
		{"linux with namespaces and nft", ok, true, ""},
		{"macOS is unsupported", Platform{GOOS: "darwin", Namespace: true, NFT: true}, false, "unsupported on darwin"},
		{"no namespaces", Platform{GOOS: "linux", NFT: true}, false, "namespaces"},
		{"no nft", Platform{GOOS: "linux", Namespace: true}, false, "nft"},
		{"self-test failed", Platform{GOOS: "linux", Namespace: true, NFT: true, SelfTest: "egress_leak"}, false, "self-test"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, why := Support(tc.p)
			if got != tc.want || !strings.Contains(why, tc.why) {
				t.Fatalf("Support = %v, %q", got, why)
			}
		})
	}
}

func TestHostPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		host string
		port int
		bad  bool
	}{
		{in: "api.github.com:443", host: "api.github.com", port: 443},
		{in: "[2001:db8::1]:8443", host: "2001:db8::1", port: 8443},
		{in: "1.2.3.4:22", host: "1.2.3.4", port: 22},
		{in: "api.github.com", bad: true},
		{in: "2001:db8::1:443", bad: true},
		{in: "host:0", bad: true},
		{in: "host:70000", bad: true},
		{in: ":443", bad: true},
		{in: "user@host:443", bad: true},
		{in: "[::1:443", bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			h, p, err := HostPort(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("HostPort(%q) = %q, %d; want an error", tc.in, h, p)
				}
				return
			}
			if err != nil || h != tc.host || p != tc.port {
				t.Fatalf("HostPort(%q) = %q, %d, %v", tc.in, h, p, err)
			}
		})
	}
}
