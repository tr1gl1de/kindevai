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
  /** Runner output stored in the Control Plane's database (KIN-52 limit, KIN-59); absent on older servers. */
  runnerOutput?: RunnerOutputStorage;
}

/** ok; high: ≥ 90% of the limit (the oldest finished output is evicted); full: new output is being dropped. */
export type RunnerOutputState = "ok" | "high" | "full";

export interface RunnerOutputStorage {
  usedBytes: number;
  limitBytes: number;
  usedPercent: number;
  state: RunnerOutputState;
  outputRetentionDays: number;
  eventRetentionDays: number;
  /** The window of droppedEvents and drops. */
  dropsWindowHours: number;
  droppedEvents: number;
  lastDroppedAt: string | null;
  /** Attempts whose output was dropped for the limit, newest first (at most 10). */
  drops: { runnerId: string; stepId: string; fence: number; events: number; lastAt: string }[];
}

// Infrastructure → Storage (GET /api/storage/machines/{id}?days=7|30, KIN-55).

/** One hourly point of the storage history (the last report of the hour). */
export interface StorageSample {
  at: string;
  totalBytes: number;
  usedBytes: number;
}

export type SpaceConsumerCategory = "repos" | "workspaces" | "sessions" | "spool" | "container_cache";

/** Size of one category; partial: the I/O-limited walk stopped early, so bytes is a lower bound. */
export interface SpaceConsumer {
  category: SpaceConsumerCategory;
  bytes: number;
  entries: number;
  partial: boolean;
}

export interface StorageMachineDetail extends StorageMachine {
  days: 7 | 30;
  /** Volume role → hourly samples, oldest first. */
  history: Partial<Record<VolumeRole, StorageSample[]>>;
  /** The runner's last measurement; null when it never measured (it needs ai-runner run --measure-space). */
  consumers: { measuredAt: string; items: SpaceConsumer[] } | null;
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

// Network profiles (KIN-30). Proxy passwords are write-only: requests may carry one, responses only passwordSet.
export type Enforcement = "off" | "monitor" | "strict";
export type DnsPolicy = "system" | "proxy";
export type NetworkLevel = "system" | "runner" | "project" | "role";

export interface NetworkRule {
  host?: string;
  cidr?: string;
  ports: number[];
}

export interface NetworkProxy {
  scheme: "http" | "https" | "socks5";
  host: string;
  port: number;
  username: string;
  url?: string;
}

export interface NetworkProfile {
  id: string;
  name: string;
  proxy: NetworkProxy | null;
  passwordSet: boolean;
  enforcement: Enforcement;
  dns: DnsPolicy;
  failClosed: boolean;
  expectedIpv4: string;
  expectedIpv6: string;
  allow: NetworkRule[];
  diagnosticUrl: string;
  assigned: { level: NetworkLevel; target: string }[];
  createdAt: string;
  updatedAt: string;
}

/** POST/PUT /api/network/profiles: password omitted keeps the stored one. */
export interface NetworkProfileRequest {
  name: string;
  proxyUrl: string;
  password?: string;
  clearPassword?: boolean;
  enforcement: Enforcement;
  dns: DnsPolicy;
  failClosed: boolean;
  expectedIpv4: string;
  expectedIpv6: string;
  allow: NetworkRule[];
  diagnosticUrl: string;
}

export interface NetworkAssignment {
  level: NetworkLevel;
  target: string;
  profileId: string;
}

export interface NetworkCheck {
  name: string;
  host: string;
  ok: boolean;
  code: string;
  latencyMs: number;
}

export interface NetworkTest {
  id: string;
  runnerId: string;
  profileId: string;
  status: "pending" | "ok" | "failed" | "timed_out";
  code: string;
  detail: string;
  expectedIpv4: string;
  expectedIpv6: string;
  observedIpv4: string;
  observedIpv6: string;
  checks: NetworkCheck[];
  durationMs: number;
  requestedAt: string;
  completedAt: string | null;
}

// Alert Center (KIN-40).
export type AlertSeverity = "info" | "warning" | "critical";
export type AlertState = "OPEN" | "ACKNOWLEDGED" | "DISMISSED" | "RESOLVED";
export type AlertAction = "acknowledge" | "dismiss" | "resolve";

export interface Alert {
  id: string;
  projectId?: string;
  projectName?: string;
  kind: string;
  severity: AlertSeverity;
  subject: string;
  details: Record<string, string>;
  state: AlertState;
  occurrences: number;
  firstSeenAt: string;
  lastSeenAt: string;
  ackedAt: string | null;
  closedAt: string | null;
  canManage: boolean;
  // KIN-61: analysis task in YouTrack.
  canSuggestAnalysis?: boolean;
  analysisTask?: AlertAnalysisTask | null;
}

export interface AlertAnalysisTask {
  projectId: string;
  youtrackProject: string;
  requestedAt: string;
  issueId?: string;
  issueUrl?: string;
  createdAt: string | null;
  attempt?: number;
  /** Code of a permanent failure; confirming again retries. */
  failure?: string;
  failedAt?: string | null;
}

export interface AlertAnalysisPreview {
  summary: string;
  description: string;
  configured: boolean;
  targets: {
    projectId: string;
    projectName: string;
    youtrackProject: string;
  }[];
  task: AlertAnalysisTask | null;
}

export type NotificationType = "approval_requested" | "alert_raised" | "task_finished";

export interface NotificationPreferences {
  language: "en" | "ru";
  disabled: NotificationType[];
  types?: NotificationType[];
  telegramLinked?: boolean;
}

// Tasks & Executions (KIN-23 API, KIN-25 UI with agent time accounting)
export type TaskState =
  | "CREATED"
  | "ANALYZING"
  | "READY"
  | "AWAITING_APPROVAL"
  | "RUNNING"
  | "VERIFYING"
  | "COMPLETED"
  | "BLOCKED"
  | "FAILED"
  | "CANCELLED";
export type WaitReason = "runner" | "network" | "approval" | "cli_limit" | "agent";
export type StepState = "PENDING" | "READY" | "CLAIMED" | "RUNNING" | "SUCCEEDED" | "WAITING" | "FAILED" | "CANCELLED";
export type AttemptState = "RUNNING" | "SUCCEEDED" | "FAILED" | "CANCELLED" | "UNKNOWN";
/** How far agent time is known: no CLI ran, still running, recorded, or a period lacks an end or a usable time. */
export type WorkStatus = "none" | "running" | "complete" | "incomplete";

export interface Task {
  id: string;
  projectId: string;
  title: string;
  youtrackIssue?: string;
  state: TaskState;
  blockReason?: WaitReason;
  reason?: string;
  planRevision: number;
  createdAt: string;
  updatedAt: string;
}

/** Agent time (sum of CLI work periods) and, separately, the calendar duration of the task's executions. */
export interface TaskTime {
  agentMs: number;
  agentStatus: WorkStatus;
  calendarMs: number;
  running: boolean;
  started: boolean;
}

export interface AgentTime {
  ms: number;
  status: WorkStatus;
}

export interface TaskListItem extends Task {
  time: TaskTime;
}

/** GET /api/tasks?projectId=&state=A,B — at most 200 tasks; total covers every task the filter selects. */
export interface TaskList {
  tasks: TaskListItem[];
  total: AgentTime;
}

export interface Attempt {
  number: number;
  runnerId: string;
  fence: number;
  state: AttemptState;
  error?: string;
  startedAt: string;
  finishedAt: string | null;
  /** CLI adapter that ran the attempt, e.g. "mock". */
  agent?: string;
  /** Work period on the runner's clock. */
  workStartedAt: string | null;
  workEndedAt: string | null;
  workStatus: WorkStatus;
  workMs: number;
  /** What the AI CLI reported (KIN-79); absent for the mock agent. */
  session?: string;
  model?: string;
  cliVersion?: string;
  usage?: AgentUsage;
}

/** AI CLI usage: tokens, cost in USD (0 when the CLI reports none) and turns (KIN-79). */
export interface AgentUsage {
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  costUsd: number;
  turns: number;
}

export interface Step {
  id: string;
  key: string;
  title: string;
  repository: string;
  dependsOn: string[];
  state: StepState;
  waitReason?: WaitReason;
  reason?: string;
  runnerId?: string;
  fence: number;
  attempts: Attempt[];
  agentTime: AgentTime;
}

export interface Execution {
  id: string;
  planRevision: number;
  planHash: string;
  state: "RUNNING" | "SUCCEEDED" | "FAILED" | "CANCELLED";
  reason?: string;
  createdAt: string;
  finishedAt: string | null;
  steps: Step[];
  agentTime: AgentTime;
  /** The AI CLI usage of all attempts (KIN-79). */
  usage?: AgentUsage;
}

/** One step of a plan revision (KIN-34): its role, the scope it may change and the steps it waits for. */
export interface TaskPlanStep extends PlanStep {
  role: "implementer" | "reviewer";
}

export interface TaskRevision {
  number: number;
  hash: string;
  risk: RiskLevel;
  summary: string;
  maxParallel: number;
  steps: TaskPlanStep[];
  /** The plan's declared impact (KIN-34): contract changes, migrations, tests, documentation. */
  contracts: string[];
  migrations: string[];
  tests: string[];
  docs: string[];
  createdBy: string;
  createdAt: string;
}

export type PlanningState = "PREPARING" | "QUEUED" | "RUNNING" | "SUCCEEDED" | "FAILED" | "CANCELLED";

/** The task's latest AI planner run (KIN-34). Its attempts' logs and workspace use stepId and attempt (the fence). */
export interface PlanningRun {
  id: string;
  state: PlanningState;
  stepId: string;
  attempt: number;
  used: number;
  maxAttempts: number;
  agent?: string;
  model?: string;
  runnerId?: string;
  /** The plan chat (KIN-43) can continue the planner's CLI session. */
  hasSession: boolean;
  waitReason?: WaitReason;
  waitDetail?: string;
  notBefore: string | null;
  /** Why the last answer was refused (the repeat is told so). */
  feedback?: string;
  reason?: string;
  revision: number;
  issueText: boolean;
  articles: string[];
  contextNote?: string;
  truncated?: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface TaskDetail {
  task: Task;
  revision: TaskRevision | null;
  planning: PlanningRun | null;
  execution: Execution | null;
  canManage: boolean;
  time: TaskTime;
}

/** One recorded runner event of an attempt; data is process output (plain text, never markup). */
export interface AttemptEvent {
  seq: number;
  kind: string;
  at: string;
  stream?: string;
  offset?: number;
  data?: string;
  bytes?: number;
  reason?: string;
  omitted?: boolean;
}

/** An instruction file of an attempt's worktree (KIN-29/70): path is the repository's untrusted text. */
export interface InstructionFile {
  path: string;
  sha256: string;
  size: number;
  symlink?: boolean;
}

/** A changed path: A added, M modified, D deleted, T type changed (renames show as D + A). */
export interface ChangedFile {
  path: string;
  status: string;
}

/** An instruction file whose hash changed during the attempt (no before: added, no after: removed). */
export interface InstructionChange {
  path: string;
  before?: string;
  after?: string;
}

export interface WorkspacePrepared {
  at: string;
  repository: string;
  branch: string;
  base: string;
  baseSha: string;
  instructions: InstructionFile[];
  instructionsTruncated?: boolean;
}

export interface WorkspaceChanges {
  at: string;
  branch: string;
  baseSha: string;
  headSha: string;
  files: ChangedFile[];
  filesTotal: number;
  truncated?: boolean;
  instructionsChanged: InstructionChange[];
}

/** GET /api/tasks/{id}/steps/{step}/attempts/{fence}/workspace: the latest reports, null where none. */
export interface AttemptWorkspace {
  prepared: WorkspacePrepared | null;
  changes: WorkspaceChanges | null;
}

export interface AttemptEventPage {
  events: AttemptEvent[];
  next: number;
  more: boolean;
}

export type MockScenario = "success" | "fail" | "flaky" | "crash" | "hang" | "gate" | "scope";
// Maintenance mode (KIN-41).
export interface MaintenanceStatus {
  enabled: boolean;
  reason: string;
  since: string | null;
  by: string;
  /** Claimed or running steps still draining. */
  activeAttempts: number;
}

/** Agent router (KIN-33): AI CLIs, platform roles and their bindings. */
export type AgentCli = "claude" | "codex";
export type AgentRole = "planner" | "implementer" | "reviewer";
export type BindingStrategy = "ordered" | "different-from-author";
export type BindingSource = "default" | "global" | "project";

export interface AgentChoice {
  agent: AgentCli;
  /** "" = the CLI's default model. */
  model: string;
}

export interface AgentBinding {
  role: AgentRole;
  primary: AgentChoice;
  fallback: AgentChoice[];
  strategy: BindingStrategy;
  source: BindingSource;
  updatedBy: string;
  updatedAt: string | null;
}

export interface AgentBindingRequest {
  primary: AgentChoice;
  fallback: AgentChoice[];
  strategy: BindingStrategy;
}

export interface AgentSwitch {
  agent: AgentCli;
  enabled: boolean;
  /** False while the switch has its default (enabled). */
  set: boolean;
  updatedBy: string;
  updatedAt: string | null;
}

/** Why a runner's CLI is held (KIN-79, KIN-83). */
export type AgentHoldCause = "rate_limit" | "auth" | "billing";

export interface AgentCooldown {
  runnerId: string;
  agent: AgentCli;
  /** End of the hold; for a parked hold only an upper bound. */
  until: string;
  reason: string;
  cause: AgentHoldCause;
  /** Holds of this cause in a row. */
  strikes: number;
  /** Waits for the Owner's "ready again" (no automatic retry). */
  parked: boolean;
}

export interface AgentSettings {
  switches: AgentSwitch[];
  bindings: AgentBinding[];
  cooldowns: AgentCooldown[];
}

export interface ProjectAgentState {
  agent: AgentCli;
  global: boolean;
  project: AgentSwitch;
  effective: boolean;
}

export interface ProjectAgentSettings {
  projectId: string;
  agents: ProjectAgentState[];
  bindings: AgentBinding[];
}
