import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { Me, RunnerOutputStorage, StorageForecast, StorageMachineDetail, StorageOverview, StorageVolume } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, renderApp } from "../../test/render";

const GB = 1e9;
const none: StorageForecast = { state: "insufficient_data", windowDays: 7, growthBytesPerDay: 0, daysTo85: null, daysTo90: null };
const vol = (v: Partial<StorageVolume>): StorageVolume => ({
  role: "workspaces", mountOrVolumeId: "/", path: "/var/lib/kindevai-runner", totalBytes: 100 * GB, availableBytes: 38 * GB,
  freeBytes: 40 * GB, usedPercent: 62, level: "ok", diskHeavyAllowed: true, stale: false, lastReportedAt: "2026-11-24T12:00:00Z",
  forecast7d: { state: "ok", windowDays: 7, growthBytesPerDay: 2 * GB, daysTo85: 12, daysTo90: 14 },
  forecast30d: { ...none, windowDays: 30, state: "not_needed" }, ...v,
});

const cpVolume = vol({ role: "data", path: "/data", usedPercent: 20, availableBytes: 80 * GB, forecast7d: none });
const wslVolumes = [vol({}), vol({ role: "windows_host", mountOrVolumeId: "/mnt/c", path: "/mnt/c", usedPercent: 91, level: "high" })];

const overview: StorageOverview = {
  generatedAt: "2026-11-24T12:00:00Z",
  thresholds: { warningPercent: 75, highPercent: 85, criticalPercent: 95, minHeadroomBytes: 2147483648 },
  machines: [
    { id: "control-plane", kind: "control_plane", name: "", availability: "online", lastHeartbeatAt: null, diskHeavyAllowed: true,
      volumes: [cpVolume] },
    { id: "r1", kind: "runner", name: "wsl-box", status: "approved", availability: "online", lastHeartbeatAt: "2026-11-24T12:00:00Z",
      diskHeavyAllowed: true, volumes: wslVolumes },
  ],
};

const hours = (n: number, from: number, to: number) =>
  Array.from({ length: n }, (_, i) => ({
    at: new Date(Date.parse("2026-11-24T12:00:00Z") - (n - 1 - i) * 3_600_000).toISOString(),
    totalBytes: 100 * GB,
    usedBytes: Math.round((from + ((to - from) * i) / Math.max(1, n - 1)) * GB),
  }));

const detail = (id: string, days: 7 | 30): StorageMachineDetail => {
  const m = overview.machines.find((x) => x.id === id)!;
  return {
    ...m, days,
    history: id === "r1" ? { workspaces: hours(days * 24, 50, 62), windows_host: [] } : { data: hours(3, 19, 20) },
    consumers: id === "r1"
      ? { measuredAt: "2026-11-24T06:00:00Z", items: [
          { category: "repos", bytes: 2 * GB, entries: 900, partial: false },
          { category: "workspaces", bytes: 31 * GB, entries: 1_000_000, partial: true },
          { category: "container_cache", bytes: 12 * GB, entries: 40, partial: false },
        ] }
      : null,
  };
};

const output: RunnerOutputStorage = {
  usedBytes: 970 * 2 ** 20, limitBytes: 2 ** 30, usedPercent: 94.7, state: "high", outputRetentionDays: 14, eventRetentionDays: 30,
  dropsWindowHours: 24, droppedEvents: 5, lastDroppedAt: "2026-11-24T10:00:00Z",
  drops: [
    { runnerId: "r1", stepId: "step-7", fence: 2, events: 4, lastAt: "2026-11-24T10:00:00Z" },
    { runnerId: "gone", stepId: "step-3", fence: 1, events: 1, lastAt: "2026-11-24T09:00:00Z" },
  ],
};

const signedIn = (me: Me = owner) => server.use(http.get("/api/auth/me", () => json(me)));

function serveStorage(seen: string[] = []) {
  server.use(
    http.get("/api/storage/overview", () => json({ ...overview, runnerOutput: output })),
    http.get("/api/storage/machines/:id", ({ params, request }) => {
      const days = new URL(request.url).searchParams.get("days") === "30" ? 30 : 7;
      seen.push(`${String(params.id)}?${days}`);
      return json(detail(String(params.id), days));
    }),
  );
}

describe("Infrastructure → Storage page", () => {
  it("is linked from the Storage card on the overview", async () => {
    signedIn();
    serveStorage();
    await renderApp("/");
    const card = await screen.findByRole("region", { name: "Storage" });
    await userEvent.click(await within(card).findByRole("link", { name: "Open storage details" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Storage" })).toBeInTheDocument();
  });

  it("shows every machine with the history chart, all volumes, forecast details and space consumers", async () => {
    signedIn();
    serveStorage();
    const { container } = await renderApp("/infrastructure/storage");
    const wsl = await screen.findByRole("region", { name: "wsl-box" });
    expect(within(wsl).getByRole("link", { name: "Runner details" })).toHaveAttribute("href", "/runners/r1");

    const ws = await within(wsl).findByRole("region", { name: "Workspaces" });
    const chart = within(ws).getByRole("img", { name: /Used space of Workspaces over the last 7 days: from 50% to 62%/ });
    expect(chart).toBeInTheDocument();
    expect(within(ws).getByText("62% used")).toBeInTheDocument();
    expect(within(ws).getByText(/38 GB free of 100 GB/)).toBeInTheDocument();
    const forecast = within(ws).getByRole("table", { name: "Forecast" });
    expect(within(forecast).getByRole("row", { name: /Last 7 days.*\+2 GB per day.*in about 12 days.*in about 14 days/ })).toBeInTheDocument();
    expect(within(forecast).getByRole("row", { name: /Last 30 days.*No forecast needed/ })).toBeInTheDocument();
    // The data behind the chart as a table: one row per day.
    const data = within(ws).getByRole("table", { name: /Daily values/ });
    expect(within(data).getAllByRole("row").length).toBeGreaterThanOrEqual(8);

    const win = within(wsl).getByRole("region", { name: /Windows volume/ });
    expect(within(win).getByText("Cleanup recommended")).toBeInTheDocument();
    expect(within(win).getByText("No history yet")).toBeInTheDocument();

    const consumers = within(wsl).getByRole("table", { name: /Space consumers/ });
    const rows = within(consumers).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.textContent)).toEqual([
      expect.stringMatching(/Workspaces and worktrees.*at least 31 GB.*1,000,000/),
      expect.stringMatching(/Container caches.*12 GB/),
      expect.stringMatching(/Git repositories.*2 GB/),
    ]);

    const cp = screen.getByRole("region", { name: "Control Plane" });
    expect(await within(cp).findByRole("img", { name: /Data directory/ })).toBeInTheDocument();
    expect(within(cp).queryByRole("table", { name: /Space consumers/ })).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  // KIN-59: usage against the limit, the retention settings and the recent "output dropped" occurrences.
  it("shows the runner output storage with its limit, retention settings and recently dropped output", async () => {
    signedIn();
    serveStorage();
    const { container } = await renderApp("/infrastructure/storage");
    const sec = await screen.findByRole("region", { name: "Runner output (Control Plane database)" });
    expect(within(sec).getByRole("meter", { name: "Runner output: 95% of the limit used" })).toHaveAttribute("value", "95");
    expect(within(sec).getByText("Near the limit")).toBeInTheDocument();
    expect(within(sec).getByText("1.1 GB (1024 MiB)")).toBeInTheDocument();
    expect(within(sec).getByText("--runner-output-limit-mb")).toBeInTheDocument();
    expect(within(sec).getByText("14 days after the execution finished")).toBeInTheDocument();
    expect(within(sec).getByText("30 days after the execution finished")).toBeInTheDocument();
    expect(within(sec).getByText(/5 output events were dropped in the last 24 h/)).toBeInTheDocument();
    const drops = within(sec).getByRole("table", { name: "Output dropped in the last 24 h" });
    const [, first, second] = within(drops).getAllByRole("row") as [HTMLElement, HTMLElement, HTMLElement];
    expect(within(first).getByRole("rowheader", { name: "wsl-box" })).toBeInTheDocument();
    expect(within(first).getByText("step-7")).toBeInTheDocument();
    expect(within(first).getByText("4")).toBeInTheDocument();
    expect(within(second).getByRole("rowheader", { name: "gone" })).toBeInTheDocument(); // a runner no longer listed
    await expectAccessible(container);
  });

  it("says when no runner output was dropped", async () => {
    signedIn();
    server.use(http.get("/api/storage/overview", () =>
      json({ ...overview, machines: [], runnerOutput: { ...output, state: "ok", usedPercent: 1, droppedEvents: 0, lastDroppedAt: null, drops: [] } })));
    await renderApp("/infrastructure/storage");
    const sec = await screen.findByRole("region", { name: "Runner output (Control Plane database)" });
    expect(within(sec).getByText("No output was dropped in the last 24 h.")).toBeInTheDocument();
    expect(within(sec).queryByRole("table")).not.toBeInTheDocument();
  });

  it("switches the history window to 30 days", async () => {
    signedIn();
    const seen: string[] = [];
    serveStorage(seen);
    await renderApp("/infrastructure/storage");
    const wsl = await screen.findByRole("region", { name: "wsl-box" });
    await within(wsl).findByRole("img", { name: /last 7 days/ });
    await userEvent.click(screen.getByRole("radio", { name: "30 days" }));
    expect(await within(wsl).findByRole("img", { name: /last 30 days/ })).toBeInTheDocument();
    expect(seen).toContain("r1?30");
  });

  it("explains how to allow measuring space consumers on a runner that never measured", async () => {
    signedIn();
    serveStorage();
    server.use(http.get("/api/storage/machines/r1", () => json({ ...detail("r1", 7), consumers: null })));
    await renderApp("/infrastructure/storage");
    const wsl = await screen.findByRole("region", { name: "wsl-box" });
    expect(await within(wsl).findByText(/ai-runner run --measure-space/)).toBeInTheDocument();
  });

  it("shows an error with retry for a machine whose details cannot be loaded", async () => {
    signedIn();
    serveStorage();
    server.use(http.get("/api/storage/machines/r1", () => apiError(500, "internal")));
    await renderApp("/infrastructure/storage");
    const wsl = await screen.findByRole("region", { name: "wsl-box" });
    expect(await within(wsl).findByRole("alert")).toBeInTheDocument();
    expect(within(wsl).getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

  it("shows an empty state before any metrics and an error when the list fails", async () => {
    signedIn();
    await renderApp("/infrastructure/storage");
    expect(await screen.findByRole("heading", { level: 2, name: "No storage metrics yet" })).toBeInTheDocument();
  });

  it("shows an error with retry when the machine list fails", async () => {
    signedIn();
    server.use(http.get("/api/storage/overview", () => apiError(500, "internal")));
    await renderApp("/infrastructure/storage");
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

  it("is in Russian too", async () => {
    signedIn();
    serveStorage();
    await renderApp("/infrastructure/storage", "ru");
    expect(await screen.findByRole("heading", { level: 1, name: "Хранилище" })).toBeInTheDocument();
    const wsl = await screen.findByRole("region", { name: "wsl-box" });
    expect(await within(wsl).findByRole("table", { name: /Потребители места/ })).toBeInTheDocument();
    expect(within(wsl).getByText("Рабочие каталоги и worktree")).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "30 дней" })).toBeInTheDocument();
    const sec = screen.getByRole("region", { name: "Вывод раннеров (БД Control Plane)" });
    expect(within(sec).getByText("Близко к лимиту")).toBeInTheDocument();
    expect(within(sec).getByText("14 дней после завершения исполнения")).toBeInTheDocument();
    expect(within(sec).getByRole("table", { name: "Вывод, отброшенный за последние 24 ч" })).toBeInTheDocument();
  });

  it("is not available to members", async () => {
    signedIn({ ...owner, role: "user" });
    await renderApp("/infrastructure/storage");
    await waitFor(() => expect(screen.queryByRole("heading", { level: 1, name: "Storage" })).not.toBeInTheDocument());
    expect(await screen.findByRole("heading", { level: 1 })).toBeInTheDocument();
  });
});
