import { useId } from "react";
import { useTranslation } from "react-i18next";
import { LOCALES, type Locale } from ".";

const NAMES: Record<Locale, string> = { en: "English", ru: "Русский" };

export function LanguageSwitcher() {
  const { t, i18n } = useTranslation();
  const id = useId();
  return (
    <span className="language-switcher">
      <label htmlFor={id}>{t("language.label")}</label>{" "}
      <select id={id} value={i18n.resolvedLanguage} onChange={(e) => void i18n.changeLanguage(e.target.value)}>
        {LOCALES.map((l) => (
          <option key={l} value={l} lang={l}>
            {NAMES[l]}
          </option>
        ))}
      </select>
    </span>
  );
}
