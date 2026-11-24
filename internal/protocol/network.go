package protocol

// Network profiles (KIN-30, KB KIN-A-22). The Control Plane delivers the runner's resolved network profiles with
// the command runner.config.update and asks for a preflight with network.test; the runner answers the test with
// runner.network.test.result (fire-and-forget: a lost result shows as a timed-out test that the Owner repeats).
//
// Proxy passwords travel only inside commands, over the authenticated TLS session: in the Control Plane's command
// log they are sealed with the installation key (bound to the runner and command id) and opened just before
// sending; the runner keeps them in a 0600 file and never logs them.
const (
	CommandConfigUpdate   = "runner.config.update" // body: ConfigUpdate
	CommandNetworkTest    = "network.test"         // body: NetworkTest
	TypeNetworkTestResult = "runner.network.test.result"
)

// Limits of network messages.
const (
	MaxNetworkScoped    = 64
	MaxNetworkEndpoints = 16
)

// NetworkProxy is an upstream proxy. On the wire Password is plaintext (inside TLS); in the Control Plane's
// command log only PasswordSealed is set.
type NetworkProxy struct {
	Scheme         string `json:"scheme"` // http | https | socks5
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Username       string `json:"username,omitempty"`
	Password       string `json:"password,omitempty"`
	PasswordSealed []byte `json:"passwordSealed,omitempty"`
}

// NetworkRule is an allowed destination: Host (exact or "*.suffix") or CIDR, with its ports.
type NetworkRule struct {
	Host  string `json:"host,omitempty"`
	CIDR  string `json:"cidr,omitempty"`
	Ports []int  `json:"ports"`
}

// NetworkProfile is a resolved (effective) network profile.
type NetworkProfile struct {
	ID            string        `json:"id,omitempty"`
	Name          string        `json:"name,omitempty"`
	Proxy         *NetworkProxy `json:"proxy,omitempty"`
	Enforcement   string        `json:"enforcement"` // off | monitor | strict
	DNS           string        `json:"dns"`         // system | proxy
	FailClosed    bool          `json:"failClosed"`
	ExpectedIPv4  string        `json:"expectedIpv4,omitempty"`
	ExpectedIPv6  string        `json:"expectedIpv6,omitempty"`
	Restricted    bool          `json:"restricted"` // Allow applies (and may be empty: nothing allowed)
	Allow         []NetworkRule `json:"allow,omitempty"`
	DiagnosticURL string        `json:"diagnosticUrl"`
}

// ScopedNetworkProfile applies to steps of Project (if set) run by the CLI Role (if set). The runner picks the
// entry matching (project, role), then (project, ""), then ("", role), else NetworkConfig.Default.
type ScopedNetworkProfile struct {
	Project string         `json:"project,omitempty"`
	Role    string         `json:"role,omitempty"`
	Profile NetworkProfile `json:"profile"`
}

// NetworkEndpoint is a required endpoint the preflight checks (AI CLI providers, GitHub, YouTrack).
type NetworkEndpoint struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// NetworkConfig is the runner's network configuration.
type NetworkConfig struct {
	Default   NetworkProfile         `json:"default"`
	Scoped    []ScopedNetworkProfile `json:"scoped,omitempty"`
	Endpoints []NetworkEndpoint      `json:"endpoints,omitempty"`
}

// ConfigUpdate is the body of runner.config.update. Revision grows with every change; a runner ignores an
// update older than the one it applied.
type ConfigUpdate struct {
	Revision int64         `json:"revision"`
	Network  NetworkConfig `json:"network"`
}

// NetworkTest is the body of network.test: run the preflight for Profile against Endpoints.
type NetworkTest struct {
	TestID    string            `json:"testId"`
	Profile   NetworkProfile    `json:"profile"`
	Endpoints []NetworkEndpoint `json:"endpoints"`
}

// NetworkCheck is one endpoint's reachability through the proxy. Code is a stable failure code, never an error
// text (which could echo a proxy URL).
type NetworkCheck struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	OK        bool   `json:"ok"`
	Code      string `json:"code,omitempty"`
	LatencyMs int64  `json:"latencyMs"`
}

// NetworkTestResult is the payload of runner.network.test.result.
type NetworkTestResult struct {
	TestID       string         `json:"testId"`
	OK           bool           `json:"ok"`
	Code         string         `json:"code,omitempty"`   // proxy_unreachable, proxy_auth_failed, diagnostic_failed, …
	Detail       string         `json:"detail,omitempty"` // short, credential-free
	ObservedIPv4 string         `json:"observedIpv4,omitempty"`
	ObservedIPv6 string         `json:"observedIpv6,omitempty"`
	Checks       []NetworkCheck `json:"checks,omitempty"`
	DurationMs   int64          `json:"durationMs"`
}
