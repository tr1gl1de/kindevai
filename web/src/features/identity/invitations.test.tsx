import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { Invitation, Project } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, renderApp } from "../../test/render";

const alpha: Project = { id: "p1", slug: "alpha", name: "Alpha", createdAt: "2026-11-24T10:00:00Z", myRole: "owner" };
const TOKEN = "tok_abcdefghijklmnopqrstuvwxyz0123456789ABCDEF";

describe("invitations (owner)", () => {
  it("creates a one-time link, lists it and revokes it", async () => {
    const invitations: Invitation[] = [];
    let created: unknown;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/users", () => json([])),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/invitations", () => json(invitations)),
      http.post("/api/invitations", async ({ request }) => {
        created = await request.json();
        const inv: Invitation = { id: "i1", projectId: "p1", projectName: "Alpha", role: "project_admin", state: "pending", createdAt: "2026-11-24T10:00:00Z", expiresAt: "2026-11-25T10:00:00Z" };
        invitations.push(inv);
        return json({ invitation: inv, token: TOKEN }, 201);
      }),
      http.post("/api/invitations/i1/revoke", () => {
        invitations[0] = { ...invitations[0]!, state: "revoked" };
        return new Response(null, { status: 204 });
      }),
    );
    const { container, client } = await renderApp("/users");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 2, name: "Invitations" })).toBeInTheDocument();
    expect(await screen.findByText("No invitations yet.")).toBeInTheDocument();
    await expectAccessible(container);

    const form = screen.getByRole("form", { name: "New invitation" });
    await user.click(within(form).getByRole("button", { name: "Create invitation link" }));
    expect(within(form).getByLabelText("Project")).toHaveAccessibleDescription("Choose a project");
    await user.selectOptions(within(form).getByLabelText("Project"), "Alpha");
    await user.selectOptions(within(form).getByLabelText("Role"), "Project admin");
    await user.selectOptions(within(form).getByLabelText("Valid for"), "1 day");
    await user.click(within(form).getByRole("button", { name: "Create invitation link" }));

    const secret = await screen.findByRole("region", { name: "Invitation link for Alpha" });
    expect(created).toEqual({ projectId: "p1", role: "project_admin", ttlHours: 24 });
    expect(within(secret).getByText(`http://localhost:3000/invite#token=${TOKEN}`)).toBeInTheDocument();
    expect(within(secret).getByRole("heading", { name: "Invitation link for Alpha" })).toHaveFocus();
    expect(within(form).getByRole("button", { name: "Create invitation link" })).toBeDisabled();
    await user.click(within(secret).getByRole("button", { name: "Done" }));
    expect(screen.queryByText(new RegExp(TOKEN))).not.toBeInTheDocument();
    expect(JSON.stringify(client.getMutationCache().getAll().map((m) => m.state.data))).not.toContain(TOKEN);

    const row = (await screen.findByRole("cell", { name: "Waiting" })).closest("tr")!;
    await user.click(within(row).getByRole("button", { name: "Revoke the invitation to Alpha" }));
    const dialog = screen.getByRole("alertdialog", { name: "Revoke the invitation to Alpha?" });
    await user.click(within(dialog).getByRole("button", { name: "Revoke" }));
    expect(await screen.findByRole("cell", { name: "Revoked" })).toBeInTheDocument();
    await expectStatus("The invitation to Alpha is revoked.");
  });

  it("asks for a project first when there is none", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)), http.get("/api/users", () => json([])));
    await renderApp("/users");
    expect(await screen.findByText("Create a project first to invite people to it.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Go to projects" })).toHaveAttribute("href", "/projects");
  });
});

describe("accept invitation page", () => {
  it("shows the offer and creates the account without a session", async () => {
    let accepted: unknown;
    let previews = 0;
    server.use(
      http.post("/api/invitations/preview", () => {
        previews++;
        return json({ projectName: "Alpha", role: "member", expiresAt: "2026-11-27T10:00:00Z" });
      }),
      http.post("/api/invitations/accept", async ({ request }) => {
        accepted = await request.json();
        const { login } = accepted as { login: string };
        if (login === "taken") return apiError(409, "login_taken", "login");
        return json({ user: { id: "u9", login, role: "user", mustChangePassword: false } }, 201);
      }),
    );
    const { container, client } = await renderApp(`/invite#token=${TOKEN}`);
    const user = userEvent.setup();
    expect(await screen.findByText(/You are invited to Alpha as Member/)).toBeInTheDocument();
    await expectAccessible(container);

    await user.type(screen.getByLabelText("Login"), "taken");
    await user.type(screen.getByLabelText("Password"), "dave's long password");
    await user.type(screen.getByLabelText("Repeat password"), "dave's long passwor");
    await user.click(screen.getByRole("button", { name: "Create account" }));
    expect(screen.getByLabelText("Repeat password")).toHaveAccessibleDescription(/do not match/i);
    await user.type(screen.getByLabelText("Repeat password"), "d");
    await user.click(screen.getByRole("button", { name: "Create account" }));
    expect(await screen.findByLabelText("Login")).toHaveAccessibleDescription(/already taken/i);
    expect(screen.getByLabelText("Password")).toHaveValue("dave's long password"); // nothing typed is lost

    await user.clear(screen.getByLabelText("Login"));
    await user.type(screen.getByLabelText("Login"), "dave");
    await user.click(screen.getByRole("button", { name: "Create account" }));
    expect(await screen.findByRole("heading", { name: "Account created" })).toHaveFocus();
    expect(accepted).toEqual({ token: TOKEN, login: "dave", password: "dave's long password" });
    window.dispatchEvent(new Event("focus")); // coming back to the tab must not re-send the token
    window.dispatchEvent(new Event("visibilitychange"));
    expect(previews).toBe(1);
    expect(JSON.stringify(client.getMutationCache().getAll().map((m) => m.state.variables))).not.toContain(TOKEN);
    await user.click(screen.getByRole("button", { name: "Go to sign in" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Sign in" })).toBeInTheDocument();
  });

  it.each([
    ["invitation_used", 410, /already been used/],
    ["invitation_expired", 410, /has expired/],
    ["invitation_revoked", 410, /was revoked/],
    ["invitation_invalid", 404, /not valid/],
  ])("explains %s", async (code, status, text) => {
    server.use(http.post("/api/invitations/preview", () => apiError(status, code)));
    await renderApp(`/invite#token=${TOKEN}`);
    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
  });

  it("explains a link without a token and works in Russian", async () => {
    const { container } = await renderApp("/invite", "ru");
    expect(await screen.findByRole("heading", { level: 1, name: "Вход по приглашению" })).toBeInTheDocument();
    expect(screen.getByText(/ссылка-приглашение недействительна/)).toBeInTheDocument();
    await expectAccessible(container);
  });
});
