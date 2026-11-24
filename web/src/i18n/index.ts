import { createInstance, type i18n as I18n } from "i18next";
import LanguageDetector from "i18next-browser-languagedetector";
import { initReactI18next } from "react-i18next";
import { en } from "./locales/en";
import { ru } from "./locales/ru";

export const resources = {
  en: { translation: en },
  ru: { translation: ru },
} as const;

export type Locale = keyof typeof resources;
export const LOCALES: readonly Locale[] = ["en", "ru"];
export const LOCALE_STORAGE_KEY = "kindevai.locale";

/**
 * Creates an isolated i18next instance. Language order: explicit `lng` (tests), the user's saved choice,
 * then the browser language; anything unsupported falls back to English.
 */
export async function createI18n(opts: { lng?: string } = {}): Promise<I18n> {
  const instance = createInstance();
  instance.on("languageChanged", (lng) => {
    document.documentElement.lang = instance.resolvedLanguage ?? lng;
  });
  await instance
    .use(LanguageDetector)
    .use(initReactI18next)
    .init({
      resources,
      lng: opts.lng,
      fallbackLng: "en",
      supportedLngs: LOCALES,
      nonExplicitSupportedLngs: true,
      load: "languageOnly",
      interpolation: { escapeValue: false }, // React already escapes
      returnNull: false,
      detection: {
        order: ["localStorage", "navigator"],
        lookupLocalStorage: LOCALE_STORAGE_KEY,
        caches: ["localStorage"],
      },
    });
  return instance;
}
