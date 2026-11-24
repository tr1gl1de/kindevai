import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { NetworkAssignment, NetworkProfile, NetworkProfileRequest, NetworkTest } from "../../api/types";

const keys = {
  profiles: ["network", "profiles"] as const,
  assignments: ["network", "assignments"] as const,
  tests: ["network", "tests"] as const,
};

export function useNetworkProfiles() {
  return useQuery({
    queryKey: keys.profiles,
    queryFn: ({ signal }) => api<NetworkProfile[]>("GET", "/api/network/profiles", undefined, signal),
  });
}

export function useNetworkAssignments() {
  return useQuery({
    queryKey: keys.assignments,
    queryFn: ({ signal }) => api<NetworkAssignment[]>("GET", "/api/network/assignments", undefined, signal),
  });
}

/** Recent tests; polled while one is still pending. */
export function useNetworkTests() {
  return useQuery({
    queryKey: keys.tests,
    queryFn: ({ signal }) => api<NetworkTest[]>("GET", "/api/network/tests", undefined, signal),
    refetchInterval: (q) => (q.state.data?.some((x) => x.status === "pending") ? 2000 : false),
  });
}

export function useSaveNetworkProfile() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, body }: { id?: string; body: NetworkProfileRequest }) =>
      id
        ? api<NetworkProfile>("PUT", `/api/network/profiles/${encodeURIComponent(id)}`, body)
        : api<NetworkProfile>("POST", "/api/network/profiles", body),
    gcTime: 0, // the request may carry a proxy password
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.profiles }),
  });
}

export function useDeleteNetworkProfile() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api<void>("DELETE", `/api/network/profiles/${encodeURIComponent(id)}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.profiles }),
  });
}

export function useAssignNetworkProfile() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (a: NetworkAssignment) => api<void>("PUT", "/api/network/assignments", a),
    onSuccess: () => Promise.all([qc.invalidateQueries({ queryKey: keys.assignments }), qc.invalidateQueries({ queryKey: keys.profiles })]),
  });
}

export function useRequestNetworkTest() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { runnerId: string; profileId: string }) => api<NetworkTest>("POST", "/api/network/tests", body),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.tests }),
  });
}
