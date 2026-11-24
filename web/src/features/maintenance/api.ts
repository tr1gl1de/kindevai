import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { MaintenanceStatus } from "../../api/types";

const key = ["maintenance"] as const;

/** Maintenance mode for the banner; polled so every open tab notices a switch and the drain progress. */
export function useMaintenance() {
  return useQuery({
    queryKey: key,
    queryFn: ({ signal }) => api<MaintenanceStatus>("GET", "/api/maintenance", undefined, signal),
    refetchInterval: 15_000,
  });
}

/** Owner: switch maintenance mode on (with a reason) or off. */
export function useSetMaintenance() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: { enabled: boolean; reason: string }) => api<MaintenanceStatus>("PUT", "/api/maintenance", req),
    onSuccess: (st) => qc.setQueryData(key, st),
  });
}
