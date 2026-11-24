import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { AuditFilter, AuditPage } from "../../api/types";

export const PAGE_SIZE = 50;

function query(filter: AuditFilter, before: string | null): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(filter)) if (v) q.set(k, v);
  if (before) q.set("before", before);
  q.set("limit", String(PAGE_SIZE));
  return q.toString();
}

/** Pages of audit events for the filter, newest first (Owner only). */
export function useAuditEvents(filter: AuditFilter) {
  return useInfiniteQuery({
    queryKey: ["audit", filter],
    initialPageParam: null as string | null,
    queryFn: ({ pageParam, signal }) => api<AuditPage>("GET", `/api/audit?${query(filter, pageParam)}`, undefined, signal),
    getNextPageParam: (last) => last.next,
  });
}

/** Event kinds recorded so far, for the filter. */
export function useAuditKinds() {
  return useQuery({ queryKey: ["audit", "kinds"], queryFn: ({ signal }) => api<string[]>("GET", "/api/audit/kinds", undefined, signal) });
}
