import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import type { Me, PlanRevision, PlanStep, RevisionDiff } from "../../api/types";
import { apiError, json, server } from "../../test/msw";
import { expectAccessible, renderApp } from "../../test/render";

const member: Me = { id: "u2", login: "bob", role: "user", mustChangePassword: false };

const step = (key: string, title: string, scope: string[], extra: Partial<PlanStep> = {}): PlanStep => ({
  key, title, repository: "acme/api", scope, dependsOn: [], capabilities: [], ...extra,
});

const rev = (n: number, summary: string, steps: PlanStep[], tests: string[]): PlanRevision => ({
  number: n,
  hash: String(n).repeat(64),
  risk: "L1",
  createdBy: n === 1 ? "planner/claude" : "chat",
  createdAt: "2026-11-24T10:00:00Z",
  plan: { summary, risk: "L1", maxParallel: 1, steps, tests, contracts: [], migrations: [], docs: [] },
});

const r1 = rev(1, "Login", [step("api", "API", ["internal/api"])], []);
const r2 = rev(2, "Login", [step("api", "API", ["internal/api"]), step("docs", "Docs", ["docs"])], ["go test ./..."]);
const r3 = rev(3, "Login with 2FA", [step("api", "API", ["internal/api", "internal/auth"]), step("web", "Web form", ["web/**"])],
  ["go test ./...", "make lint"]);

const diff32: RevisionDiff = {
  task: { id: "t1", title: "Add login", state: "READY", planRevision: 3 },
  revision: r3,
  previous: r2,
  changes: [
    { kind: "modified", field: "summary", before: "Login", after: "Login with 2FA", added: [], removed: [] },
    { kind: "modified", field: "tests", before: "go test ./...", after: "go test ./..., make lint", added: ["make lint"], removed: [] },
    { kind: "modified", stepKey: "api", field: "scope", before: "internal/api", after: "internal/api, internal/auth",
      added: ["internal/auth"], removed: [] },
    { kind: "removed", stepKey: "docs", field: "step", before: "Docs", after: "", added: [], removed: [] },
    { kind: "added", stepKey: "web", field: "step", before: "", after: "Web form", added: [], removed: [] },
  ],
};
const diff31: RevisionDiff = { ...diff32, previous: r1, changes: [diff32.changes[0]!] };

function revisions() {
  const seen: string[] = [];
  server.use(
    http.get("/api/auth/me", () => json(member)),
    http.get("/api/tasks/t1/revisions/:n", ({ params, request }) => {
      const base = new URL(request.url).searchParams.get("base");
      seen.push(`${String(params.n)}${base ? `?base=${base}` : ""}`);
      if (params.n === "1") return json<RevisionDiff>({ ...diff32, revision: r1, previous: null, changes: [] });
      if (params.n === "9") return apiError(404, "not_found");
      return json(base === "1" ? diff31 : diff32);
    }),
  );
  return seen;
}

describe("plan revision page", () => {
  it("shows what changed since the previous revision as a structured diff, for a project member", async () => {
    revisions();
    const { container } = await renderApp("/tasks/t1/plan/revisions/3");
    expect(await screen.findByRole("heading", { level: 1, name: "Plan revision 3" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Add login" })).toHaveAttribute("href", "/tasks/t1");
    expect(screen.getByText("Current revision")).toBeInTheDocument();

    const changes = screen.getByRole("region", { name: "Changes since revision 2" });
    const plan = within(changes).getByRole("region", { name: "Plan" });
    expect(within(plan).getByText("Login with 2FA").tagName).toBe("INS");
    expect(within(plan).getByText("make lint").closest("ins")).not.toBeNull();

    const added = within(changes).getByRole("region", { name: "Steps added (1)" });
    expect(within(added).getByText("Web form")).toBeInTheDocument();
    expect(within(added).getByText("web/**")).toBeInTheDocument();
    const removed = within(changes).getByRole("region", { name: "Steps removed (1)" });
    expect(within(removed).getByText("Docs")).toBeInTheDocument();
    const changed = within(changes).getByRole("region", { name: "Steps changed (1)" });
    const api = within(changed).getByRole("article", { name: /API/ });
    expect(within(api).getByText("Scope")).toBeInTheDocument();
    expect(within(api).getByText("internal/auth").closest("ins")).not.toBeNull();

    const nav = screen.getByRole("navigation", { name: "Plan revisions" });
    expect(within(nav).getByRole("link", { name: /Revision 2/ })).toHaveAttribute("href", "/tasks/t1/plan/revisions/2");
    expect(within(nav).queryByRole("link", { name: /Revision 4/ })).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("compares with any other revision", async () => {
    const seen = revisions();
    await renderApp("/tasks/t1/plan/revisions/3");
    const user = userEvent.setup();
    const select = await screen.findByLabelText("Compare with");
    expect(select).toHaveValue("2");
    await user.selectOptions(select, "1");
    expect(await screen.findByRole("region", { name: "Changes since revision 1" })).toBeInTheDocument();
    expect(seen).toContain("3?base=1");
  });

  it("says the first revision has nothing to compare with and still shows its plan", async () => {
    revisions();
    await renderApp("/tasks/t1/plan/revisions/1");
    expect(await screen.findByRole("heading", { level: 1, name: "Plan revision 1" })).toBeInTheDocument();
    expect(screen.getByText("This is the first revision of the plan: there is nothing to compare it with.")).toBeInTheDocument();
    expect(screen.getByText("Replaced by revision 3")).toBeInTheDocument();
    const plan = screen.getByRole("region", { name: "Steps of revision 1" });
    expect(within(plan).getByText("internal/api")).toBeInTheDocument();
  });

  it("explains a missing revision and links back to the task", async () => {
    revisions();
    await renderApp("/tasks/t1/plan/revisions/9");
    expect(await screen.findByRole("heading", { level: 1, name: "Revision not found" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to the task" })).toHaveAttribute("href", "/tasks/t1");
  });

  it("offers a retry when loading fails", async () => {
    server.use(http.get("/api/auth/me", () => json(member)), http.get("/api/tasks/t1/revisions/2", () => HttpResponse.error()));
    await renderApp("/tasks/t1/plan/revisions/2");
    expect(await screen.findByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

  it("is translated into Russian", async () => {
    revisions();
    const { container } = await renderApp("/tasks/t1/plan/revisions/3", "ru");
    expect(await screen.findByRole("heading", { level: 1, name: "Ревизия плана 3" })).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Изменения с ревизии 2" })).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Добавленные шаги (1)" })).toBeInTheDocument();
    await expectAccessible(container);
  });
});
