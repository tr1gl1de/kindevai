import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { CreateProjectRequest, Member, Project, ProjectRole } from "../../api/types";

export const keys = {
  all: ["projects"] as const,
  one: (id: string) => ["projects", id] as const,
  members: (id: string) => ["projects", id, "members"] as const,
};

export function useProjects() {
  return useQuery({ queryKey: keys.all, queryFn: ({ signal }) => api<Project[]>("GET", "/api/projects", undefined, signal) });
}

export function useProject(id: string) {
  return useQuery({
    queryKey: keys.one(id),
    queryFn: ({ signal }) => api<Project>("GET", `/api/projects/${encodeURIComponent(id)}`, undefined, signal),
  });
}

export function useMembers(id: string, enabled: boolean) {
  return useQuery({
    queryKey: keys.members(id),
    enabled,
    queryFn: ({ signal }) => api<Member[]>("GET", `/api/projects/${encodeURIComponent(id)}/members`, undefined, signal),
  });
}

export function useCreateProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: CreateProjectRequest) => api<Project>("POST", "/api/projects", req),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.all }),
  });
}

export function useSetMember(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ login, role }: { login: string; role: ProjectRole }) =>
      api<Member>("PUT", `/api/projects/${encodeURIComponent(projectId)}/members/${encodeURIComponent(login)}`, { role }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.members(projectId) }),
  });
}

export function useRemoveMember(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (login: string) =>
      api<void>("DELETE", `/api/projects/${encodeURIComponent(projectId)}/members/${encodeURIComponent(login)}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.members(projectId) }),
  });
}

const translit: Record<string, string> = {
  а: "a", б: "b", в: "v", г: "g", д: "d", е: "e", ё: "e", ж: "zh", з: "z", и: "i", й: "y", к: "k", л: "l", м: "m",
  н: "n", о: "o", п: "p", р: "r", с: "s", т: "t", у: "u", ф: "f", х: "h", ц: "ts", ч: "ch", ш: "sh", щ: "sch",
  ъ: "", ы: "y", ь: "", э: "e", ю: "yu", я: "ya",
};

/** Suggests a slug from a project name (Cyrillic is transliterated). */
export function slugify(name: string): string {
  return [...name.toLowerCase()]
    .map((c) => translit[c] ?? c)
    .join("")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 40)
    .replace(/-+$/g, "");
}
