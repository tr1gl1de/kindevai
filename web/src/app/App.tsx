import { useTranslation } from "react-i18next";
import { Route, Routes } from "react-router";
import { LoginPage } from "../features/identity/LoginPage";
import { SetupWizard } from "../features/identity/SetupWizard";
import { useMe, useSetupStatus } from "../features/identity/api";
import { OverviewPage } from "../features/dashboard/OverviewPage";
import { ProjectPage } from "../features/projects/ProjectPage";
import { ProjectsPage } from "../features/projects/ProjectsPage";
import { NotFoundPage } from "../features/projects/NotFoundPage";
import { ErrorState, Loading } from "../ui/feedback";
import { NotifyProvider } from "../ui/notify";
import { AppShell } from "./AppShell";

/** Gate: setup wizard → sign-in → application, with loading and error states for each step. */
export function App() {
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

  return (
    <NotifyProvider>
      <AppShell me={me.data}>
        <Routes>
          <Route path="/" element={<OverviewPage />} />
          <Route path="/projects" element={<ProjectsPage me={me.data} />} />
          <Route path="/projects/:id" element={<ProjectPage me={me.data} />} />
          <Route path="*" element={<NotFoundPage />} />
        </Routes>
      </AppShell>
    </NotifyProvider>
  );
}
