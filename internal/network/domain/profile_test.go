package domain_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/network/domain"
)

func proxy(t *testing.T, s string) *domain.Proxy {
	t.Helper()
	p, _, err := domain.ParseProxyURL(s)
	if err != nil {
		t.Fatal(err)
	}
	return &p
}

func profile(t *testing.T, in domain.ProfileInput) domain.Profile {
	t.Helper()
	if in.Name == "" {
		in.Name = "p"
	}
	p, err := domain.NewProfile("id-"+in.Name, in)
	if err != nil {
		t.Fatalf("NewProfile(%+v): %v", in, err)
	}
	return p
}

func code(err error) string {
	var ve *domain.ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

func TestNewProfileStrictImpliesFailClosedAndProxyDNS(t *testing.T) {
	t.Parallel()
	p := profile(t, domain.ProfileInput{Name: "strict", Proxy: proxy(t, "socks5://proxy.example:1080"),
		Enforcement: domain.EnforcementStrict, DNS: domain.DNSSystem, FailClosed: false})
	if !p.FailClosed || p.DNS != domain.DNSProxy {
		t.Fatalf("strict profile = %+v, want fail-closed and proxy DNS", p)
	}
	_, err := domain.NewProfile("x", domain.ProfileInput{Name: "no proxy", Enforcement: domain.EnforcementStrict})
	if code(err) != "strict_requires_proxy" {
		t.Fatalf("strict without proxy: %v", err)
	}
}

func TestNewProfileValidation(t *testing.T) {
	t.Parallel()
	px := func() *domain.Proxy { return proxy(t, "http://proxy.example:3128") }
	cases := []struct {
		name string
		in   domain.ProfileInput
		code string
	}{
		{"empty name", domain.ProfileInput{Name: " "}, "required"},
		{"unknown enforcement", domain.ProfileInput{Name: "a", Enforcement: "loose"}, "enforcement"},
		{"unknown dns", domain.ProfileInput{Name: "a", DNS: "doh"}, "dns_policy"},
		{"ipv4 field with ipv6", domain.ProfileInput{Name: "a", Proxy: px(), ExpectedIPv4: "2001:db8::1"}, "ip_family"},
		{"ipv6 field with ipv4", domain.ProfileInput{Name: "a", Proxy: px(), ExpectedIPv6: "203.0.113.7"}, "ip_family"},
		{"private expected ip", domain.ProfileInput{Name: "a", Proxy: px(), ExpectedIPv4: "10.0.0.1"}, "ip_not_public"},
		{"loopback expected ip", domain.ProfileInput{Name: "a", Proxy: px(), ExpectedIPv4: "127.0.0.1"}, "ip_not_public"},
		{"garbage ip", domain.ProfileInput{Name: "a", ExpectedIPv4: "1.2.3"}, "ip_format"},
		{"http diagnostic", domain.ProfileInput{Name: "a", DiagnosticURL: "http://ip.example"}, "diagnostic_url"},
		{"diagnostic with credentials", domain.ProfileInput{Name: "a", DiagnosticURL: "https://u:p@ip.example"}, "diagnostic_url"},
		{"allow any host", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{Host: "*", Ports: []int{443}}}}, "allow_host"},
		{"allow a whole TLD", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{Host: "*.com", Ports: []int{443}}}}, "allow_host"},
		{"allow localhost", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{Host: "localhost", Ports: []int{443}}}}, "allow_host"},
		{"allow the internet", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{CIDR: "0.0.0.0/0", Ports: []int{443}}}}, "allow_cidr_too_wide"},
		{"allow all ipv6", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{CIDR: "::/0", Ports: []int{443}}}}, "allow_cidr_too_wide"},
		{"allow loopback", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{CIDR: "127.0.0.0/8", Ports: []int{443}}}}, "allow_cidr_forbidden"},
		{"allow cloud metadata", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{CIDR: "169.254.169.254/32", Ports: []int{80}}}}, "allow_cidr_forbidden"},
		{"allow a wide net containing metadata", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{CIDR: "160.0.0.0/4", Ports: []int{80}}}}, "allow_cidr_too_wide"},
		{"allow ipv6 link-local", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{CIDR: "fe80::/64", Ports: []int{443}}}}, "allow_cidr_forbidden"},
		{"host and cidr", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{Host: "a.example", CIDR: "203.0.113.0/24", Ports: []int{443}}}}, "allow_rule"},
		{"no ports", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{Host: "a.example"}}}, "allow_ports"},
		{"port zero", domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{{Host: "a.example", Ports: []int{0}}}}, "allow_ports"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := domain.NewProfile("x", c.in); code(err) != c.code {
				t.Fatalf("NewProfile = %v, want code %s", err, c.code)
			}
		})
	}
}

func TestNewProfileNormalizesAllowlist(t *testing.T) {
	t.Parallel()
	p := profile(t, domain.ProfileInput{Name: "a", Allow: []domain.RuleInput{
		{Host: "API.Anthropic.com", Ports: []int{443, 443}},
		{Host: "*.GitHub.com", Ports: []int{22, 443}},
		{CIDR: "203.0.113.9/24", Ports: []int{443}},
	}})
	want := []domain.Rule{
		{Host: "api.anthropic.com", Ports: []int{443}},
		{Host: "*.github.com", Ports: []int{22, 443}},
		{CIDR: "203.0.113.0/24", Ports: []int{443}},
	}
	if !reflect.DeepEqual(p.Allow, want) {
		t.Fatalf("Allow = %+v, want %+v", p.Allow, want)
	}
}

func TestResolveStrictCannotBeLiftedBelow(t *testing.T) {
	t.Parallel()
	system := profile(t, domain.ProfileInput{Name: "system", Proxy: proxy(t, "socks5://proxy.example:1080"),
		Enforcement: domain.EnforcementStrict, ExpectedIPv4: "203.0.113.7"})
	runner := profile(t, domain.ProfileInput{Name: "runner", Enforcement: domain.EnforcementOff, DNS: domain.DNSSystem})
	project := profile(t, domain.ProfileInput{Name: "project", Enforcement: domain.EnforcementMonitor,
		Proxy: proxy(t, "http://other.example:3128"), ExpectedIPv4: "198.51.100.1", DiagnosticURL: "https://ip.evil.example"})
	role := profile(t, domain.ProfileInput{Name: "role", Enforcement: domain.EnforcementOff})

	e := domain.Resolve([]domain.Layer{
		{Level: domain.LevelSystem, Profile: &system},
		{Level: domain.LevelRunner, Profile: &runner},
		{Level: domain.LevelProject, Profile: &project},
		{Level: domain.LevelRole, Profile: &role},
	})
	if e.Enforcement != domain.EnforcementStrict || !e.FailClosed || e.DNS != domain.DNSProxy {
		t.Fatalf("effective = %+v, want strict, fail-closed, proxy DNS kept from the system level", e.Profile)
	}
	if e.StrictFrom != domain.LevelSystem {
		t.Fatalf("StrictFrom = %q, want system", e.StrictFrom)
	}
	// Under strict from a wider level, a narrower one cannot redirect egress to another proxy.
	if e.Proxy == nil || e.Proxy.Host != "proxy.example" || e.ProxyProfileID != system.ID {
		t.Fatalf("proxy = %+v from %q, want the system's", e.Proxy, e.ProxyProfileID)
	}
	if e.ExpectedIPv4 != "203.0.113.7" {
		t.Fatalf("expected IPv4 = %q, want inherited", e.ExpectedIPv4)
	}
	if e.DiagnosticURL != domain.DefaultDiagnosticURL {
		t.Fatalf("diagnostic URL = %q, want the default", e.DiagnosticURL)
	}
}

func TestResolveAllowlistOnlyNarrows(t *testing.T) {
	t.Parallel()
	system := profile(t, domain.ProfileInput{Name: "system", Allow: []domain.RuleInput{
		{Host: "*.github.com", Ports: []int{443}},
		{Host: "api.anthropic.com", Ports: []int{443}},
		{CIDR: "203.0.113.0/24", Ports: []int{443, 8443}},
	}})
	project := profile(t, domain.ProfileInput{Name: "project", Allow: []domain.RuleInput{
		{Host: "api.github.com", Ports: []int{443, 22}}, // 22 is not allowed above: dropped
		{Host: "evil.example", Ports: []int{443}},       // not covered: dropped
		{Host: "*.anthropic.com", Ports: []int{443}},    // wider than the exact host above: dropped
		{CIDR: "203.0.113.128/25", Ports: []int{8443}},  // inside: kept
		{CIDR: "198.51.100.0/24", Ports: []int{443}},    // outside: dropped
	}})
	e := domain.Resolve([]domain.Layer{{Level: domain.LevelSystem, Profile: &system}, {Level: domain.LevelProject, Profile: &project}})
	want := []domain.Rule{{Host: "api.github.com", Ports: []int{443}}, {CIDR: "203.0.113.128/25", Ports: []int{8443}}}
	if !reflect.DeepEqual(e.Allow, want) {
		t.Fatalf("Allow = %+v, want %+v", e.Allow, want)
	}
	if !e.Restricted {
		t.Fatal("an allowlist that narrowed to rules must stay restricted")
	}

	// A lower level with no allowlist inherits the upper one; an empty intersection allows nothing (not everything).
	none := profile(t, domain.ProfileInput{Name: "none", Allow: []domain.RuleInput{{Host: "evil.example", Ports: []int{443}}}})
	e = domain.Resolve([]domain.Layer{{Level: domain.LevelSystem, Profile: &system}, {Level: domain.LevelRole, Profile: &none}})
	if len(e.Allow) != 0 || !e.Restricted {
		t.Fatalf("effective = %+v, want restricted with no destinations", e.Profile)
	}
}

func TestResolveWithoutProfiles(t *testing.T) {
	t.Parallel()
	e := domain.Resolve([]domain.Layer{{Level: domain.LevelSystem}, {Level: domain.LevelRunner}})
	if e.Enforcement != domain.EnforcementOff || e.Proxy != nil || e.Restricted {
		t.Fatalf("effective = %+v, want direct, unenforced", e.Profile)
	}
}

func TestNewAssignment(t *testing.T) {
	t.Parallel()
	ok := []struct {
		level  domain.Level
		target string
	}{{domain.LevelSystem, ""}, {domain.LevelRunner, "r1"}, {domain.LevelProject, "p1"}, {domain.LevelRole, "claude"}}
	for _, c := range ok {
		if _, err := domain.NewScope(string(c.level), c.target); err != nil {
			t.Errorf("NewScope(%s, %q): %v", c.level, c.target, err)
		}
	}
	bad := []struct{ level, target string }{{"system", "x"}, {"runner", ""}, {"role", "Claude Code"}, {"team", "a"}}
	for _, c := range bad {
		if _, err := domain.NewScope(c.level, c.target); err == nil {
			t.Errorf("NewScope(%s, %q) accepted", c.level, c.target)
		}
	}
}

func TestResolveNarrowerLevelMayChangeProxyWithoutStrict(t *testing.T) {
	t.Parallel()
	system := profile(t, domain.ProfileInput{Name: "system", Proxy: proxy(t, "socks5://proxy.example:1080"), Enforcement: domain.EnforcementMonitor})
	project := profile(t, domain.ProfileInput{Name: "project", Proxy: proxy(t, "http://other.example:3128"), ExpectedIPv4: "198.51.100.1"})
	e := domain.Resolve([]domain.Layer{{Level: domain.LevelSystem, Profile: &system}, {Level: domain.LevelProject, Profile: &project}})
	if e.Proxy.Host != "other.example" || e.ExpectedIPv4 != "198.51.100.1" || e.Enforcement != domain.EnforcementMonitor {
		t.Fatalf("effective = %+v", e.Profile)
	}
}
