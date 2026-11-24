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
