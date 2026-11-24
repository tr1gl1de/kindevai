import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { SecretMeta } from "../../api/types";

/** Health of the YouTrack adapter: ok, degraded (recent failures) or circuit_open (calls paused). */
export type IntakeState = "ok" | "degraded" | "circuit_open";

/** Why an issue produced no task. */
export type IntakeReason = "unknown_user" | "not_allowed" | "issue_not_found" | "other_project" | "invalid_issue" | "unavailable";

export interface IntakeEntry {
  issueId: string;
  outcome: "queued" | "created" | "updated" | "unchanged" | "rejected";
  reason?: IntakeReason | string;
  /** Platform login of the account the task runs for. */
  initiator?: string;
  /** YouTrack login of the issue's reporter. */
  reporter?: string;
  taskId?: string;
  channel: "poll" | "webhook" | "manual";
  processedAt: string;
}

/** GET /api/projects/{id}/youtrack (members who may start AI in the project). */
export interface ProjectYouTrack {
  connected: boolean;
  binding: { shortName: string; triggerTag: string } | null;
  status: IntakeState;
  lastPolledAt: string | null;
  recent: IntakeEntry[];
}

export interface YouTrackUserLink {
  userId: string;
  login: string;
  youtrackLogin: string;
  updatedAt: string;
}

/** GET /api/integrations/youtrack/intake (Owner only). The webhook secret is write-only. */
export interface IntakeSettings {
  pollIntervalSeconds: number;
  webhookSecret: SecretMeta;
  webhookPath: string;
  webhookHeader: string;
  links: YouTrackUserLink[];
  health: { state: IntakeState; consecutiveFailures: number; lastError?: string; lastFailureAt: string | null };
}

const keys = {
  settings: ["integrations", "intake"] as const,
  project: (id: string) => ["integrations", "intake", "project", id] as const,
};

/** How often the project view refreshes while a run by ID is queued. */
export const QUEUED_POLL_MS = 2000;

export function useIntakeSettings() {
  return useQuery({
    queryKey: keys.settings,
    queryFn: ({ signal }) => api<IntakeSettings>("GET", "/api/integrations/youtrack/intake", undefined, signal),
  });
}

function useSettingsMutation<V>(fn: (v: V) => Promise<IntakeSettings>) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    gcTime: 0, // the webhook secret must not stay in the mutation cache
    onSuccess: (s) => qc.setQueryData(keys.settings, s),
  });
}

export function useSavePollInterval() {
  return useSettingsMutation((pollIntervalSeconds: number) =>
    api<IntakeSettings>("PUT", "/api/integrations/youtrack/intake", { pollIntervalSeconds }),
  );
}

export function useSaveWebhookSecret() {
  return useSettingsMutation((secret: string) => api<IntakeSettings>("PUT", "/api/integrations/youtrack/webhook", { secret }));
}

export function useDisableWebhook() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api<void>("DELETE", "/api/integrations/youtrack/webhook"),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.settings }),
  });
}

export function useLinkYouTrackUser() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: { login: string; youtrackLogin: string }) => api<YouTrackUserLink>("POST", "/api/integrations/youtrack/users", req),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.settings }),
  });
}

export function useUnlinkYouTrackUser() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (userId: string) => api<void>("DELETE", `/api/integrations/youtrack/users/${encodeURIComponent(userId)}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.settings }),
  });
}

const projectPath = (id: string) => `/api/projects/${encodeURIComponent(id)}`;

export function useProjectYouTrack(id: string) {
  return useQuery({
    queryKey: keys.project(id),
    queryFn: ({ signal }) => api<ProjectYouTrack>("GET", `${projectPath(id)}/youtrack`, undefined, signal),
    // A run by ID is fetched in the background; refresh until it has an outcome.
    refetchInterval: (q) => (q.state.data?.recent.some((r) => r.outcome === "queued") ? QUEUED_POLL_MS : false),
  });
}

export function useRunIssue(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (issueId: string) => api<IntakeEntry>("POST", `${projectPath(id)}/youtrack/runs`, { issueId }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.project(id) }),
  });
}

export function useSaveTriggerTag(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (triggerTag: string) =>
      api<{ shortName: string; triggerTag: string }>("PUT", `${projectPath(id)}/integrations/youtrack/trigger`, { triggerTag }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.project(id) }),
  });
}
