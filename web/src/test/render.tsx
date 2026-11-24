import { QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
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
    rules: { "color-contrast": { enabled: false } }, // jsdom has no layout/colours; contrast is checked in CSS tokens
  });
  const problems = result.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`);
  if (problems.length) throw new Error(`axe violations:\n${problems.join("\n")}`);
}

/** Waits until the polite status region announces text (messages are set asynchronously to re-announce repeats). */
export async function expectStatus(text: string | RegExp) {
  const { screen, waitFor } = await import("@testing-library/react");
  const { expect } = await import("vitest");
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(text));
}
