import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { Approval, ApprovalDetail, DecisionRequest, RiskPolicy, RiskRule } from "../../api/types";

export type ApprovalFilter = "pending" | "decided" | "all";

const keys = {
  all: ["approvals"] as const,
  list: (f: ApprovalFilter) => ["approvals", "list", f] as const,
  one: (id: string) => ["approvals", "one", id] as const,
  policy: (projectId: string) => ["risk-policy", projectId] as const,
};

const path = (id: string) => `/api/approvals/${encodeURIComponent(id)}`;

export function useApprovals(filter: ApprovalFilter) {
  return useQuery({
    queryKey: keys.list(filter),
    queryFn: ({ signal }) => api<Approval[]>("GET", `/api/approvals?state=${filter}`, undefined, signal),
  });
}

export function useApproval(id: string) {
  return useQuery({ queryKey: keys.one(id), queryFn: ({ signal }) => api<ApprovalDetail>("GET", path(id), undefined, signal) });
}

/** Approve or reject the revision the Owner reviewed; every approvals view is refreshed afterwards. */
export function useDecide(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ approve, ...body }: DecisionRequest & { approve: boolean }) =>
      api<Approval>("POST", `${path(id)}/${approve ? "approve" : "reject"}`, body),
    onSettled: () => qc.invalidateQueries({ queryKey: keys.all }),
  });
}

const policyPath = (projectId: string) => `/api/projects/${encodeURIComponent(projectId)}/risk-policy`;

export function useRiskPolicy(projectId: string) {
  return useQuery({
    queryKey: keys.policy(projectId),
    queryFn: ({ signal }) => api<RiskPolicy>("GET", policyPath(projectId), undefined, signal),
  });
}

export function useSaveRiskPolicy(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (rules: RiskRule[]) => api<RiskPolicy>("PUT", policyPath(projectId), { rules }),
    onSuccess: (p) => qc.setQueryData(keys.policy(projectId), p),
  });
}
