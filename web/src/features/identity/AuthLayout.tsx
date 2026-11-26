import type { ReactNode } from "react";
import { LanguageSwitcher } from "../../i18n/LanguageSwitcher";
import { BrandLogo } from "../../ui/BrandLogo";
import "./identity.css";

/** Centered single-column layout for the wizard and the sign-in page. */
export function AuthLayout({ children }: { children: ReactNode }) {
  return (
    <div className="auth-layout">
      <header className="auth-header">
        <span className="brand">
          <BrandLogo />
        </span>
        <LanguageSwitcher />
      </header>
      <main id="main" className="auth-card">
        {children}
      </main>
    </div>
  );
}
