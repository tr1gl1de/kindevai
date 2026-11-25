import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { Me, TelegramLinkStatus, TelegramSettings } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, fill, renderApp } from "../../test/render";
import { applyThemeParams, parseLaunch, startPath, startTarget, type TelegramLaunch } from "./webapp";

const INIT = "query_id=AAQ&user=%7B%22id%22%3A42%2C%22first_name%22%3A%22Ann%22%2C%22language_code%22%3A%22ru%22%7D&auth_date=1800000000&hash=abc";
const launch = (startParam = ""): TelegramLaunch => ({ initData: INIT, startParam, languageCode: "ru" });
const alice: Me = { id: "u2", login: "alice", role: "user", mustChangePassword: false };

describe("Telegram launch data", () => {
  it("reads initData, the start parameter and the language from the URL fragment", () => {
    const signed = INIT + "&start_param=link_abc";
    const l = parseLaunch(`#tgWebAppData=${encodeURIComponent(signed)}&tgWebAppVersion=8.0`);
    expect(l).toEqual({ initData: signed, startParam: "link_abc", languageCode: "ru" });
    expect(parseLaunch("#token=x")).toBeNull();
    expect(startTarget("link_abc")).toEqual({ kind: "link", token: "abc" });
    expect(startTarget("invite_x-y_z")).toEqual({ kind: "invite", token: "x-y_z" });
    expect(startTarget("other_1")).toBeNull();
  });

  it("maps notification deep links to their pages, a finished task to its task page (KIN-62)", () => {
    expect(startPath({ kind: "task", token: "T1" })).toBe("/tasks/T1");
    expect(startPath({ kind: "approval", token: "AP1" })).toBe("/approvals/AP1");
    expect(startPath({ kind: "alert", token: "A1" })).toBe("/alerts");
    expect(startPath({ kind: "link", token: "x" })).toBeNull();
    expect(startTarget("task_T1")).toEqual({ kind: "task", token: "T1" });
  });

  it("applies only colour theme params as --tg-theme-* variables", () => {
    const el = document.createElement("div");
    applyThemeParams(JSON.stringify({ bg_color: "#112233", button_text_color: "#fff", evil: "red;}", "x-y": "#000" }), el);
    expect(el.style.getPropertyValue("--tg-theme-bg-color")).toBe("#112233");
    expect(el.style.getPropertyValue("--tg-theme-button-text-color")).toBe("#fff");
    expect(el.style.getPropertyValue("--tg-theme-evil")).toBe("");
    applyThemeParams("not json", el);
  });
});

describe("Mini App sign-in", () => {
  it("signs in with the raw initData and opens the app", async () => {
    let sent: unknown;
    let signedIn = false;
    server.use(
      http.get("/api/auth/me", () => (signedIn ? json(alice) : apiError(401, "unauthenticated"))),
      http.post("/api/telegram/login", async ({ request }) => {
        sent = await request.json();
        signedIn = true;
        return json({ user: alice });
      }),
    );
    await renderApp("/", "en", launch());
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeInTheDocument();
    expect(sent).toEqual({ initData: INIT });
  });

  it("explains an unlinked Telegram account and offers the password (RU)", async () => {
    server.use(http.post("/api/telegram/login", () => apiError(401, "telegram_not_linked")));
    const { container } = await renderApp("/", "ru", launch());
    expect(await screen.findByRole("heading", { level: 1, name: "Этот аккаунт Telegram не привязан" })).toBeInTheDocument();
    await expectAccessible(container);
    await userEvent.setup().click(screen.getByRole("button", { name: "Войти по паролю" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Вход" })).toBeInTheDocument();
  });

  it("asks to reopen the app when initData is too old", async () => {
    server.use(http.post("/api/telegram/login", () => apiError(401, "telegram_auth_expired")));
    await renderApp("/", "en", launch());
    expect(await screen.findByRole("alert")).toHaveTextContent("Close the app and open it again from Telegram.");
  });
});

describe("Link Telegram — Mini App side", () => {
  it("shows the target account, confirms and shows the code for the website", async () => {
    const calls: string[] = [];
    server.use(
      http.post("/api/telegram/link/preview", () => {
        calls.push("preview");
        return json({ login: "alice", expiresAt: "2026-11-24T12:10:00Z" });
      }),
      http.post("/api/telegram/link/confirm", async ({ request }) => {
        calls.push(`confirm ${JSON.stringify(await request.json())}`);
        return json({ login: "alice", expiresAt: "2026-11-24T12:10:00Z", code: "123456" });
      }),
    );
    const { container } = await renderApp("/", "en", launch("link_abc"));
    expect(await screen.findByText("Link this Telegram account to the account alice?")).toBeInTheDocument();
    await expectAccessible(container);
    await userEvent.setup().click(screen.getByRole("button", { name: "Link to alice" }));
    expect(await screen.findByRole("heading", { name: "Enter this code on the website" })).toHaveFocus();
    expect(screen.getByText("123456")).toBeInTheDocument();
    expect(calls).toEqual(["preview", `confirm ${JSON.stringify({ initData: INIT })}`]);
    await expectAccessible(container);
    server.use(http.post("/api/telegram/login", () => json({ user: alice })));
    await userEvent.setup().click(screen.getByRole("button", { name: "I entered the code — continue" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeInTheDocument();
  });

  it("explains a link already opened by someone else", async () => {
    server.use(http.post("/api/telegram/link/preview", () => apiError(409, "link_claimed")));
    await renderApp("/", "en", launch("link_abc"));
    expect(await screen.findByRole("alert")).toHaveTextContent("already opened by another Telegram account");
  });
});

describe("Link Telegram — website side", () => {
  it("creates the link, takes the code from Telegram, then unlinks", async () => {
    let status: TelegramLinkStatus = { configured: true, identity: null, request: null };
    let code: unknown;
    server.use(
      http.get("/api/auth/me", () => json(alice)),
      http.get("/api/telegram/link", () => json(status)),
      http.post("/api/telegram/link", () => {
        status = { ...status, request: { state: "awaiting_code", expiresAt: "2026-11-24T12:10:00Z", telegramName: "@ann" } };
        return json({ state: "awaiting_telegram", expiresAt: "2026-11-24T12:10:00Z", url: "https://t.me/kindev_bot/app?startapp=link_tok" }, 201);
      }),
      http.post("/api/telegram/link/complete", async ({ request }) => {
        code = await request.json();
        if ((code as { code: string }).code !== "123456") return apiError(422, "link_code_wrong", "code");
        status = { configured: true, identity: { telegramId: "42", username: "ann", linkedAt: "2026-11-24T12:05:00Z" }, request: null };
        return json(status.identity);
      }),
      http.delete("/api/telegram/link", () => {
        status = { configured: true, identity: null, request: null };
        return new Response(null, { status: 204 });
      }),
    );
    const { container } = await renderApp("/account/password");
    const user = userEvent.setup();
    const section = await screen.findByRole("region", { name: "Telegram" });
    await user.click(await within(section).findByRole("button", { name: "Link Telegram" }));
    // The mocked status already moved on: the code form appears.
    const form = await within(section).findByRole("form", { name: "Confirmation code" });
    expect(within(section).getByText(/Confirmed in Telegram by @ann/)).toBeInTheDocument();
    await expectAccessible(container);
    await user.click(within(form).getByRole("button", { name: "Link this account" }));
    expect(within(form).getByLabelText("Code from Telegram")).toHaveFocus();
    await fill(user, within(form).getByLabelText("Code from Telegram"), "654321");
    await user.click(within(form).getByRole("button", { name: "Link this account" }));
    expect(await within(form).findByText(/Wrong code/)).toBeInTheDocument();
    await user.clear(within(form).getByLabelText("Code from Telegram"));
    await fill(user, within(form).getByLabelText("Code from Telegram"), "123 456");
    await user.click(within(form).getByRole("button", { name: "Link this account" }));
    await expectStatus("Telegram is linked.");
    expect(code).toEqual({ code: "123456" });
    expect(await within(section).findByText(/Linked to Telegram @ann/)).toBeInTheDocument();

    await user.click(within(section).getByRole("button", { name: "Unlink Telegram" }));
    const dialog = screen.getByRole("alertdialog", { name: "Unlink Telegram from this account?" });
    await user.click(within(dialog).getByRole("button", { name: "Unlink Telegram" }));
    await expectStatus("Telegram is unlinked.");
    expect(await within(section).findByRole("button", { name: "Link Telegram" })).toBeInTheDocument();
  });

  it("shows the one-time link while waiting for Telegram", async () => {
    let status: TelegramLinkStatus = { configured: true, identity: null, request: null };
    server.use(
      http.get("/api/auth/me", () => json(alice)),
      http.get("/api/telegram/link", () => json(status)),
      http.post("/api/telegram/link", () => {
        status = { ...status, request: { state: "awaiting_telegram", expiresAt: "2026-11-24T12:10:00Z" } };
        return json({ state: "awaiting_telegram", expiresAt: "2026-11-24T12:10:00Z", url: "https://t.me/kindev_bot/app?startapp=link_tok" }, 201);
      }),
    );
    await renderApp("/account/password");
    const section = await screen.findByRole("region", { name: "Telegram" });
    await userEvent.setup().click(await within(section).findByRole("button", { name: "Link Telegram" }));
    expect(await within(section).findByRole("link", { name: "https://t.me/kindev_bot/app?startapp=link_tok" })).toBeInTheDocument();
    expect(within(section).getByText("Waiting for the confirmation in Telegram…")).toBeInTheDocument();
  });

  it("says when Telegram is not set up", async () => {
    server.use(http.get("/api/auth/me", () => json(alice)));
    await renderApp("/account/password");
    expect(await screen.findByText(/not set up on this platform yet/)).toBeInTheDocument();
  });
});

describe("Telegram settings (owner)", () => {
  it("saves the bot and keeps the token write-only", async () => {
    let settings: TelegramSettings = { configured: false, botUsername: "", appShortName: "", tokenSet: false, updatedAt: null };
    let saved: unknown;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/telegram/settings", () => json(settings)),
      http.put("/api/telegram/settings", async ({ request }) => {
        saved = await request.json();
        settings = { configured: true, botUsername: "kindev_bot", appShortName: "app", tokenSet: true, updatedAt: "2026-11-24T12:00:00Z" };
        return json(settings);
      }),
    );
    const { container, client } = await renderApp("/");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("link", { name: "Telegram" }));
    const form = await screen.findByRole("form", { name: "Telegram" });
    await expectAccessible(container);
    await user.click(within(form).getByRole("button", { name: "Save Telegram settings" }));
    expect(within(form).getByLabelText("Bot username")).toHaveFocus();
    expect(within(form).getByText("Paste the bot token from BotFather.")).toBeInTheDocument();
    await fill(user, within(form).getByLabelText("Bot username"), "@kindev_bot");
    await fill(user, within(form).getByLabelText("Mini App short name"), "app");
    const token = "123456789:TEST_token-for-unit-tests_only_000";
    await fill(user, within(form).getByLabelText("Bot token"), token);
    await user.click(within(form).getByRole("button", { name: "Save Telegram settings" }));
    await expectStatus("Telegram settings saved.");
    expect(saved).toEqual({ botUsername: "@kindev_bot", appShortName: "app", botToken: token });
    await waitFor(() => expect(within(form).getByLabelText("Bot token")).toHaveValue(""));
    expect(within(form).getByText(/Leave empty to keep it/)).toBeInTheDocument();
    await waitFor(() => expect(JSON.stringify(client.getMutationCache().getAll().map((m) => m.state.variables))).not.toContain(token));
  });
});

describe("Invitation from the Mini App", () => {
  it("accepts with initData and continues to the Telegram sign-in", async () => {
    let accepted: unknown;
    let signedIn = false;
    server.use(
      http.get("/api/auth/me", () => (signedIn ? json(alice) : apiError(401, "unauthenticated"))),
      http.post("/api/invitations/preview", () => json({ projectName: "Alpha", role: "member", expiresAt: "2026-11-27T10:00:00Z" })),
      http.post("/api/invitations/accept", async ({ request }) => {
        accepted = await request.json();
        return json({ user: { ...alice, login: "dave" } }, 201);
      }),
      http.post("/api/telegram/login", () => {
        signedIn = true;
        return json({ user: alice });
      }),
    );
    await renderApp("/", "en", launch("invite_tok123"));
    const user = userEvent.setup();
    const form = await screen.findByRole("form", { name: "Join by invitation" });
    await fill(user, within(form).getByLabelText("Login"), "dave");
    await fill(user, within(form).getByLabelText("Password"), "a long enough password");
    await fill(user, within(form).getByLabelText("Repeat password"), "a long enough password");
    await user.click(within(form).getByRole("button", { name: "Create account" }));
    expect(await screen.findByText(/linked to this Telegram account/)).toBeInTheDocument();
    expect(accepted).toEqual({ token: "tok123", login: "dave", password: "a long enough password", initData: INIT });
    await user.click(screen.getByRole("button", { name: "Continue" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeInTheDocument();
  });
});
