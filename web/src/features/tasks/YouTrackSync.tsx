import { useTranslation } from "react-i18next";
import type { DocsDecision, Publication } from "../../api/types";

const knownErrors = new Set(["conflict", "unauthorized", "not_configured", "not_found"]);

/** The task's YouTrack write-back (KIN-38) as a details row: awaiting publication, failed, or published. */
export function PublicationStatus({ p }: { p: Publication }) {
  const { t } = useTranslation();
  if (p.pending + p.failed + p.published === 0) return null;
  const error = p.lastError
    ? knownErrors.has(p.lastError)
      ? t(`tasks.publicationError_${p.lastError}`)
      : t("tasks.publicationErrorOther", { code: p.lastError })
    : "";
  return (
    <>
      <dt>{t("tasks.publicationTitle")}</dt>
      <dd>
        {p.failed > 0 && <span className="badge pr-failed">{t("tasks.publicationFailed", { n: p.failed })}</span>}
        {p.pending > 0 && (
          <>
            {p.failed > 0 && " "}
            <span className="badge pr-opening" role="status">
              {t("tasks.publicationPending", { n: p.pending })}
            </span>
          </>
        )}
        {p.failed + p.pending === 0 && <span className="badge pr-open">{t("tasks.publicationDone")}</span>}
        {error && <span className="muted"> — {error}</span>}
      </dd>
    </>
  );
}

/** The documentation decision of the latest completed run (KIN-38). */
export function DocsSection({ d }: { d: DocsDecision }) {
  const { t } = useTranslation();
  return (
    <section aria-labelledby="task-docs" className="section">
      <h2 id="task-docs">{t("tasks.docsTitle")}</h2>
      <div className="card">
        {d.status === "updated" && (
          <>
            <p>{t("tasks.docs_updated")}</p>
            <ul>
              {d.articles.map((a) => (
                <li key={`${a.article} ${a.section}`}>
                  <code>{a.article}</code> — {a.section}
                </li>
              ))}
            </ul>
          </>
        )}
        {d.status === "not_applicable" && <p>{t("tasks.docs_not_applicable", { reason: d.reason ?? "" })}</p>}
        {d.status === "unstated" && <p>{t("tasks.docs_unstated")}</p>}
        {d.rejected.length > 0 && <p className="muted">{t("tasks.docsRejected", { ids: d.rejected.join(", ") })}</p>}
      </div>
    </section>
  );
}
