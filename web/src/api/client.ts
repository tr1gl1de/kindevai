import type { Health } from "./types";

export async function fetchHealth(signal?: AbortSignal): Promise<Health> {
  const res = await fetch("/api/health", { signal, headers: { Accept: "application/json" } });
  if (!res.ok) throw new Error(`health check failed: ${res.status}`);
  return (await res.json()) as Health;
}
