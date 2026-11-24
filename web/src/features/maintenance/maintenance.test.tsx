import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { Me, MaintenanceStatus } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, renderApp } from "../../test/render";

const alice: Me = { id: "u2", login: "alice", role: "user", mustChangePassword: false };
const on: MaintenanceStatus = { enabled: true, reason: "Moving servers", since: "2026-11-25T12:00:00Z", by: "owner", activeAttempts: 2 };
const off: MaintenanceStatus = { enabled: false, reason: "", since: null, by: "", activeAttempts: 0 };

describe("maintenance mode", () => {
  it("shows every user a read-only banner with the reason and the drain progress", async () => {
    server.use(
      http.get("/api/auth/me", () => json(alice)),
      http.get("/api/maintenance", () => json(on)),
    );
    const { container } = await renderApp("/");
    const banner = await screen.findByRole("region", { name: "Maintenance mode" });
    expect(banner).toHaveTextContent("The platform is read-only");
    expect(banner).toHaveTextContent("Reason: Moving servers");
    expect(banner).toHaveTextContent("2 steps are still running.");
    expect(within(banner).queryByRole("link")).toBeNull(); // only the Owner manages it
    await expectAccessible(container);
  });

  it("shows no banner when the mode is off", async () => {
    server.use(http.get("/api/auth/me", () => json(alice)));
    await renderApp("/");
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Maintenance mode" })).toBeNull();
  });

  it("lets the Owner switch it on with a reason and off again (RU)", async () => {
    let state = off;
    const sent: unknown[] = [];
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/maintenance", () => json(state)),
      http.put("/api/maintenance", async ({ request }) => {
        const body = (await request.json()) as { enabled: boolean; reason: string };
        sent.push(body);
        state = body.enabled ? { ...on, reason: body.reason } : off;
        return json(state);
      }),
    );
    const { container } = await renderApp("/settings/maintenance", "ru");
    expect(await screen.findByRole("heading", { level: 1, name: "Режим обслуживания" })).toBeInTheDocument();
    await expectAccessible(container);
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText("Причина (видна всем)"), "Переезд");
    await user.click(screen.getByRole("button", { name: "Включить режим обслуживания" }));
    await expectStatus("Режим обслуживания включён.");
    expect(await screen.findByRole("region", { name: "Режим обслуживания" })).toHaveTextContent("Причина: Переезд");
    await user.click(screen.getByRole("button", { name: "Выключить режим обслуживания" }));
    await expectStatus("Режим обслуживания выключен.");
    expect(sent).toEqual([{ enabled: true, reason: "Переезд" }, { enabled: false, reason: "" }]);
  });

  it("explains a change refused in maintenance mode", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/maintenance", () => json(on)),
      http.put("/api/maintenance", () => apiError(503, "maintenance")),
    );
    await renderApp("/settings/maintenance");
    await userEvent.setup().click(await screen.findByRole("button", { name: "Update reason" }));
    expect(await screen.findByText(/changes are paused\. Try again/)).toBeInTheDocument();
  });
});
