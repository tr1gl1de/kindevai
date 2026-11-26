import { QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import axe from "axe-core";
import { I18nextProvider } from "react-i18next";
import { MemoryRouter } from "react-router";
import type { ReactElement } from "react";
import { App } from "../app/App";
import { createQueryClient } from "../app/queryClient";
import { createI18n, type Locale } from "../i18n";
import type { TelegramLaunch } from "../features/telegram/webapp";

/** Renders ui inside an isolated i18n instance with a fixed locale. */
export async function renderWithI18n(ui: ReactElement, locale: Locale = "en") {
  const i18n = await createI18n({ lng: locale });
  return { i18n, ...render(<I18nextProvider i18n={i18n}>{ui}</I18nextProvider>) };
}

/** Renders the whole app at a route with fresh query cache and i18n. */
export async function renderApp(route = "/", locale: Locale = "en", telegram: TelegramLaunch | null = null) {
  const client = createQueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const i18n = await createI18n({ lng: locale });
  const result = render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <MemoryRouter initialEntries={[route]}>
          <App telegram={telegram} />
        </MemoryRouter>
      </I18nextProvider>
    </QueryClientProvider>,
  );
  return { client, ...result };
}

/** Fails with a readable list if axe finds WCAG A/AA violations. */
export async function expectAccessible(container: HTMLElement) {
  const result = await axe.run(container, {
    runOnly: { type: "tag", values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"] },
    resultTypes: ["violations"], // only violations are reported; skip collecting the passing nodes
    // jsdom has no layout or colours: contrast is checked in CSS tokens, and target-size would only measure 0x0 boxes
    // (it was the costliest rule here, KIN-82).
    rules: { "color-contrast": { enabled: false }, "target-size": { enabled: false } },
  });
  const problems = result.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`);
  if (problems.length) throw new Error(`axe violations:\n${problems.join("\n")}`);
}

/**
 * Enters text into a field as one paste (one input event) instead of a keystroke each: user.type re-renders React
 * per character, which made long form scenarios time out on a loaded machine (KIN-82). Use user.type only where
 * keystrokes are the behaviour under test (special keys, key-by-key reactions).
 */
export async function fill(user: UserEvent, field: HTMLElement, text: string) {
  await user.click(field);
  await user.paste(text);
}

/** Waits until the polite status region announces text (messages are set asynchronously to re-announce repeats). */
export async function expectStatus(text: string | RegExp) {
  const { screen, waitFor } = await import("@testing-library/react");
  const { expect } = await import("vitest");
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(text));
}
