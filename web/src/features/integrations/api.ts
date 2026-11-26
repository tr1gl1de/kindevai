import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type {
  GitHubApp,
  GitHubRequest,
  Integrations,
  ProjectIntegrations,
  ProjectRepository,
  RepositoryRequest,
  YouTrackConnection,
  YouTrackRequest,
} from "../../api/types";

const keys = {
  all: ["integrations"] as const,
  project: (id: string) => ["integrations", "project", id] as const,
};

/** How often the page asks for the result of a running connection test. */
export const CHECK_POLL_MS = 1500;

export function useIntegrations() {
  return useQuery({
    queryKey: keys.all,
    queryFn: ({ signal }) => api<Integrations>("GET", "/api/integrations", undefined, signal),
    // The test runs in the background (outbox); poll until it has a result.
    refetchInterval: (q) =>
      q.state.data?.youtrack?.check.status === "pending" || q.state.data?.github?.check.status === "pending" ? CHECK_POLL_MS : false,
  });
}

function useYouTrackMutation<V>(fn: (v: V) => Promise<YouTrackConnection>) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    // The request may carry the token: do not keep variables around in the mutation cache.
    gcTime: 0,
    onSuccess: (yt) => qc.setQueryData<Integrations>(keys.all, (old) => ({ github: null, ...old, youtrack: yt })),
  });
}

export function useSaveYouTrack() {
  return useYouTrackMutation((req: YouTrackRequest) => api<YouTrackConnection>("PUT", "/api/integrations/youtrack", req));
}

export function useCheckYouTrack() {
  return useYouTrackMutation(() => api<YouTrackConnection>("POST", "/api/integrations/youtrack/check"));
}

export function useDisconnectYouTrack() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api<void>("DELETE", "/api/integrations/youtrack"),
    onSuccess: () => qc.setQueryData<Integrations>(keys.all, (old) => ({ github: null, ...old, youtrack: null })),
  });
}

const projectPath = (id: string) => `/api/projects/${encodeURIComponent(id)}`;

function useGitHubMutation<V>(fn: (v: V) => Promise<GitHubApp>) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    // The request may carry the private key: do not keep variables around in the mutation cache.
    gcTime: 0,
    onSuccess: (gh) => {
      qc.setQueryData<Integrations>(keys.all, (old) => ({ youtrack: null, ...old, github: gh }));
      // A new key re-reads every repository from GitHub.
      void qc.invalidateQueries({ queryKey: ["integrations", "project"] });
    },
  });
}

export function useSaveGitHub() {
  return useGitHubMutation((req: GitHubRequest) => api<GitHubApp>("PUT", "/api/integrations/github", req));
}

export function useCheckGitHub() {
  return useGitHubMutation(() => api<GitHubApp>("POST", "/api/integrations/github/check"));
}

export function useDisconnectGitHub() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api<void>("DELETE", "/api/integrations/github"),
    onSuccess: () => qc.setQueryData<Integrations>(keys.all, (old) => ({ youtrack: null, ...old, github: null })),
  });
}

export function useProjectIntegrations(id: string) {
  return useQuery({
    queryKey: keys.project(id),
    queryFn: ({ signal }) => api<ProjectIntegrations>("GET", `${projectPath(id)}/integrations`, undefined, signal),
    // Reads from GitHub run in the background (outbox); poll until they have a result.
    refetchInterval: (q) => (q.state.data?.repositories.some((r) => r.github?.sync.status === "pending") ? CHECK_POLL_MS : false),
  });
}

export function useSyncRepository(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (repoId: string) => api<ProjectRepository>("POST", `${projectPath(id)}/repositories/${encodeURIComponent(repoId)}/sync`),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.project(id) }),
  });
}

export function useBindYouTrackProject(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (shortName: string) => api<ProjectIntegrations>("PUT", `${projectPath(id)}/integrations/youtrack`, { shortName }),
    onSuccess: (pi) => qc.setQueryData(keys.project(id), pi),
  });
}

export function useAddRepository(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: RepositoryRequest) => api<ProjectRepository>("POST", `${projectPath(id)}/repositories`, req),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.project(id) }),
  });
}

export function useUpdateRepository(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ repoId, integrationBranch, testCommand }: { repoId: string; integrationBranch: string; testCommand?: string }) =>
      api<ProjectRepository>("PUT", `${projectPath(id)}/repositories/${encodeURIComponent(repoId)}`, { integrationBranch, testCommand }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.project(id) }),
  });
}

export function useRemoveRepository(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (repoId: string) => api<void>("DELETE", `${projectPath(id)}/repositories/${encodeURIComponent(repoId)}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.project(id) }),
  });
}
