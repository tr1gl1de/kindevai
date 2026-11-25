import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type {
  AgentBinding,
  AgentBindingRequest,
  AgentSettings,
  Me,
  Project,
  ProjectAgentSettings,
} from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, renderApp } from "../../test/render";

const bindings: AgentBinding[] = [
  {
    role: "planner",
    primary: { agent: "claude", model: "" },
    fallback: [{ agent: "codex", model: "" }],
    strategy: "ordered",
    source: "default",
    updatedBy: "",
    updatedAt: null,
  },
  {
    role: "implementer",
    primary: { agent: "codex", model: "gpt-5" },
    fallback: [],
    strategy: "ordered",
    source: "global",
    updatedBy: "owner",
    updatedAt: "2026-11-25T10:00:00Z",
  },
  {
    role: "reviewer",
    primary: { agent: "codex", model: "" },
    fallback: [{ agent: "claude", model: "" }],
    strategy: "different-from-author",
    source: "default",
    updatedBy: "",
    updatedAt: null,
  },
];

const settings: AgentSettings = {
  switches: [
    {
      agent: "claude",
      enabled: true,
      set: false,
      updatedBy: "",
      updatedAt: null,
    },
    {
      agent: "codex",
      enabled: false,
      set: true,
      updatedBy: "owner",
      updatedAt: "2026-11-25T10:00:00Z",
    },
  ],
  bindings,
  cooldowns: [
    {
      runnerId: "r-wsl",
      agent: "claude",
      until: "2026-11-25T13:00:00Z",
      reason: "usage limit reached",
    },
  ],
};

describe("agents page (Owner)", () => {
  it("shows switches, bindings and cool-downs, and is accessible", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/agents", () => json(settings)),
    );
    const { container } = await renderApp("/agents");
    const row = (
      await screen.findByRole("rowheader", { name: "Codex" })
    ).closest("tr")!;
    expect(within(row).getByText("Disabled")).toBeInTheDocument();
    expect(
      within(row).getByRole("button", { name: "Enable Codex" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: "Implementer" }),
    ).toBeInTheDocument();
    expect(screen.getByText("usage limit reached")).toBeInTheDocument();
    expect(
      screen.getByRole("combobox", { name: "Review strategy" }),
    ).toHaveValue("different-from-author");
    await expectAccessible(container);
  });

  it("disables a CLI and confirms an emergency stop", async () => {
    const calls: string[] = [];
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/agents", () => json(settings)),
      http.put("/api/agents/switches/:agent", async ({ params, request }) => {
        calls.push(
          `switch ${params.agent} ${JSON.stringify(await request.json())}`,
        );
        return new Response(null, { status: 204 });
      }),
      http.post("/api/agents/switches/:agent/emergency-stop", ({ params }) => {
        calls.push(`stop ${params.agent}`);
        return json({ stopped: 2 });
      }),
    );
    await renderApp("/agents");
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole("button", { name: "Disable Claude Code" }),
    );
    await expectStatus(
      "Claude Code disabled. Running sessions finish; no new ones start.",
    );
    await user.click(
      screen.getByRole("button", { name: "Emergency stop Claude Code" }),
    );
    const dialog = screen.getByRole("alertdialog", {
      name: "Stop Claude Code now?",
    });
    await user.click(within(dialog).getByRole("button", { name: "Stop now" }));
    await expectStatus("Claude Code stopped: 2 sessions cancelled.");
    expect(calls).toEqual(['switch claude {"enabled":false}', "stop claude"]);
  });

  it("saves a binding with a fallback and shows a field error from the server", async () => {
    let body: AgentBindingRequest | undefined;
    let first = true;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/agents", () => json(settings)),
      http.put("/api/agents/bindings/implementer", async ({ request }) => {
        body = (await request.json()) as AgentBindingRequest;
        if (first) {
          first = false;
          return json(
            {
              error: "validation",
              message: "model_format",
              field: "fallback[0].model",
            },
            422,
          );
        }
        return new Response(null, { status: 204 });
      }),
    );
    await renderApp("/agents");
    const user = userEvent.setup();
    const form = (
      await screen.findByRole("heading", { name: "Implementer" })
    ).closest("form")!;
    await user.click(
      within(form).getByRole("button", { name: "Add fallback" }),
    );
    const fb = within(form).getByRole("group", { name: "Fallback 1" });
    await user.selectOptions(
      within(fb).getByRole("combobox", { name: "CLI" }),
      "claude",
    );
    await user.type(within(fb).getByRole("textbox", { name: "Model" }), "opus");
    await user.click(
      within(form).getByRole("button", { name: "Save Implementer binding" }),
    );
    expect(
      await within(fb).findByText(/Use letters, digits/),
    ).toBeInTheDocument();
    await user.click(
      within(form).getByRole("button", { name: "Save Implementer binding" }),
    );
    await expectStatus("Implementer binding saved.");
    expect(body).toEqual({
      primary: { agent: "codex", model: "gpt-5" },
      fallback: [{ agent: "claude", model: "opus" }],
      strategy: "ordered",
    });
  });

  it("is shown in Russian", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/agents", () => json(settings)),
    );
    await renderApp("/agents", "ru");
    expect(
      await screen.findByRole("heading", { name: "Агенты", level: 1 }),
    ).toBeInTheDocument();
    expect(
      await screen.findByRole("button", { name: "Аварийно остановить Codex" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: "Ревьюер" }),
    ).toBeInTheDocument();
  });
});

describe("project agents panel (Project Admin)", () => {
  const admin: Me = {
    id: "u2",
    login: "alice",
    role: "user",
    mustChangePassword: false,
  };
  const web: Project = {
    id: "p1",
    slug: "web",
    name: "Web",
    myRole: "project_admin",
    createdAt: "2026-11-24T10:00:00Z",
  };
  const project: ProjectAgentSettings = {
    projectId: "p1",
    agents: [
      {
        agent: "claude",
        global: true,
        project: {
          agent: "claude",
          enabled: true,
          set: false,
          updatedBy: "",
          updatedAt: null,
        },
        effective: true,
      },
      {
        agent: "codex",
        global: false,
        project: {
          agent: "codex",
          enabled: false,
          set: true,
          updatedBy: "alice",
          updatedAt: null,
        },
        effective: false,
      },
    ],
    bindings: bindings.map((b) =>
      b.role === "implementer" ? { ...b, source: "project" } : b,
    ),
  };

  it("disables a CLI in the project; a CLI the Owner disabled cannot be enabled", async () => {
    let calls = 0;
    server.use(
      http.get("/api/auth/me", () => json(admin)),
      http.get("/api/projects/p1", () => json(web)),
      http.get("/api/projects/p1/members", () => json([])),
      http.get("/api/projects/p1/risk-policy", () =>
        apiError(403, "forbidden"),
      ),
      http.get("/api/projects/p1/youtrack", () => apiError(404, "not_bound")),
      http.get("/api/projects/p1/agents", () => json(project)),
      http.put("/api/projects/p1/agents/switches/:agent", () => {
        calls++;
        return new Response(null, { status: 204 });
      }),
    );
    const { container } = await renderApp("/projects/p1");
    const row = (
      await screen.findByRole("rowheader", { name: "Codex" })
    ).closest("tr")!;
    expect(
      within(row).getByText("Only the Owner can enable it"),
    ).toBeInTheDocument();
    expect(within(row).queryByRole("button")).toBeNull();
    const user = userEvent.setup();
    await user.click(
      screen.getByRole("button", {
        name: "Disable Claude Code in this project",
      }),
    );
    await waitFor(() => expect(calls).toBe(1));
    expect(
      screen.getByRole("button", { name: "Use the global binding" }),
    ).toBeInTheDocument();
    await expectAccessible(container);
  });
});
