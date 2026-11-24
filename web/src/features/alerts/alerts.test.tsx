import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { Alert, ApprovalDetail, Me, NotificationPreferences } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, renderApp } from "../../test/render";
import type { TelegramLaunch } from "../telegram/webapp";

const alice: Me = { id: "u2", login: "alice", role: "user", mustChangePassword: false };

const runner: Alert = {
  id: "A1",
  kind: "runner_unreachable",
  severity: "warning",
  subject: "r1",
  details: { runnerId: "r1" },
  state: "OPEN",
  occurrences: 3,
  firstSeenAt: "2026-11-25T10:00:00Z",
  lastSeenAt: "2026-11-25T10:20:00Z",
  ackedAt: null,
  closedAt: null,
  canManage: true,
};
const disk: Alert = {
  ...runner,
  id: "A2",
  kind: "storage_level",
  severity: "critical",
  subject: "m1/data",
  details: { usedPercent: "96" },
  occurrences: 1,
  projectId: "p1",
  projectName: "Alpha",
};

describe("alert center", () => {
  // KIN-59: the Control Plane's runner output storage is full.
  it("describes the runner output storage alert in English and Russian", async () => {
    const full: Alert = { ...runner, id: "A9", kind: "runner_output_full", subject: "control-plane",
      details: { limitBytes: "1073741824", limitMiB: "1024" } };
    server.use(http.get("/api/auth/me", () => json(owner)), http.get("/api/alerts", () => json([full])));
    const { unmount } = await renderApp("/alerts");
    expect(await screen.findByText("Runner output storage is full: new output is dropped")).toBeInTheDocument();
    unmount();
    await renderApp("/alerts", "ru");
    expect(await screen.findByText("Хранилище вывода раннеров заполнено: новый вывод отбрасывается")).toBeInTheDocument();
  });

  it("lists active alerts with severity and acknowledges one", async () => {
    let action = "";
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/alerts", () => json(action ? [{ ...runner, state: "ACKNOWLEDGED" }, disk] : [runner, disk])),
      http.post("/api/alerts/:id/:action", ({ params }) => {
        action = `${String(params.id)}/${String(params.action)}`;
        return json({ ...runner, state: "ACKNOWLEDGED" });
      }),
    );
    const { container } = await renderApp("/alerts");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Alerts" })).toBeInTheDocument();
    const row = (await screen.findByText("Runner r1 is unreachable (no heartbeats)")).closest("tr")!;
    expect(within(row).getByText("Warning")).toBeInTheDocument();
    expect(within(row).getByText("Platform")).toBeInTheDocument();
    expect(within(row).getByText("Seen 3 times")).toBeInTheDocument();
    const diskRow = screen.getByText("Disk m1/data is 96% full").closest("tr")!;
    expect(within(diskRow).getByText("Critical")).toBeInTheDocument();
    expect(within(diskRow).getByText("Alpha")).toBeInTheDocument();
    const nav = screen.getByRole("navigation", { name: "Main" });
    expect(within(nav).getByRole("link", { name: /Alerts.*2 open alerts/ })).toBeInTheDocument();
    await expectAccessible(container);

    await user.click(within(row).getByRole("button", { name: "Acknowledge: Runner r1 is unreachable (no heartbeats)" }));
    await expectStatus("Alert acknowledged.");
    expect(action).toBe("A1/acknowledge");
    await waitFor(() => expect(within(row).queryByRole("button", { name: /^Acknowledge/ })).not.toBeInTheDocument());
    expect(within(row).getByText("Acknowledged")).toBeInTheDocument();
  });

  it("shows project alerts read-only to a member and explains errors", async () => {
    server.use(
      http.get("/api/auth/me", () => json(alice)),
      http.get("/api/alerts", () => json([{ ...disk, canManage: false }])),
    );
    await renderApp("/alerts");
    const row = (await screen.findByText("Disk m1/data is 96% full")).closest("tr")!;
    expect(within(row).queryByRole("button")).not.toBeInTheDocument();
  });

  it("reports a failed action", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/alerts", () => json([runner])),
      http.post("/api/alerts/A1/resolve", () => apiError(403, "forbidden")),
    );
    await renderApp("/alerts");
    await userEvent.setup().click(await screen.findByRole("button", { name: /^Resolve/ }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/permission/i);
  });

  it("shows an empty state and works in Russian", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    const first = await renderApp("/alerts");
    expect(await screen.findByText("No active alerts.")).toBeInTheDocument();
    first.unmount();
    server.use(http.get("/api/alerts", () => json([disk])));
    await renderApp("/alerts", "ru");
    expect(await screen.findByRole("heading", { level: 1, name: "Алерты" })).toBeInTheDocument();
    expect(await screen.findByText("Диск m1/data заполнен на 96%")).toBeInTheDocument();
    expect(screen.getByText("Критично")).toBeInTheDocument();
  });
});

describe("notification preferences", () => {
  it("turns a notification type off and picks the language", async () => {
    let saved: unknown;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/notifications/preferences", () =>
        json<NotificationPreferences>({ language: "en", disabled: [], types: ["approval_requested", "alert_raised", "task_finished"], telegramLinked: true }),
      ),
      http.put("/api/notifications/preferences", async ({ request }) => {
        saved = await request.json();
        return json({ ...(saved as object), types: ["approval_requested", "alert_raised", "task_finished"], telegramLinked: true });
      }),
    );
    const { container } = await renderApp("/account/password");
    const user = userEvent.setup();
    const section = await screen.findByRole("region", { name: "Notifications" });
    const box = await within(section).findByRole("checkbox", { name: "New or escalated alerts" });
    expect(box).toBeChecked();
    await user.click(box);
    await user.selectOptions(within(section).getByLabelText("Notification language"), "ru");
    await user.click(within(section).getByRole("button", { name: "Save notifications" }));
    await expectStatus("Notification settings saved.");
    expect(saved).toEqual({ language: "ru", disabled: ["alert_raised"] });
    await expectAccessible(container);
  });
});

describe("Telegram deep links", () => {
  const launch = (startParam: string): TelegramLaunch => ({ initData: "query_id=x&hash=abc", startParam, languageCode: "en" });

  it("opens the approval from a notification after the Mini App sign-in, where deciding happens", async () => {
    const detail = {
      approval: { id: "AP1", projectId: "p1", projectName: "Alpha", taskId: "t1", taskTitle: "Add billing", planRevision: 1,
        planHash: "1".repeat(64), scope: "execute", reason: "plan", paths: [], risk: "L2", findings: [], state: "pending",
        requestedAt: "2026-11-25T10:00:00Z", decidedAt: null },
      task: { id: "t1", title: "Add billing", state: "AWAITING_APPROVAL", planRevision: 1 },
      revision: { number: 1, hash: "1".repeat(64), risk: "L2", createdBy: "planner", createdAt: "2026-11-25T10:00:00Z",
        plan: { summary: "s", risk: "L2", maxParallel: 1, steps: [] } },
      previous: null, changes: [], related: [], canDecide: true,
    } as unknown as ApprovalDetail;
    server.use(http.get("/api/auth/me", () => json(owner)), http.get("/api/approvals/AP1", () => json(detail)));
    await renderApp("/", "en", launch("approval_AP1"));
    expect(await screen.findByRole("heading", { level: 1, name: "Add billing" })).toBeInTheDocument();
  });

  it("opens the alert center from an alert notification", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)), http.get("/api/alerts", () => json([runner])));
    await renderApp("/", "en", launch("alert_A1"));
    expect(await screen.findByRole("heading", { level: 1, name: "Alerts" })).toBeInTheDocument();
  });
});
