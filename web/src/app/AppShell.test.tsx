// KIN-120: the navigation is a sidebar on desktop and a drawer behind a Menu button below 1024 px (CSS decides which
// one shows; the button's state and the drawer's behaviour are tested here, the widths in test/layout.test.ts).
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import { json, owner, server } from "../test/msw";
import { expectAccessible, renderApp } from "../test/render";

describe("navigation drawer", () => {
  it("opens from the Menu button, closes on Escape and returns focus to the button", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    const { container } = await renderApp("/");
    await screen.findByRole("heading", { level: 1, name: "Overview" });
    const user = userEvent.setup();
    const menu = within(screen.getByRole("banner")).getByRole("button", { name: "Menu" });
    const nav = screen.getByRole("navigation", { name: "Main" });
    expect(menu).toHaveAttribute("aria-expanded", "false");
    expect(menu).toHaveAttribute("aria-controls", nav.id);

    await user.click(menu);
    expect(menu).toHaveAttribute("aria-expanded", "true");
    await expectAccessible(container);
    await user.keyboard("{Escape}");
    expect(menu).toHaveAttribute("aria-expanded", "false");
    expect(menu).toHaveFocus();
  });

  it("leaves Escape to a dialog open over the page", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    await renderApp("/");
    await screen.findByRole("heading", { level: 1, name: "Overview" });
    const user = userEvent.setup();
    const menu = screen.getByRole("button", { name: "Menu" });
    await user.click(menu);
    const dialog = document.createElement("div");
    dialog.setAttribute("role", "dialog");
    document.body.append(dialog);
    try {
      await user.keyboard("{Escape}");
      expect(menu).toHaveAttribute("aria-expanded", "true");
    } finally {
      dialog.remove();
    }
  });

  it("closes after choosing a page", async () => {
    server.use(http.get("/api/auth/me", () => json(owner)));
    await renderApp("/");
    await screen.findByRole("heading", { level: 1, name: "Overview" });
    const user = userEvent.setup();
    const menu = screen.getByRole("button", { name: "Menu" });
    await user.click(menu);
    await user.click(within(screen.getByRole("navigation", { name: "Main" })).getByRole("link", { name: "Projects" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Projects" })).toBeInTheDocument();
    await waitFor(() => expect(menu).toHaveAttribute("aria-expanded", "false"));
  });

  it("names the button in Russian and counts open alerts on it while closed", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/alerts", () =>
        json([{ id: "a1", kind: "runner_offline", severity: "warning", subject: "r1", details: {}, state: "OPEN",
          occurrences: 1, firstSeenAt: "2026-11-26T10:00:00Z", lastSeenAt: "2026-11-26T10:00:00Z", ackedAt: null,
          closedAt: null, canManage: true }]),
      ),
    );
    await renderApp("/", "ru");
    await screen.findByRole("heading", { level: 1, name: "Обзор" });
    expect(await screen.findByRole("button", { name: /Меню.*1 открытый алерт/ })).toBeInTheDocument();
  });
});
