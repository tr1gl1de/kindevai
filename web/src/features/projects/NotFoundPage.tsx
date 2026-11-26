import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import { PageHeading } from "../../ui/feedback";

export function NotFoundPage() {
  const { t } = useTranslation();
  return (
    <>
      <PageHeading>{t("projects.notFoundTitle")}</PageHeading>
      <p>{t("projects.notFoundText")}</p>
      <Link to="/projects">{t("projects.back")}</Link>
    </>
  );
}
