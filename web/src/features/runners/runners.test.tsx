import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { EnrollmentToken, Me, Project, Runner } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, renderApp } from "../../test/render";
import { needsHttpsForRemote } from "./api";

const TOKEN = "rtok_abcdefghijklmnopqrstuvwxyz0123456789ABCDEF";
const FP = "SHA256:WdYR/VIHb2knhxnmd3MKU6WpNAryvOmk4PylABREeIs";
const alpha: Project = { id: "p1", slug: "alpha", name: "Alpha", createdAt: "2026-11-24T10:00:00Z", myRole: "owner" };
const beta: Project = { id: "p2", slug: "beta", name: "Beta", createdAt: "2026-11-24T10:00:00Z", myRole: "owner" };

const pending: Runner = {
  id: "r1",
  status: "pending",
  fingerprint: FP,
  publicKey: "2hznQhRgvaYtn8K3hLubpx7uDIisg76parXO5pADfQA=",
  hostname: "wsl-box",
  os: "linux",
  arch: "amd64",
  version: "0.1.0",
  enrolledAt: "2026-11-24T10:00:00Z",
  approvedAt: null,
  disabledAt: null,
  revokedAt: null,
  lastSeenAt: null,
  projects: [],
  availability: "never_connected",
  lastHeartbeatAt: null,
  capabilities: null,
};

describe("runners list (owner)", () => {
  it("adds a runner with a one-time token and an install hint without the token", async () => {
    const tokens: EnrollmentToken[] = [];
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/runners", () => json([pending, { ...pending, id: "r2", hostname: "mac-mini", os: "darwin", arch: "arm64", status: "approved", projects: [{ id: "p1", name: "Alpha" }] }])),
      http.get("/api/runners/enrollment-tokens", () => json(tokens)),
      http.post("/api/runners/enrollment-tokens", () => {
        const tok: EnrollmentToken = { id: "t1", state: "pending", createdAt: "2026-11-24T10:00:00Z", expiresAt: "2026-11-24T10:10:00Z" };
        tokens.push(tok);
        return json({ enrollmentToken: tok, token: TOKEN }, 201);
      }),
      http.post("/api/runners/enrollment-tokens/t1/revoke", () => {
        tokens[0] = { ...tokens[0]!, state: "revoked" };
        return new Response(null, { status: 204 });
      }),
    );
    const { container, client } = await renderApp("/runners");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Runners" })).toBeInTheDocument();
    const row = (await screen.findByRole("link", { name: "wsl-box" })).closest("tr")!;
    expect(within(row).getByText("Waiting for approval")).toBeInTheDocument();
    expect(within(row).getByText(FP)).toBeInTheDocument();
    expect(within(row).getByText("linux/amd64")).toBeInTheDocument();
    const mac = screen.getByRole("link", { name: "mac-mini" }).closest("tr")!;
    expect(within(mac).getByText("Approved")).toBeInTheDocument();
    expect(within(mac).getByText("Alpha")).toBeInTheDocument();
    await expectAccessible(container);

    await user.click(screen.getByRole("button", { name: "Add runner" }));
    const secret = await screen.findByRole("region", { name: "Enrollment token" });
    expect(within(secret).getByRole("heading", { name: "Enrollment token" })).toHaveFocus();
    expect(within(secret).getByText(TOKEN)).toBeInTheDocument();
    const command = within(secret).getByText("ai-runner enroll --server http://localhost:3000");
    expect(command.textContent).not.toContain(TOKEN); // the token never goes on the command line
    expect(within(secret).getByRole("button", { name: "Copy token" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add runner" })).toBeDisabled();
    await expectAccessible(container);
    await user.click(within(secret).getByRole("button", { name: "Done" }));
    expect(screen.queryByText(TOKEN)).not.toBeInTheDocument();
    expect(JSON.stringify(client.getMutationCache().getAll().map((m) => m.state.data))).not.toContain(TOKEN);

    const tokenRow = (await screen.findByRole("cell", { name: "Waiting" })).closest("tr")!;
    await user.click(within(tokenRow).getByRole("button", { name: "Revoke" }));
    const dialog = screen.getByRole("alertdialog", { name: "Revoke this enrollment token?" });
    await user.click(within(dialog).getByRole("button", { name: "Revoke" }));
    await expectStatus("The enrollment token is revoked.");
  });

  it("explains why a token can no longer be revoked", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/runners", () => json([])),
      http.get("/api/runners/enrollment-tokens", () => json([{ id: "t1", state: "pending", createdAt: "2026-11-24T10:00:00Z", expiresAt: "2026-11-24T10:10:00Z" }])),
      http.post("/api/runners/enrollment-tokens/t1/revoke", () => json({ error: "validation", message: "token_not_pending", field: "token" }, 422)),
    );
    await renderApp("/runners");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Revoke" }));
    await user.click(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Revoke" }));
    expect(await screen.findByText(/no longer waiting: it was used, revoked or has expired/)).toBeInTheDocument();
  });

  it("explains the empty state and works in Russian", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/runners", () => json([])),
      http.get("/api/runners/enrollment-tokens", () => json([])),
    );
    const { container } = await renderApp("/runners", "ru");
    expect(await screen.findByRole("heading", { level: 1, name: "Раннеры" })).toBeInTheDocument();
    expect(await screen.findByText("Раннеров пока нет.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Добавить раннер" })).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("is not offered to other users", async () => {
    const member: Me = { id: "u2", login: "bob", role: "user", mustChangePassword: false };
    server.use(http.get("/api/auth/me", () => json(member)));
    await renderApp("/runners");
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Runners" })).not.toBeInTheDocument();
  });
});

describe("runner card (owner)", () => {
  it("shows the fingerprint to verify and approves with explicit projects", async () => {
    let runner = pending;
    let approved: unknown;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha, beta])),
      http.get("/api/runners/r1", () => json(runner)),
      http.post("/api/runners/r1/approve", async ({ request }) => {
        approved = await request.json();
        runner = { ...runner, status: "approved", approvedAt: "2026-11-24T10:05:00Z", projects: [{ id: "p2", name: "Beta" }] };
        return json(runner);
      }),
      http.post("/api/runners/r1/disable", () => {
        runner = { ...runner, status: "disabled" };
        return json(runner);
      }),
    );
    const { container } = await renderApp("/runners/r1");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "wsl-box" })).toBeInTheDocument();
    const verify = screen.getByRole("region", { name: "Verify the fingerprint" });
    expect(within(verify).getByText(FP)).toBeInTheDocument();
    expect(within(verify).getByText("ai-runner fingerprint")).toBeInTheDocument();
    await expectAccessible(container);

    await user.click(screen.getByRole("button", { name: "Approve runner" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Choose at least one project.");
    expect(screen.getByRole("checkbox", { name: "Alpha" })).toHaveFocus();
    await user.click(screen.getByRole("checkbox", { name: "Beta" }));
    await user.click(screen.getByRole("button", { name: "Approve runner" }));
    await expectStatus("wsl-box is approved.");
    expect(approved).toEqual({ projectIds: ["p2"] });
    expect(screen.getByText("Approved")).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "Beta" })).toBeChecked();

    await user.click(screen.getByRole("button", { name: "Disable" }));
    await user.click(within(screen.getByRole("alertdialog", { name: "Disable wsl-box?" })).getByRole("button", { name: "Disable" }));
    await expectStatus("wsl-box is disabled.");
    expect(screen.getByRole("button", { name: "Enable" })).toBeInTheDocument();
  });

  it("rejects a pending runner by revoking it", async () => {
    let runner = pending;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/runners/r1", () => json(runner)),
      http.post("/api/runners/r1/revoke", () => {
        runner = { ...runner, status: "revoked", revokedAt: "2026-11-24T10:05:00Z" };
        return json(runner);
      }),
    );
    await renderApp("/runners/r1");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Revoke" }));
    const dialog = screen.getByRole("alertdialog", { name: "Revoke wsl-box?" });
    await user.click(within(dialog).getByRole("button", { name: "Revoke" }));
    await expectStatus("wsl-box is revoked.");
    expect(screen.getByText(/can never connect again/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve runner" })).not.toBeInTheDocument();
  });

  it("shows not found for an unknown runner", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)), http.get("/api/runners/zz", () => apiError(404, "not_found")));
    await renderApp("/runners/zz");
    expect(await screen.findByRole("heading", { name: "Runner not found" })).toBeInTheDocument();
  });
});

describe("needsHttpsForRemote", () => {
  it("warns only for plain HTTP on a non-loopback host", () => {
    expect(needsHttpsForRemote("http://192.168.1.5:8080")).toBe(true);
    expect(needsHttpsForRemote("http://localhost:8080")).toBe(false);
    expect(needsHttpsForRemote("http://127.0.0.1:8080")).toBe(false);
    expect(needsHttpsForRemote("https://ai.example.com")).toBe(false);
  });
});
