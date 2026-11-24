package protocol

// Runner telemetry (KIN-20, KB KIN-A-18/KIN-A-26): after runner.authorized the runner reports its liveness,
// capabilities and disk metrics. All three are runner → server, fire-and-forget (no ACK). The Control Plane
// stamps them with its own clock; the runner's clock is never trusted. Unknown payload fields are ignored, so a
// newer runner can add fields without being disconnected by an older Control Plane.
const (
	TypeHeartbeat    = "runner.heartbeat"    // every HeartbeatIntervalSeconds
	TypeCapabilities = "runner.capabilities" // once per connection, after runner.authorized
	TypeStorage      = "runner.storage"      // after runner.authorized and every StorageIntervalSeconds
)

// Telemetry cadence. The Control Plane marks a runner UNREACHABLE after MissedHeartbeats missed heartbeats.
const (
	HeartbeatIntervalSeconds = 15
	MissedHeartbeats         = 3
	StorageIntervalSeconds   = 300
)

// Bounds of telemetry payloads; a payload beyond them closes the connection with CloseProtocolError.
const (
	MaxTools   = 16
	MaxVolumes = 8
)

// Heartbeat is the payload of runner.heartbeat. Running assignments and capacity arrive with the scheduler
// (KIN-23); until then the heartbeat carries only the runner's send time, informational.
type Heartbeat struct {
	SentAt string `json:"sentAt,omitempty"` // RFC 3339, the runner's clock
}

// Tool is a command-line tool the runner found on its PATH.
type Tool struct {
	Name    string `json:"name"`    // claude, codex, git, docker
	Version string `json:"version"` // first line of `<name> --version`, sanitized; may be empty
}

// Capabilities is the payload of runner.capabilities.
type Capabilities struct {
	OS           string `json:"os"`           // GOOS
	Arch         string `json:"arch"`         // GOARCH
	WSL          bool   `json:"wsl"`          // Linux under WSL2
	Tools        []Tool `json:"tools"`        // found tools only
	StrictEgress bool   `json:"strictEgress"` // the runner can enforce strict egress (KIN-30); false until then
}

// Volume roles of runner.storage.
const (
	VolumeWorkspaces  = "workspaces"   // the filesystem holding runner-data (repos, worktrees, sessions)
	VolumeWindowsHost = "windows_host" // WSL2: the Windows volume holding the distro's ext4.vhdx, when visible
	VolumeData        = "data"         // Control Plane: the filesystem holding its data directory (not on the wire)
)

// Volume is one filesystem's metrics (KIN-A-26 disk_total_bytes, disk_available_bytes, disk_free_bytes,
// mount_or_volume_id). Available is what an unprivileged process can still write; used% derives from it.
type Volume struct {
	ID             string `json:"id"`   // mount point of the filesystem, e.g. "/" or "/mnt/c"
	Role           string `json:"role"` // VolumeWorkspaces or VolumeWindowsHost
	Path           string `json:"path"` // the directory measured
	TotalBytes     int64  `json:"totalBytes"`
	AvailableBytes int64  `json:"availableBytes"`
	FreeBytes      int64  `json:"freeBytes"`
}

// Storage is the payload of runner.storage.
type Storage struct {
	Volumes []Volume `json:"volumes"`
}
