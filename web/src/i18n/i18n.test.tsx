import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nextProvider, useTranslation } from "react-i18next";
import { describe, expect, it } from "vitest";
import { LOCALE_STORAGE_KEY, createI18n, resources } from ".";
import { LanguageSwitcher } from "./LanguageSwitcher";

function Title() {
  const { t } = useTranslation();
  return <h1>{t("app.title")}</h1>;
}

function flatKeys(obj: object, prefix = ""): string[] {
  return Object.entries(obj).flatMap(([k, v]) =>
    typeof v === "object" && v !== null ? flatKeys(v as object, `${prefix}${k}.`) : [`${prefix}${k}`],
  );
}

describe("resources", () => {
  it("ru defines exactly the keys of en, none empty", () => {
    expect(flatKeys(resources.ru.translation).sort()).toEqual(flatKeys(resources.en.translation).sort());
    const leaves = JSON.stringify(resources.ru.translation);
    expect(leaves).not.toContain('""');
  });
});

describe("createI18n", () => {
  it.each([
    ["ru-RU", "ru"],
    ["en-GB", "en"],
    ["de-DE", "en"],
  ])("browser language %s resolves to %s", async (lng, want) => {
    const i18n = await createI18n({ lng });
    expect(i18n.resolvedLanguage).toBe(want);
  });
});

describe("LanguageSwitcher", () => {
  it("switches language, remembers the choice and updates <html lang>", async () => {
    localStorage.removeItem(LOCALE_STORAGE_KEY);
    const i18n = await createI18n({ lng: "en" });
    render(
      <I18nextProvider i18n={i18n}>
        <Title />
        <LanguageSwitcher />
      </I18nextProvider>,
    );
    expect(screen.getByRole("heading")).toHaveTextContent(resources.en.translation.app.title);
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Language" }), "ru");
    expect(screen.getByRole("heading")).toHaveTextContent(resources.ru.translation.app.title);
    expect(screen.getByRole("combobox", { name: "Язык" })).toHaveValue("ru");
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe("ru");
    expect(document.documentElement.lang).toBe("ru");
  });
});
