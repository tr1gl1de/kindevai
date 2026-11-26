import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type {
  AgentBindingRequest,
  AgentCli,
  AgentRole,
  AgentSettings,
  ProjectAgentSettings,
} from "../../api/types";

/** Scope of an agent configuration: the System level (projectId undefined) or one project. */
export type AgentScope = { projectId?: string };

const keys = {
  global: ["agents"] as const,
  project: (id: string) => ["agents", "project", id] as const,
};

const base = ({ projectId }: AgentScope) =>
  projectId === undefined
    ? "/api/agents"
    : `/api/projects/${encodeURIComponent(projectId)}/agents`;

export function useAgentSettings() {
  return useQuery({
    queryKey: keys.global,
    queryFn: ({ signal }) =>
      api<AgentSettings>("GET", "/api/agents", undefined, signal),
  });
}

export function useProjectAgents(projectId: string) {
  return useQuery({
    queryKey: keys.project(projectId),
    queryFn: ({ signal }) =>
      api<ProjectAgentSettings>("GET", base({ projectId }), undefined, signal),
  });
}

function useInvalidateAgents() {
  const qc = useQueryClient();
  return () => qc.invalidateQueries({ queryKey: keys.global });
}

export function useSetAgentSwitch(scope: AgentScope) {
  const invalidate = useInvalidateAgents();
  return useMutation({
    mutationFn: ({ agent, enabled }: { agent: AgentCli; enabled: boolean }) =>
      api<void>("PUT", `${base(scope)}/switches/${agent}`, { enabled }),
    onSuccess: invalidate,
  });
}

export function useEmergencyStop() {
  const invalidate = useInvalidateAgents();
  return useMutation({
    mutationFn: (agent: AgentCli) =>
      api<{ stopped: number }>(
        "POST",
        `/api/agents/switches/${agent}/emergency-stop`,
      ),
    onSuccess: invalidate,
  });
}

/** The Owner's "CLI ready again" for a runner's held CLI (KIN-83). */
export function useMarkCLIReady() {
  const invalidate = useInvalidateAgents();
  return useMutation({
    mutationFn: ({ runnerId, agent }: { runnerId: string; agent: AgentCli }) =>
      api<void>(
        "POST",
        `/api/agents/cooldowns/${encodeURIComponent(runnerId)}/${agent}/ready`,
      ),
    onSuccess: invalidate,
  });
}

export function useSaveBinding(scope: AgentScope) {
  const invalidate = useInvalidateAgents();
  return useMutation({
    mutationFn: ({
      role,
      body,
    }: {
      role: AgentRole;
      body: AgentBindingRequest;
    }) => api<void>("PUT", `${base(scope)}/bindings/${role}`, body),
    onSuccess: invalidate,
  });
}

export function useResetBinding(scope: AgentScope) {
  const invalidate = useInvalidateAgents();
  return useMutation({
    mutationFn: (role: AgentRole) =>
      api<void>("DELETE", `${base(scope)}/bindings/${role}`),
    onSuccess: invalidate,
  });
}
