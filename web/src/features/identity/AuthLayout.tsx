import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { LanguageSwitcher } from "../../i18n/LanguageSwitcher";

/** Centered single-column layout for the wizard and the sign-in page. */
export function AuthLayout({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  return (
    <div className="auth-layout">
      <header className="auth-header">
        <span className="brand">{t("app.title")}</span>
        <LanguageSwitcher />
      </header>
      <main id="main" className="auth-card">
        {children}
      </main>
    </div>
  );
}
