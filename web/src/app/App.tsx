import { useTranslation } from "react-i18next";
import { Route, Routes } from "react-router";
import { LoginPage } from "../features/identity/LoginPage";
import { SetupWizard } from "../features/identity/SetupWizard";
import { useMe, useSetupStatus } from "../features/identity/api";
import { OverviewPage } from "../features/dashboard/OverviewPage";
import { ProjectPage } from "../features/projects/ProjectPage";
import { ProjectsPage } from "../features/projects/ProjectsPage";
import { AccountPage } from "../features/identity/AccountPage";
import { ForcedPasswordChange } from "../features/identity/ForcedPasswordChange";
import { UsersPage } from "../features/identity/UsersPage";
import { PageNotFound } from "./PageNotFound";
import { ErrorState, Loading } from "../ui/feedback";
import { NotifyProvider } from "../ui/notify";
import { AppShell } from "./AppShell";

/** Gate: setup wizard → sign-in → application, with loading and error states for each step. */
export function App() {
  return (
    <NotifyProvider>
      <Gate />
    </NotifyProvider>
  );
}

function Gate() {
  const { t } = useTranslation();
  const setup = useSetupStatus();
  const me = useMe(setup.data?.setupRequired === false);

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
  if (!me.data) return <LoginPage />;
  if (me.data.mustChangePassword) return <ForcedPasswordChange />;

  return (
    <AppShell me={me.data}>
        <Routes>
          <Route path="/" element={<OverviewPage />} />
          <Route path="/projects" element={<ProjectsPage me={me.data} />} />
          <Route path="/projects/:id" element={<ProjectPage me={me.data} />} />
          <Route path="/account/password" element={<AccountPage />} />
          {me.data.role === "owner" && <Route path="/users" element={<UsersPage />} />}
          <Route path="*" element={<PageNotFound />} />
        </Routes>
    </AppShell>
  );
}
