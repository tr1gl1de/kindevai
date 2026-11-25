import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import { formatDateTime } from "../../ui/format";
import { useMaintenance } from "./api";
import "./maintenance.css";

/** Shown on every page while maintenance mode is on: the platform is read-only and new steps are paused. */
export function MaintenanceBanner({ owner }: { owner: boolean }) {
  const { t, i18n } = useTranslation();
  const st = useMaintenance().data;
  if (!st?.enabled) return null;
  return (
    <section className="maintenance-banner" aria-labelledby="maintenance-banner-title">
      <h2 id="maintenance-banner-title">{t("maintenance.bannerTitle")}</h2>
      <p>{t("maintenance.bannerText")}</p>
      {st.reason && <p>{t("maintenance.bannerReason", { reason: st.reason })}</p>}
      <p className="muted">
        {st.since && t("maintenance.bannerSince", { date: formatDateTime(st.since, i18n.language), by: st.by })}{" "}
        {t("maintenance.draining", { count: st.activeAttempts })}
      </p>
      {owner && <Link to="/settings/maintenance">{t("maintenance.manage")}</Link>}
    </section>
  );
}
