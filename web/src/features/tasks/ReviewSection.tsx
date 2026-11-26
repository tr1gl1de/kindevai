import { useTranslation } from "react-i18next";
import type { Finding, Handoff, ReviewLoop, TestRun, Verdict } from "../../api/types";

const shownFiles = 20;

/** A step's implementation and review loop (KIN-35): where it is, the latest handoff and the latest verdict. */
export function ReviewSection({ review, stepKey }: { review: ReviewLoop; stepKey: string }) {
  const { t } = useTranslation();
  const phase =
    review.phase === "publish"
      ? t("tasks.reviewPhasePublish")
      : review.phase === "review"
      ? t("tasks.reviewPhaseReview")
      : review.round > 0
        ? t("tasks.reviewPhaseImplementFix", { round: review.round, max: review.maxRounds })
        : t("tasks.reviewPhaseImplement");
  const headingId = `review-${stepKey}`;
  return (
    <section className="review-loop" aria-labelledby={headingId}>
      <h5 id={headingId}>{t("tasks.reviewTitle")}</h5>
      <p>
        <span className="badge review-phase">{phase}</span>
        {review.pinnedRunner && <span className="muted"> {t("tasks.reviewPinned", { runner: review.pinnedRunner.slice(0, 10) })}</span>}
      </p>
      {review.handoff && <HandoffBlock h={review.handoff} />}
      {review.verdict && <VerdictBlock v={review.verdict} stepKey={stepKey} />}
    </section>
  );
}

function HandoffBlock({ h }: { h: Handoff }) {
  const { t } = useTranslation();
  return (
    <div className="handoff">
      <h6>{t("tasks.handoffTitle")}</h6>
      {h.agent && <p className="muted">{t("tasks.handoffAgent", { agent: h.agent, n: h.attempt })}</p>}
      {h.summary && <p className="prewrap">{h.summary}</p>}
      {h.baseSha && h.headSha && (
        <p className="muted">
          {t("tasks.handoffCommits", { base: h.baseSha.slice(0, 10), head: h.headSha.slice(0, 10) })}
        </p>
      )}
      <TestEvidence status={h.testStatus} run={h.tests} />
      {h.files.length > 0 && (
        <details>
          <summary>{t("tasks.handoffFiles", { n: h.filesTotal })}</summary>
          <ul className="file-list">
            {h.files.slice(0, shownFiles).map((f) => (
              <li key={f}>
                <code>{f}</code>
              </li>
            ))}
          </ul>
          {h.filesTotal > shownFiles && <p className="muted">{t("tasks.handoffFilesMore", { n: h.filesTotal - shownFiles })}</p>}
        </details>
      )}
      {h.docs.length > 0 && <TextList title={t("tasks.handoffDocs")} items={h.docs} />}
      {h.openQuestions.length > 0 && <TextList title={t("tasks.handoffQuestions")} items={h.openQuestions} />}
    </div>
  );
}

function TextList({ title, items }: { title: string; items: string[] }) {
  return (
    <>
      <p className="list-title">{title}</p>
      <ul>
        {items.map((x, i) => (
          <li key={i}>{x}</li>
        ))}
      </ul>
    </>
  );
}

/** The runner's test evidence: the status is text, never colour alone. */
function TestEvidence({ status, run }: { status: Handoff["testStatus"]; run: TestRun | null }) {
  const { t, i18n } = useTranslation();
  const seconds = run ? new Intl.NumberFormat(i18n.language, { maximumFractionDigits: 1 }).format(run.durationMs / 1000) : "";
  return (
    <div className="tests">
      <p>
        {t("tasks.testsTitle")}: <span className={`badge tests-${status}`}>{t(`tasks.tests_${status}`)}</span>
        {run && (
          <span className="muted">
            {" "}
            {t("tasks.testsDetail", { command: run.command, code: run.exitCode, seconds })}
            {run.timedOut && ` · ${t("tasks.testsTimedOut")}`}
          </span>
        )}
      </p>
      {run && run.output && (
        <details>
          <summary>{t("tasks.testsOutput")}</summary>
          {/* oxlint-disable-next-line jsx-a11y/no-noninteractive-tabindex -- a scrollable log must be reachable by keyboard (WCAG 2.1.1) */}
          <pre className="log" tabIndex={0} aria-label={t("tasks.testsOutput")}>
            {run.output}
          </pre>
        </details>
      )}
    </div>
  );
}

function VerdictBlock({ v, stepKey }: { v: Verdict; stepKey: string }) {
  const { t } = useTranslation();
  return (
    <div className="verdict">
      <h6>{t("tasks.verdictTitle")}</h6>
      <p>
        <span className={`badge verdict-${v.verdict}`}>{t(`tasks.verdict_${v.verdict}`)}</span>
        {v.agent && <span className="muted"> {t("tasks.verdictBy", { agent: v.agent, n: v.attempt })}</span>}
      </p>
      {v.summary && <p className="prewrap">{v.summary}</p>}
      {v.findings.length > 0 && (
        <div className="table-wrap">
          <table>
            <caption className="visually-hidden">
              {t("tasks.findingsCaption")} ({stepKey})
            </caption>
            <thead>
              <tr>
                <th scope="col">{t("tasks.colSeverity")}</th>
                <th scope="col">{t("tasks.colLocation")}</th>
                <th scope="col">{t("tasks.colFinding")}</th>
              </tr>
            </thead>
            <tbody>
              {v.findings.map((f, i) => (
                <tr key={i}>
                  <td>{t(`tasks.severity_${f.severity}`)}</td>
                  <td>{f.file ? <code>{location(f)}</code> : "—"}</td>
                  <td className="prewrap">{f.message}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function location(f: Finding): string {
  return f.line ? `${f.file}:${f.line}` : (f.file ?? "");
}
