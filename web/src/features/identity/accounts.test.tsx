import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { Account, Me } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, renderApp } from "../../test/render";

const carol: Account = {
  id: "u2", login: "carol", role: "user", mustChangePassword: false, disabled: false, createdAt: "2026-11-24T10:00:00Z",
};

describe("forced password change", () => {
  it("blocks the app until a new password is set", async () => {
    let me: Me = { id: "u2", login: "carol", role: "user", mustChangePassword: true };
    let body: unknown;
    server.use(
      http.get("/api/auth/me", () => json(me)),
      http.post("/api/auth/password", async ({ request }) => {
        body = await request.json();
        me = { ...me, mustChangePassword: false };
        return new Response(null, { status: 204 });
      }),
    );
    const { container } = await renderApp("/projects");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Set your password" })).toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "Main" })).not.toBeInTheDocument();
    await expectAccessible(container);
    await user.type(screen.getByLabelText("Current (temporary) password"), "temp-pass-12345");
    await user.type(screen.getByLabelText("New password"), "carol's own phrase");
    await user.type(screen.getByLabelText("Repeat new password"), "carol's own phrase!");
    await user.click(screen.getByRole("button", { name: "Save password" }));
    expect(screen.getByLabelText("Repeat new password")).toHaveAccessibleDescription(/do not match/i);
    await user.type(screen.getByLabelText("Repeat new password"), "{Backspace}");
    await user.click(screen.getByRole("button", { name: "Save password" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Projects" })).toBeInTheDocument();
    expect(body).toEqual({ currentPassword: "temp-pass-12345", newPassword: "carol's own phrase" });
  });

  it("explains a wrong current password", async () => {
    server.use(
      http.get("/api/auth/me", () => json({ ...owner, mustChangePassword: true })),
      http.post("/api/auth/password", () => apiError(401, "invalid_credentials", "currentPassword")),
    );
    await renderApp("/");
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText("Current (temporary) password"), "nope");
    await user.type(screen.getByLabelText("New password"), "a brand new phrase");
    await user.type(screen.getByLabelText("Repeat new password"), "a brand new phrase");
    await user.click(screen.getByRole("button", { name: "Save password" }));
    expect(await screen.findByLabelText("Current (temporary) password")).toHaveAccessibleDescription(/current password is wrong/i);
  });
});

describe("users page (owner)", () => {
  it("creates a user and shows the temporary password once", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    const accounts: Account[] = [{ ...carol, id: "u1", login: "owner", role: "owner" }];
    server.use(
      http.get("/api/users", () => json(accounts)),
      http.post("/api/users", async ({ request }) => {
        const { login } = (await request.json()) as { login: string };
        if (login === "owner") return apiError(409, "login_taken", "login");
        const u = { ...carol, login, mustChangePassword: true };
        accounts.push(u);
        return json({ user: u, temporaryPassword: "Tmp7pass8word9xy" }, 201);
      }),
    );
    const { container, client } = await renderApp("/users");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Users" })).toBeInTheDocument();
    await expectAccessible(container);
    const form = screen.getByRole("form", { name: "New user" });
    await user.type(within(form).getByLabelText("Login"), "owner");
    await user.click(within(form).getByRole("button", { name: "Create user" }));
    expect(await within(form).findByLabelText("Login")).toHaveAccessibleDescription(/already taken/i);
    await user.clear(within(form).getByLabelText("Login"));
    await user.type(within(form).getByLabelText("Login"), "carol");
    await user.click(within(form).getByRole("button", { name: "Create user" }));
    const secret = await screen.findByRole("region", { name: "Temporary password for carol" });
    expect(within(secret).getByText("Tmp7pass8word9xy")).toBeInTheDocument();
    expect(within(secret).getByText(/shown only once/i)).toBeInTheDocument();
    expect(within(secret).getByRole("heading", { name: "Temporary password for carol" })).toHaveFocus();
    expect(await screen.findByRole("cell", { name: "carol" })).toBeInTheDocument();
    // One secret at a time: new creates/resets wait until this one is acknowledged.
    expect(within(form).getByRole("button", { name: "Create user" })).toBeDisabled();
    await user.click(within(secret).getByRole("button", { name: "Done" }));
    expect(screen.queryByText("Tmp7pass8word9xy")).not.toBeInTheDocument();
    expect(JSON.stringify(client.getMutationCache().getAll().map((m) => m.state.data))).not.toContain("Tmp7pass8word9xy");
  });

  it("disables with confirmation and resets passwords", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    let current = { ...carol };
    server.use(
      http.get("/api/users", () => json([current])),
      http.post("/api/users/u2/disable", () => {
        current = { ...current, disabled: true };
        return new Response(null, { status: 204 });
      }),
      http.post("/api/users/u2/reset-password", () => json({ temporaryPassword: "Reset2pass3word4" })),
    );
    await renderApp("/users");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Disable carol" }));
    const dialog = screen.getByRole("alertdialog", { name: "Disable carol?" });
    await user.click(within(dialog).getByRole("button", { name: "Disable" }));
    await expectStatus("carol is disabled and signed out everywhere.");
    expect(await screen.findByText("Disabled")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Enable carol" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Reset password for carol" }));
    await user.click(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Reset password" }));
    expect(await screen.findByText("Reset2pass3word4")).toBeInTheDocument();
  });

  it("is not available to other users", async () => {
    server.use(http.get("/api/auth/me", () => json({ id: "u2", login: "bob", role: "user", mustChangePassword: false })));
    await renderApp("/users");
    expect(await screen.findByRole("heading", { level: 1, name: "Page not found" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Users" })).not.toBeInTheDocument();
  });
});

describe("change password from the menu", () => {
  it("is reachable from the shell and confirms success", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.post("/api/auth/password", () => new Response(null, { status: 204 })),
    );
    await renderApp("/");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("link", { name: "Account" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Account" })).toBeInTheDocument();
    await user.type(screen.getByLabelText("Current password"), "correct horse battery");
    await user.type(screen.getByLabelText("New password"), "an even better phrase");
    await user.type(screen.getByLabelText("Repeat new password"), "an even better phrase");
    await user.click(screen.getByRole("button", { name: "Save password" }));
    await expectStatus("Password changed. Other sessions were signed out.");
  });
});

it("tells the owner when copying is not possible", async () => {
  server.use(
    http.get("/api/auth/me", () => json(owner)),
    http.get("/api/users", () => json([])),
    http.post("/api/users", () => json({ user: { ...carol, mustChangePassword: true }, temporaryPassword: "Tmp7pass8word9xy" }, 201)),
  );
  await renderApp("/users");
  const user = userEvent.setup();
  // user-event installs its own clipboard stub in setup(); remove it afterwards to simulate an insecure context.
  Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
  await user.type(await screen.findByLabelText("Login"), "carol");
  await user.click(screen.getByRole("button", { name: "Create user" }));
  await user.click(await screen.findByRole("button", { name: "Copy" }));
  await expectStatus(/select the password and copy it manually/i);
});
