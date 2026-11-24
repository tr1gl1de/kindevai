package domain

import (
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Enforcement is how strictly a profile's egress is enforced. Ordered: off < monitor < strict.
type Enforcement string

const (
	// EnforcementOff: the profile only configures the proxy for tools that honour it; nothing is checked.
	EnforcementOff Enforcement = "off"
	// EnforcementMonitor: the proxy and expected IP are checked before a step (preflight), but traffic is not
	// confined; a failed check is reported.
	EnforcementMonitor Enforcement = "monitor"
	// EnforcementStrict: all traffic of the CLI and its children must leave through the proxy, confined by the
	// runner's sandbox (KIN-31); fail-closed, remote DNS only. Runners that cannot enforce it get no such steps.
	EnforcementStrict Enforcement = "strict"
)

func (e Enforcement) rank() int {
	switch e {
	case EnforcementMonitor:
		return 1
	case EnforcementStrict:
		return 2
	}
	return 0
}

// DNSPolicy is where host names are resolved. Ordered: system < proxy.
type DNSPolicy string

const (
	DNSSystem DNSPolicy = "system" // the runner's resolver
	DNSProxy  DNSPolicy = "proxy"  // only the proxy resolves (SOCKS5 remote DNS / HTTP CONNECT by name); no direct DNS
)

func (d DNSPolicy) rank() int {
	if d == DNSProxy {
		return 1
	}
	return 0
}

// DefaultDiagnosticURL is the trusted public endpoint that echoes the caller's IP address in plain text, used to
// observe the egress IP when a profile does not name its own (Owner-configurable per profile; KIN-30 decision).
const DefaultDiagnosticURL = "https://api64.ipify.org"

// Rule is one allowed destination: exactly one of Host (an exact name or "*.suffix") and CIDR, with its ports.
type Rule struct {
	Host  string `json:"host,omitempty"`
	CIDR  string `json:"cidr,omitempty"`
	Ports []int  `json:"ports"`
}

// Limits of a profile.
const (
	MaxNameLen  = 64
	MaxRules    = 64
	MaxRulePort = 16
)

// Profile is a network profile: the data a runner needs to confine and check a CLI's egress.
type Profile struct {
	ID           string
	Name         string
	Proxy        *Proxy // nil: direct connection
	Enforcement  Enforcement
	DNS          DNSPolicy
	FailClosed   bool   // no direct fallback when the proxy or the checks fail (always true for strict)
	ExpectedIPv4 string // public egress IPv4 the diagnostic endpoint must report ("" = not checked)
	ExpectedIPv6 string
	Allow        []Rule // empty: no destination restriction beyond the proxy
	// DiagnosticURL is the https endpoint returning the observed egress IP ("" = DefaultDiagnosticURL).
	DiagnosticURL string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RuleInput is an allowlist rule as entered.
type RuleInput struct {
	Host  string `json:"host,omitempty"`
	CIDR  string `json:"cidr,omitempty"`
	Ports []int  `json:"ports"`
}

// ProfileInput is a profile as entered by the Owner (the proxy already parsed with ParseProxyURL or NewProxy).
type ProfileInput struct {
	Name          string
	Proxy         *Proxy
	Enforcement   Enforcement
	DNS           DNSPolicy
	FailClosed    bool
	ExpectedIPv4  string
	ExpectedIPv6  string
	Allow         []RuleInput
	DiagnosticURL string
}

// NewProfile validates and normalizes a profile. Strict implies fail-closed and proxy-only DNS and requires a
// proxy. Allowlist rules may not open everything (bare "*", a top-level wildcard, 0.0.0.0/0, ::/0, prefixes wider
// than /8 or /16) nor loopback, link-local (cloud metadata) or multicast ranges.
func NewProfile(id string, in ProfileInput) (Profile, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Profile{}, invalid("name", "required")
	}
	if utf8.RuneCountInString(name) > MaxNameLen || strings.ContainsFunc(name, unicode.IsControl) {
		return Profile{}, invalid("name", "name_format")
	}
	p := Profile{ID: id, Name: name, Proxy: in.Proxy, Enforcement: in.Enforcement, DNS: in.DNS, FailClosed: in.FailClosed}
	if p.Enforcement == "" {
		p.Enforcement = EnforcementOff
	}
	switch p.Enforcement {
	case EnforcementOff, EnforcementMonitor, EnforcementStrict:
	default:
		return Profile{}, invalid("enforcement", "enforcement")
	}
	if p.DNS == "" {
		p.DNS = DNSSystem
	}
	if p.DNS != DNSSystem && p.DNS != DNSProxy {
		return Profile{}, invalid("dns", "dns_policy")
	}
	if p.Proxy != nil {
		px, err := NewProxy(string(p.Proxy.Scheme), p.Proxy.Host, p.Proxy.Port, p.Proxy.Username)
		if err != nil {
			return Profile{}, err
		}
		p.Proxy = &px
	}
	if p.Enforcement == EnforcementStrict {
		if p.Proxy == nil {
			return Profile{}, invalid("proxy", "strict_requires_proxy")
		}
		p.FailClosed, p.DNS = true, DNSProxy
	}
	var err error
	if p.ExpectedIPv4, err = publicIP("expectedIpv4", in.ExpectedIPv4, true); err != nil {
		return Profile{}, err
	}
	if p.ExpectedIPv6, err = publicIP("expectedIpv6", in.ExpectedIPv6, false); err != nil {
		return Profile{}, err
	}
	if p.DiagnosticURL, err = diagnosticURL(in.DiagnosticURL); err != nil {
		return Profile{}, err
	}
	if len(in.Allow) > MaxRules {
		return Profile{}, invalid("allow", "allow_too_many")
	}
	for _, r := range in.Allow {
		rule, err := newRule(r)
		if err != nil {
			return Profile{}, err
		}
		p.Allow = append(p.Allow, rule)
	}
	return p, nil
}

func publicIP(field, s string, v4 bool) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return "", invalid(field, "ip_format")
	}
	if a.Is4In6() {
		a = a.Unmap()
	}
	if a.Is4() != v4 {
		return "", invalid(field, "ip_family")
	}
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return "", invalid(field, "ip_not_public")
	}
	return a.String(), nil
}

func diagnosticURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil || len(s) > 512 || strings.ContainsFunc(s, unicode.IsControl) || u.Scheme != "https" ||
		u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", invalid("diagnosticUrl", "diagnostic_url")
	}
	if _, ok := normalizeHost(u.Hostname()); !ok {
		return "", invalid("diagnosticUrl", "diagnostic_url")
	}
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

// forbiddenRanges may never be allowed destinations: they reach the runner host itself or cloud metadata.
var forbiddenRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func newRule(in RuleInput) (Rule, error) {
	host, cidr := strings.TrimSpace(in.Host), strings.TrimSpace(in.CIDR)
	var r Rule
	switch {
	case (host == "") == (cidr == ""):
		return Rule{}, invalid("allow", "allow_rule")
	case host != "":
		h, ok := allowedHost(host)
		if !ok {
			return Rule{}, invalid("allow", "allow_host")
		}
		r.Host = h
	default:
		pfx, err := netip.ParsePrefix(cidr)
		if err != nil || pfx.Addr().Zone() != "" {
			return Rule{}, invalid("allow", "allow_cidr")
		}
		if pfx.Addr().Is4In6() {
			pfx = netip.PrefixFrom(pfx.Addr().Unmap(), pfx.Bits()-96)
		}
		pfx = pfx.Masked()
		if (pfx.Addr().Is4() && pfx.Bits() < 8) || (pfx.Addr().Is6() && pfx.Bits() < 16) {
			return Rule{}, invalid("allow", "allow_cidr_too_wide")
		}
		for _, f := range forbiddenRanges {
			if f.Overlaps(pfx) {
				return Rule{}, invalid("allow", "allow_cidr_forbidden")
			}
		}
		r.CIDR = pfx.String()
	}
	if len(in.Ports) == 0 || len(in.Ports) > MaxRulePort {
		return Rule{}, invalid("allow", "allow_ports")
	}
	for _, p := range in.Ports {
		if p < 1 || p > 65535 {
			return Rule{}, invalid("allow", "allow_ports")
		}
		r.Ports = append(r.Ports, p)
	}
	slices.Sort(r.Ports)
	r.Ports = slices.Compact(r.Ports)
	return r, nil
}

// allowedHost accepts "a.b" style names and "*.a.b" wildcards (at least two labels under the wildcard), never an
// IP literal (use a CIDR) or a single-label name such as localhost.
func allowedHost(s string) (string, bool) {
	s = strings.ToLower(s)
	wild := strings.HasPrefix(s, "*.")
	name := strings.TrimPrefix(s, "*.")
	if _, err := netip.ParseAddr(name); err == nil {
		return "", false
	}
	h, ok := normalizeHost(name)
	if !ok || strings.Count(h, ".") < 1 || strings.HasSuffix(h, ".localhost") {
		return "", false
	}
	if wild {
		return "*." + h, true
	}
	return h, true
}

// Level is a level of the profile hierarchy, from the widest to the most specific.
type Level string

const (
	LevelSystem  Level = "system"
	LevelRunner  Level = "runner"
	LevelProject Level = "project"
	LevelRole    Level = "role" // a CLI role: claude, codex, zcode, …
)

// Levels lists the hierarchy in resolution order.
var Levels = []Level{LevelSystem, LevelRunner, LevelProject, LevelRole}

// Scope is where a profile is assigned: the level and its target (empty for system, a runner id, a project id or
// a CLI role name).
type Scope struct {
	Level  Level
	Target string
}

var roleRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// NewScope validates a scope.
func NewScope(level, target string) (Scope, error) {
	target = strings.TrimSpace(target)
	switch Level(level) {
	case LevelSystem:
		if target != "" {
			return Scope{}, invalid("target", "target_format")
		}
	case LevelRunner, LevelProject:
		if target == "" || len(target) > 128 || strings.ContainsFunc(target, func(r rune) bool { return unicode.IsControl(r) || r == ' ' }) {
			return Scope{}, invalid("target", "target_format")
		}
	case LevelRole:
		if !roleRE.MatchString(target) {
			return Scope{}, invalid("target", "role_format")
		}
	default:
		return Scope{}, invalid("level", "level")
	}
	return Scope{Level: Level(level), Target: target}, nil
}

// Assignment binds a profile to a scope.
type Assignment struct {
	Scope     Scope
	ProfileID string
	UpdatedAt time.Time
}

// Layer is one level's assigned profile (nil when nothing is assigned there).
type Layer struct {
	Level   Level
	Profile *Profile
}

// Effective is the profile that applies to a runner, project and CLI role.
type Effective struct {
	Profile
	// ProxyProfileID is the profile whose proxy (and password) applies.
	ProxyProfileID string
	// StrictFrom is the widest level that demanded strict egress ("" when not strict); a narrower level cannot
	// lift it.
	StrictFrom Level
	// Restricted: an allowlist applies (Allow may then be empty: nothing is allowed).
	Restricted bool
	// Sources are the levels that contributed a profile, widest first.
	Sources []Level
}

// Resolve merges layers from the widest to the most specific. A narrower level may replace the proxy, the
// expected IPs and the diagnostic endpoint, but never relax security: enforcement, DNS policy and fail-closed
// only tighten, and its allowlist only narrows what the wider levels allow (rules not covered by them are
// dropped, ports are intersected). Strict egress set at a wider level therefore cannot be lifted below.
func Resolve(layers []Layer) Effective {
	e := Effective{Profile: Profile{Enforcement: EnforcementOff, DNS: DNSSystem}}
	for _, l := range layers {
		p := l.Profile
		if p == nil {
			continue
		}
		e.Sources = append(e.Sources, l.Level)
		e.ID, e.Name = p.ID, p.Name
		if p.Proxy != nil {
			px := *p.Proxy
			e.Proxy, e.ProxyProfileID = &px, p.ID
		}
		if p.Enforcement.rank() > e.Enforcement.rank() {
			e.Enforcement = p.Enforcement
		}
		if p.Enforcement == EnforcementStrict && e.StrictFrom == "" {
			e.StrictFrom = l.Level
		}
		if p.DNS.rank() > e.DNS.rank() {
			e.DNS = p.DNS
		}
		e.FailClosed = e.FailClosed || p.FailClosed
		if p.ExpectedIPv4 != "" {
			e.ExpectedIPv4 = p.ExpectedIPv4
		}
		if p.ExpectedIPv6 != "" {
			e.ExpectedIPv6 = p.ExpectedIPv6
		}
		if p.DiagnosticURL != "" {
			e.DiagnosticURL = p.DiagnosticURL
		}
		if len(p.Allow) > 0 {
			if e.Restricted {
				e.Allow = narrow(e.Allow, p.Allow)
			} else {
				e.Allow = slices.Clone(p.Allow)
			}
			e.Restricted = true
		}
	}
	if e.DiagnosticURL == "" {
		e.DiagnosticURL = DefaultDiagnosticURL
	}
	return e
}

// narrow keeps the lower rules covered by an upper rule, with the ports both allow.
func narrow(upper, lower []Rule) []Rule {
	out := []Rule{}
	for _, l := range lower {
		var ports []int
		for _, u := range upper {
			if !covers(u, l) {
				continue
			}
			for _, p := range l.Ports {
				if slices.Contains(u.Ports, p) {
					ports = append(ports, p)
				}
			}
		}
		if len(ports) == 0 {
			continue
		}
		slices.Sort(ports)
		out = append(out, Rule{Host: l.Host, CIDR: l.CIDR, Ports: slices.Compact(ports)})
	}
	return out
}

func covers(u, l Rule) bool {
	switch {
	case u.Host != "" && l.Host != "":
		if !strings.HasPrefix(u.Host, "*.") {
			return u.Host == l.Host
		}
		suffix := u.Host[1:] // ".example.com"
		name := strings.TrimPrefix(l.Host, "*")
		if strings.HasPrefix(l.Host, "*.") {
			return name == suffix || strings.HasSuffix(name, suffix)
		}
		return strings.HasSuffix(l.Host, suffix)
	case u.CIDR != "" && l.CIDR != "":
		up, err1 := netip.ParsePrefix(u.CIDR)
		lp, err2 := netip.ParsePrefix(l.CIDR)
		return err1 == nil && err2 == nil && up.Bits() <= lp.Bits() && up.Contains(lp.Addr())
	}
	return false
}

// Endpoint is a required endpoint Test Network checks through the proxy.
type Endpoint struct {
	Name string
	URL  string
}

// RequiredEndpoints are the endpoints a runner must reach: the AI CLI providers, GitHub and, when configured, the
// installation's YouTrack.
func RequiredEndpoints(youTrackURL string) []Endpoint {
	eps := []Endpoint{
		{Name: "anthropic", URL: "https://api.anthropic.com"},
		{Name: "openai", URL: "https://api.openai.com"},
		{Name: "github", URL: "https://github.com"},
		{Name: "github_api", URL: "https://api.github.com"},
	}
	if youTrackURL != "" {
		eps = append(eps, Endpoint{Name: "youtrack", URL: youTrackURL})
	}
	return eps
}
