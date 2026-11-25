import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse, http } from "msw";
import { describe, expect, it } from "vitest";
import type { GitHubApp, Integrations, Me, Project, ProjectIntegrations, YouTrackConnection } from "../../api/types";
import { json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, fill, renderApp } from "../../test/render";

const TOKEN = "perm:cm9vdA==.NDQtMQ==.s3cr3tT0kenValue";
const alpha: Project = { id: "p1", slug: "alpha", name: "Alpha", createdAt: "2026-11-24T10:00:00Z", myRole: "owner" };

/** A 422 as the API sends it: the validation code travels in message. */
const invalid = (code: string, field: string) => HttpResponse.json({ error: "validation", message: code, field }, { status: 422 });

function connection(over: Partial<YouTrackConnection> = {}): YouTrackConnection {
  return {
    baseUrl: "https://acme.youtrack.cloud",
    token: { set: true, updatedAt: "2026-11-24T10:00:00Z" },
    updatedAt: "2026-11-24T10:00:00Z",
    check: { status: "none", requestedAt: null, checkedAt: null },
    ...over,
  };
}

describe("integrations page (owner)", () => {
  it("connects YouTrack write-only and tests the connection in the background", async () => {
    let state: Integrations = { github: null, youtrack: null };
    const bodies: unknown[] = [];
    let polls = 0;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/integrations", () => {
        if (state.youtrack?.check.status === "pending" && ++polls >= 1) {
          state = { github: null, youtrack: connection({ check: { status: "ok", detail: "robot", requestedAt: "2026-11-24T10:00:00Z", checkedAt: "2026-11-24T10:00:01Z" } }) };
        }
        return json(state);
      }),
      http.put("/api/integrations/youtrack", async ({ request }) => {
        bodies.push(await request.json());
        state = { github: null, youtrack: connection() };
        return json(state.youtrack);
      }),
      http.post("/api/integrations/youtrack/check", () => {
        state = { github: null, youtrack: connection({ check: { status: "pending", requestedAt: "2026-11-24T10:00:00Z", checkedAt: null } }) };
        return json(state.youtrack, 202);
      }),
    );
    const { container, client } = await renderApp("/integrations");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Integrations" })).toBeInTheDocument();
    expect(await screen.findByText("Not connected")).toBeInTheDocument();
    await expectAccessible(container);

    await user.click(screen.getByRole("button", { name: "Save connection" }));
    expect(screen.getByLabelText("Base URL")).toHaveFocus();
    expect(screen.getByLabelText("Permanent token")).toHaveAttribute("aria-invalid", "true");

    await fill(user, screen.getByLabelText("Base URL"), "https://acme.youtrack.cloud");
    const tokenInput = screen.getByLabelText("Permanent token");
    expect(tokenInput).toHaveAttribute("type", "password");
    await fill(user, tokenInput, TOKEN);
    await user.click(screen.getByRole("button", { name: "Save connection" }));
    await expectStatus("YouTrack connection saved.");
    expect(bodies).toEqual([{ baseUrl: "https://acme.youtrack.cloud", token: TOKEN }]);
    expect(screen.getByLabelText("Permanent token")).toHaveValue("");
    expect(screen.getByText(/Token set, updated/)).toBeInTheDocument();
    expect(container.innerHTML).not.toContain(TOKEN);
    expect(JSON.stringify(client.getMutationCache().getAll().map((m) => m.state))).not.toContain(TOKEN);

    await user.click(screen.getByRole("button", { name: "Test connection" }));
    expect(await screen.findByText("Testing…")).toBeInTheDocument();
    expect(await screen.findByText("Connected as robot.")).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("shows server validation next to the field and keeps the token out of the next save", async () => {
    const bodies: unknown[] = [];
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/integrations", () =>
        json({ github: null, youtrack: connection({ check: { status: "failed", code: "unauthorized", detail: "HTTP 401", requestedAt: null, checkedAt: "2026-11-24T10:00:01Z" } }) }),
      ),
      http.put("/api/integrations/youtrack", async ({ request }) => {
        bodies.push(await request.json());
        return invalid("base_url_scheme", "baseUrl");
      }),
    );
    await renderApp("/integrations");
    const user = userEvent.setup();
    expect(await screen.findByText(/YouTrack rejected the token/)).toBeInTheDocument();
    const url = await screen.findByLabelText("Base URL");
    expect(url).toHaveValue("https://acme.youtrack.cloud");
    await user.clear(url);
    await fill(user, url, "ftp://acme");
    await user.click(screen.getByRole("button", { name: "Save connection" }));
    expect(await screen.findByText("Use an http:// or https:// address.")).toBeInTheDocument();
    expect(url).toHaveFocus();
    expect(bodies).toEqual([{ baseUrl: "ftp://acme", token: "" }]);
  });

  it("disconnects after confirmation", async () => {
    let state: Integrations = { github: null, youtrack: connection() };
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/integrations", () => json(state)),
      http.delete("/api/integrations/youtrack", () => {
        state = { github: null, youtrack: null };
        return new Response(null, { status: 204 });
      }),
    );
    await renderApp("/integrations");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Disconnect" }));
    const dialog = screen.getByRole("alertdialog", { name: "Disconnect YouTrack?" });
    await user.click(within(dialog).getByRole("button", { name: "Disconnect" }));
    await expectStatus("YouTrack disconnected.");
    expect(await screen.findByText("Not connected")).toBeInTheDocument();
  });

  it("is in Russian", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/integrations", () => json({ github: null, youtrack: null })),
    );
    const { container } = await renderApp("/integrations", "ru");
    expect(await screen.findByRole("heading", { level: 1, name: "Интеграции" })).toBeInTheDocument();
    expect(await screen.findByLabelText("Постоянный токен")).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("is hidden from non-owners", async () => {
    const member: Me = { ...owner, id: "u2", login: "bob", role: "user" };
    server.use(
      http.get("/api/auth/me", () => json(member)),
      http.get("/api/projects", () => json([])),
    );
    await renderApp("/integrations");
    expect(await screen.findByRole("heading", { level: 1, name: "Page not found" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Integrations" })).not.toBeInTheDocument();
  });
});

describe("project integrations (owner)", () => {
  it("binds a YouTrack project and manages repositories", async () => {
    let state: ProjectIntegrations = { projectId: "p1", youtrackProject: "", repositories: [] };
    const repo = {
      id: "r1",
      owner: "acme",
      name: "web",
      fullName: "acme/web",
      integrationBranch: "dev",
      protectedBranches: ["main", "master"],
      createdAt: "2026-11-24T10:00:00Z",
      updatedAt: "2026-11-24T10:00:00Z",
      github: { installed: false, defaultBranch: "", protection: "unknown" as const, unprotected: [], sync: { status: "none" as const, requestedAt: null, checkedAt: null } },
    };
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects/p1", () => json(alpha)),
      http.get("/api/projects/p1/members", () => json([])),
      http.get("/api/projects/p1/integrations", () => json(state)),
      http.put("/api/projects/p1/integrations/youtrack", async ({ request }) => {
        const { shortName } = (await request.json()) as { shortName: string };
        state = { ...state, youtrackProject: shortName.toUpperCase() };
        return json(state);
      }),
      http.post("/api/projects/p1/repositories", async ({ request }) => {
        const body = (await request.json()) as { owner: string; name: string; integrationBranch: string };
        if (body.integrationBranch === "main") return invalid("branch_protected", "integrationBranch");
        state = { ...state, repositories: [repo] };
        return json(repo, 201);
      }),
      http.put("/api/projects/p1/repositories/r1", async ({ request }) => {
        const { integrationBranch, testCommand } = (await request.json()) as { integrationBranch: string; testCommand: string };
        if (testCommand.includes("\n")) return invalid("test_command_format", "testCommand");
        state = { ...state, repositories: [{ ...repo, integrationBranch, testCommand }] };
        return json(state.repositories[0]);
      }),
      http.delete("/api/projects/p1/repositories/r1", () => {
        state = { ...state, repositories: [] };
        return new Response(null, { status: 204 });
      }),
    );
    const { container } = await renderApp("/projects/p1");
    const user = userEvent.setup();
    const section = await screen.findByRole("region", { name: "Integrations" });
    expect(await within(section).findByText("No repositories yet.")).toBeInTheDocument();
    await expectAccessible(container);

    await fill(user, within(section).getByLabelText("YouTrack project"), "kin");
    await user.click(within(section).getByRole("button", { name: "Save YouTrack project" }));
    await expectStatus("Alpha is linked to YouTrack project KIN.");

    const add = within(section).getByRole("form", { name: "Add repository" });
    await fill(user, within(add).getByLabelText("Owner"), "acme");
    await fill(user, within(add).getByLabelText("Repository"), "web");
    const branch = within(add).getByLabelText("Integration branch");
    expect(branch).toHaveValue("dev");
    await user.clear(branch);
    await fill(user, branch, "main");
    await user.click(within(add).getByRole("button", { name: "Add repository" }));
    expect(await within(add).findByText("main and master are protected; choose another branch.")).toBeInTheDocument();
    await user.clear(branch);
    await fill(user, branch, "dev");
    await user.click(within(add).getByRole("button", { name: "Add repository" }));
    await expectStatus("acme/web added.");
    const row = (await within(section).findByRole("cell", { name: "acme/web" })).closest("tr")!;
    expect(within(row).getByText("main, master")).toBeInTheDocument();
    expect(within(row).getByText("Not configured")).toBeInTheDocument(); // KIN-35: no test command yet

    await user.click(within(row).getByRole("button", { name: "Change branch of acme/web" }));
    const edit = within(row).getByLabelText("Integration branch");
    await user.clear(edit);
    await fill(user, edit, "next");
    await fill(user, within(row).getByLabelText("Test command"), "make test");
    await user.click(within(row).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(within(section).getByRole("cell", { name: "next" })).toBeInTheDocument());
    expect(within(section).getByText("make test")).toBeInTheDocument();

    const row2 = within(section).getByRole("cell", { name: "acme/web" }).closest("tr")!;
    await user.click(within(row2).getByRole("button", { name: "Remove acme/web" }));
    const dialog = screen.getByRole("alertdialog", { name: "Remove acme/web from Alpha?" });
    await user.click(within(dialog).getByRole("button", { name: "Remove" }));
    await expectStatus("acme/web removed.");
    expect(await within(section).findByText("No repositories yet.")).toBeInTheDocument();
  });
});

const PEM = "-----BEGIN RSA PRIVATE KEY-----\nMIIEsecretkeymaterial\n-----END RSA PRIVATE KEY-----";
const HOOK = "whsec-0123456789abcdef";

function githubApp(over: Partial<GitHubApp> = {}): GitHubApp {
  return {
    appId: 42,
    privateKey: { set: true, updatedAt: "2026-11-24T10:00:00Z" },
    webhookSecret: { set: true, updatedAt: "2026-11-24T10:00:00Z" },
    updatedAt: "2026-11-24T10:00:00Z",
    check: { status: "none", requestedAt: null, checkedAt: null },
    lastDeliveryAt: null,
    ...over,
  };
}

describe("GitHub App (owner)", () => {
  it("configures the App write-only, explains minimal permissions and tests it in the background", async () => {
    let state: Integrations = { youtrack: null, github: null };
    const bodies: unknown[] = [];
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/integrations", () => {
        if (state.github?.check.status === "pending") {
          state = { ...state, github: githubApp({ check: { status: "ok", detail: "kindev-bot", requestedAt: null, checkedAt: "2026-11-24T10:00:01Z" } }) };
        }
        return json(state);
      }),
      http.put("/api/integrations/github", async ({ request }) => {
        const body = (await request.json()) as { appId: string };
        bodies.push(body);
        if (body.appId === "x") return invalid("app_id_format", "appId");
        state = { ...state, github: githubApp() };
        return json(state.github);
      }),
      http.post("/api/integrations/github/check", () => {
        state = { ...state, github: githubApp({ check: { status: "pending", requestedAt: "2026-11-24T10:00:00Z", checkedAt: null } }) };
        return json(state.github, 202);
      }),
    );
    const { container, client } = await renderApp("/integrations");
    const user = userEvent.setup();
    const section = await screen.findByRole("region", { name: "GitHub" });
    expect(await within(section).findByText("No GitHub App configured")).toBeInTheDocument();
    await user.click(within(section).getByText("How to set up the GitHub App"));
    expect(within(section).getByText("Contents: read and write")).toBeInTheDocument();
    expect(within(section).getByText(/Do not grant Administration or Workflows/)).toBeInTheDocument();
    expect(within(section).getByText(`${window.location.origin}/api/webhooks/github`)).toBeInTheDocument();
    await expectAccessible(container);

    await user.click(within(section).getByRole("button", { name: "Save GitHub App" }));
    expect(within(section).getByLabelText("App ID")).toHaveFocus();
    expect(within(section).getByLabelText("Private key")).toHaveAttribute("aria-invalid", "true");

    await fill(user, within(section).getByLabelText("App ID"), "x");
    await fill(user, within(section).getByLabelText("Private key"), PEM);
    const secret = within(section).getByLabelText("Webhook secret");
    expect(secret).toHaveAttribute("type", "password");
    await fill(user, secret, HOOK);
    await user.click(within(section).getByRole("button", { name: "Save GitHub App" }));
    expect(await within(section).findByText("Enter the numeric App ID from the App's settings page.")).toBeInTheDocument();
    expect(within(section).getByLabelText("Private key")).toHaveValue(PEM); // never clear what the user typed

    await user.clear(within(section).getByLabelText("App ID"));
    await fill(user, within(section).getByLabelText("App ID"), "42");
    await user.click(within(section).getByRole("button", { name: "Save GitHub App" }));
    await expectStatus("GitHub App saved. Registered repositories are being read from GitHub.");
    expect(bodies.at(-1)).toEqual({ appId: "42", privateKey: PEM, webhookSecret: HOOK });
    expect(await within(section).findByLabelText("Private key")).toHaveValue("");
    expect(container.innerHTML).not.toContain("secretkeymaterial");
    expect(container.innerHTML).not.toContain(HOOK);
    expect(JSON.stringify(client.getMutationCache().getAll().map((m) => m.state))).not.toContain(HOOK);
    expect(within(section).getByText("No webhook received yet.")).toBeInTheDocument();

    await user.click(within(section).getByRole("button", { name: "Test App" }));
    expect(await within(section).findByText("Authenticated as kindev-bot.")).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("is in Russian", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/integrations", () => json({ youtrack: null, github: githubApp({ lastDeliveryAt: "2026-11-24T10:00:00Z", lastEvent: "push" }) })),
    );
    const { container } = await renderApp("/integrations", "ru");
    const section = await screen.findByRole("region", { name: "GitHub" });
    expect(await within(section).findByLabelText("Закрытый ключ")).toBeInTheDocument();
    expect(within(section).getByText(/Последний вебхук: push/)).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("warns about unprotected main/master and refreshes a repository from GitHub", async () => {
    const base = {
      id: "r1",
      owner: "acme",
      name: "web",
      fullName: "acme/web",
      integrationBranch: "dev",
      protectedBranches: ["main", "master"],
      createdAt: "2026-11-24T10:00:00Z",
      updatedAt: "2026-11-24T10:00:00Z",
    };
    const done = { status: "ok" as const, detail: "unprotected", requestedAt: null, checkedAt: "2026-11-24T10:00:01Z" };
    let state: ProjectIntegrations = {
      projectId: "p1",
      youtrackProject: "",
      repositories: [{ ...base, github: { installed: true, defaultBranch: "main", protection: "unprotected", unprotected: ["main"], sync: done } }],
    };
    let syncs = 0;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects/p1", () => json(alpha)),
      http.get("/api/projects/p1/members", () => json([])),
      http.get("/api/projects/p1/integrations", () => json(state)),
      http.post("/api/projects/p1/repositories/r1/sync", () => {
        syncs++;
        state = {
          ...state,
          repositories: [{ ...base, github: { installed: true, defaultBranch: "main", protection: "protected", unprotected: [], sync: done } }],
        };
        return json(state.repositories[0], 202);
      }),
    );
    const { container } = await renderApp("/projects/p1");
    const user = userEvent.setup();
    const row = (await screen.findByRole("cell", { name: "acme/web" })).closest("tr")!;
    expect(within(row).getByText("App installed; default branch main.")).toBeInTheDocument();
    expect(within(row).getByText("Not protected on GitHub: main. Add a ruleset or branch protection.")).toBeInTheDocument();
    await expectAccessible(container);
    await user.click(within(row).getByRole("button", { name: "Refresh acme/web from GitHub" }));
    expect(await screen.findByText("main/master protected on GitHub.")).toBeInTheDocument();
    expect(syncs).toBe(1);
  });

  it("explains a repository the App is not installed on", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects/p1", () => json(alpha)),
      http.get("/api/projects/p1/members", () => json([])),
      http.get("/api/projects/p1/integrations", () =>
        json({
          projectId: "p1",
          youtrackProject: "",
          repositories: [
            {
              id: "r1",
              owner: "acme",
              name: "web",
              fullName: "acme/web",
              integrationBranch: "dev",
              protectedBranches: ["main", "master"],
              createdAt: "2026-11-24T10:00:00Z",
              updatedAt: "2026-11-24T10:00:00Z",
              github: {
                installed: false,
                defaultBranch: "",
                protection: "unknown",
                unprotected: [],
                sync: { status: "failed", code: "not_installed", detail: "x", requestedAt: null, checkedAt: "2026-11-24T10:00:01Z" },
              },
            },
          ],
        }),
      ),
    );
    await renderApp("/projects/p1", "ru");
    expect(await screen.findByText("Приложение не установлено на этот репозиторий. Установите его в GitHub и обновите.")).toBeInTheDocument();
  });
});
