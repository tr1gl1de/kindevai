import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { Approval, ApprovalDetail, Me, PlanRevision, Project, RiskPolicy } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, fill, renderApp } from "../../test/render";

const H1 = "1".repeat(64);
const H2 = "2".repeat(64);
const alice: Me = { id: "u2", login: "alice", role: "user", mustChangePassword: false };

const exec: Approval = {
  id: "a2",
  projectId: "p1",
  projectName: "Alpha",
  taskId: "t1",
  taskTitle: "Add billing tables",
  planRevision: 2,
  planHash: H2,
  scope: "execute",
  reason: "plan",
  paths: [],
  risk: "L2",
  proposedRisk: "L1",
  findings: [{ category: "migrations", risk: "L2", pattern: "**/migrations/**", repository: "acme/api", path: "db/migrations" }],
  state: "pending",
  requestedAt: "2026-11-24T10:00:00Z",
  decidedAt: null,
};
const integ: Approval = { ...exec, id: "a3", scope: "integrate_dev", findings: [] };
const old: Approval = { ...exec, id: "a1", planRevision: 1, planHash: H1, state: "superseded", decidedAt: "2026-11-24T10:00:00Z" };

const rev = (n: number, hash: string, summary: string, scope: string[]): PlanRevision => ({
  number: n,
  hash,
  risk: "L2",
  createdBy: n === 1 ? "planner" : "chat",
  createdAt: "2026-11-24T10:00:00Z",
  plan: {
    summary,
    risk: "L1",
    maxParallel: 1,
    steps: [{ key: "backend", title: "Billing API", repository: "acme/api", scope, dependsOn: [], capabilities: ["go"] }],
  },
});

const detail = (a: Approval, canDecide: boolean, taskState = "AWAITING_APPROVAL"): ApprovalDetail => ({
  approval: a,
  task: { id: "t1", title: a.taskTitle, state: taskState, planRevision: 2 },
  revision: rev(2, H2, "Billing tables and API", ["db/migrations", "internal/billing"]),
  previous: rev(1, H1, "Billing tables", ["db/migrations"]),
  changes: [
    { kind: "modified", field: "summary", before: "Billing tables", after: "Billing tables and API" },
    { kind: "modified", stepKey: "backend", field: "scope", before: "db/migrations", after: "db/migrations, internal/billing" },
  ],
  related: [old, a.id === exec.id ? exec : a, integ],
  canDecide,
});

describe("approval center (owner)", () => {
  it("lists pending requests, shows why the risk was raised and the revision diff, and approves with a comment", async () => {
    let body: unknown;
    let decided = false;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/approvals", ({ request }) => {
        const state = new URL(request.url).searchParams.get("state");
        if (state === "pending") return json(decided ? [integ] : [exec, integ]);
        return json([exec, integ, old]);
      }),
      http.get("/api/approvals/a2", () => json(detail(decided ? { ...exec, state: "approved", approver: "owner", decidedAt: "2026-11-24T11:00:00Z" } : exec, !decided))),
      http.post("/api/approvals/a2/approve", async ({ request }) => {
        body = await request.json();
        decided = true;
        return json({ ...exec, state: "approved", approver: "owner", comment: "migration reviewed" });
      }),
    );
    const { container } = await renderApp("/approvals");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Approvals" })).toBeInTheDocument();
    const nav = screen.getByRole("navigation", { name: "Main" });
    expect(within(nav).getByRole("link", { name: "Approvals" })).toBeInTheDocument();
    const rows = await screen.findAllByRole("link", { name: "Add billing tables" });
    expect(rows).toHaveLength(2);
    const execRow = rows[0]!.closest("tr")!;
    expect(within(execRow).getByText("Run the plan")).toBeInTheDocument();
    expect(within(execRow).getByText("L2")).toBeInTheDocument();
    expect(within(rows[1]!.closest("tr")!).getByText("Merge into dev")).toBeInTheDocument();
    await expectAccessible(container);

    await user.click(rows[0]!);
    expect(await screen.findByRole("heading", { level: 1, name: "Add billing tables" })).toBeInTheDocument();
    expect(screen.getByText(/raised from L1 to L2/i)).toBeInTheDocument();
    expect(screen.getByText("**/migrations/**")).toBeInTheDocument();
    const diff = screen.getByRole("region", { name: "Changes since revision 1" });
    expect(within(diff).getByText("Billing tables and API")).toBeInTheDocument();
    expect(within(diff).getByText("db/migrations, internal/billing")).toBeInTheDocument();
    const others = screen.getByRole("region", { name: "Other requests for this task" });
    expect(within(others).getByText("Merge into dev")).toBeInTheDocument();
    await expectAccessible(container);

    await fill(user, screen.getByLabelText("Comment"), "migration reviewed");
    await user.click(screen.getByRole("button", { name: "Approve revision 2" }));
    await expectStatus("Approved.");
    expect(body).toEqual({ planRevision: 2, planHash: H2, comment: "migration reviewed" });
    expect(await screen.findByText(/approved by owner/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve revision 2" })).not.toBeInTheDocument();
  });

  it("needs a comment to reject and explains a stale revision", async () => {
    let calls = 0;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/approvals/a2", () => json(detail(exec, true))),
      http.post("/api/approvals/a2/reject", () => {
        calls++;
        return apiError(409, "stale_revision");
      }),
    );
    await renderApp("/approvals/a2");
    const user = userEvent.setup();
    await screen.findByRole("heading", { level: 1, name: "Add billing tables" });
    await user.click(screen.getByRole("button", { name: "Reject revision 2" }));
    const comment = screen.getByLabelText("Comment");
    expect(comment).toHaveAttribute("aria-invalid", "true");
    expect(comment).toHaveAccessibleDescription(/explain why/i);
    expect(comment).toHaveFocus();
    expect(calls).toBe(0);

    await fill(user, comment, "split the migration");
    await user.click(screen.getByRole("button", { name: "Reject revision 2" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/Revision 2 is no longer current/i);
    expect(calls).toBe(1);
    expect(comment).toHaveValue("split the migration");
  });

  it("decides merging into dev only after verification", async () => {
    let state = "READY";
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/approvals/a3", () => json(detail(integ, true, state))),
      http.post("/api/approvals/a3/approve", () => apiError(409, "not_ready")),
    );
    const first = await renderApp("/approvals/a3");
    await screen.findByRole("heading", { level: 1, name: "Add billing tables" });
    expect(screen.getByText(/after the task has passed verification/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve revision 2" })).not.toBeInTheDocument();
    first.unmount();

    state = "VERIFYING"; // the page still shows VERIFYING, but the task moved on meanwhile: the server refuses
    await renderApp("/approvals/a3");
    await userEvent.setup().click(await screen.findByRole("button", { name: "Approve revision 2" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/not ready/i);
  });

  it("shows the files of a step that went beyond its approved scope", async () => {
    const scope: Approval = { ...exec, id: "a9", reason: "scope_exceeded", stepKey: "backend", paths: ["web/app.ts"], findings: [] };
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/approvals/a9", () => json({ ...detail(scope, true), previous: null, changes: [] })),
    );
    const { container } = await renderApp("/approvals/a9");
    await screen.findByRole("heading", { level: 1, name: "Add billing tables" });
    const files = screen.getByRole("region", { name: "Files beyond the approved scope" });
    expect(within(files).getByText("web/app.ts")).toBeInTheDocument();
    expect(screen.getByText(/step backend changed files/i)).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: /changes since/i })).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("shows an empty state", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)), http.get("/api/approvals", () => json([])));
    await renderApp("/approvals");
    expect(await screen.findByText("Nothing waits for a decision.")).toBeInTheDocument();
  });
});

describe("approval center (project admin)", () => {
  it("shows the details read-only", async () => {
    server.use(http.get("/api/auth/me", () => json(alice)), http.get("/api/approvals/a2", () => json(detail(exec, false))));
    const { container } = await renderApp("/approvals/a2");
    await screen.findByRole("heading", { level: 1, name: "Add billing tables" });
    expect(screen.getByText("Only the Owner can approve or reject.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /approve/i })).not.toBeInTheDocument();
    await expectAccessible(container);
  });
});

describe("approval center in Russian", () => {
  it("translates the list", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)), http.get("/api/approvals", () => json([exec])));
    await renderApp("/approvals", "ru");
    expect(await screen.findByRole("heading", { level: 1, name: "Согласования" })).toBeInTheDocument();
    expect(await screen.findByText("Выполнение плана")).toBeInTheDocument();
  });
});

describe("project risk policy", () => {
  const project: Project = { id: "p1", slug: "alpha", name: "Alpha", createdAt: "2026-11-24T10:00:00Z", myRole: "owner" };
  const policy: RiskPolicy = {
    custom: false,
    rules: [
      { category: "paths", risk: "L2", patterns: [".github/workflows/**"] },
      { category: "migrations", risk: "L2", patterns: ["**/migrations/**"] },
      { category: "security", risk: "L3", patterns: ["**/auth/**"] },
      { category: "db_schema", risk: "L2", patterns: ["**/schema.*"] },
    ],
  };
  const projectHandlers = [
    http.get("/api/projects/p1", () => json(project)),
    http.get("/api/projects/p1/members", () => json([])),
    http.get("/api/projects/p1/integrations", () => json({ projectId: "p1", youtrackProject: "", repositories: [] })),
    http.get("/api/integrations/youtrack", () => json({ configured: false })),
  ];

  it("lets the Owner edit the rules", async () => {
    let body: unknown;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      ...projectHandlers,
      http.get("/api/projects/p1/risk-policy", () => json(policy)),
      http.put("/api/projects/p1/risk-policy", async ({ request }) => {
        body = await request.json();
        const b = body as { rules: RiskPolicy["rules"] };
        if (b.rules[0]!.patterns.includes("../x")) return json({ error: "validation", message: "pattern_format", field: "rules[0].patterns" }, 422);
        return json({ custom: true, rules: b.rules });
      }),
    );
    await renderApp("/projects/p1");
    const user = userEvent.setup();
    const section = await screen.findByRole("region", { name: "Risk policy" });
    expect(await within(section).findByText(/default rules apply/i)).toBeInTheDocument();
    const paths = within(section).getByLabelText("Paths: patterns");
    await user.clear(paths);
    await user.type(paths, "billing/**{enter}../x");
    await user.click(within(section).getByRole("button", { name: "Save risk policy" }));
    expect(await within(section).findByText(/relative path pattern/i)).toBeInTheDocument();
    expect(paths).toHaveAttribute("aria-invalid", "true");

    await user.clear(paths);
    await fill(user, paths, "billing/**");
    await user.selectOptions(within(section).getByLabelText("Paths: minimum risk"), "L3");
    await user.click(within(section).getByRole("button", { name: "Save risk policy" }));
    await expectStatus("Risk policy saved.");
    expect((body as { rules: RiskPolicy["rules"] }).rules[0]).toEqual({ category: "paths", risk: "L3", patterns: ["billing/**"] });
  });

  it("is read-only for a project admin", async () => {
    server.use(
      http.get("/api/auth/me", () => json(alice)),
      http.get("/api/projects/p1", () => json({ ...project, myRole: "project_admin" })),
      http.get("/api/projects/p1/members", () => json([])),
      http.get("/api/projects/p1/risk-policy", () => json(policy)),
    );
    const { container } = await renderApp("/projects/p1");
    const section = await screen.findByRole("region", { name: "Risk policy" });
    expect(await within(section).findByText("**/auth/**")).toBeInTheDocument();
    expect(within(section).queryByRole("button", { name: "Save risk policy" })).not.toBeInTheDocument();
    await expectAccessible(container);
  });
});
