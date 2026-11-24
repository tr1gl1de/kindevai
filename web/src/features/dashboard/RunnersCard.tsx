import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import { ErrorState } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useRunners } from "../runners/api";

/** Overview card (KIN-55): live runner counts — online, unreachable, waiting for approval — refreshed every 30 s. */
export function RunnersCard() {
  const { t } = useTranslation();
  const q = useRunners(30_000);
  const rs = (q.data ?? []).filter((r) => r.status !== "revoked");
  const approved = rs.filter((r) => r.status === "approved");
  const online = approved.filter((r) => r.availability === "online").length;
  const unreachable = approved.filter((r) => r.availability === "unreachable").length;
  const pending = rs.filter((r) => r.status === "pending").length;
  const attention = q.isSuccess && (online === 0 || unreachable > 0 || pending > 0);
  return (
    <section className={`card${attention ? " warn" : ""}`} aria-labelledby="ov-runners">
      <h2 id="ov-runners">{t("overview.runnersTitle")}</h2>
      {q.isError ? (
        <ErrorState message={errorMessage(t, q.error)} onRetry={() => void q.refetch()} />
      ) : q.isPending ? (
        <p aria-busy="true">{t("app.loading")}</p>
      ) : rs.length === 0 ? (
        <>
          <p>{t("overview.runnersNone")}</p>
          <Link to="/runners">{t("overview.addRunner")}</Link>
        </>
      ) : (
        <>
          <ul className="runner-counts" aria-label={t("overview.runnerCounts")}>
            <li>{t("overview.runnersOnline", { count: online })}</li>
            <li>{t("overview.runnersUnreachable", { count: unreachable })}</li>
            <li>{t("overview.runnersPending", { count: pending })}</li>
          </ul>
          <Link to="/runners">{t("overview.openRunners")}</Link>
        </>
      )}
    </section>
  );
}
