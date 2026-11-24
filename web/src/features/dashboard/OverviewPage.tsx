import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import { PageHeading } from "../../ui/feedback";
import { useProjects } from "../projects/api";

/** Landing page: what is set up and what is still missing (the platform never pretends to be ready). */
export function OverviewPage() {
  const { t } = useTranslation();
  const projects = useProjects();
  return (
    <>
      <PageHeading>{t("overview.title")}</PageHeading>
      <div className="cards">
        <section className="card" aria-labelledby="ov-projects">
          <h2 id="ov-projects">{t("overview.projectsTitle")}</h2>
          <p aria-busy={projects.isPending}>
            {projects.data ? t("overview.projectsCount", { count: projects.data.length }) : t("app.loading")}
          </p>
          <Link to="/projects">{t("overview.openProjects")}</Link>
        </section>
        <section className="card warn" aria-labelledby="ov-runners">
          <h2 id="ov-runners">{t("overview.runnersTitle")}</h2>
          <p>{t("overview.runnersEmpty")}</p>
        </section>
        <section className="card warn" aria-labelledby="ov-integrations">
          <h2 id="ov-integrations">{t("overview.integrationsTitle")}</h2>
          <p>{t("overview.integrationsEmpty")}</p>
        </section>
      </div>
    </>
  );
}
