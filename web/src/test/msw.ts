import { HttpResponse, http } from "msw";
import { setupServer } from "msw/node";
import type { ApiErrorBody, Health, Me, SetupStatus } from "../api/types";

export const json = <T,>(body: T, status = 200) => HttpResponse.json(body as object, { status });
export const apiError = (status: number, error: string, field?: string) =>
  HttpResponse.json<ApiErrorBody>({ error, message: error, ...(field ? { field } : {}) }, { status });

export const owner: Me = { id: "u1", login: "owner", role: "owner", mustChangePassword: false };

/** Default: installed platform, signed-out visitor. Tests override with server.use(...). */
export const handlers = [
  http.get("/api/health", () => json<Health>({ status: "ok", version: "test" })),
  http.get("/api/setup/status", () => json<SetupStatus>({ setupRequired: false })),
  http.get("/api/auth/me", () => apiError(401, "unauthenticated")),
  http.get("/api/projects", () => json([])),
  http.get("/api/invitations", () => json([])),
];

export const server = setupServer(...handlers);
