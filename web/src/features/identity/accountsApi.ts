import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { Account, ChangePasswordRequest, CreateUserRequest, Me, TemporaryPasswordResult } from "../../api/types";
import { keys as identityKeys } from "./api";

const usersKey = ["users"] as const;

export function useUsers(enabled: boolean) {
  return useQuery({ queryKey: usersKey, enabled, queryFn: ({ signal }) => api<Account[]>("GET", "/api/users", undefined, signal) });
}

export function useCreateUser() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: CreateUserRequest) => api<TemporaryPasswordResult>("POST", "/api/users", req),
    onSuccess: () => qc.invalidateQueries({ queryKey: usersKey }),
  });
}

export function useSetDisabled() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, disabled }: { id: string; disabled: boolean }) =>
      api<void>("POST", `/api/users/${encodeURIComponent(id)}/${disabled ? "disable" : "enable"}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: usersKey }),
  });
}

export function useResetPassword() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api<TemporaryPasswordResult>("POST", `/api/users/${encodeURIComponent(id)}/reset-password`),
    onSuccess: () => qc.invalidateQueries({ queryKey: usersKey }),
  });
}

export function useChangePassword() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: ChangePasswordRequest) => api<void>("POST", "/api/auth/password", req),
    onSuccess: () => qc.setQueryData<Me | null>(identityKeys.me, (me) => (me ? { ...me, mustChangePassword: false } : me)),
  });
}
