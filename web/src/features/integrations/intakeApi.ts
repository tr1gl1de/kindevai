import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { SecretMeta } from "../../api/types";

/** Health of the YouTrack adapter: ok, degraded (recent failures) or circuit_open (calls paused). */
export type IntakeState = "ok" | "degraded" | "circuit_open";

/** Why an issue produced no task. */
export type IntakeReason = "unknown_user" | "not_allowed" | "tag_not_allowed" | "issue_not_found" | "other_project" | "invalid_issue" | "unavailable";

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

/** A project's YouTrack binding; autoPlan: intake tasks go to the AI planner at once (KIN-85). */
export interface YouTrackBinding {
  shortName: string;
  triggerTag: string;
  autoPlan: boolean;
  /** KIN-38: task state → YouTrack state its issue is moved to; unmapped states change nothing (comments only). */
  states?: Record<string, string>;
}

/** The task states a project may map to YouTrack states, in workflow order (KB KIN-A-14). */
export const TASK_STATES = [
  "CREATED",
  "ANALYZING",
  "READY",
  "AWAITING_APPROVAL",
  "RUNNING",
  "VERIFYING",
  "COMPLETED",
  "BLOCKED",
  "FAILED",
  "CANCELLED",
] as const;
export type TaskStateName = (typeof TASK_STATES)[number];

/** GET /api/projects/{id}/youtrack (members who may start AI in the project). */
export interface ProjectYouTrack {
  connected: boolean;
  binding: YouTrackBinding | null;
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
      api<YouTrackBinding>("PUT", `${projectPath(id)}/integrations/youtrack/trigger`, { triggerTag }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.project(id) }),
  });
}

/** Turns automatic planning of the project's intake tasks on or off (Owner, project admin). */
export function useSaveAutoPlan(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (autoPlan: boolean) => api<YouTrackBinding>("PUT", `${projectPath(id)}/integrations/youtrack/autoplan`, { autoPlan }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.project(id) }),
  });
}

/** Saves the task state → YouTrack state map (Owner, project admin); empty values are dropped by the caller. */
export function useSaveStateMap(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (states: Record<string, string>) =>
      api<{ states: Record<string, string> }>("PUT", `${projectPath(id)}/integrations/youtrack/states`, { states }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.project(id) }),
  });
}
