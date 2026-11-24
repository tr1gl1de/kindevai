import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { NavLink } from "react-router";
import type { Me } from "../api/types";
import { useLogout } from "../features/identity/api";
import { LanguageSwitcher } from "../i18n/LanguageSwitcher";
import { useAlerts } from "../features/alerts/api";
import { MaintenanceBanner } from "../features/maintenance/MaintenanceBanner";

export function AppShell({ me, children }: { me: Me; children: ReactNode }) {
  const { t } = useTranslation();
  const logout = useLogout();
  const alerts = useAlerts("active");
  const open = alerts.data?.filter((a) => a.state === "OPEN").length ?? 0;
  return (
    <div className="shell">
      <a className="skip-link" href="#main">
        {t("app.skipToContent")}
      </a>
      <header className="topbar">
        <span className="brand">{t("app.title")}</span>
        <nav aria-label={t("nav.label")} className="nav">
          <NavLink to="/" end>
            {t("nav.overview")}
          </NavLink>
          <NavLink to="/projects">{t("nav.projects")}</NavLink>
          <NavLink to="/tasks">{t("nav.tasks")}</NavLink>
          <NavLink to="/approvals">{t("nav.approvals")}</NavLink>
          <NavLink to="/alerts">
            {t("nav.alerts")}
            {open > 0 && (
              <span className="nav-count">
                {open}
                <span className="visually-hidden"> {t("nav.alertsOpen", { count: open })}</span>
              </span>
            )}
          </NavLink>
          {me.role === "owner" && <NavLink to="/users">{t("nav.users")}</NavLink>}
          {me.role === "owner" && <NavLink to="/runners">{t("nav.runners")}</NavLink>}
          {me.role === "owner" && <NavLink to="/infrastructure/storage">{t("nav.storage")}</NavLink>}
          {me.role === "owner" && <NavLink to="/integrations">{t("nav.integrations")}</NavLink>}
          {me.role === "owner" && <NavLink to="/network">{t("nav.network")}</NavLink>}
          {me.role === "owner" && <NavLink to="/audit">{t("nav.audit")}</NavLink>}
          {me.role === "owner" && <NavLink to="/settings/telegram">{t("nav.telegram")}</NavLink>}
          {me.role === "owner" && <NavLink to="/settings/maintenance">{t("nav.maintenance")}</NavLink>}
          <NavLink to="/account/password" className="nav-account">
            {t("nav.changePassword")}
          </NavLink>
        </nav>
        <div className="user-area">
          <span className="user">
            <span className="visually-hidden">{t("app.signedInAs")} </span>
            <span>{me.login}</span>
          </span>
          <LanguageSwitcher />
          <button type="button" className="button subtle" onClick={() => logout.mutate()} disabled={logout.isPending}>
            {t("login.signOut")}
          </button>
        </div>
      </header>
      <main id="main" className="content" tabIndex={-1}>
        <MaintenanceBanner owner={me.role === "owner"} />
        {children}
      </main>
    </div>
  );
}
