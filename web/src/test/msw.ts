import { HttpResponse, http } from "msw";
import { setupServer } from "msw/node";
import type { Health } from "../api/types";

/** Default API handlers; tests override them with server.use(...). */
export const handlers = [http.get("/api/health", () => HttpResponse.json<Health>({ status: "ok", version: "test" }))];

export const server = setupServer(...handlers);
