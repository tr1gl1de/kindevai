import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, type Locale } from "../i18n";

/** Renders ui inside an isolated i18n instance with a fixed locale. */
export async function renderWithI18n(ui: ReactElement, locale: Locale = "en") {
  const i18n = await createI18n({ lng: locale });
  return { i18n, ...render(<I18nextProvider i18n={i18n}>{ui}</I18nextProvider>) };
}
