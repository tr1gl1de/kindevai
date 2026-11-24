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
  http.get("/api/projects/:id/youtrack", () => json({ connected: false, binding: null, status: "ok", lastPolledAt: null, recent: [] })),
  http.get("/api/integrations/youtrack/intake", () =>
    json({ pollIntervalSeconds: 60, webhookSecret: { set: false, updatedAt: null }, webhookPath: "/api/webhooks/youtrack",
      webhookHeader: "X-Kindevai-Webhook-Secret", links: [], health: { state: "ok", consecutiveFailures: 0, lastFailureAt: null } }),
  ),
  http.get("/api/runners", () => json([])),
  http.get("/api/telegram/link", () => json({ configured: false, identity: null, request: null })),
  http.get("/api/alerts", () => json([])),
  http.get("/api/maintenance", () => json({ enabled: false, reason: "", since: null, by: "", activeAttempts: 0 })),
  http.get("/api/notifications/preferences", () =>
    json({ language: "en", disabled: [], types: ["approval_requested", "alert_raised", "task_finished"], telegramLinked: false }),
  ),
  http.get("/api/storage/overview", () =>
    json({ generatedAt: "2026-11-24T12:00:00Z", machines: [],
      thresholds: { warningPercent: 75, highPercent: 85, criticalPercent: 95, minHeadroomBytes: 2147483648 } }),
  ),
];

export const server = setupServer(...handlers);
