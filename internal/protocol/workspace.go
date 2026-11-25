package protocol

// Workspaces (KIN-29, KB KIN-A-16/KIN-A-18). For an assignment with a Base the runner keeps a bare repository
// cache in repos/ and runs the attempt in its own git worktree under workspaces/, on the branch
// feature/<task>--<step> started from origin/<base>. It reports the worktree and the repository's native
// instruction files (workspace.prepared) before starting the process, and what the attempt changed
// (workspace.changes, the input of KIN-51 scope enforcement) before its terminal event.

// GitCredential is a short-lived git token for one repository. It is plaintext on the wire (inside the
// authenticated TLS session) and formats as [redacted], so it never reaches a log line.
type GitCredential struct {
	Token string `json:"token"`
}

// String redacts the token.
func (GitCredential) String() string { return "[redacted]" }

// GoString redacts the token.
func (GitCredential) GoString() string { return "[redacted]" }

// InstructionFile is one repository-native instruction file (AGENTS.md, CLAUDE.md, .claude/…, .codex/…): untrusted
// content the CLI reads natively. Only its path and hash are reported; a symlink is never followed (Symlink set,
// SHA256 is the hash of the link target text).
type InstructionFile struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Symlink bool   `json:"symlink,omitempty"`
}

// WorkspacePrepared is the body of workspace.prepared.
type WorkspacePrepared struct {
	Repository            string            `json:"repository"`
	Branch                string            `json:"branch"`
	Base                  string            `json:"base"`
	BaseSHA               string            `json:"baseSha"`
	Instructions          []InstructionFile `json:"instructions"`
	InstructionsTruncated bool              `json:"instructionsTruncated,omitempty"`
}

// ChangedFile is one path the attempt changed against the base: Status A (added), M (modified), D (deleted),
// T (type changed).
type ChangedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// InstructionChange is an instruction file whose hash changed during the attempt (empty: absent).
type InstructionChange struct {
	Path   string `json:"path"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// WorkspaceChanges is the body of workspace.changes: the branch head against the base (uncommitted work is
// committed to the branch first). Files holds at most what fits a message; FilesTotal counts all of them.
// An Interim report is sent while the process runs (KIN-51): the worktree against the base, untracked files
// included, nothing committed and no HeadSHA. The Control Plane checks every report against the step's approved
// scope (it never trusts a runner-side verdict); a Truncated report stops the step like a violation. An attempt
// that sent workspace.prepared must send a final (non-Interim) report before execution.completed: a completion
// without one stops the step too (KIN-78).
type WorkspaceChanges struct {
	Branch              string              `json:"branch"`
	BaseSHA             string              `json:"baseSha"`
	HeadSHA             string              `json:"headSha"`
	Files               []ChangedFile       `json:"files"`
	FilesTotal          int                 `json:"filesTotal"`
	Truncated           bool                `json:"truncated,omitempty"`
	InstructionsChanged []InstructionChange `json:"instructionsChanged,omitempty"`
	Interim             bool                `json:"interim,omitempty"`
}
