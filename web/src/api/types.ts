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
