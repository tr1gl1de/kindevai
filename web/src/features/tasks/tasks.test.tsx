import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { TelegramLaunch } from "../telegram/webapp";
import type { Approval, Me, Project, Step, TaskDetail, TaskList, TaskListItem } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, renderApp } from "../../test/render";
import { stages } from "./TaskPage";

const member: Me = { id: "u2", login: "bob", role: "user", mustChangePassword: false };
const alpha: Project = { id: "p1", slug: "alpha", name: "Alpha", myRole: "owner", createdAt: "2026-11-24T10:00:00Z" };
const beta: Project = { ...alpha, id: "p2", slug: "beta", name: "Beta" };

const listItem = (over: Partial<TaskListItem>): TaskListItem => ({
  id: "t1",
  projectId: "p1",
  title: "Add login",
  state: "RUNNING",
  planRevision: 1,
  createdAt: "2026-11-24T10:00:00Z",
  updatedAt: "2026-11-24T10:05:00Z",
  time: { agentMs: 90_000, agentStatus: "running", calendarMs: 120_000, running: true, started: true },
  ...over,
});

const step = (over: Partial<Step>): Step => ({
  id: "s1",
  key: "backend",
  title: "API",
  repository: "acme/app",
  dependsOn: [],
  state: "SUCCEEDED",
  fence: 2,
  attempts: [],
  agentTime: { ms: 0, status: "none" },
  ...over,
});

const detail = (over: Partial<TaskDetail> = {}): TaskDetail => ({
  task: { id: "t1", projectId: "p1", title: "Add login", state: "VERIFYING", planRevision: 1,
    createdAt: "2026-11-24T10:00:00Z", updatedAt: "2026-11-24T10:05:00Z" },
  revision: { number: 1, hash: "a".repeat(64), risk: "L1", summary: "Login API and page", createdBy: "mock-planner",
    createdAt: "2026-11-24T10:00:00Z" },
  execution: {
    id: "e1", planRevision: 1, planHash: "a".repeat(64), state: "SUCCEEDED", createdAt: "2026-11-24T10:00:00Z",
    finishedAt: "2026-11-24T10:05:00Z", agentTime: { ms: 70_000, status: "incomplete" },
    steps: [
      step({
        agentTime: { ms: 70_000, status: "complete" },
        attempts: [
          { number: 1, runnerId: "runner-abcdef-123", fence: 1, state: "FAILED", error: "exit status 1",
            startedAt: "2026-11-24T10:00:00Z", finishedAt: "2026-11-24T10:00:30Z", agent: "mock",
            workStartedAt: "2026-11-24T10:00:01Z", workEndedAt: "2026-11-24T10:00:31Z", workStatus: "complete", workMs: 30_000 },
          { number: 2, runnerId: "runner-abcdef-123", fence: 2, state: "SUCCEEDED",
            startedAt: "2026-11-24T10:01:00Z", finishedAt: "2026-11-24T10:01:40Z", agent: "mock",
            workStartedAt: "2026-11-24T10:01:00Z", workEndedAt: "2026-11-24T10:01:40Z", workStatus: "complete", workMs: 40_000 },
        ],
      }),
      step({ id: "s2", key: "frontend", title: "UI", dependsOn: ["backend"], state: "CANCELLED",
        attempts: [{ number: 1, runnerId: "r2", fence: 1, state: "CANCELLED", startedAt: "2026-11-24T10:02:00Z",
          finishedAt: "2026-11-24T10:02:10Z", agent: "mock", workStartedAt: "2026-11-24T10:02:00Z", workEndedAt: null,
          workStatus: "incomplete", workMs: 0 }] }),
    ],
  },
  canManage: true,
  time: { agentMs: 70_000, agentStatus: "incomplete", calendarMs: 300_000, running: false, started: true },
  ...over,
});

describe("task list", () => {
  it("filters by project and state, and shows agent time apart from the calendar duration", async () => {
    const asked: string[] = [];
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha, beta])),
      http.get("/api/tasks", ({ request }) => {
        const q = new URL(request.url).searchParams;
        asked.push(q.toString());
        const body: TaskList = q.get("state") === "FAILED"
          ? { tasks: [], total: { ms: 0, status: "none" } }
          : { tasks: [listItem({}), listItem({ id: "t2", projectId: "p2", title: "Fix CI", state: "BLOCKED", blockReason: "runner",
              time: { agentMs: 0, agentStatus: "none", calendarMs: 0, running: false, started: false } })],
              total: { ms: 90_000, status: "incomplete" } };
        return json(body);
      }),
    );
    const { container } = await renderApp("/tasks?projectId=p1");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Tasks & executions" })).toBeInTheDocument();
    const row = (await screen.findByRole("link", { name: "Add login" })).closest("tr") as HTMLElement;
    expect(within(row).getByText("1 min 30 sec")).toBeInTheDocument(); // agent time
    expect(within(row).getByText("2 min")).toBeInTheDocument(); // calendar
    const other = screen.getByRole("link", { name: "Fix CI" }).closest("tr") as HTMLElement;
    expect(within(other).getByText("Waiting for a runner")).toBeInTheDocument();
    expect(within(other).getByText("Not started")).toBeInTheDocument();
    const total = screen.getByRole("region", { name: "Agent time for this filter" });
    expect(within(total).getByText("Accounting incomplete")).toBeInTheDocument();
    expect(asked[0]).toBe("projectId=p1");
    await expectAccessible(container);

    await user.selectOptions(screen.getByLabelText("State"), "FAILED");
    expect(await screen.findByRole("heading", { name: "No tasks match this filter" })).toBeInTheDocument();
    expect(asked.at(-1)).toBe("projectId=p1&state=FAILED");
  });

  it("creates a task in a project the user manages and validates the title", async () => {
    let body: unknown;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/tasks", () => json<TaskList>({ tasks: [], total: { ms: 0, status: "none" } })),
      http.post("/api/tasks", async ({ request }) => {
        body = await request.json();
        return json(detail({ task: { ...detail().task, title: "Try it", state: "CREATED" } }), 201);
      }),
    );
    await renderApp("/tasks");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { name: "No tasks yet" })).toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Create task" }));
    expect(screen.getByLabelText("Title")).toHaveAccessibleDescription("Fill in this field.");
    expect(screen.getByLabelText("Title")).toHaveFocus();
    await user.type(screen.getByLabelText("Title"), "Try it");
    await user.click(screen.getByRole("button", { name: "Create task" }));
    await expectStatus("Task “Try it” created.");
    expect(body).toEqual({ projectId: "p1", title: "Try it" });
  });

  it("is translated to Russian", async () => {
    server.use(
      http.get("/api/auth/me", () => json(member)),
      http.get("/api/projects", () => json([{ ...alpha, myRole: "member" }])),
      http.get("/api/tasks", () => json<TaskList>({ tasks: [listItem({ state: "FAILED", time: { agentMs: 3_600_000,
        agentStatus: "complete", calendarMs: 3_600_000, running: false, started: true } })], total: { ms: 3_600_000, status: "complete" } })),
    );
    const { container } = await renderApp("/tasks", "ru");
    expect(await screen.findByRole("heading", { level: 1, name: "Задачи и исполнения" })).toBeInTheDocument();
    expect(await screen.findByText("Ошибка")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Создать задачу" })).not.toBeInTheDocument(); // members cannot create
    await expectAccessible(container);
  });
});

describe("task detail", () => {
  it("shows the plan, steps by stage, attempts with agent and runner, agent vs calendar time, and the log as text", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/tasks/t1", () => json(detail())),
      http.get("/api/approvals", ({ request }) => {
        expect(new URL(request.url).searchParams.get("taskId")).toBe("t1");
        return json<Partial<Approval>[]>([{ id: "a1", scope: "execute", planRevision: 1, state: "approved" }]);
      }),
      http.get("/api/tasks/t1/steps/s1/attempts/2/events", ({ request }) => {
        const after = new URL(request.url).searchParams.get("after");
        if (after === "0")
          return json({ events: [{ seq: 1, kind: "execution.started", at: "2026-11-24T10:01:00Z" },
            { seq: 2, kind: "execution.output", at: "2026-11-24T10:01:01Z", stream: "stdout", data: "<b>mock agent: done</b>\n" }],
          next: 2, more: true });
        return json({ events: [{ seq: 3, kind: "execution.completed", at: "2026-11-24T10:01:40Z" }], next: 3, more: false });
      }),
    );
    const { container } = await renderApp("/tasks/t1");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Add login" })).toBeInTheDocument();
    expect(screen.getByText("Login API and page")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Stage 1" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Stage 2" })).toBeInTheDocument();
    expect(screen.getByText(/After: backend/)).toBeInTheDocument();
    const time = screen.getByRole("region", { name: "Time" });
    expect(within(time).getByText("1 min 10 sec")).toBeInTheDocument();
    expect(within(time).getByText("5 min")).toBeInTheDocument();
    expect(within(time).getAllByText("Accounting incomplete").length).toBeGreaterThan(0);
    const attempts = screen.getByRole("table", { name: "Attempts of step backend" });
    expect(within(attempts).getAllByRole("link", { name: "runner-abc" })[0]).toHaveAttribute("href", "/runners/runner-abcdef-123");
    expect(within(attempts).getAllByText("mock")).toHaveLength(2);
    expect(within(attempts).getByText("40 sec")).toBeInTheDocument();
    expect(within(attempts).getByText("exit status 1")).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "Run the plan, revision 1" })).toHaveAttribute("href", "/approvals/a1");

    await user.click(screen.getByText("Output and events of attempt 2"));
    const log = await screen.findByLabelText("Log of attempt 2");
    await waitFor(() => expect(log).toHaveTextContent("Process completed"));
    expect(log).toHaveTextContent("<b>mock agent: done</b>"); // shown as text, never as markup
    expect(log.querySelector("b")).toBeNull();
    await expectAccessible(container);
  });

  it("cancels after naming the task, and retries a failed task (Owner / project admin only)", async () => {
    let state: TaskDetail["task"]["state"] = "RUNNING";
    let cancelBody: unknown;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/approvals", () => json([])),
      http.get("/api/tasks/t1", () => json(detail({ task: { ...detail().task, state } }))),
      http.post("/api/tasks/t1/cancel", async ({ request }) => {
        cancelBody = await request.json();
        state = "FAILED"; // then retry it
        return json(detail({ task: { ...detail().task, state } }));
      }),
      http.post("/api/tasks/t1/retry", () => {
        state = "READY";
        return json(detail({ task: { ...detail().task, state, planRevision: 2 } }));
      }),
    );
    await renderApp("/tasks/t1");
    const user = userEvent.setup();
    expect(screen.queryByRole("button", { name: "Retry task" })).not.toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Cancel task" }));
    const dialog = screen.getByRole("alertdialog", { name: "Cancel “Add login”?" });
    await user.click(within(dialog).getByRole("button", { name: "Cancel task" }));
    await expectStatus("Task cancelled.");
    expect(cancelBody).toEqual({ reason: "Cancelled in the web UI" });
    await user.click(await screen.findByRole("button", { name: "Retry task" }));
    await expectStatus("Task planned again.");
  });

  it("plans a new task with the mock planner; members see no actions", async () => {
    let scenario: unknown;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/approvals", () => json([])),
      http.get("/api/tasks/t1", () => json(detail({ task: { ...detail().task, state: "CREATED", planRevision: 0 }, revision: null, execution: null,
        time: { agentMs: 0, agentStatus: "none", calendarMs: 0, running: false, started: false } }))),
      http.post("/api/tasks/t1/mock-run", async ({ request }) => {
        scenario = await request.json();
        return json(detail({ task: { ...detail().task, state: "READY" } }), 202);
      }),
    );
    await renderApp("/tasks/t1");
    const user = userEvent.setup();
    expect(await screen.findByText("Not run yet.")).toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText("Mock scenario"), "flaky");
    await user.click(screen.getByRole("button", { name: "Plan with the mock planner" }));
    await expectStatus("Mock plan created; the task starts when a runner is free.");
    expect(scenario).toEqual({ scenario: "flaky" });
  });

  it("hides actions and approvals from members and says when a task is not found", async () => {
    server.use(
      http.get("/api/auth/me", () => json(member)),
      http.get("/api/projects", () => json([{ ...alpha, myRole: "member" }])),
      http.get("/api/tasks/t1", () => json(detail({ canManage: false, task: { ...detail().task, state: "RUNNING" } }))),
      http.get("/api/tasks/nope", () => apiError(404, "not_found")),
    );
    const { unmount } = await renderApp("/tasks/t1");
    expect(await screen.findByRole("heading", { level: 1, name: "Add login" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Cancel task" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Approvals" })).not.toBeInTheDocument();
    expect(screen.getAllByText("runner-abc")[0]?.tagName).toBe("CODE"); // runner pages are Owner-only
    unmount();
    await renderApp("/tasks/nope");
    expect(await screen.findByRole("heading", { level: 1, name: "Task not found" })).toBeInTheDocument();
  });
});

describe("Telegram deep link", () => {
  it("opens the task page from a task notification (startapp=task_<id>)", async () => {
    const launch: TelegramLaunch = { initData: "query_id=x&hash=abc", startParam: "task_t1", languageCode: "en" };
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/approvals", () => json([])),
      http.get("/api/tasks/t1", () => json(detail())),
    );
    await renderApp("/", "en", launch);
    expect(await screen.findByRole("heading", { level: 1, name: "Add login" })).toBeInTheDocument();
  });
});

describe("stages", () => {
  it("groups steps by dependency depth", () => {
    const s = (key: string, dependsOn: string[] = []) => step({ id: key, key, dependsOn });
    const got = stages([s("a"), s("b", ["a"]), s("c"), s("d", ["b", "c"])]).map((g) => g.map((x) => x.key));
    expect(got).toEqual([["a", "c"], ["b"], ["d"]]);
  });
});
