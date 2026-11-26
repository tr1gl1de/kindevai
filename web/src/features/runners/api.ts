import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type {
  CreateEnrollmentTokenResult,
  EnrollmentToken,
  Runner,
  StorageMachineDetail,
  StorageOverview,
} from "../../api/types";

const keys = {
  runners: ["runners"] as const,
  runner: (id: string) => ["runners", id] as const,
  tokens: ["runner-enrollment-tokens"] as const,
};

const path = (id: string) => `/api/runners/${encodeURIComponent(id)}`;

/** The runner list; refetchInterval keeps it live (the Overview card, KIN-55). */
export function useRunners(refetchInterval?: number) {
  return useQuery({
    queryKey: keys.runners,
    refetchInterval,
    queryFn: ({ signal }) => api<Runner[]>("GET", "/api/runners", undefined, signal),
  });
}

export function useRunner(id: string) {
  return useQuery({ queryKey: keys.runner(id), queryFn: ({ signal }) => api<Runner>("GET", path(id), undefined, signal) });
}

export function useEnrollmentTokens() {
  return useQuery({
    queryKey: keys.tokens,
    queryFn: ({ signal }) => api<EnrollmentToken[]>("GET", "/api/runners/enrollment-tokens", undefined, signal),
  });
}

export function useCreateEnrollmentToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api<CreateEnrollmentTokenResult>("POST", "/api/runners/enrollment-tokens"),
    gcTime: 0, // the response carries the one-time token
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.tokens }),
  });
}

export function useRevokeEnrollmentToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api<void>("POST", `/api/runners/enrollment-tokens/${encodeURIComponent(id)}/revoke`),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.tokens }),
  });
}

export type RunnerAction =
  | { kind: "approve"; projectIds: string[] }
  | { kind: "projects"; projectIds: string[] }
  | { kind: "maxParallel"; maxParallelSteps: number | null }
  | { kind: "disable" | "enable" | "revoke" };

/** One mutation for every Owner action on a runner; the response is the updated runner. */
export function useRunnerAction(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (a: RunnerAction) => {
      if (a.kind === "approve") return api<Runner>("POST", `${path(id)}/approve`, { projectIds: a.projectIds });
      if (a.kind === "projects") return api<Runner>("PUT", `${path(id)}/projects`, { projectIds: a.projectIds });
      if (a.kind === "maxParallel")
        return api<Runner>("PUT", `${path(id)}/max-parallel-steps`, { maxParallelSteps: a.maxParallelSteps });
      return api<Runner>("POST", `${path(id)}/${a.kind}`);
    },
    onSuccess: (r) => {
      qc.setQueryData(keys.runner(id), r);
      void qc.invalidateQueries({ queryKey: keys.runners, exact: true });
    },
  });
}

/**
 * The enroll command the Owner runs on the new machine. The token is never part of it: ai-runner asks for it on
 * stdin (or reads --token-file), so it stays out of shell history and process lists.
 */
export function enrollCommand(origin = window.location.origin): string {
  return `ai-runner enroll --server ${origin}`;
}

/** Remote runners need HTTPS (the Caddy profile); plain HTTP is only for a runner on the same machine. */
export function needsHttpsForRemote(origin = window.location.origin): boolean {
  const u = new URL(origin);
  return u.protocol === "http:" && !["localhost", "127.0.0.1", "[::1]"].includes(u.hostname);
}

/** Storage Overview of the Control Plane and the runners (Owner only); refreshed while the dashboard is open. */
export function useStorageOverview(enabled: boolean) {
  return useQuery({
    queryKey: ["storage-overview"] as const,
    enabled,
    refetchInterval: 60_000,
    queryFn: ({ signal }) => api<StorageOverview>("GET", "/api/storage/overview", undefined, signal),
  });
}

/** Infrastructure → Storage: one machine with its history window (7 or 30 days) and space consumers (KIN-55). */
export function useStorageMachine(id: string, days: 7 | 30) {
  return useQuery({
    queryKey: ["storage-machine", id, days] as const,
    refetchInterval: 5 * 60_000,
    placeholderData: keepPreviousData, // keep the chart while the other window loads
    queryFn: ({ signal }) =>
      api<StorageMachineDetail>("GET", `/api/storage/machines/${encodeURIComponent(id)}?days=${days}`, undefined, signal),
  });
}
