import { useTranslation } from "react-i18next";
import type { PullRequest } from "../../api/types";

/**
 * The task's change set (KIN-36): each published branch and its pull request into the integration branch, with CI
 * on its head, the CI fix rounds and the merge (KIN-37).
 */
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
            {p.state === "open" && (
              <p>
                <span className={`badge ci-${p.ci ?? "none"}`}>{t(`tasks.ci_${p.ci ?? "none"}`)}</span>
                {(p.ciRound ?? 0) > 0 && <> · {t("tasks.ciRound", { n: p.ciRound ?? 0, max: p.ciMaxRounds ?? 0 })}</>}
                {p.merge && (
                  <>
                    {" "}
                    · <span className={`badge merge-${p.merge}`}>{t(`tasks.merge_${p.merge}`)}</span>
                  </>
                )}
              </p>
            )}
            {p.ci === "failed" && p.ciDetail && <p className="muted">{p.ciDetail}</p>}
            {p.merge === "merged" && p.mergeSha && (
              <p className="muted">{t("tasks.mergedAs", { base: p.base ?? "", sha: p.mergeSha.slice(0, 10) })}</p>
            )}
            {(p.merge === "waiting" || p.merge === "failed") && p.mergeError && <p>{p.mergeError}</p>}
          </li>
        ))}
      </ul>
    </section>
  );
}
