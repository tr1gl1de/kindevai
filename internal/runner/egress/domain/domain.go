// Package domain is the runner's egress model for strict egress (KIN-31, KB KIN-A-22): the nftables ruleset of an
// attempt's network namespace, the allowlist the local proxy adapter enforces, the leak tests of the preflight and
// their verdict, and whether this platform can enforce strict egress at all. Pure Go, no I/O.
package domain

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Enforcement modes of a network profile (KIN-30).
const (
	EnforcementOff     = "off"
	EnforcementMonitor = "monitor"
	EnforcementStrict  = "strict"
)

// DNS policies of a network profile (KIN-30).
const (
	DNSSystem = "system"
	DNSProxy  = "proxy"
)

// Policy is what the sandbox needs from a strict profile.
type Policy struct {
	Enforcement string
	DNS         string
	HasProxy    bool
}

// ErrNotStrict: the profile does not ask for strict egress (no sandbox is built).
var ErrNotStrict = errors.New("the network profile is not strict")

// ErrInvalidPolicy: a strict profile the sandbox cannot honour (no proxy, or DNS outside the proxy). KIN-30
// rejects such profiles; the runner refuses them again (fail-closed) instead of trusting it.
var ErrInvalidPolicy = errors.New("strict egress needs a proxy and DNS through the proxy")

// TableName is the nftables table of an attempt's namespace.
const TableName = "kindev_egress"

// Ruleset returns the nftables ruleset of an attempt's network namespace. The namespace has no interface but its
// own loopback (the proxy adapter is reached through a forwarder on it), so the rules are the second wall: every
// packet that does not stay on that loopback is rejected, for IPv4 and IPv6 alike, DNS and other UDP included.
// The loopback is the namespace's own, never the host's.
func Ruleset(p Policy) (string, error) {
	if p.Enforcement != EnforcementStrict {
		return "", ErrNotStrict
	}
	if !p.HasProxy || p.DNS != DNSProxy {
		return "", ErrInvalidPolicy
	}
	var b strings.Builder
	b.WriteString("table inet " + TableName + " {\n")
	b.WriteString("\tchain output {\n")
	b.WriteString("\t\ttype filter hook output priority 0; policy drop;\n")
	b.WriteString("\t\tmeta l4proto { tcp, udp } th dport 53 counter reject\n") // DNS only through the proxy
	b.WriteString("\t\toifname \"lo\" accept\n")
	b.WriteString("\t\tcounter reject\n")
	b.WriteString("\t}\n")
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority 0; policy drop;\n")
	b.WriteString("\t\tiifname \"lo\" accept\n")
	b.WriteString("\t}\n")
	b.WriteString("\tchain forward {\n")
	b.WriteString("\t\ttype filter hook forward priority 0; policy drop;\n")
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String(), nil
}

// Rule is an allowed destination of the proxy adapter: Host (exact, or "*.suffix" for its subdomains) or CIDR (for
// literal IP targets only: names are resolved by the proxy, never here), with its ports.
type Rule struct {
	Host  string
	CIDR  string
	Ports []int
}

// Allowlist limits the proxy adapter's destinations. When Restricted is false any destination is allowed (it
// still leaves only through the proxy); when true only the rules' (possibly none).
type Allowlist struct {
	Restricted bool
	Rules      []Rule
}

// Allows reports whether the adapter may open a tunnel to host:port. host is a name or a literal IP (IPv6 without
// brackets).
func (a Allowlist) Allows(host string, port int) bool {
	if port < 1 || port > 65535 || host == "" {
		return false
	}
	if !a.Restricted {
		return true
	}
	name := strings.TrimSuffix(strings.ToLower(host), ".")
	ip, ipErr := netip.ParseAddr(name)
	for _, r := range a.Rules {
		if !slices.Contains(r.Ports, port) {
			continue
		}
		switch {
		case r.CIDR != "":
			pfx, err := netip.ParsePrefix(r.CIDR)
			if err == nil && ipErr == nil && pfx.Contains(ip.Unmap()) {
				return true
			}
		case strings.HasPrefix(r.Host, "*."):
			if ipErr != nil && strings.HasSuffix(name, strings.ToLower(r.Host[1:])) {
				return true
			}
		case ipErr != nil && name == strings.ToLower(r.Host):
			return true
		}
	}
	return false
}

// Reserved reports destinations the proxy adapter never opens, whatever the allowlist: localhost names and
// loopback, link-local (cloud metadata), unspecified and multicast addresses. Through a proxy running on the host
// they would reach the host itself.
func Reserved(host string) bool {
	name := strings.TrimSuffix(strings.ToLower(host), ".")
	switch name {
	case "localhost", "localhost6", "ip6-localhost", "ip6-loopback", "metadata", "metadata.google.internal":
		return true
	}
	if strings.HasSuffix(name, ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(name)
	if err != nil {
		// A non-canonical IPv4 form ("127.1", "2130706433", "0x7f.1", "0177.0.0.1") that a resolver would still
		// turn into an address: never forwarded as a name.
		last := name[strings.LastIndexByte(name, '.')+1:]
		return last == "" || strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x")
	}
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast()
}

// Leak tests of the preflight: each must fail inside the sandbox.
const (
	LeakTCP4    = "direct_tcp_ipv4"   // a TCP connection to a public IPv4 address
	LeakTCP6    = "direct_tcp_ipv6"   // the same over IPv6
	LeakDNS     = "dns"               // a DNS query to the system's resolvers or a public one
	LeakNoProxy = "http_ignore_proxy" // an HTTP request by a tool that ignores HTTP_PROXY
	LeakUnix    = "unix_socket"       // a host Unix socket (Docker daemon, D-Bus, WSL interop)
	LeakVsock   = "vsock"             // an AF_VSOCK socket (the WSL2/Hyper-V host channel, outside any netns)
)

// LeakTests is every leak test, in report order.
var LeakTests = []string{LeakTCP4, LeakTCP6, LeakDNS, LeakNoProxy, LeakUnix, LeakVsock}

// Stable failure codes of a sandbox verdict.
const (
	CodeSetupFailed = "sandbox_setup_failed"
	CodeLeak        = "egress_leak"
	CodeProxy       = "proxy_check_failed"
	CodeUnsupported = "strict_egress_unsupported"
)

// Leak is the outcome of one leak test: Blocked is the only acceptable result.
type Leak struct {
	Test    string `json:"test"`
	Blocked bool   `json:"blocked"`
	Target  string `json:"target,omitempty"` // what was tried (an address or socket path; never a credential)
}

// Verdict is the result of a sandbox's preflight. Probe is the proxy/egress IP code (empty: passed, or not run in
// a self-test).
type Verdict struct {
	Setup string `json:"setup,omitempty"` // a setup failure (empty: the sandbox was built)
	Leaks []Leak `json:"leaks"`
	Probe string `json:"probe,omitempty"`
	// ProxyChecked: the proxy and egress IP were checked (false in a self-test).
	ProxyChecked bool `json:"proxyChecked"`
}

// Verified reports whether the sandbox may run an agent: built, every leak test run and blocked, and (unless
// selfTest) the proxy path and egress IP verified.
func (v Verdict) Verified(selfTest bool) error {
	if v.Setup != "" {
		return fmt.Errorf("%s: %s", CodeSetupFailed, v.Setup)
	}
	for _, t := range LeakTests {
		i := slices.IndexFunc(v.Leaks, func(l Leak) bool { return l.Test == t })
		if i < 0 {
			return fmt.Errorf("%s: leak test %s did not run", CodeLeak, t)
		}
		if !v.Leaks[i].Blocked {
			return fmt.Errorf("%s: %s reached %s", CodeLeak, t, v.Leaks[i].Target)
		}
	}
	if selfTest {
		return nil
	}
	if !v.ProxyChecked {
		return fmt.Errorf("%s: not run", CodeProxy)
	}
	if v.Probe != "" {
		return fmt.Errorf("%s: %s", CodeProxy, v.Probe)
	}
	return nil
}

// Platform is what the runner found about its host for strict egress.
type Platform struct {
	GOOS      string
	Namespace bool   // user, network and mount namespaces can be created by this user
	NFT       bool   // the nft binary is available
	SelfTest  string // "" when the self-test sandbox verified; else why not
}

// Support says whether strict egress can be enforced here, and why not. macOS has no equivalent of Linux
// namespaces: strict egress is not claimed there (post-MVP).
func Support(p Platform) (bool, string) {
	switch {
	case p.GOOS != "linux":
		return false, "unsupported on " + p.GOOS + " (strict egress needs Linux or WSL2 namespaces)"
	case !p.Namespace:
		return false, "user and network namespaces are not available to this user " +
			"(check kernel.unprivileged_userns_clone / kernel.apparmor_restrict_unprivileged_userns)"
	case !p.NFT:
		return false, "nft (nftables) is not installed"
	case p.SelfTest != "":
		return false, "the sandbox self-test failed: " + p.SelfTest
	}
	return true, ""
}

// HostPort splits a CONNECT authority ("host:port", "[v6]:port").
func HostPort(authority string) (string, int, error) {
	i := strings.LastIndexByte(authority, ':')
	if i <= 0 {
		return "", 0, errors.New("missing port")
	}
	host, ps := authority[:i], authority[i+1:]
	if strings.HasPrefix(host, "[") {
		if !strings.HasSuffix(host, "]") {
			return "", 0, errors.New("bad IPv6 literal")
		}
		host = host[1 : len(host)-1]
	} else if strings.Contains(host, ":") {
		return "", 0, errors.New("IPv6 literal without brackets")
	}
	port, err := strconv.Atoi(ps)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, errors.New("bad port")
	}
	if host == "" || strings.ContainsAny(host, " \t\r\n/@") {
		return "", 0, errors.New("bad host")
	}
	return host, port, nil
}
