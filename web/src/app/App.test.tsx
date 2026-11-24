import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import { apiError, json, owner, server } from "../test/msw";
import { expectAccessible, renderApp } from "../test/render";

function signedIn() {
  server.use(http.get("/api/auth/me", () => json(owner)));
}

describe("first-run setup", () => {
  it("shows the wizard, validates, creates the owner and signs in", async () => {
    let setupBody: unknown;
    let signedInNow = false;
    server.use(
      http.get("/api/setup/status", () => json({ setupRequired: !signedInNow })),
      http.post("/api/setup", async ({ request }) => {
        setupBody = await request.json();
        return json({ login: "owner" }, 201);
      }),
      http.post("/api/auth/login", () => {
        signedInNow = true;
        return json({ user: owner });
      }),
      http.get("/api/auth/me", () => (signedInNow ? json(owner) : apiError(401, "unauthenticated"))),
    );
    const { container } = await renderApp("/");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Set up AI Platform" })).toBeInTheDocument();
    await expectAccessible(container);

    await user.type(screen.getByLabelText("Setup token"), "tok");
    await user.type(screen.getByLabelText("Owner login"), "owner");
    await user.type(screen.getByLabelText("Password"), "short");
    await user.type(screen.getByLabelText("Repeat password"), "different");
    await user.type(screen.getByLabelText("Platform name"), "Acme AI");
    await user.click(screen.getByRole("button", { name: "Create owner and finish setup" }));
    const pw = screen.getByLabelText("Password");
    expect(pw).toHaveAttribute("aria-invalid", "true");
    expect(pw).toHaveFocus();
    expect(screen.getByLabelText("Repeat password")).toHaveAccessibleDescription(/do not match/i);
    expect(screen.getByRole("alert")).toHaveTextContent(/fix 2 fields/i);

    await user.clear(pw);
    await user.type(pw, "correct horse battery");
    await user.clear(screen.getByLabelText("Repeat password"));
    await user.type(screen.getByLabelText("Repeat password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Create owner and finish setup" }));

    expect(await screen.findByRole("heading", { level: 1, name: "Projects" })).toBeInTheDocument();
    expect(setupBody).toEqual({ token: "tok", login: "owner", password: "correct horse battery", platformName: "Acme AI" });
  });

  it("explains an invalid setup token and keeps the form", async () => {
    server.use(
      http.get("/api/setup/status", () => json({ setupRequired: true })),
      http.post("/api/setup", () => apiError(401, "invalid_setup_token")),
    );
    await renderApp("/");
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText("Setup token"), "bad");
    await user.type(screen.getByLabelText("Owner login"), "owner");
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.type(screen.getByLabelText("Repeat password"), "correct horse battery");
    await user.type(screen.getByLabelText("Platform name"), "Acme");
    await user.click(screen.getByRole("button", { name: "Create owner and finish setup" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/setup token is invalid or expired/i);
    expect(screen.getByLabelText("Setup token")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText("Owner login")).toHaveValue("owner");
  });
});

describe("sign in", () => {
  it("shows a uniform error for wrong credentials, then signs in", async () => {
    let ok = false;
    server.use(
      http.post("/api/auth/login", async ({ request }) => {
        const body = (await request.json()) as { password: string };
        if (body.password !== "correct horse battery") return apiError(401, "invalid_credentials");
        ok = true;
        return json({ user: owner });
      }),
      http.get("/api/auth/me", () => (ok ? json(owner) : apiError(401, "unauthenticated"))),
    );
    const { container } = await renderApp("/projects");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Sign in" })).toBeInTheDocument();
    await expectAccessible(container);
    await user.type(screen.getByLabelText("Login"), "owner");
    await user.type(screen.getByLabelText("Password"), "nope");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Wrong login or password.");
    expect(screen.getByLabelText("Login")).toHaveValue("owner");
    await user.clear(screen.getByLabelText("Password"));
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Projects" })).toBeInTheDocument();
  });

  it("tells the user to wait when rate limited", async () => {
    server.use(http.post("/api/auth/login", () => apiError(429, "rate_limited")));
    await renderApp("/");
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText("Login"), "owner");
    await user.type(screen.getByLabelText("Password"), "whatever-123");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/too many attempts/i);
  });

  it("works in Russian", async () => {
    await renderApp("/", "ru");
    expect(await screen.findByRole("heading", { level: 1, name: "Вход" })).toBeInTheDocument();
    expect(screen.getByLabelText("Логин")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Войти" })).toBeInTheDocument();
  });
});

describe("app shell", () => {
  it("has landmarks, a skip link, the user menu and signs out", async () => {
    signedIn();
    let out = false;
    server.use(
      http.post("/api/auth/logout", () => {
        out = true;
        return new Response(null, { status: 204 });
      }),
      http.get("/api/auth/me", () => (out ? apiError(401, "unauthenticated") : json(owner))),
    );
    const { container } = await renderApp("/");
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Skip to content" })).toHaveAttribute("href", "#main");
    const nav = screen.getByRole("navigation", { name: "Main" });
    expect(within(nav).getByRole("link", { name: "Projects" })).toBeInTheDocument();
    expect(screen.getByText("owner")).toBeInTheDocument();
    // Integrations and runners are not configured yet: the overview says so instead of pretending to be ready.
    expect(screen.getByText(/no runners connected/i)).toBeInTheDocument();
    await expectAccessible(container);
    await userEvent.setup().click(screen.getByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Sign in" })).toBeInTheDocument();
  });

  it("shows an error state with retry when the server is unreachable", async () => {
    let fail = true;
    server.use(http.get("/api/setup/status", () => (fail ? apiError(500, "internal") : json({ setupRequired: false }))));
    await renderApp("/");
    expect(await screen.findByRole("alert")).toHaveTextContent(/cannot reach the control plane/i);
    fail = false;
    await userEvent.setup().click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(screen.getByRole("heading", { level: 1, name: "Sign in" })).toBeInTheDocument());
  });
});
