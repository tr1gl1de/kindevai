import { HttpResponse, http } from "msw";
import { describe, expect, it } from "vitest";
import { server } from "../test/msw";
import { api } from "./client";

describe("api", () => {
  it("returns JSON and sends JSON bodies", async () => {
    server.use(http.post("/api/echo", async ({ request }) => HttpResponse.json({ got: await request.json(), ct: request.headers.get("content-type") })));
    await expect(api("POST", "/api/echo", { a: 1 })).resolves.toEqual({ got: { a: 1 }, ct: "application/json" });
  });

  it("maps error bodies to ApiError with code and field", async () => {
    server.use(http.post("/api/x", () => HttpResponse.json({ error: "validation", message: "m", field: "slug" }, { status: 422 })));
    await expect(api("POST", "/api/x", {})).rejects.toMatchObject({ status: 422, code: "validation", field: "slug" });
  });

  it("handles 204 and non-JSON errors", async () => {
    server.use(
      http.delete("/api/x", () => new HttpResponse(null, { status: 204 })),
      http.get("/api/x", () => new HttpResponse("<html>bad gateway</html>", { status: 502 })),
    );
    await expect(api("DELETE", "/api/x")).resolves.toBeUndefined();
    await expect(api("GET", "/api/x")).rejects.toMatchObject({ status: 502, code: "unknown" });
  });

  it("reports network failures as status 0", async () => {
    server.use(http.get("/api/down", () => HttpResponse.error()));
    await expect(api("GET", "/api/down")).rejects.toMatchObject({ status: 0, code: "network" });
  });
});
