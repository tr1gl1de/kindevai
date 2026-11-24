import { useTranslation } from "react-i18next";
import { Route, Routes, useLocation } from "react-router";
import { AcceptInvitePage } from "../features/identity/AcceptInvitePage";
import { LoginPage } from "../features/identity/LoginPage";
import { SetupWizard } from "../features/identity/SetupWizard";
import { useMe, useSetupStatus } from "../features/identity/api";
import { OverviewPage } from "../features/dashboard/OverviewPage";
import { ProjectPage } from "../features/projects/ProjectPage";
import { ProjectsPage } from "../features/projects/ProjectsPage";
import { AccountPage } from "../features/identity/AccountPage";
import { ForcedPasswordChange } from "../features/identity/ForcedPasswordChange";
import { UsersPage } from "../features/identity/UsersPage";
import { AuditPage } from "../features/audit/AuditPage";
import { RunnerPage } from "../features/runners/RunnerPage";
import { ApprovalPage } from "../features/approvals/ApprovalPage";
import { ApprovalsPage } from "../features/approvals/ApprovalsPage";
import { RunnersPage } from "../features/runners/RunnersPage";
import { NetworkPage } from "../features/network/NetworkPage";
import { StoragePage } from "../features/runners/StoragePage";
import { IntegrationsPage } from "../features/integrations/IntegrationsPage";
import { PageNotFound } from "./PageNotFound";
import { ErrorState, Loading } from "../ui/feedback";
import { NotifyProvider } from "../ui/notify";
import { AppShell } from "./AppShell";
import { TelegramContext, useTelegramLaunch } from "../features/telegram/TelegramContext";
import { LinkConfirmPage } from "../features/telegram/LinkConfirmPage";
import { MiniAppSignIn } from "../features/telegram/MiniAppSignIn";
import { TelegramSettingsPage } from "../features/telegram/TelegramSettingsPage";
import { startTarget, type TelegramLaunch } from "../features/telegram/webapp";
import { useState } from "react";

/**
 * Gate: setup wizard → sign-in → application, with loading and error states for each step. Inside the Telegram
 * Mini App (telegram launch data present) sign-in uses Telegram, and startapp links open the invitation or the
 * Link Telegram confirmation first.
 */
export function App({ telegram = null }: { telegram?: TelegramLaunch | null }) {
  return (
    <TelegramContext.Provider value={telegram}>
      <NotifyProvider>
        <Gate />
      </NotifyProvider>
    </TelegramContext.Provider>
  );
}

function Gate() {
  const { t } = useTranslation();
  const setup = useSetupStatus();
  const me = useMe(setup.data?.setupRequired === false);
  const location = useLocation();
  const tg = useTelegramLaunch();
  const [startDone, setStartDone] = useState(false);
  const target = tg && !startDone ? startTarget(tg.startParam) : null;

  // Invitation links are for people without an account: no setup or session checks apply.
  if (location.pathname === "/invite") return <AcceptInvitePage />;
  if (tg && target?.kind === "invite")
    return <AcceptInvitePage telegramToken={target.token} onDone={() => setStartDone(true)} />;
  if (tg && target?.kind === "link") return <LinkConfirmPage initData={tg.initData} onDone={() => setStartDone(true)} />;

  if (setup.isPending || (setup.data?.setupRequired === false && me.isPending)) {
    return (
      <main id="main" className="center">
        <Loading />
      </main>
    );
  }
  const failed = setup.isError ? setup : me.isError ? me : null;
  if (failed) {
    return (
      <main id="main" className="center">
        <ErrorState message={t("app.unreachable")} onRetry={() => void failed.refetch()} />
      </main>
    );
  }
  if (setup.data?.setupRequired) return <SetupWizard />;
  if (!me.data) return tg ? <MiniAppSignIn initData={tg.initData} /> : <LoginPage />;
  if (me.data.mustChangePassword) return <ForcedPasswordChange />;

  return (
    <AppShell me={me.data}>
        <Routes>
          <Route path="/" element={<OverviewPage />} />
          <Route path="/projects" element={<ProjectsPage me={me.data} />} />
          <Route path="/projects/:id" element={<ProjectPage me={me.data} />} />
          <Route path="/approvals" element={<ApprovalsPage />} />
          <Route path="/approvals/:id" element={<ApprovalPage />} />
          <Route path="/account/password" element={<AccountPage />} />
          {me.data.role === "owner" && <Route path="/users" element={<UsersPage />} />}
          {me.data.role === "owner" && <Route path="/runners" element={<RunnersPage />} />}
          {me.data.role === "owner" && <Route path="/runners/:id" element={<RunnerPage />} />}
          {me.data.role === "owner" && <Route path="/infrastructure/storage" element={<StoragePage />} />}
          {me.data.role === "owner" && <Route path="/integrations" element={<IntegrationsPage />} />}
          {me.data.role === "owner" && <Route path="/network" element={<NetworkPage />} />}
          {me.data.role === "owner" && <Route path="/audit" element={<AuditPage />} />}
          {me.data.role === "owner" && <Route path="/settings/telegram" element={<TelegramSettingsPage />} />}
          <Route path="*" element={<PageNotFound />} />
        </Routes>
    </AppShell>
  );
}
