import { useTranslation } from "react-i18next";
import type { PullRequest } from "../../api/types";

/** The task's change set (KIN-36): each published branch and its pull request into the integration branch. */
export function PullRequestsSection({ prs }: { prs: PullRequest[] }) {
  const { t } = useTranslation();
  if (prs.length === 0) return null;
  return (
    <section aria-labelledby="task-prs" className="section">
      <h2 id="task-prs">{t("tasks.prsTitle")}</h2>
      <ul className="card-list">
        {prs.map((p) => (
          <li key={`${p.repository} ${p.branch}`} className="card">
            <p>
              {p.state === "open" && p.url ? (
                <a href={p.url} target="_blank" rel="noreferrer noopener">
                  {t("tasks.prLink", { repository: p.repository, n: p.number ?? 0 })}
                </a>
              ) : (
                <code>{p.repository}</code>
              )}{" "}
              <span className={`badge pr-${p.state}`}>{t(`tasks.prState_${p.state}`)}</span>
            </p>
            <p className="muted">
              {t("tasks.prBranch", { branch: p.branch, base: p.base || "…", head: p.headSha.slice(0, 10) })}
            </p>
            {p.state === "opening" && <p className="muted">{t("tasks.prOpening")}</p>}
            {p.state === "failed" && <p>{t("tasks.prFailed", { error: p.error ?? "" })}</p>}
          </li>
        ))}
      </ul>
    </section>
  );
}
