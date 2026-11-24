import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { api } from "../../api/client";
import type { Approval, AttemptEventPage, MockScenario, TaskDetail, TaskList, TaskState } from "../../api/types";

export interface TaskFilter {
  projectId: string; // "" = every visible project
  state: TaskState | ""; // "" = all states
}

const keys = {
  all: ["tasks"] as const,
  list: (f: TaskFilter) => ["tasks", "list", f.projectId, f.state] as const,
  one: (id: string) => ["tasks", "one", id] as const,
  approvals: (id: string) => ["approvals", "task", id] as const,
};

const path = (id: string) => `/api/tasks/${encodeURIComponent(id)}`;

const terminal: TaskState[] = ["COMPLETED", "FAILED", "CANCELLED"];
export const isTerminal = (s: TaskState) => terminal.includes(s);

/** How often task views refresh while work is going on. */
export const LIVE_REFRESH_MS = 3000;
/** A closed task keeps refreshing briefly: the runner reports a cancelled process's end a few seconds later. */
const SETTLE_MS = 60_000;

function live(d: TaskDetail | undefined): boolean {
  if (!d) return false;
  if (!isTerminal(d.task.state) || d.time.agentStatus === "running") return true;
  return d.time.agentStatus === "incomplete" && Date.now() - Date.parse(d.task.updatedAt) < SETTLE_MS;
}

export function useTasks(f: TaskFilter) {
  const qs = new URLSearchParams();
  if (f.projectId) qs.set("projectId", f.projectId);
  if (f.state) qs.set("state", f.state);
  return useQuery({
    queryKey: keys.list(f),
    queryFn: ({ signal }) => api<TaskList>("GET", `/api/tasks?${qs.toString()}`, undefined, signal),
    refetchInterval: (q) => (q.state.data?.tasks.some((t) => !isTerminal(t.state) || t.time.running) ? LIVE_REFRESH_MS : false),
  });
}

export function useTask(id: string) {
  return useQuery({
    queryKey: keys.one(id),
    queryFn: ({ signal }) => api<TaskDetail>("GET", path(id), undefined, signal),
    refetchInterval: (q) => (live(q.state.data) ? LIVE_REFRESH_MS : false),
  });
}

/** The task's approvals (visible to the Owner and project admins only). */
export function useTaskApprovals(id: string, enabled: boolean) {
  return useQuery({
    queryKey: keys.approvals(id),
    enabled,
    queryFn: ({ signal }) => api<Approval[]>("GET", `/api/approvals?taskId=${encodeURIComponent(id)}`, undefined, signal),
  });
}

function useTaskAction<V>(id: string, fn: (v: V) => Promise<TaskDetail>) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: (d) => qc.setQueryData(keys.one(id), d),
    onSettled: () => qc.invalidateQueries({ queryKey: keys.all }),
  });
}

export function useCreateTask() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { projectId: string; title: string }) => api<TaskDetail>("POST", "/api/tasks", body),
    onSettled: () => qc.invalidateQueries({ queryKey: keys.all }),
  });
}

export const useMockRun = (id: string) =>
  useTaskAction(id, (scenario: MockScenario) => api<TaskDetail>("POST", `${path(id)}/mock-run`, { scenario }));
export const useCancelTask = (id: string) =>
  useTaskAction(id, (reason: string) => api<TaskDetail>("POST", `${path(id)}/cancel`, { reason }));
export const useRetryTask = (id: string) => useTaskAction(id, () => api<TaskDetail>("POST", `${path(id)}/retry`, {}));

/** One line of an attempt's log: an output chunk or a lifecycle event. */
export interface LogEntry {
  seq: number;
  kind: string;
  at: string;
  stream?: string;
  text: string;
  bytes?: number;
  omitted?: boolean;
}

/** The UI keeps at most this much of an attempt's log; older entries are dropped (and the view says so). */
export const MAX_LOG_CHARS = 256 * 1024;
/** Pause between polls while the attempt runs (a full page is followed up right away). */
export const LOG_POLL_MS = 2000;

export interface AttemptLogState {
  entries: LogEntry[];
  trimmed: boolean;
  loading: boolean;
  error: unknown;
}

const sleep = (ms: number, signal: AbortSignal) =>
  new Promise<void>((resolve) => {
    const t = window.setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        window.clearTimeout(t);
        resolve();
      },
      { once: true },
    );
  });

/**
 * Streams an attempt's recorded events by polling with a cursor: one request at a time (backpressure: the next
 * page is asked for only after the previous one arrived), at most 32 events per page, a bounded buffer in memory.
 * While the attempt is live the log is polled every LOG_POLL_MS; once it ended, the remaining pages are read and
 * polling stops. Mount it with a new key to start over (retry after an error).
 */
export function useAttemptLog(taskId: string, stepId: string, fence: number, isLive: boolean): AttemptLogState {
  const [entries, setEntries] = useState<LogEntry[]>([]);
  const [trimmed, setTrimmed] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  const liveRef = useRef(isLive);
  useEffect(() => {
    liveRef.current = isLive;
  }, [isLive]);

  useEffect(() => {
    const ctl = new AbortController();
    const base = `${path(taskId)}/steps/${encodeURIComponent(stepId)}/attempts/${fence}/events`;
    let after = 0;
    let size = 0;
    let buf: LogEntry[] = [];
    void (async () => {
      try {
        for (;;) {
          const wasLive = liveRef.current;
          const page = await api<AttemptEventPage>("GET", `${base}?after=${after}`, undefined, ctl.signal);
          if (ctl.signal.aborted) return;
          if (page.events.length > 0) {
            const add = page.events.map<LogEntry>((e) => ({
              seq: e.seq,
              kind: e.kind,
              at: e.at,
              stream: e.stream,
              text: e.data ?? e.reason ?? "",
              bytes: e.bytes,
              omitted: e.omitted,
            }));
            buf = buf.concat(add);
            size += add.reduce((n, e) => n + e.text.length, 0);
            let cut = 0;
            while (size > MAX_LOG_CHARS && cut < buf.length - 1) size -= buf[cut++]?.text.length ?? 0;
            if (cut > 0) {
              buf = buf.slice(cut);
              setTrimmed(true);
            }
            setEntries(buf);
          }
          setLoading(false);
          after = page.next;
          if (page.more) continue;
          if (!wasLive) return; // the attempt had ended before this read: nothing more will come
          await sleep(LOG_POLL_MS, ctl.signal);
          if (ctl.signal.aborted) return;
        }
      } catch (e) {
        if (!ctl.signal.aborted) {
          setError(e);
          setLoading(false);
        }
      }
    })();
    return () => ctl.abort();
  }, [taskId, stepId, fence]);

  return { entries, trimmed, loading, error };
}
