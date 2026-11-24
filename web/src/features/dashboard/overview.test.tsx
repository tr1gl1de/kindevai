import { screen, within } from "@testing-library/react";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { Me, Runner, RunnerAvailability, RunnerStatus } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, renderApp } from "../../test/render";

const runner = (id: string, status: RunnerStatus, availability: RunnerAvailability): Runner => ({
  id, status, availability, fingerprint: "SHA256:x", publicKey: "k", hostname: id, os: "linux", arch: "amd64", version: "0.1.0",
  enrolledAt: "2026-11-24T10:00:00Z", approvedAt: null, disabledAt: null, revokedAt: null, lastSeenAt: null, projects: [],
  lastHeartbeatAt: null, capabilities: null,
});

const signedIn = (me: Me = owner) => server.use(http.get("/api/auth/me", () => json(me)));

describe("runners card on the overview", () => {
  it("shows live counts of online, unreachable and waiting-for-approval runners", async () => {
    signedIn();
    server.use(
      http.get("/api/runners", () =>
        json([
          runner("a", "approved", "online"),
          runner("b", "approved", "online"),
          runner("c", "approved", "unreachable"),
          runner("d", "pending", "never_connected"),
          runner("e", "revoked", "unreachable"), // gone for good: not counted
          runner("f", "disabled", "online"),
        ]),
      ),
    );
    const { container } = await renderApp("/");
    const card = await screen.findByRole("region", { name: "Runners" });
    const counts = await within(card).findByRole("list", { name: "Runner counts" });
    expect(within(counts).getByText("2 online")).toBeInTheDocument();
    expect(within(counts).getByText("1 unreachable")).toBeInTheDocument();
    expect(within(counts).getByText("1 waiting for approval")).toBeInTheDocument();
    expect(within(card).queryByText(/No runners/)).not.toBeInTheDocument();
    expect(within(card).getByRole("link", { name: "Open runners" })).toHaveAttribute("href", "/runners");
    await expectAccessible(container);
  });

  it("says so when no runner is enrolled and links to adding one", async () => {
    signedIn();
    await renderApp("/");
    const card = await screen.findByRole("region", { name: "Runners" });
    expect(await within(card).findByText(/No runners enrolled yet/)).toBeInTheDocument();
    expect(within(card).getByRole("link", { name: "Add a runner" })).toHaveAttribute("href", "/runners");
  });

  it("shows an error with retry", async () => {
    signedIn();
    server.use(http.get("/api/runners", () => apiError(500, "internal")));
    await renderApp("/");
    const card = await screen.findByRole("region", { name: "Runners" });
    expect(await within(card).findByRole("alert")).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

  it("is in Russian too", async () => {
    signedIn();
    server.use(http.get("/api/runners", () => json([runner("a", "approved", "online"), runner("d", "pending", "never_connected")])));
    await renderApp("/", "ru");
    const card = await screen.findByRole("region", { name: "Раннеры" });
    expect(await within(card).findByText("1 в сети")).toBeInTheDocument();
    expect(within(card).getByText("0 недоступно")).toBeInTheDocument();
    expect(within(card).getByText("1 ждёт одобрения")).toBeInTheDocument();
  });

  it("is not shown to members, who cannot see runners", async () => {
    signedIn({ ...owner, role: "user" });
    await renderApp("/");
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Runners" })).not.toBeInTheDocument();
  });
});
