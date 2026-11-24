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
