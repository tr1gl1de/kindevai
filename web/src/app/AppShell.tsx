import { useEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { NavLink, useLocation } from "react-router";
import { BrandLogo } from "../ui/BrandLogo";
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
  const drawer = useDrawer();
  const openAlerts = open > 0 && (
    <span className="nav-count">
      {open}
      <span className="visually-hidden"> {t("nav.alertsOpen", { count: open })}</span>
    </span>
  );
  return (
    <div className="shell" data-nav={drawer.open ? "open" : "closed"}>
      <a className="skip-link" href="#main">
        {t("app.skipToContent")}
      </a>
      <header className="topbar">
        <button
          ref={drawer.buttonRef}
          type="button"
          className="button subtle menu-button"
          aria-expanded={drawer.open}
          aria-controls="main-nav"
          onClick={drawer.toggle}
        >
          <svg className="menu-icon" aria-hidden="true" focusable="false" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
            <path d="M4 6h16M4 12h16M4 18h16" />
          </svg>
          {t("nav.menu")}
          {!drawer.open && openAlerts}
        </button>
        <span className="brand">
          <BrandLogo />
        </span>
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
      <nav id="main-nav" aria-label={t("nav.label")} className="sidebar">
        <NavLink to="/" end>
          {t("nav.overview")}
        </NavLink>
        <NavLink to="/projects">{t("nav.projects")}</NavLink>
        <NavLink to="/tasks">{t("nav.tasks")}</NavLink>
        <NavLink to="/approvals">{t("nav.approvals")}</NavLink>
        <NavLink to="/alerts">
          {t("nav.alerts")}
          {openAlerts}
        </NavLink>
        {me.role === "owner" && <NavLink to="/users">{t("nav.users")}</NavLink>}
        {me.role === "owner" && <NavLink to="/runners">{t("nav.runners")}</NavLink>}
        {me.role === "owner" && <NavLink to="/infrastructure/storage">{t("nav.storage")}</NavLink>}
        {me.role === "owner" && <NavLink to="/integrations">{t("nav.integrations")}</NavLink>}
        {me.role === "owner" && <NavLink to="/agents">{t("nav.agents")}</NavLink>}
        {me.role === "owner" && <NavLink to="/network">{t("nav.network")}</NavLink>}
        {me.role === "owner" && <NavLink to="/audit">{t("nav.audit")}</NavLink>}
        {me.role === "owner" && <NavLink to="/settings/telegram">{t("nav.telegram")}</NavLink>}
        {me.role === "owner" && <NavLink to="/settings/maintenance">{t("nav.maintenance")}</NavLink>}
        <NavLink to="/account/password" className="nav-account">
          {t("nav.changePassword")}
        </NavLink>
      </nav>
      <main id="main" className="content" tabIndex={-1}>
        <MaintenanceBanner owner={me.role === "owner"} />
        {children}
      </main>
    </div>
  );
}

/**
 * The navigation drawer below 1024 px (CSS shows the sidebar instead on wider screens): the Menu button toggles it,
 * Escape closes it and returns focus to the button, and choosing a page closes it.
 */
function useDrawer() {
  // Open for the page it was opened on: any navigation closes it without an extra render pass.
  const { pathname } = useLocation();
  const [openOn, setOpenOn] = useState<string | null>(null);
  const open = openOn === pathname;
  const buttonRef = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      setOpenOn(null);
      buttonRef.current?.focus();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open]);
  return { open, buttonRef, toggle: () => setOpenOn(open ? null : pathname) };
}
