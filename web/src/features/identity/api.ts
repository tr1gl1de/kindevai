import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, isApiError } from "../../api/client";
import type { LoginRequest, LoginResult, Me, SetupRequest, SetupStatus } from "../../api/types";

export const keys = {
  setup: ["setup-status"] as const,
  me: ["me"] as const,
};

export function useSetupStatus() {
  return useQuery({ queryKey: keys.setup, queryFn: ({ signal }) => api<SetupStatus>("GET", "/api/setup/status", undefined, signal) });
}

/** Current user, or null when signed out (401 is a state, not an error). */
export function useMe(enabled: boolean) {
  return useQuery({
    queryKey: keys.me,
    enabled,
    queryFn: async ({ signal }) => {
      try {
        return await api<Me>("GET", "/api/auth/me", undefined, signal);
      } catch (e) {
        if (isApiError(e, "unauthenticated")) return null;
        throw e;
      }
    },
  });
}

export function useLogin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: LoginRequest) => api<LoginResult>("POST", "/api/auth/login", req),
    onSuccess: (res) => {
      // Never show a previous session's data to the user who just signed in.
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== keys.me[0] && q.queryKey[0] !== keys.setup[0] });
      qc.setQueryData(keys.me, res.user);
    },
  });
}

export function useLogout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api<void>("POST", "/api/auth/logout"),
    onSettled: () => {
      // Drop everything the signed-in user could see, keep the gate's own state.
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== keys.me[0] && q.queryKey[0] !== keys.setup[0] });
      qc.setQueryData(keys.me, null);
    },
  });
}

export function useCompleteSetup() {
  return useMutation({ mutationFn: (req: SetupRequest) => api<{ login: string }>("POST", "/api/setup", req) });
}
