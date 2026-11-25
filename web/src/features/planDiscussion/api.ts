import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../../api/client";
import type { DiscussionLimitCode, DiscussionLimits, DiscussionMessage, DiscussionThread } from "../../api/types";

const keys = {
  thread: (taskId: string) => ["plan-discussion", taskId] as const,
  limits: ["plan-discussion-limits"] as const,
};

const path = (taskId: string) => `/api/tasks/${encodeURIComponent(taskId)}/discussion`;

/** While the AI works on a message the thread is polled often, so states and the answer show up without a reload. */
export const ACTIVE_POLL_MS = 1500;
/** Otherwise slowly: another admin's messages and answers still appear. */
export const IDLE_POLL_MS = 15_000;

/** A message still waits for the AI (queued, runner offline or being answered). */
export const isOpen = (m: DiscussionMessage) => m.role === "user" && (m.status === "PENDING" || m.status === "DELIVERED");

export function useDiscussion(taskId: string) {
  return useQuery({
    queryKey: keys.thread(taskId),
    queryFn: ({ signal }) => api<DiscussionThread>("GET", path(taskId), undefined, signal),
    refetchInterval: (q) => {
      const d = q.state.data;
      if (!d) return false;
      return d.inFlight || d.messages.some(isOpen) ? ACTIVE_POLL_MS : IDLE_POLL_MS;
    },
  });
}

function useThreadAction<V>(taskId: string, fn: (v: V) => Promise<DiscussionMessage>) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSettled: () => qc.invalidateQueries({ queryKey: keys.thread(taskId) }),
  });
}

/** Sends a message (202: the AI answers asynchronously). */
export const usePostMessage = (taskId: string) =>
  useThreadAction(taskId, (text: string) => api<DiscussionMessage>("POST", `${path(taskId)}/messages`, { text }));

/** Sends the last FAILED message again (a new turn). */
export const useRetryMessage = (taskId: string) =>
  useThreadAction(taskId, (id: string) =>
    api<DiscussionMessage>("POST", `${path(taskId)}/messages/${encodeURIComponent(id)}/retry`, {}),
  );

export function useDiscussionLimits() {
  return useQuery({
    queryKey: keys.limits,
    queryFn: ({ signal }) => api<DiscussionLimits>("GET", "/api/plan-discussion/limits", undefined, signal),
  });
}

export function useSaveDiscussionLimits() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (l: DiscussionLimits) => api<DiscussionLimits>("PUT", "/api/plan-discussion/limits", l),
    onSuccess: (l) => qc.setQueryData(keys.limits, l),
    onSettled: () => qc.invalidateQueries({ queryKey: ["plan-discussion"] }),
  });
}

export interface LimitHit {
  code: DiscussionLimitCode;
  limit: number;
  retryAfterSec: number;
  /** When the next message is allowed (ISO time), if the limit says. */
  at?: string;
}

const limitCodes: DiscussionLimitCode[] = ["message_too_long", "answer_in_flight", "plan_message_limit", "hourly_message_limit"];

/** The limit a 429 from the discussion names, or undefined for any other error; now is when it was received. */
export function limitHit(e: unknown, now = Date.now()): LimitHit | undefined {
  if (!(e instanceof ApiError) || e.status !== 429 || !e.body) return undefined;
  const code = e.body.code;
  if (typeof code !== "string" || !limitCodes.includes(code as DiscussionLimitCode)) return undefined;
  const num = (v: unknown) => (typeof v === "number" && Number.isFinite(v) && v > 0 ? v : 0);
  const retryAfterSec = num(e.body.retryAfterSec);
  const at = retryAfterSec > 0 ? new Date(now + retryAfterSec * 1000).toISOString() : undefined;
  return { code: code as DiscussionLimitCode, limit: num(e.body.limit), retryAfterSec, at };
}
