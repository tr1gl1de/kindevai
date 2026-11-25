import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import type {
  AgentSettings,
  Approval,
  ApprovalDetail,
  DiscussionLimits,
  DiscussionMessage,
  DiscussionThread,
  Me,
  PlanRevision,
  Project,
  TaskDetail,
} from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, fill, renderApp } from "../../test/render";

const member: Me = { id: "u2", login: "bob", role: "user", mustChangePassword: false };
const alpha: Project = { id: "p1", slug: "alpha", name: "Alpha", myRole: "owner", createdAt: "2026-11-24T10:00:00Z" };
const H = (c: string) => c.repeat(64);
const limits: DiscussionLimits = { maxMessageChars: 4000, maxMessagesPerPlan: 20, maxMessagesPerHour: 30, answerTimeoutSec: 600 };

const task = (over: Partial<TaskDetail> = {}): TaskDetail => ({
  task: { id: "t1", projectId: "p1", title: "Add login", state: "AWAITING_APPROVAL", planRevision: 2,
    createdAt: "2026-11-24T10:00:00Z", updatedAt: "2026-11-24T10:05:00Z" },
  revision: { number: 2, hash: H("b"), risk: "L2", summary: "Login API and page", createdBy: "planner/claude",
    createdAt: "2026-11-24T10:04:00Z", maxParallel: 1, steps: [], contracts: [], migrations: [], tests: [], docs: [] },
  planning: null,
  execution: null,
  canManage: true,
  time: { agentMs: 0, agentStatus: "none", calendarMs: 0, running: false, started: false },
  ...over,
});

const msg = (over: Partial<DiscussionMessage>): DiscussionMessage => ({
  id: "m1",
  role: "user",
  author: "u1",
  planRevision: 1,
  text: "Split the API step in two",
  status: "ANSWERED",
  createdAt: "2026-11-24T10:01:00Z",
  updatedAt: "2026-11-24T10:01:00Z",
  ...over,
});

const thread = (over: Partial<DiscussionThread> = {}): DiscussionThread => ({
  messages: [],
  limits,
  canWrite: true,
  inFlight: false,
  used: 0,
  hasSession: true,
  ...over,
});

const approval = (over: Partial<Approval> = {}): Approval => ({
  id: "a2", projectId: "p1", projectName: "Alpha", taskId: "t1", taskTitle: "Add login", planRevision: 2, planHash: H("b"),
  scope: "execute", reason: "plan", paths: [], risk: "L2", findings: [], state: "pending",
  requestedAt: "2026-11-24T10:04:00Z", decidedAt: null, ...over,
});

const answered = thread({
  used: 1,
  messages: [
    msg({}),
    msg({ id: "m2", role: "assistant", author: "planner/claude", agent: "claude", model: "sonnet", replyTo: "m1",
      text: "Done. **Two steps** now:\n\n- `api` first\n- then `ui`\n\n```json\n{\"summary\":\"Login API and page\"}\n```" }),
    msg({ id: "m3", role: "system", author: "system", notice: "revision_created", revision: 2, replyTo: "m1",
      text: "revision 2 created" }),
  ],
});

/** Task page with the given thread; the discussion endpoint is recorded. */
function taskPage(th: DiscussionThread | (() => DiscussionThread), d: TaskDetail = task(), approvals: Approval[] = []) {
  server.use(
    http.get("/api/projects", () => json([alpha])),
    http.get("/api/tasks/t1", () => json(d)),
    http.get("/api/approvals", () => json(approvals)),
    http.get("/api/tasks/t1/discussion", () => json(typeof th === "function" ? th() : th)),
  );
}

describe("plan discussion on the task page", () => {
  it("shows the thread with each message's revision, the AI answer and the new revision with its diff and re-approval", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    taskPage(answered, task(), [approval(), approval({ id: "a1", planRevision: 1, planHash: H("a"), state: "superseded" })]);
    const { container } = await renderApp("/tasks/t1");
    const panel = await screen.findByRole("region", { name: "Plan discussion" });
    const log = await within(panel).findByRole("log", { name: "Messages" });
    const items = within(log).getAllByRole("listitem");
    expect(within(items[0]!).getByText("Split the API step in two")).toBeInTheDocument();
    expect(within(items[0]!).getByText("You")).toBeInTheDocument();
    expect(within(items[0]!).getByText("About revision 1")).toBeInTheDocument();
    expect(within(items[0]!).getByText("Answered")).toBeInTheDocument();
    const answer = items[1]!;
    expect(within(answer).getByText("AI · claude · sonnet")).toBeInTheDocument();
    expect(within(answer).getByText("Two steps").tagName).toBe("STRONG");
    expect(within(answer).getByText("api").tagName).toBe("CODE");
    expect(within(answer).getAllByRole("listitem")).toHaveLength(2);
    expect(within(answer).getByText("Proposed plan (JSON)")).toBeInTheDocument();

    const card = within(log).getByRole("article", { name: "Revision 2" });
    expect(within(card).getByText("The AI created revision 2 of the plan from this answer.")).toBeInTheDocument();
    expect(within(card).getByRole("link", { name: "Changes since revision 1" })).toHaveAttribute("href", "/tasks/t1/plan/revisions/2");
    expect(within(card).getByText(/needs approval again/i)).toBeInTheDocument();
    expect(within(panel).getByText("1 of 20 messages used")).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("renders AI text as text: HTML and links in an answer are never executed or turned into elements (XSS)", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    const evil = '<img src=x onerror="alert(1)"><script>alert(2)</script> [click](javascript:alert(3)) <a href="https://evil">x</a>';
    taskPage(thread({ messages: [msg({ id: "m9", role: "assistant", author: "planner/claude", agent: "claude", text: evil })] }));
    await renderApp("/tasks/t1");
    const log = await screen.findByRole("log", { name: "Messages" });
    expect(log).toHaveTextContent('<img src=x onerror="alert(1)"><script>alert(2)</script>');
    expect(log.querySelector("img, script, iframe, a, [onerror]")).toBeNull();
    expect(within(log).queryByRole("link")).not.toBeInTheDocument();
  });

  it("counts characters, checks the length before sending, sends and follows the answer until it arrives", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    let posted: unknown;
    let polls = 0;
    const pending = msg({ id: "m5", planRevision: 2, text: "Add tests", status: "PENDING" });
    const state = { th: thread({ limits: { ...limits, maxMessageChars: 20 } }) };
    taskPage(() => {
      polls++;
      return state.th;
    });
    server.use(
      http.post("/api/tasks/t1/discussion/messages", async ({ request }) => {
        posted = await request.json();
        state.th = thread({ limits: state.th.limits, inFlight: true, used: 1, messages: [{ ...pending, status: "DELIVERED" }] });
        return json(pending, 202);
      }),
    );
    await renderApp("/tasks/t1");
    const user = userEvent.setup();
    const box = await screen.findByLabelText("Message to the AI");
    expect(screen.getByText("0 / 20")).toBeInTheDocument();
    expect(screen.getByText("No messages yet. Ask the AI to change or explain the plan.")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(box).toHaveAccessibleDescription(/Write a message first/);
    expect(box).toHaveFocus();

    await fill(user, box, "This message is far too long");
    expect(screen.getByText("28 / 20")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(box).toHaveAccessibleDescription(/longer than 20 characters/);
    expect(posted).toBeUndefined();
    expect(box).toHaveValue("This message is far too long");

    await user.clear(box);
    await fill(user, box, "Add tests");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await expectStatus("Message sent. The AI will answer here.");
    expect(posted).toEqual({ text: "Add tests" });
    expect(box).toHaveValue("");
    const log = await screen.findByRole("log", { name: "Messages" });
    expect(await within(log).findByText("AI is answering")).toBeInTheDocument();
    expect(screen.getByText(/The AI is answering the last message/)).toBeInTheDocument();

    // The answer arrives: the thread is polled while the AI works on a message.
    const before = polls;
    state.th = thread({ used: 1, messages: [{ ...pending, status: "ANSWERED" },
      msg({ id: "m6", role: "assistant", author: "planner/codex", agent: "codex", planRevision: 2, text: "Tests added." })] });
    expect(await within(log).findByText("Tests added.")).toBeInTheDocument();
    expect(polls).toBeGreaterThan(before);
  });

  it("shows a limit that refused the message with its reason and when to continue, keeping the text", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    taskPage(thread({ used: 3 }));
    server.use(
      http.post("/api/tasks/t1/discussion/messages", () =>
        HttpResponse.json(
          { error: "rate_limited", message: "x", code: "hourly_message_limit", limit: 30, retryAfterSec: 1500 },
          { status: 429, headers: { "Retry-After": "1500" } },
        ),
      ),
    );
    await renderApp("/tasks/t1");
    const user = userEvent.setup();
    const box = await screen.findByLabelText("Message to the AI");
    await fill(user, box, "One more idea");
    await user.click(screen.getByRole("button", { name: "Send" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("You sent 30 messages in the last hour. Try again in 25 min.");
    expect(alert).toHaveTextContent(/You can send again at/);
    expect(box).toHaveValue("One more idea");
  });

  it("offers to send the last failed message again", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    let retried = "";
    taskPage(thread({ used: 2, messages: [
      msg({ id: "m1", status: "FAILED", reason: "runner went offline" }),
      msg({ id: "m2", text: "Second try", status: "FAILED", reason: "no answer within 10m" }),
    ] }));
    server.use(
      http.post("/api/tasks/t1/discussion/messages/:msg/retry", ({ params }) => {
        retried = String(params.msg);
        return json(msg({ id: "m2", text: "Second try", status: "PENDING" }), 202);
      }),
    );
    await renderApp("/tasks/t1");
    const user = userEvent.setup();
    const log = await screen.findByRole("log", { name: "Messages" });
    expect(within(log).getByText("runner went offline")).toBeInTheDocument();
    expect(within(log).getAllByText("No answer")).toHaveLength(2);
    const buttons = within(log).getAllByRole("button", { name: "Send again" });
    expect(buttons).toHaveLength(1); // only the last failed message
    await user.click(buttons[0]!);
    await expectStatus("Message sent again.");
    expect(retried).toBe("m2");
  });

  it("is read-only for a project member and explains why", async () => {
    server.use(http.get("/api/auth/me", () => json(member)));
    taskPage(thread({ canWrite: false, messages: [msg({}), msg({ id: "m4", text: "Queued one", status: "PENDING" })] }),
      task({ canManage: false }));
    const { container } = await renderApp("/tasks/t1");
    const panel = await screen.findByRole("region", { name: "Plan discussion" });
    expect(await within(panel).findByText("Split the API step in two")).toBeInTheDocument();
    expect(within(panel).queryByLabelText("Message to the AI")).not.toBeInTheDocument();
    expect(within(panel).getByText(/Only the Owner and project admins can write/)).toBeInTheDocument();
    expect(within(panel).getByText("Waiting for a runner")).toBeInTheDocument(); // queued: the runner may be offline
    expect(within(panel).queryByRole("button", { name: "Send again" })).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("tells a project admin when the plan cannot change in the task's state", async () => {
    server.use(http.get("/api/auth/me", () => json(member)));
    taskPage(thread({ canWrite: false }), task({ task: { ...task().task, state: "RUNNING" } }));
    await renderApp("/tasks/t1");
    expect(await screen.findByText(/The plan can be discussed while the task is ready or waiting for approval/)).toBeInTheDocument();
    expect(screen.queryByLabelText("Message to the AI")).not.toBeInTheDocument();
  });

  it("shows an error with a retry when the thread cannot be loaded", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    taskPage(thread());
    let fail = true;
    server.use(http.get("/api/tasks/t1/discussion", () => (fail ? apiError(500, "internal") : json(thread()))));
    await renderApp("/tasks/t1");
    const user = userEvent.setup();
    const panel = await screen.findByRole("region", { name: "Plan discussion" });
    expect(await within(panel).findByRole("alert")).toBeInTheDocument();
    fail = false;
    await user.click(within(panel).getByRole("button", { name: "Try again" }));
    expect(await within(panel).findByLabelText("Message to the AI")).toBeInTheDocument();
  });

  it("keeps the thread and the draft when a background poll fails", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    let fail = false;
    const busy = thread({ inFlight: true, used: 1, messages: [msg({ status: "DELIVERED" })] });
    taskPage(busy);
    server.use(http.get("/api/tasks/t1/discussion", () => (fail ? apiError(500, "internal") : json(busy))));
    await renderApp("/tasks/t1");
    const user = userEvent.setup();
    const box = await screen.findByLabelText("Message to the AI");
    await fill(user, box, "Half-written idea");
    fail = true;
    expect(await screen.findByText(/Could not refresh the discussion/)).toBeInTheDocument();
    expect(screen.getByLabelText("Message to the AI")).toHaveValue("Half-written idea");
    expect(screen.getByRole("log", { name: "Messages" })).toHaveTextContent("Split the API step in two");
  });

  it("is translated to Russian", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    taskPage(answered, task(), [approval()]);
    const { container } = await renderApp("/tasks/t1", "ru");
    const panel = await screen.findByRole("region", { name: "Обсуждение плана" });
    expect(await within(panel).findByRole("article", { name: "Ревизия 2" })).toBeInTheDocument();
    expect(within(panel).getByLabelText("Сообщение для AI")).toBeInTheDocument();
    expect(within(panel).getByRole("button", { name: "Отправить" })).toBeInTheDocument();
    await expectAccessible(container);
  });
});

const rev = (n: number, hash: string): PlanRevision => ({
  number: n, hash, risk: "L2", createdBy: "planner/claude", createdAt: "2026-11-24T10:00:00Z",
  plan: { summary: "Login", risk: "L2", maxParallel: 1,
    steps: [{ key: "api", title: "API", repository: "acme/api", scope: [], dependsOn: [], capabilities: [] }],
    tests: [], contracts: [], migrations: [], docs: [] },
});

describe("plan discussion in the approval center", () => {
  it("shows the chat, names the revision on both decision buttons and offers the current revision when one is stale", async () => {
    const stale = approval({ id: "a1", planRevision: 1, planHash: H("a") });
    let current = false;
    const detail = (): ApprovalDetail => ({
      approval: current ? { ...stale, state: "superseded" } : stale,
      task: { id: "t1", title: "Add login", state: "AWAITING_APPROVAL", planRevision: 2 },
      revision: rev(1, H("a")),
      previous: null,
      changes: [],
      related: [stale, approval()],
      canDecide: true,
    });
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/approvals/a1", () => json(detail())),
      http.post("/api/approvals/a1/approve", () => {
        current = true;
        return apiError(409, "stale_revision");
      }),
      http.get("/api/tasks/t1/discussion", () => json(answered)),
    );
    const { container } = await renderApp("/approvals/a1");
    const user = userEvent.setup();
    const panel = await screen.findByRole("region", { name: "Plan discussion" });
    expect(await within(panel).findByRole("article", { name: "Revision 2" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reject revision 1" })).toBeInTheDocument();
    await expectAccessible(container);

    await user.click(screen.getByRole("button", { name: "Approve revision 1" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Revision 1 is no longer current");
    const open = await within(alert).findByRole("link", { name: "Review revision 2" });
    expect(open).toHaveAttribute("href", "/approvals/a2");
  });
});

describe("stale decision without a pending current revision", () => {
  it("links to the task's current plan", async () => {
    const stale = approval({ id: "a1", planRevision: 1, planHash: H("a") });
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/approvals/a1", () => json<ApprovalDetail>({ approval: stale,
        task: { id: "t1", title: "Add login", state: "READY", planRevision: 2 }, revision: rev(1, H("a")), previous: null,
        changes: [], related: [stale], canDecide: true })),
      http.post("/api/approvals/a1/approve", () => apiError(409, "stale_revision")),
    );
    await renderApp("/approvals/a1");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Approve revision 1" }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByRole("link", { name: "Open the task's current plan" })).toHaveAttribute("href", "/tasks/t1");
  });
});

describe("plan discussion limits (Owner)", () => {
  it("edits the limits with range checks before saving", async () => {
    let saved: unknown;
    const settings: AgentSettings = { switches: [], bindings: [], cooldowns: [] };
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/agents", () => json(settings)),
      http.get("/api/plan-discussion/limits", () => json({ ...limits, answerTimeoutSec: 630 })),
      http.put("/api/plan-discussion/limits", async ({ request }) => {
        saved = await request.json();
        return json(saved);
      }),
    );
    const { container } = await renderApp("/agents");
    const user = userEvent.setup();
    const section = await screen.findByRole("region", { name: "Plan discussion limits" });
    const chars = await within(section).findByLabelText("Characters per message");
    await waitFor(() => expect(chars).toHaveValue(4000));
    expect(chars).toHaveAccessibleDescription(/From 1 to 8000/);
    await expectAccessible(container);

    await user.clear(chars);
    await fill(user, chars, "9000");
    await user.click(within(section).getByRole("button", { name: "Save limits" }));
    expect(chars).toHaveAccessibleDescription(/between 1 and 8000/);
    expect(chars).toHaveFocus();
    expect(saved).toBeUndefined();

    await user.clear(chars);
    await fill(user, chars, "2000");
    await user.click(within(section).getByRole("button", { name: "Save limits" }));
    await expectStatus("Plan discussion limits saved.");
    // The timeout field shows whole minutes; untouched, its exact seconds are kept.
    expect(saved).toEqual({ maxMessageChars: 2000, maxMessagesPerPlan: 20, maxMessagesPerHour: 30, answerTimeoutSec: 630 });
    const timeout = within(section).getByLabelText("Answer timeout, minutes");
    await user.clear(timeout);
    await fill(user, timeout, "15");
    await user.click(within(section).getByRole("button", { name: "Save limits" }));
    await waitFor(() =>
      expect(saved).toEqual({ maxMessageChars: 2000, maxMessagesPerPlan: 20, maxMessagesPerHour: 30, answerTimeoutSec: 900 }));
  });
});
