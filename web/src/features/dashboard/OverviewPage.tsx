import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import { ErrorState, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useMe } from "../identity/api";
import { useProjects } from "../projects/api";
import { StorageOverviewCard } from "../runners/StorageOverviewCard";
import { RunnersCard } from "./RunnersCard";

/** Landing page: what is set up and what is still missing (the platform never pretends to be ready). */
export function OverviewPage() {
  const { t } = useTranslation();
  const projects = useProjects();
  const owner = useMe(false).data?.role === "owner"; // the signed-in user App already loaded; never refetched here
  return (
    <>
      <PageHeading>{t("overview.title")}</PageHeading>
      <div className="cards">
        <section className="card" aria-labelledby="ov-projects">
          <h2 id="ov-projects">{t("overview.projectsTitle")}</h2>
          {projects.isError ? (
            <ErrorState message={errorMessage(t, projects.error)} onRetry={() => void projects.refetch()} />
          ) : (
            <p aria-busy={projects.isPending}>
              {projects.data ? t("overview.projectsCount", { count: projects.data.length }) : t("app.loading")}
            </p>
          )}
          <Link to="/projects">{t("overview.openProjects")}</Link>
        </section>
        {owner && <RunnersCard />}
        <section className="card warn" aria-labelledby="ov-integrations">
          <h2 id="ov-integrations">{t("overview.integrationsTitle")}</h2>
          <p>{t("overview.integrationsEmpty")}</p>
        </section>
        {owner && <StorageOverviewCard />}
      </div>
    </>
  );
}
