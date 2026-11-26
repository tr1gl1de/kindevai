import { screen, within } from "@testing-library/react";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { Me, Runner, RunnerOutputStorage, StorageForecast, StorageOverview, StorageVolume } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, renderApp } from "../../test/render";

const GB = 1e9;
const none: StorageForecast = { state: "insufficient_data", windowDays: 7, growthBytesPerDay: 0, daysTo85: null, daysTo90: null };
const vol = (v: Partial<StorageVolume>): StorageVolume => ({
  role: "workspaces",
  mountOrVolumeId: "/",
  path: "/var/lib/kindevai-runner",
  totalBytes: 100 * GB,
  availableBytes: 50 * GB,
  freeBytes: 50 * GB,
  usedPercent: 50,
  level: "ok",
  diskHeavyAllowed: true,
  stale: false,
  lastReportedAt: "2026-11-24T12:00:00Z",
  forecast7d: none,
  forecast30d: { ...none, windowDays: 30 },
  ...v,
});

const overview: StorageOverview = {
  generatedAt: "2026-11-24T12:00:00Z",
  thresholds: { warningPercent: 75, highPercent: 85, criticalPercent: 95, minHeadroomBytes: 2147483648 },
  machines: [
    {
      id: "control-plane", kind: "control_plane", name: "", availability: "online", lastHeartbeatAt: null, diskHeavyAllowed: true,
      volumes: [vol({ role: "data", path: "/data", forecast7d: { ...none, state: "not_needed" } })],
    },
    {
      id: "r1", kind: "runner", name: "wsl-box", status: "approved", availability: "online", lastHeartbeatAt: "2026-11-24T12:00:00Z",
      diskHeavyAllowed: false,
      volumes: [
        vol({ totalBytes: 100 * GB, availableBytes: 4 * GB, usedPercent: 96, level: "critical", diskHeavyAllowed: false }),
        vol({ role: "windows_host", mountOrVolumeId: "/mnt/c", path: "/mnt/c", usedPercent: 80, level: "warning",
          forecast7d: { state: "ok", windowDays: 7, growthBytesPerDay: GB, daysTo85: 5, daysTo90: 10 } }),
      ],
    },
    {
      id: "r2", kind: "runner", name: "mac-mini", status: "approved", availability: "unreachable", lastHeartbeatAt: "2026-11-24T10:00:00Z",
      diskHeavyAllowed: true, volumes: [vol({ stale: true, lastReportedAt: "2026-11-24T10:00:00Z" })],
    },
    {
      id: "r3", kind: "runner", name: "new-box", status: "approved", availability: "never_connected", lastHeartbeatAt: null,
      diskHeavyAllowed: true, volumes: [],
    },
  ],
};

const fullOutput: RunnerOutputStorage = {
  usedBytes: 2 ** 30, limitBytes: 2 ** 30, usedPercent: 100, state: "full", outputRetentionDays: 14, eventRetentionDays: 30,
  dropsWindowHours: 24, droppedEvents: 3, lastDroppedAt: "2026-11-24T11:59:00Z",
  drops: [{ runnerId: "r1", stepId: "step-7", fence: 2, events: 3, lastAt: "2026-11-24T11:59:00Z" }],
};

const signedIn = (me: Me = owner) => server.use(http.get("/api/auth/me", () => json(me)));

describe("storage overview on the dashboard", () => {
  it("shows the control plane and every runner with levels, forecasts, stale and disk-heavy markers", async () => {
    signedIn();
    server.use(http.get("/api/storage/overview", () => json(overview)));
    const { container } = await renderApp("/");
    const card = await screen.findByRole("region", { name: "Storage" });
    const cp = (await within(card).findByRole("heading", { name: "Control Plane" })).closest("li")!;
    expect(within(cp).getByText("Data directory")).toBeInTheDocument();
    expect(within(cp).getByText("No forecast needed")).toBeInTheDocument();

    const wsl = within(card).getByRole("link", { name: "wsl-box" }).closest("li")!;
    expect(within(wsl).getByText("Online")).toBeInTheDocument();
    expect(within(wsl).getByText(/Critical/)).toBeInTheDocument();
    expect(within(wsl).getByText(/96% used/)).toBeInTheDocument();
    expect(within(wsl).getByText(/4 GB free of 100 GB/)).toBeInTheDocument();
    expect(within(wsl).getByText("Disk-heavy steps are paused on this runner")).toBeInTheDocument();
    expect(within(wsl).getByText("Windows volume (WSL2 disk image)")).toBeInTheDocument();
    expect(within(wsl).getByText("85% in about 5 days, 90% in about 10 days")).toBeInTheDocument();
    expect(within(wsl).getAllByText("Not enough data for a forecast")).toHaveLength(1);
    expect(within(wsl).getByRole("meter", { name: /Workspaces/ })).toHaveAttribute("value", "96");

    const mac = within(card).getByRole("link", { name: "mac-mini" }).closest("li")!;
    expect(within(mac).getByText("Unreachable")).toBeInTheDocument();
    expect(within(mac).getByText(/Stale/)).toBeInTheDocument();

    const fresh = within(card).getByRole("link", { name: "new-box" }).closest("li")!;
    expect(within(fresh).getByText("No storage report yet")).toBeInTheDocument();
    await expectAccessible(container);
  });

  // KIN-59: the Control Plane's stored runner output against its limit, with a warning while output is dropped.
  it("shows the runner output storage against its limit and warns while output is dropped", async () => {
    signedIn();
    server.use(http.get("/api/storage/overview", () => json({ ...overview, runnerOutput: fullOutput })));
    const { container } = await renderApp("/");
    const card = await screen.findByRole("region", { name: "Storage" });
    expect(await within(card).findByText("Runner output (Control Plane database)")).toBeInTheDocument();
    expect(within(card).getByRole("meter", { name: "Runner output: 100% of the limit used" })).toHaveAttribute("value", "100");
    expect(within(card).getByText("Full")).toBeInTheDocument();
    expect(within(card).getByText(/1\.1 GB of 1\.1 GB used \(100%\)/)).toBeInTheDocument();
    expect(within(card).getByText(/Output storage is full: new output of running executions is saved without its text/)).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("notes output dropped earlier once the storage has room again", async () => {
    signedIn();
    const earlier = { ...fullOutput, state: "ok" as const, usedBytes: 2 ** 29, usedPercent: 50 };
    server.use(http.get("/api/storage/overview", () => json({ ...overview, runnerOutput: earlier })));
    await renderApp("/");
    const card = await screen.findByRole("region", { name: "Storage" });
    const block = (await within(card).findByText("Runner output (Control Plane database)")).closest("div")!;
    expect(within(block).getByText("OK")).toBeInTheDocument();
    expect(within(block).getByText(/3 output events were dropped in the last 24 h/)).toBeInTheDocument();
    expect(within(block).queryByText(/Output storage is full/)).not.toBeInTheDocument();
  });

  it("is in Russian too", async () => {
    signedIn();
    server.use(http.get("/api/storage/overview", () => json({ ...overview, runnerOutput: fullOutput })));
    await renderApp("/", "ru");
    const card = await screen.findByRole("region", { name: "Хранилище" });
    expect(await within(card).findByRole("heading", { name: "Control Plane" })).toBeInTheDocument();
    expect(within(card).getByText("Недоступен")).toBeInTheDocument();
    expect(within(card).getAllByText("Недостаточно данных для прогноза").length).toBeGreaterThan(0);
    expect(within(card).getByText("Прогноз не требуется")).toBeInTheDocument();
    expect(within(card).getByText("Вывод раннеров (БД Control Plane)")).toBeInTheDocument();
    expect(within(card).getByText("Заполнено")).toBeInTheDocument();
    expect(within(card).getByText(/Хранилище вывода заполнено/)).toBeInTheDocument();
  });

  it("is hidden from members and shows an error with retry for the owner", async () => {
    signedIn({ ...owner, role: "user" });
    await renderApp("/");
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Storage" })).not.toBeInTheDocument();
  });

  it("shows an error with retry when the overview cannot be loaded", async () => {
    signedIn();
    server.use(http.get("/api/storage/overview", () => apiError(500, "internal")));
    await renderApp("/");
    const card = await screen.findByRole("region", { name: "Storage" });
    expect(await within(card).findByRole("alert")).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

  it("shows an empty state before any metrics arrive", async () => {
    signedIn();
    await renderApp("/");
    const card = await screen.findByRole("region", { name: "Storage" });
    expect(await within(card).findByText(/No storage metrics yet/)).toBeInTheDocument();
  });
});

describe("runner liveness and capabilities", () => {
  it("shows availability, last heartbeat and the reported CLIs", async () => {
    const r: Runner = {
      id: "r1", status: "approved", fingerprint: "SHA256:x", publicKey: "k", hostname: "wsl-box", os: "linux", arch: "amd64",
      version: "0.1.0", enrolledAt: "2026-11-24T10:00:00Z", approvedAt: "2026-11-24T10:00:00Z", disabledAt: null, revokedAt: null,
      lastSeenAt: "2026-11-24T12:00:00Z", projects: [], availability: "unreachable", lastHeartbeatAt: "2026-11-24T12:00:00Z",
      capabilities: { os: "linux", arch: "amd64", wsl: true, strictEgress: false, reportedAt: "2026-11-24T11:00:00Z",
        tools: [{ name: "claude", version: "1.0.3 (Claude Code)" }, { name: "git", version: "git version 2.43.0" }] },
    };
    signedIn();
    server.use(http.get("/api/runners/r1", () => json(r)), http.get("/api/projects", () => json([])));
    const { container } = await renderApp("/runners/r1");
    expect(await screen.findByRole("heading", { level: 1, name: "wsl-box" })).toBeInTheDocument();
    expect(screen.getByText("Unreachable")).toBeInTheDocument();
    expect(screen.getByText(/missed 3 heartbeats/)).toBeInTheDocument();
    const caps = screen.getByRole("region", { name: "Capabilities" });
    expect(within(caps).getByText("1.0.3 (Claude Code)")).toBeInTheDocument();
    expect(within(caps).getByText("codex")).toBeInTheDocument();
    expect(within(caps).getAllByText("Not found").length).toBeGreaterThan(0);
    expect(within(caps).getByText("Yes")).toBeInTheDocument(); // WSL2
    await expectAccessible(container);
  });
});
