import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import { PageHeading } from "../ui/feedback";

export function PageNotFound() {
  const { t } = useTranslation();
  return (
    <>
      <PageHeading>{t("nav.notFound")}</PageHeading>
      <p>{t("nav.notFoundText")}</p>
      <Link to="/">{t("nav.home")}</Link>
    </>
  );
}
