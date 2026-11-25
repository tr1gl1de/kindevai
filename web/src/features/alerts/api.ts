import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type {
  Alert,
  AlertAction,
  AlertAnalysisPreview,
  NotificationPreferences,
} from "../../api/types";

export type AlertFilter = "active" | "all";

const keys = {
  all: ["alerts"] as const,
  list: (f: AlertFilter) => ["alerts", "list", f] as const,
  prefs: ["notification-preferences"] as const,
};

/** Alerts visible to the signed-in user; refreshed every minute so the navigation count stays current. */
export function useAlerts(filter: AlertFilter) {
  return useQuery({
    queryKey: keys.list(filter),
    queryFn: ({ signal }) =>
      api<Alert[]>("GET", `/api/alerts?state=${filter}`, undefined, signal),
    refetchInterval: 60_000,
  });
}

export function useAlertAction() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, action }: { id: string; action: AlertAction }) =>
      api<Alert>("POST", `/api/alerts/${encodeURIComponent(id)}/${action}`),
    onSettled: () => qc.invalidateQueries({ queryKey: keys.all }),
  });
}

/** Draft of an alert's analysis task (KIN-61); creates nothing. */
export function useAnalysisPreview(id: string | undefined, lang: string) {
  return useQuery({
    queryKey: ["alerts", "analysis", id, lang],
    enabled: id !== undefined,
    queryFn: ({ signal }) =>
      api<AlertAnalysisPreview>(
        "GET",
        `/api/alerts/${encodeURIComponent(id ?? "")}/analysis-task?lang=${lang}`,
        undefined,
        signal,
      ),
  });
}

/** The user's explicit confirmation: the server enqueues the YouTrack issue. */
export function useSuggestAnalysis() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      language,
      projectId,
    }: {
      id: string;
      language: string;
      projectId?: string;
    }) =>
      api<Alert>(
        "POST",
        `/api/alerts/${encodeURIComponent(id)}/analysis-task`,
        { language, projectId },
      ),
    onSettled: () => qc.invalidateQueries({ queryKey: keys.all }),
  });
}

export function useNotificationPreferences() {
  return useQuery({
    queryKey: keys.prefs,
    queryFn: ({ signal }) =>
      api<NotificationPreferences>(
        "GET",
        "/api/notifications/preferences",
        undefined,
        signal,
      ),
  });
}

export function useSaveNotificationPreferences() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (p: NotificationPreferences) =>
      api<NotificationPreferences>("PUT", "/api/notifications/preferences", {
        language: p.language,
        disabled: p.disabled,
      }),
    onSuccess: (p) => qc.setQueryData(keys.prefs, p),
  });
}
