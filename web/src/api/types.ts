// Shared API request/response shapes — the single source for the web UI. They mirror the Go JSON types in
// internal/server, internal/platform/httpx, internal/identity/adapters/httpapi and internal/projects/adapters/httpapi.

export interface Health {
  status: "ok";
  version: string;
}

/** Every API error. `field` is set for validation errors. */
export interface ApiErrorBody {
  error: string;
  message: string;
  field?: string;
}

export interface SetupStatus {
  setupRequired: boolean;
}

export interface SetupRequest {
  token: string;
  login: string;
  password: string;
  platformName: string;
}

export type PlatformRole = "owner" | "user";

export interface Me {
  id: string;
  login: string;
  role: PlatformRole;
  /** Signed in with a temporary password: only the password change is allowed. */
  mustChangePassword: boolean;
}

/** An account as listed for the Owner. */
export interface Account extends Me {
  disabled: boolean;
  createdAt: string;
}

export interface CreateUserRequest {
  login: string;
}

/** A server-generated password, shown to the Owner once. */
export interface TemporaryPasswordResult {
  user?: Account;
  temporaryPassword: string;
}

export interface ChangePasswordRequest {
  currentPassword: string;
  newPassword: string;
}

export interface LoginRequest {
  login: string;
  password: string;
}

export interface LoginResult {
  user: Me;
}

export type ProjectRole = "project_admin" | "member";

export interface Project {
  id: string;
  slug: string;
  name: string;
  /** "owner" for the platform Owner, otherwise the caller's project role. */
  myRole: "owner" | ProjectRole;
  createdAt: string;
}

export interface CreateProjectRequest {
  name: string;
  slug: string;
}

export interface Member {
  userId: string;
  login: string;
  role: ProjectRole;
}

export interface SetMemberRequest {
  role: ProjectRole;
}

export type InvitationState = "pending" | "used" | "revoked" | "expired";

export interface Invitation {
  id: string;
  projectId: string;
  /** Empty when the project was deleted. */
  projectName: string;
  role: ProjectRole;
  state: InvitationState;
  createdAt: string;
  expiresAt: string;
}

export interface CreateInvitationRequest {
  projectId: string;
  role: ProjectRole;
  /** 1–168; omitted = 72. */
  ttlHours?: number;
}

/** The token is shown to the Owner once; the invite link carries it in the URL fragment. */
export interface CreateInvitationResult {
  invitation: Invitation;
  token: string;
}

export interface InvitationPreview {
  projectName: string;
  role: ProjectRole;
  expiresAt: string;
}

export interface AcceptInvitationRequest {
  token: string;
  login: string;
  password: string;
  /** Raw Telegram initData when accepted inside the Mini App: links that Telegram account to the new one. */
  initData?: string;
}

export interface AuditActor {
  type: "user" | "system" | "runner";
  id: string;
  /** Login (or name) at the time of the event. */
  label: string;
}

/** One append-only audit record; values that look like secrets arrive as "[redacted]". */
export interface AuditEvent {
  id: number;
  at: string;
  actor: AuditActor;
  /** "<namespace>.<action>", e.g. "invitation.accepted". */
  kind: string;
  projectId?: string;
  /** "<type>:<id>" of what was acted on. */
  subject?: string;
  details?: Record<string, string>;
}

/** GET /api/audit: newest first; next is the `before` cursor of the following page, null on the last one. */
export interface AuditPage {
  events: AuditEvent[];
  next: string | null;
}

/** Query of GET /api/audit; empty fields do not filter. kind is a full kind or a namespace ("invitation"). */
export interface AuditFilter {
  actor?: string;
  project?: string;
  kind?: string;
  /** RFC 3339, inclusive. */
  from?: string;
  /** RFC 3339, exclusive. */
  to?: string;
}

export type RunnerStatus = "pending" | "approved" | "disabled" | "revoked";
export type EnrollmentTokenState = "pending" | "used" | "revoked" | "expired";

/** A one-time enrollment token as listed for the Owner (never the token itself). */
export interface EnrollmentToken {
  id: string;
  state: EnrollmentTokenState;
  createdAt: string;
  expiresAt: string;
  runnerId?: string;
}

/** POST /api/runners/enrollment-tokens: the token is shown to the Owner once. */
export interface CreateEnrollmentTokenResult {
  enrollmentToken: EnrollmentToken;
  token: string;
}

/** An enrolled runner machine. The fingerprint is OpenSSH-style ("SHA256:…") of its Ed25519 public key. */
export interface Runner {
  id: string;
  status: RunnerStatus;
  fingerprint: string;
  publicKey: string;
  /** Reported by the machine; informational, not proof of identity. */
  hostname: string;
  os: string;
  arch: string;
  version: string;
  enrolledAt: string;
  approvedAt: string | null;
  disabledAt: string | null;
  revokedAt: string | null;
  lastSeenAt: string | null;
  projects: { id: string; name: string }[];
  /** KIN-20: liveness from heartbeats (unreachable after 3 missed) and the runner's last capability report. */
  availability: RunnerAvailability;
  lastHeartbeatAt: string | null;
  capabilities: RunnerCapabilities | null;
}

export type RunnerAvailability = "never_connected" | "online" | "unreachable";

/** Self-reported by the runner; informational. */
export interface RunnerCapabilities {
  os: string;
  arch: string;
  wsl: boolean;
  strictEgress: boolean;
  tools: { name: string; version: string }[];
  reportedAt: string;
}

// Storage Overview (GET /api/storage/overview, KIN-20; metrics per KB KIN-A-26).

export type StorageLevel = "ok" | "warning" | "high" | "critical";
export type VolumeRole = "workspaces" | "windows_host" | "data";

/** Fill-up forecast from the median daily growth; days are null unless state is "ok" (0 = already reached). */
export interface StorageForecast {
  state: "insufficient_data" | "not_needed" | "ok";
  windowDays: number;
  growthBytesPerDay: number;
  daysTo85: number | null;
  daysTo90: number | null;
}

export interface StorageVolume {
  role: VolumeRole;
  mountOrVolumeId: string;
  path: string;
  totalBytes: number;
  availableBytes: number;
  freeBytes: number;
  usedPercent: number;
  level: StorageLevel;
  diskHeavyAllowed: boolean;
  /** The machine is offline or the report is older than 15 minutes. */
  stale: boolean;
  lastReportedAt: string;
  forecast7d: StorageForecast;
  forecast30d: StorageForecast;
}

export interface StorageMachine {
  id: string;
  kind: "control_plane" | "runner";
  /** The runner's hostname; empty for the Control Plane. */
  name: string;
  status?: RunnerStatus;
  availability: RunnerAvailability;
  lastHeartbeatAt: string | null;
  diskHeavyAllowed: boolean;
  volumes: StorageVolume[];
}

export interface StorageOverview {
  generatedAt: string;
  thresholds: { warningPercent: number; highPercent: number; criticalPercent: number; minHeadroomBytes: number };
  machines: StorageMachine[];
}

/** Body of approve and PUT projects: an explicit, non-empty list. */
export interface RunnerProjectsRequest {
  projectIds: string[];
}

// Telegram Mini App sign-in and account linking (internal/identity/adapters/httpapi/telegram.go).

export interface TelegramInitDataRequest {
  initData: string;
}

export interface TelegramSettings {
  configured: boolean;
  botUsername: string;
  appShortName: string;
  /** The bot token is write-only: only whether it is stored. */
  tokenSet: boolean;
  updatedAt: string | null;
}

export interface TelegramSettingsRequest {
  botUsername: string;
  appShortName: string;
  /** Empty keeps the stored token. */
  botToken: string;
}

export interface TelegramIdentity {
  telegramId: string;
  username: string;
  linkedAt: string;
}

export type TelegramLinkState = "awaiting_telegram" | "awaiting_code" | "linked" | "cancelled" | "expired";

export interface TelegramLinkRequest {
  state: TelegramLinkState;
  expiresAt: string;
  telegramName?: string;
  /** Only in the response that created the request. */
  url?: string;
}

export interface TelegramLinkStatus {
  configured: boolean;
  identity: TelegramIdentity | null;
  request: TelegramLinkRequest | null;
}

export interface TelegramLinkPreview {
  login: string;
  expiresAt: string;
  /** Only from confirm: the code to enter on the website. */
  code?: string;
}

/** A stored credential as the API shows it: never the value, only whether it is set and when it changed. */
export interface SecretMeta {
  set: boolean;
  updatedAt: string | null;
}

export type CheckStatus = "none" | "pending" | "ok" | "failed";

/** The last "Test connection" run; it runs asynchronously, poll while pending. */
export interface ConnectionCheck {
  status: CheckStatus;
  /** Failure code: unauthorized | not_found | unreachable | redirected | unexpected_status | bad_response | bad_url. */
  code?: string;
  /** On success the account login; on failure a short detail such as "HTTP 502". */
  detail?: string;
  requestedAt: string | null;
  checkedAt: string | null;
}

export interface YouTrackConnection {
  baseUrl: string;
  token: SecretMeta;
  updatedAt: string;
  check: ConnectionCheck;
}

/** The installation's GitHub App (KIN-28); key and webhook secret are write-only. */
export interface GitHubApp {
  appId: number;
  privateKey: SecretMeta;
  webhookSecret: SecretMeta;
  updatedAt: string;
  /** "Test" authenticates as the App; on success detail is the App's slug. */
  check: ConnectionCheck;
  lastDeliveryAt: string | null;
  lastEvent?: string;
}

/** PUT /api/integrations/github: empty privateKey / webhookSecret keep the stored ones. */
export interface GitHubRequest {
  appId: string;
  privateKey: string;
  webhookSecret: string;
}

/** GET /api/integrations (Owner only). */
export interface Integrations {
  youtrack: YouTrackConnection | null;
  github: GitHubApp | null;
}

/** PUT /api/integrations/youtrack: an empty token keeps the stored one (write-only). */
export interface YouTrackRequest {
  baseUrl: string;
  token: string;
}

export interface ProjectRepository {
  id: string;
  owner: string;
  name: string;
  fullName: string;
  integrationBranch: string;
  /** Never written by automation (main, master). */
  protectedBranches: string[];
  createdAt: string;
  updatedAt: string;
  /** What the GitHub App last read about the repository. */
  github: RepositoryGitHub;
}

export interface RepositoryGitHub {
  installed: boolean;
  defaultBranch: string;
  protection: "unknown" | "protected" | "unprotected";
  /** Existing main/master without a ruleset or branch protection. */
  unprotected: string[];
  /** The last read from GitHub; failure codes as in ConnectionCheck plus not_installed. */
  sync: ConnectionCheck;
}

/** GET /api/projects/{id}/integrations (Owner only); youtrackProject is "" when unbound. */
export interface ProjectIntegrations {
  projectId: string;
  youtrackProject: string;
  repositories: ProjectRepository[];
}

export interface RepositoryRequest {
  owner: string;
  name: string;
  integrationBranch: string;
}

// Approval Center (KIN-24)
export type RiskLevel = "L0" | "L1" | "L2" | "L3";
export type ApprovalScope = "execute" | "integrate_dev";
export type ApprovalReason = "plan" | "scope_exceeded";
export type ApprovalState = "pending" | "approved" | "rejected" | "superseded";
export type RiskCategory = "paths" | "migrations" | "security" | "db_schema";

/** A policy rule that matched a path of the plan (or a changed file). */
export interface RiskFinding {
  category: RiskCategory;
  risk: RiskLevel;
  pattern: string;
  repository: string;
  path: string;
}

export interface Approval {
  id: string;
  projectId: string;
  projectName?: string;
  taskId: string;
  taskTitle: string;
  planRevision: number;
  planHash: string;
  scope: ApprovalScope;
  reason: ApprovalReason;
  stepKey?: string;
  /** Files beyond the approved scope (reason scope_exceeded). */
  paths: string[];
  risk: RiskLevel;
  proposedRisk?: RiskLevel;
  findings: RiskFinding[];
  state: ApprovalState;
  /** Login of the Owner, or "policy" for an automatic L0/L1 integrate_dev grant. */
  approver?: string;
  comment?: string;
  requestedAt: string;
  decidedAt: string | null;
}

export interface PlanStep {
  key: string;
  title: string;
  repository: string;
  scope: string[];
  dependsOn: string[];
  capabilities: string[];
}

export interface PlanRevision {
  number: number;
  hash: string;
  /** Effective risk after the policy. */
  risk: RiskLevel;
  createdBy: string;
  createdAt: string;
  plan: { summary: string; risk: RiskLevel; maxParallel: number; steps: PlanStep[] };
}

export interface PlanChange {
  kind: "added" | "removed" | "modified";
  stepKey?: string;
  field: string;
  before: string;
  after: string;
}

/** GET /api/approvals/{id} */
export interface ApprovalDetail {
  approval: Approval;
  task: { id: string; title: string; state: string; planRevision: number };
  revision: PlanRevision;
  previous: PlanRevision | null;
  changes: PlanChange[];
  related: Approval[];
  canDecide: boolean;
}

/** POST /api/approvals/{id}/approve|reject: the revision and hash the Owner reviewed. */
export interface DecisionRequest {
  planRevision: number;
  planHash: string;
  comment: string;
}

export interface RiskRule {
  category: RiskCategory;
  risk: RiskLevel;
  patterns: string[];
}

/** GET/PUT /api/projects/{id}/risk-policy; custom is false while the default policy applies. */
export interface RiskPolicy {
  custom: boolean;
  rules: RiskRule[];
}
