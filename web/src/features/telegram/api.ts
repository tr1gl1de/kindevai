import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type {
  LoginResult,
  TelegramIdentity,
  TelegramLinkPreview,
  TelegramLinkRequest,
  TelegramLinkStatus,
  TelegramSettings,
  TelegramSettingsRequest,
} from "../../api/types";
import { keys } from "../identity/api";

const linkKey = ["telegram-link"] as const;
const settingsKey = ["telegram-settings"] as const;

/** Polls while a link request waits for the Mini App, so the code form appears by itself. */
export function useTelegramLink() {
  return useQuery({
    queryKey: linkKey,
    queryFn: ({ signal }) => api<TelegramLinkStatus>("GET", "/api/telegram/link", undefined, signal),
    refetchInterval: (q) => (q.state.data?.request?.state === "awaiting_telegram" ? 3000 : false),
  });
}

export function useStartLink() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api<TelegramLinkRequest>("POST", "/api/telegram/link"),
    gcTime: 0, // carries the one-time link
    onSuccess: () => qc.invalidateQueries({ queryKey: linkKey }),
  });
}

export function useCancelLink() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api<void>("POST", "/api/telegram/link/cancel"),
    onSuccess: () => qc.invalidateQueries({ queryKey: linkKey }),
  });
}

export function useCompleteLink() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (code: string) => api<TelegramIdentity>("POST", "/api/telegram/link/complete", { code }),
    onSettled: () => qc.invalidateQueries({ queryKey: linkKey }),
  });
}

export function useUnlink() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api<void>("DELETE", "/api/telegram/link"),
    onSuccess: () => qc.invalidateQueries({ queryKey: linkKey }),
  });
}

export function useTelegramSettings() {
  return useQuery({ queryKey: settingsKey, queryFn: ({ signal }) => api<TelegramSettings>("GET", "/api/telegram/settings", undefined, signal) });
}

export function useSaveTelegramSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: TelegramSettingsRequest) => api<TelegramSettings>("PUT", "/api/telegram/settings", req),
    gcTime: 0, // the variables hold the bot token
    onSuccess: (s) => qc.setQueryData(settingsKey, s),
  });
}

/** Mini App sign-in with the raw initData; on success the gate sees the user. */
export function useMiniAppLogin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (initData: string) => api<LoginResult>("POST", "/api/telegram/login", { initData }),
    onSuccess: (res) => {
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== keys.me[0] && q.queryKey[0] !== keys.setup[0] });
      qc.setQueryData(keys.me, res.user);
    },
  });
}

/** Checked once per Mini App start (the link token is single-use and the endpoint rate limited). */
export function useLinkPreview(initData: string) {
  return useQuery({
    queryKey: ["telegram-link-preview"],
    gcTime: 0,
    retry: false,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    queryFn: ({ signal }) => api<TelegramLinkPreview>("POST", "/api/telegram/link/preview", { initData }, signal),
  });
}

export function useConfirmLink() {
  return useMutation({
    mutationFn: (initData: string) => api<TelegramLinkPreview>("POST", "/api/telegram/link/confirm", { initData }),
    gcTime: 0,
  });
}
