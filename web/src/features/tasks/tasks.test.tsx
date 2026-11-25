import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { TelegramLaunch } from "../telegram/webapp";
import type { Approval, AttemptWorkspace, Me, PlanningRun, Project, Step, TaskDetail, TaskList, TaskListItem } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, fill, renderApp } from "../../test/render";
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
    createdAt: "2026-11-24T10:00:00Z", maxParallel: 1, steps: [], contracts: [], migrations: [], tests: [], docs: [] },
  planning: null,
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
    await fill(user, screen.getByLabelText("Title"), "Try it");
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
    expect(screen.getByRole("link", { name: "Revision details" })).toHaveAttribute("href", "/tasks/t1/plan/revisions/1");
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

  it("shows the AI CLI's usage, session and version per attempt and in total, and its events in the log", async () => {
    const usage = { inputTokens: 1200, outputTokens: 300, cacheReadTokens: 400, cacheWriteTokens: 100, costUsd: 0.125, turns: 4 };
    const base = detail();
    const cli = detail({
      execution: { ...base.execution!, usage,
        steps: [step({ attempts: [{ number: 1, runnerId: "r1", fence: 1, state: "SUCCEEDED", startedAt: "2026-11-24T10:00:00Z",
          finishedAt: "2026-11-24T10:00:30Z", agent: "claude", workStartedAt: "2026-11-24T10:00:01Z",
          workEndedAt: "2026-11-24T10:00:31Z", workStatus: "complete", workMs: 30_000, session: "sess-1",
          cliVersion: "2.1.0", usage }] })] },
    });
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/tasks/t1", () => json(cli)),
      http.get("/api/approvals", () => json([])),
      http.get("/api/tasks/t1/steps/s1/attempts/1/events", () =>
        json({ events: [{ seq: 1, kind: "workspace.preparing", at: "2026-11-24T10:00:01Z", reason: "fetching" },
          { seq: 2, kind: "execution.agent", at: "2026-11-24T10:00:02Z", reason: "tool_use · Edit · <b>x</b>" }],
          next: 2, more: false })),
    );
    const { container } = await renderApp("/tasks/t1");
    const user = userEvent.setup();
    const attempts = await screen.findByRole("table", { name: "Attempts of step backend" });
    expect(within(attempts).getByRole("columnheader", { name: "Usage" })).toBeInTheDocument();
    expect(within(attempts).getByText("2,000 tokens · $0.125 · 4 turns")).toHaveAttribute("title",
      "1,200 input, 300 output, 400 cache read, 100 cache write tokens");
    expect(within(attempts).getByText("session sess-1 · CLI 2.1.0")).toBeInTheDocument();
    expect(screen.getAllByText("2,000 tokens · $0.125 · 4 turns")).toHaveLength(2); // the execution's total too
    await user.click(screen.getByText("Output and events of attempt 1"));
    const log = await screen.findByLabelText("Log of attempt 1");
    await waitFor(() => expect(log).toHaveTextContent("Agent: tool_use · Edit · <b>x</b>"));
    expect(log).toHaveTextContent("Preparing the workspace: fetching the base branch"); // KIN-71
    expect(log.querySelector("b")).toBeNull();
    await expectAccessible(container);
  });

  it("shows a step's implementation and review loop: phase, handoff, test evidence and the verdict's findings (KIN-35)", async () => {
    const base = detail();
    const reviewed = detail({
      execution: { ...base.execution!,
        steps: [step({
          attempts: [
            { number: 1, runnerId: "r1", fence: 1, state: "SUCCEEDED", startedAt: "2026-11-24T10:00:00Z", finishedAt: "2026-11-24T10:00:30Z",
              agent: "codex", workStartedAt: null, workEndedAt: null, workStatus: "none", workMs: 0, role: "implementer", round: 0,
              testStatus: "failed" },
            { number: 2, runnerId: "r1", fence: 2, state: "SUCCEEDED", startedAt: "2026-11-24T10:01:00Z", finishedAt: "2026-11-24T10:01:30Z",
              agent: "claude", workStartedAt: null, workEndedAt: null, workStatus: "none", workMs: 0, role: "reviewer", round: 0 },
          ],
          review: { phase: "implement", round: 1, maxRounds: 2, pinnedRunner: "r1",
            handoff: { attempt: 1, agent: "codex", summary: "Added the <b>endpoint</b>", docs: ["README: login"], openQuestions: ["Rate limit?"],
              baseSha: "aaaaaaaaaaaa", headSha: "bbbbbbbbbbbb", files: ["api/login.go"], filesTotal: 1, testStatus: "passed",
              tests: { command: "make test", exitCode: 0, timedOut: false, durationMs: 1500, output: "ok" } },
            verdict: { attempt: 2, agent: "claude", round: 0, verdict: "changes_requested", summary: "Almost",
              findings: [{ severity: "major", file: "api/login.go", line: 12, message: "Check the error" }] } },
        })] },
    });
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/tasks/t1", () => json(reviewed)),
      http.get("/api/approvals", () => json([])),
    );
    const { container } = await renderApp("/tasks/t1");
    const loop = await screen.findByRole("region", { name: "Implementation and review" });
    expect(within(loop).getByText("Fixing review findings (round 1 of 2)")).toBeInTheDocument();
    expect(within(loop).getByText("Added the <b>endpoint</b>")).toBeInTheDocument(); // untrusted: text, never markup
    expect(within(loop).getByText("Commits aaaaaaaaaa…bbbbbbbbbb")).toBeInTheDocument();
    expect(within(loop).getByText("Passed")).toBeInTheDocument();
    expect(within(loop).getByText("make test · exit code 0 · 1.5 s")).toBeInTheDocument();
    expect(within(loop).getByText("Rate limit?")).toBeInTheDocument();
    expect(within(loop).getByText("Changes requested")).toBeInTheDocument();
    const findings = within(loop).getByRole("table", { name: "Review findings (backend)" });
    expect(within(findings).getByText("api/login.go:12")).toBeInTheDocument();
    expect(within(findings).getByText("Major")).toBeInTheDocument();
    const attempts = screen.getByRole("table", { name: "Attempts of step backend" });
    expect(within(attempts).getByText("reviewer")).toBeInTheDocument();
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

describe("attempt workspace", () => {
  const sha = (c: string) => c.repeat(40);
  const ws: AttemptWorkspace = {
    prepared: {
      at: "2026-11-24T10:01:00Z", repository: "acme/app", branch: "feature/t1--s1", base: "dev", baseSha: sha("b"),
      instructions: [
        { path: "AGENTS.md", sha256: "a".repeat(64), size: 2048 },
        { path: "<img src=x onerror=alert(1)>/CLAUDE.md", sha256: "c".repeat(64), size: 10, symlink: true },
        { path: ".claude/rules/old.md", sha256: "d".repeat(64), size: 5 },
      ],
    },
    changes: {
      at: "2026-11-24T10:01:40Z", branch: "feature/t1--s1", baseSha: sha("b"), headSha: sha("e"),
      files: [
        { path: "api/login.go", status: "A" },
        { path: "api/server.go", status: "M" },
        { path: "AGENTS.md", status: "M" },
        { path: "legacy.go", status: "D" },
      ],
      filesTotal: 6, truncated: true,
      instructionsChanged: [
        { path: "AGENTS.md", before: "a".repeat(64), after: "f".repeat(64) },
        { path: ".claude/rules/old.md", before: "d".repeat(64) },
        { path: ".claude/skills/new/SKILL.md", after: "9".repeat(64) },
      ],
    },
  };

  it("shows the branch, base and head commits, instruction files with changes marked, and changed files with counts", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/approvals", () => json([])),
      http.get("/api/tasks/t1", () => json(detail())),
      http.get("/api/tasks/t1/steps/s1/attempts/2/workspace", () => json(ws)),
    );
    const { container } = await renderApp("/tasks/t1");
    const user = userEvent.setup();
    await user.click(await screen.findByText("Workspace of attempt 2"));
    const facts = await screen.findByRole("region", { name: "Workspace of attempt 2" });
    expect(within(facts).getByText("feature/t1--s1")).toBeInTheDocument();
    expect(within(facts).getByText("dev")).toBeInTheDocument();
    expect(within(facts).getByText("bbbbbbbbbbbb")).toBeInTheDocument();
    expect(within(facts).getByText("eeeeeeeeeeee")).toBeInTheDocument();

    const instr = screen.getByRole("table", { name: "Instruction files of attempt 2" });
    const rows = within(instr).getAllByRole("row");
    expect(rows).toHaveLength(5); // header + 3 prepared + 1 added during the attempt
    expect(within(instr).getByText("<img src=x onerror=alert(1)>/CLAUDE.md")).toBeInTheDocument(); // text, not markup
    expect(instr.querySelector("img")).toBeNull();
    expect(within(instr).getByText("Symlink")).toBeInTheDocument();
    expect(within(instr).getByText("2 kB")).toBeInTheDocument();
    const agents = within(instr).getByText("AGENTS.md").closest("tr");
    expect(agents).toHaveTextContent("aaaaaaaaaaaa");
    expect(agents).toHaveTextContent("Changed during the attempt");
    expect(within(instr).getByText(".claude/rules/old.md").closest("tr")).toHaveTextContent("Removed during the attempt");
    expect(within(instr).getByText(".claude/skills/new/SKILL.md").closest("tr")).toHaveTextContent("Added during the attempt");

    const changed = screen.getByRole("list", { name: "Changed files of attempt 2" });
    expect(within(changed).getAllByRole("listitem")).toHaveLength(4);
    expect(within(changed).getByText("legacy.go").closest("li")).toHaveTextContent("Deleted");
    expect(screen.getByText("1 added · 2 modified · 1 deleted")).toBeInTheDocument();
    expect(screen.getByText("Showing 4 of 6 changed files.")).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("is translated to Russian and explains an attempt without a workspace", async () => {
    server.use(
      http.get("/api/auth/me", () => json(member)),
      http.get("/api/projects", () => json([{ ...alpha, myRole: "member" }])),
      http.get("/api/tasks/t1", () => json(detail({ canManage: false }))),
      http.get("/api/tasks/t1/steps/s1/attempts/1/workspace", () => json<AttemptWorkspace>({ prepared: null, changes: null })),
      http.get("/api/tasks/t1/steps/s1/attempts/2/workspace", () =>
        json<AttemptWorkspace>({ prepared: { ...ws.prepared!, instructions: [] }, changes: null })),
    );
    const { container } = await renderApp("/tasks/t1", "ru");
    const user = userEvent.setup();
    await user.click((await screen.findAllByText("Рабочий каталог попытки 1"))[0]!); // step backend
    expect(await screen.findByText(/Раннер не сообщил о рабочем каталоге/)).toBeInTheDocument();
    await user.click(screen.getByText("Рабочий каталог попытки 2"));
    expect(await screen.findByText("Файлы инструкций в репозитории не найдены.")).toBeInTheDocument();
    expect(screen.getByText("Раннер не сообщил об изменённых файлах.")).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("says what failed and retries", async () => {
    let fail = true;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/approvals", () => json([])),
      http.get("/api/tasks/t1", () => json(detail())),
      http.get("/api/tasks/t1/steps/s1/attempts/2/workspace", () => (fail ? apiError(500, "internal") : json(ws))),
    );
    await renderApp("/tasks/t1");
    const user = userEvent.setup();
    await user.click(await screen.findByText("Workspace of attempt 2"));
    const retry = await screen.findByRole("button", { name: /try again/i });
    fail = false;
    await user.click(retry);
    expect(await screen.findByRole("table", { name: "Instruction files of attempt 2" })).toBeInTheDocument();
  });
});

describe("AI planner (KIN-34)", () => {
  const run = (over: Partial<PlanningRun> = {}): PlanningRun => ({
    id: "R1", state: "QUEUED", stepId: "planner_R1", attempt: 1, used: 1, maxAttempts: 2, agent: "claude", model: "opus",
    runnerId: "r1", hasSession: true, notBefore: null, feedback: "invalid steps[0].repository: repository_not_in_project",
    revision: 0, issueText: true, articles: ["KIN-A-15"], createdAt: "2026-11-24T10:00:00Z", updatedAt: "2026-11-24T10:01:00Z",
    ...over,
  });
  const planned = detail({
    task: { ...detail().task, state: "AWAITING_APPROVAL", planRevision: 1 },
    revision: { ...detail().revision!, risk: "L2", createdBy: "planner/claude", maxParallel: 2,
      steps: [
        { key: "api", title: "Login endpoint", repository: "acme/app", role: "implementer", scope: ["internal/login/**"],
          dependsOn: [], capabilities: ["go"] },
        { key: "review", title: "Review", repository: "acme/app", role: "reviewer", scope: [], dependsOn: ["api"], capabilities: [] },
      ],
      contracts: [], migrations: ["0026 sessions table"], tests: ["login e2e"], docs: [] },
    planning: run({ state: "SUCCEEDED", revision: 1, used: 0, feedback: undefined }),
    execution: null,
  });

  it("starts the planner, shows a refused answer, then the plan's steps, scope and impact", async () => {
    let current = detail({ task: { ...detail().task, state: "CREATED", planRevision: 0 }, revision: null, execution: null,
      time: { agentMs: 0, agentStatus: "none", calendarMs: 0, running: false, started: false } });
    let posted = 0;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/approvals", () => json([])),
      http.get("/api/tasks/t1", () => json(current)),
      http.post("/api/tasks/t1/plan", () => {
        posted++;
        current = detail({ ...current, task: { ...current.task, state: "ANALYZING" }, planning: run() });
        return json(current, 202);
      }),
    );
    const { container } = await renderApp("/tasks/t1");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Plan with AI" }));
    await expectStatus("The AI planner is analysing the task.");
    expect(posted).toBe(1);
    const planner = await screen.findByRole("region", { name: "AI planner" });
    expect(within(planner).getByText("Waiting for a runner")).toBeInTheDocument();
    expect(within(planner).getByText(/repository_not_in_project/)).toBeInTheDocument();
    expect(within(planner).getByText("issue text · articles KIN-A-15")).toBeInTheDocument();
    expect(within(planner).getByText("1 of 2")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Plan with AI" })).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("shows the plan revision with steps by role and scope, and offers a replan", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/approvals", () => json([])),
      http.get("/api/tasks/t1", () => json(planned)),
    );
    const { container } = await renderApp("/tasks/t1");
    const table = await screen.findByRole("table", { name: "Plan steps (up to 2 at once)" });
    expect(within(table).getByText("internal/login/**")).toBeInTheDocument();
    expect(within(table).getByText("Reviewer")).toBeInTheDocument();
    expect(within(table).getByText("Nothing (review only)")).toBeInTheDocument();
    expect(screen.getByText("0026 sessions table")).toBeInTheDocument();
    expect(screen.getByText("Plan created")).toBeInTheDocument();
    expect(screen.getByText("The planner's CLI session is kept: the plan discussion continues it.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Replan with AI" })).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("is translated to Russian", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/approvals", () => json([])),
      http.get("/api/tasks/t1", () => json(planned)),
    );
    const { container } = await renderApp("/tasks/t1", "ru");
    expect(await screen.findByRole("region", { name: "AI-планировщик" })).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Шаги плана (одновременно до 2)" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Перепланировать с AI" })).toBeInTheDocument();
    await expectAccessible(container);
  });
});
