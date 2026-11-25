import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse, http } from "msw";
import { describe, expect, it } from "vitest";
import type { Me, Project, YouTrackConnection } from "../../api/types";
import { json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, fill, renderApp } from "../../test/render";
import type { IntakeSettings, ProjectYouTrack } from "./intakeApi";

const member: Me = { id: "u2", login: "alice", role: "user", mustChangePassword: false };
const alpha: Project = { id: "p1", slug: "alpha", name: "Alpha", createdAt: "2026-11-24T10:00:00Z", myRole: "member" };
const invalid = (code: string, field: string) => HttpResponse.json({ error: "validation", message: code, field }, { status: 422 });

const connection: YouTrackConnection = {
  baseUrl: "https://acme.youtrack.cloud",
  token: { set: true, updatedAt: "2026-11-24T10:00:00Z" },
  updatedAt: "2026-11-24T10:00:00Z",
  check: { status: "ok", detail: "robot", requestedAt: null, checkedAt: null },
};

function settings(over: Partial<IntakeSettings> = {}): IntakeSettings {
  return {
    pollIntervalSeconds: 60,
    webhookSecret: { set: false, updatedAt: null },
    webhookPath: "/api/webhooks/youtrack",
    webhookHeader: "X-Kindevai-Webhook-Secret",
    links: [],
    health: { state: "ok", consecutiveFailures: 0, lastFailureAt: null },
    ...over,
  };
}

function view(over: Partial<ProjectYouTrack> = {}): ProjectYouTrack {
  return {
    connected: true,
    binding: { shortName: "KIN", triggerTag: "kindevai", autoPlan: false },
    status: "ok",
    lastPolledAt: "2026-11-24T10:00:00Z",
    recent: [],
    ...over,
  };
}

function projectRoutes(me: Me, state: { v: ProjectYouTrack }, myRole?: Project["myRole"]) {
  server.use(
    http.get("/api/auth/me", () => json(me)),
    http.get("/api/projects/p1", () => json({ ...alpha, myRole: myRole ?? (me.role === "owner" ? "owner" : "member") })),
    http.get("/api/projects/p1/members", () => json([])),
    http.get("/api/projects/p1/integrations", () => json({ projectId: "p1", youtrackProject: "KIN", repositories: [] })),
    http.get("/api/projects/p1/youtrack", () => json(state.v)),
  );
}

describe("YouTrack intake settings (owner)", () => {
  it("saves the poll interval, sets a write-only webhook secret and links YouTrack logins", async () => {
    let s = settings();
    const bodies: unknown[] = [];
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/integrations", () => json({ youtrack: connection })),
      http.get("/api/integrations/youtrack/intake", () => json(s)),
      http.put("/api/integrations/youtrack/intake", async ({ request }) => {
        const b = (await request.json()) as { pollIntervalSeconds: number };
        s = { ...s, pollIntervalSeconds: b.pollIntervalSeconds };
        return json(s);
      }),
      http.put("/api/integrations/youtrack/webhook", async ({ request }) => {
        bodies.push(await request.json());
        s = { ...s, webhookSecret: { set: true, updatedAt: "2026-11-24T11:00:00Z" } };
        return json(s);
      }),
      http.post("/api/integrations/youtrack/users", async ({ request }) => {
        const b = (await request.json()) as { login: string; youtrackLogin: string };
        if (b.youtrackLogin === "taken") return HttpResponse.json({ error: "youtrack_login_taken", message: "x", field: "youtrackLogin" }, { status: 409 });
        const l = { userId: "u2", login: b.login, youtrackLogin: b.youtrackLogin, updatedAt: "2026-11-24T11:00:00Z" };
        s = { ...s, links: [l] };
        return json(l);
      }),
      http.delete("/api/integrations/youtrack/users/u2", () => {
        s = { ...s, links: [] };
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { container } = await renderApp("/integrations");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 2, name: "YouTrack intake" })).toBeInTheDocument();
    expect(await screen.findByText(/Webhook disabled: polling alone/)).toBeInTheDocument();
    await expectAccessible(container);

    const poll = screen.getByRole("form", { name: "Polling" });
    const interval = within(poll).getByLabelText("Poll interval, seconds");
    await user.clear(interval);
    await fill(user, interval, "5");
    await user.click(within(poll).getByRole("button", { name: "Save interval" }));
    expect(interval).toHaveAttribute("aria-invalid", "true");
    await user.clear(interval);
    await fill(user, interval, "120");
    await user.click(within(poll).getByRole("button", { name: "Save interval" }));
    await expectStatus("Poll interval saved.");

    const hook = screen.getByRole("form", { name: "Webhook (optional)" });
    await user.click(within(hook).getByRole("button", { name: "Generate secret" }));
    const secretInput = within(hook).getByLabelText("Webhook secret");
    const secret = (secretInput as HTMLInputElement).value;
    expect(secret.length).toBeGreaterThanOrEqual(32);
    await user.click(within(hook).getByRole("button", { name: "Save secret" }));
    await expectStatus("Webhook secret saved.");
    expect(bodies).toEqual([{ secret }]);
    expect(container.innerHTML).not.toContain(secret);
    expect(await screen.findByText(/Webhook enabled, secret updated/)).toBeInTheDocument();

    const link = screen.getByRole("form", { name: "Link user" });
    await fill(user, within(link).getByLabelText("Platform login"), "alice");
    await fill(user, within(link).getByLabelText("YouTrack login"), "taken");
    await user.click(within(link).getByRole("button", { name: "Link user" }));
    expect(await within(link).findByText("This YouTrack login is already linked to another account.")).toBeInTheDocument();
    await user.clear(within(link).getByLabelText("YouTrack login"));
    await fill(user, within(link).getByLabelText("YouTrack login"), "a.smith");
    await user.click(within(link).getByRole("button", { name: "Link user" }));
    await expectStatus("alice is linked to YouTrack login a.smith.");
    await user.click(await screen.findByRole("button", { name: "Unlink alice" }));
    const dialog = screen.getByRole("alertdialog", { name: "Unlink alice from YouTrack login a.smith?" });
    await user.click(within(dialog).getByRole("button", { name: "Unlink" }));
    await expectStatus("alice unlinked.");
    expect(await screen.findByText("No YouTrack logins linked yet.")).toBeInTheDocument();
  });
});

describe("project YouTrack panel", () => {
  it("lets a member run an issue by ID and explains why an issue got no task", async () => {
    const state = {
      v: view({
        recent: [
          { issueId: "KIN-3", outcome: "rejected", reason: "tag_not_allowed", reporter: "alice.yt", channel: "poll", processedAt: "2026-11-24T10:30:00Z" },
          { issueId: "KIN-2", outcome: "rejected", reason: "not_allowed", reporter: "bob.yt", channel: "poll", processedAt: "2026-11-24T10:00:00Z" },
          { issueId: "KIN-1", outcome: "created", initiator: "alice", taskId: "t1", channel: "poll", processedAt: "2026-11-24T09:00:00Z" },
        ],
      }),
    };
    const runs: unknown[] = [];
    projectRoutes(member, state);
    server.use(
      http.post("/api/projects/p1/youtrack/runs", async ({ request }) => {
        const b = (await request.json()) as { issueId: string };
        runs.push(b);
        if (b.issueId === "X-1") return invalid("issue_project", "issueId");
        const e = { issueId: b.issueId, outcome: "queued" as const, initiator: "alice", channel: "manual" as const, processedAt: "2026-11-24T11:00:00Z" };
        state.v = { ...state.v, recent: [e, ...state.v.recent] };
        return json(e, 202);
      }),
    );
    const { container } = await renderApp("/projects/p1");
    const user = userEvent.setup();
    expect(await screen.findByText("Issues of YouTrack project KIN tagged “kindevai” become tasks.")).toBeInTheDocument();
    expect(screen.getByText(/Creating an issue is not the same as the right to start AI/)).toBeInTheDocument();
    expect(screen.getByText(/Whoever applied the trigger tag is not linked to an account that may start AI/)).toBeInTheDocument();
    expect(screen.queryByText("YouTrack: alice.yt")).not.toBeInTheDocument(); // the reporter is not who tagged it
    expect(screen.queryByLabelText("Trigger tag")).not.toBeInTheDocument(); // Owner only
    await expectAccessible(container);

    const form = screen.getByRole("form", { name: "Run by issue ID" });
    await user.click(within(form).getByRole("button", { name: "Run" }));
    expect(within(form).getByLabelText("Issue ID")).toHaveFocus();
    await fill(user, within(form).getByLabelText("Issue ID"), "X-1");
    await user.click(within(form).getByRole("button", { name: "Run" }));
    expect(await within(form).findByText("This issue belongs to another YouTrack project than the one linked here.")).toBeInTheDocument();
    await user.clear(within(form).getByLabelText("Issue ID"));
    await fill(user, within(form).getByLabelText("Issue ID"), "KIN-5");
    await user.click(within(form).getByRole("button", { name: "Run" }));
    await expectStatus("KIN-5 queued. It appears below once YouTrack answers.");
    expect(await screen.findByText("Queued")).toBeInTheDocument();
    expect(runs).toEqual([{ issueId: "X-1" }, { issueId: "KIN-5" }]);
  });

  it("shows states for an unconnected installation, an unbound project and an outage (RU)", async () => {
    const state = { v: view({ connected: false, binding: null }) };
    projectRoutes(member, state);
    const first = await renderApp("/projects/p1", "ru");
    expect(await screen.findByText(/YouTrack не подключён/)).toBeInTheDocument();
    first.unmount();

    state.v = view({ binding: null });
    const second = await renderApp("/projects/p1", "ru");
    expect(await screen.findByText("Проект ещё не привязан к проекту YouTrack.")).toBeInTheDocument();
    second.unmount();

    state.v = view({ status: "degraded", lastPolledAt: null });
    const { container } = await renderApp("/projects/p1", "ru");
    expect(await screen.findByText(/Проблемы: последние запросы к YouTrack/)).toBeInTheDocument();
    expect(screen.getByText("Ещё не проверялось.")).toBeInTheDocument();
    expect(screen.getByRole("form", { name: "Запустить по ID задачи" })).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("lets the Owner change the trigger tag", async () => {
    const state = { v: view() };
    const tags: unknown[] = [];
    projectRoutes(owner, state);
    server.use(
      http.put("/api/projects/p1/integrations/youtrack/trigger", async ({ request }) => {
        const b = (await request.json()) as { triggerTag: string };
        tags.push(b);
        if (b.triggerTag.includes("}")) return invalid("tag_format", "triggerTag");
        state.v = view({ binding: { shortName: "KIN", triggerTag: b.triggerTag, autoPlan: false } });
        return json({ shortName: "KIN", triggerTag: b.triggerTag, autoPlan: false });
      }),
    );
    await renderApp("/projects/p1");
    const user = userEvent.setup();
    const input = await screen.findByLabelText("Trigger tag");
    await user.clear(input);
    await user.type(input, "a}}b");
    await user.click(screen.getByRole("button", { name: "Save tag" }));
    expect(await screen.findByText(/Use up to 50 letters/)).toBeInTheDocument();
    await user.clear(screen.getByLabelText("Trigger tag"));
    await fill(user, screen.getByLabelText("Trigger tag"), "ai run");
    await user.click(screen.getByRole("button", { name: "Save tag" }));
    await expectStatus("Trigger tag saved.");
    expect(await screen.findByText("Issues of YouTrack project KIN tagged “ai run” become tasks.")).toBeInTheDocument();
  });

  it("lets the project admin turn on automatic planning; members only see it (KIN-85)", async () => {
    const state = { v: view() };
    const puts: unknown[] = [];
    projectRoutes(member, state, "project_admin");
    server.use(
      http.put("/api/projects/p1/integrations/youtrack/autoplan", async ({ request }) => {
        const b = (await request.json()) as { autoPlan: boolean };
        puts.push(b);
        state.v = view({ binding: { shortName: "KIN", triggerTag: "kindevai", autoPlan: b.autoPlan } });
        return json(state.v.binding);
      }),
    );
    const first = await renderApp("/projects/p1");
    const user = userEvent.setup();
    const box = await screen.findByRole("checkbox", { name: "Plan new tasks from YouTrack with AI automatically" });
    expect(box).not.toBeChecked();
    await expectAccessible(first.container);
    await user.click(box);
    await expectStatus("New tasks from YouTrack are now planned by AI automatically.");
    expect(await screen.findByRole("checkbox", { name: "Plan new tasks from YouTrack with AI automatically" })).toBeChecked();
    expect(puts).toEqual([{ autoPlan: true }]);
    first.unmount();

    projectRoutes(member, state);
    await renderApp("/projects/p1", "ru");
    expect(await screen.findByText("Новые задачи из YouTrack планируются AI автоматически.")).toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
  });
});
