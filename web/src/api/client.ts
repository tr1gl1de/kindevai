import type { ApiErrorBody, Health } from "./types";

/** A failed API call. status 0 means the server could not be reached. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly field?: string,
    /** Machine detail from the body's message (e.g. a validation code). */
    readonly detail?: string,
  ) {
    super(`${status} ${code}`);
    this.name = "ApiError";
  }
}

export function isApiError(e: unknown, code?: string): e is ApiError {
  return e instanceof ApiError && (code === undefined || e.code === code);
}

/** JSON request to the Control Plane (same origin, session cookie). */
export async function api<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      signal,
      credentials: "same-origin",
      headers: body === undefined ? { Accept: "application/json" } : { Accept: "application/json", "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (e) {
    if (signal?.aborted) throw e;
    throw new ApiError(0, "network");
  }
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  let data: unknown;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    data = undefined;
  }
  if (!res.ok) {
    const err = data as Partial<ApiErrorBody> | undefined;
    throw new ApiError(res.status, err?.error ?? "unknown", err?.field, err?.message);
  }
  return data as T;
}

export function fetchHealth(signal?: AbortSignal): Promise<Health> {
  return api<Health>("GET", "/api/health", undefined, signal);
}
