import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { PlanningRun, TaskDetail, TaskRevision } from "../../api/types";
import { formatDateTime } from "../../ui/format";
import { RiskBadge } from "../approvals/badges";
import { AttemptLog } from "./AttemptLog";
import { AttemptWorkspace } from "./AttemptWorkspace";

const shortHash = (h: string) => h.slice(0, 12);

/** The task's plan: the current revision with its steps and declared impact, and the AI planner's latest run. */
export function PlanSection({ d }: { d: TaskDetail }) {
  const { t } = useTranslation();
  return (
    <section aria-labelledby="task-plan" className="section">
      <h2 id="task-plan">{t("tasks.planTitle")}</h2>
      {d.planning && <PlannerRun taskId={d.task.id} run={d.planning} />}
      {d.revision ? <Revision rev={d.revision} /> : !d.planning && <p className="muted">{t("tasks.noPlan")}</p>}
      {/* KIN-44: the plan discussion chat (Owner / project admin) mounts here, under the revision it discusses. */}
    </section>
  );
}

function Revision({ rev }: { rev: TaskRevision }) {
  const { t, i18n } = useTranslation();
  const impact = (["contracts", "migrations", "tests", "docs"] as const).filter((k) => rev[k].length > 0);
  return (
    <>
      <dl className="details">
        <dt>{t("tasks.revision")}</dt>
        <dd>
          <code>{t("tasks.revisionValue", { number: rev.number, hash: shortHash(rev.hash) })}</code>
        </dd>
        <dt>{t("tasks.risk")}</dt>
        <dd>
          <RiskBadge risk={rev.risk} />
        </dd>
        <dt>{t("tasks.summary")}</dt>
        <dd className="plan-summary">{rev.summary}</dd>
        <dt>{t("tasks.plannedBy")}</dt>
        <dd>
          {rev.createdBy} · {formatDateTime(rev.createdAt, i18n.language)}
        </dd>
        {impact.map((k) => (
          <ImpactRow key={k} label={t(`tasks.impact_${k}`)} items={rev[k]} />
        ))}
      </dl>
      {rev.steps.length > 0 && (
        <div className="table-wrap">
          <table>
            <caption>{t("tasks.planStepsCaption", { n: rev.maxParallel })}</caption>
            <thead>
              <tr>
                <th scope="col">{t("tasks.colStep")}</th>
                <th scope="col">{t("tasks.repository")}</th>
                <th scope="col">{t("tasks.colRole")}</th>
                <th scope="col">{t("tasks.colScope")}</th>
                <th scope="col">{t("tasks.colAfter")}</th>
              </tr>
            </thead>
            <tbody>
              {rev.steps.map((s) => (
                <tr key={s.key}>
                  <td>
                    {s.title}
                    <div className="muted">
                      <code>{s.key}</code>
                      {s.capabilities.length > 0 && ` · ${t("tasks.needs", { caps: s.capabilities.join(", ") })}`}
                    </div>
                  </td>
                  <td>
                    <code>{s.repository}</code>
                  </td>
                  <td>{t(`tasks.role_${s.role}`)}</td>
                  <td>
                    {s.scope.length > 0 ? (
                      <ul className="plain-list">
                        {s.scope.map((p) => (
                          <li key={p}>
                            <code>{p}</code>
                          </li>
                        ))}
                      </ul>
                    ) : (
                      <span className="muted">{t("tasks.scopeNone")}</span>
                    )}
                  </td>
                  <td>{s.dependsOn.join(", ")}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

function ImpactRow({ label, items }: { label: string; items: string[] }) {
  return (
    <>
      <dt>{label}</dt>
      <dd>
        <ul className="plain-list">
          {items.map((x) => (
            <li key={x}>{x}</li>
          ))}
        </ul>
      </dd>
    </>
  );
}

const activePlanning = (s: PlanningRun["state"]) => s === "PREPARING" || s === "QUEUED" || s === "RUNNING";

/** The AI planner's run: its state, CLI, attempts, what its context package held and the attempt's log. */
function PlannerRun({ taskId, run }: { taskId: string; run: PlanningRun }) {
  const { t, i18n } = useTranslation();
  const context = [
    run.issueText ? t("tasks.plannerIssueText") : t("tasks.plannerNoIssueText"),
    run.articles.length > 0 && t("tasks.plannerArticles", { ids: run.articles.join(", ") }),
  ].filter(Boolean);
  return (
    <div className={`card planner planner-${run.state.toLowerCase()}`} role="group" aria-label={t("tasks.plannerTitle")}>
      <h3>{t("tasks.plannerTitle")}</h3>
      <dl className="details">
        <dt>{t("tasks.plannerState")}</dt>
        <dd>
          <span className="badge">{t(`tasks.planning_${run.state}`)}</span>
          {activePlanning(run.state) && run.state !== "RUNNING" && run.waitDetail && <div className="muted">{run.waitDetail}</div>}
          {run.notBefore && run.state === "QUEUED" && (
            <div className="muted">{t("tasks.plannerNotBefore", { at: formatDateTime(run.notBefore, i18n.language) })}</div>
          )}
        </dd>
        {run.agent && (
          <>
            <dt>{t("tasks.colAgent")}</dt>
            <dd>
              {run.model ? `${run.agent} · ${run.model}` : run.agent}
              {run.runnerId && <div className="muted">{t("tasks.plannerRunner", { runner: run.runnerId })}</div>}
            </dd>
          </>
        )}
        <dt>{t("tasks.plannerAttempts")}</dt>
        <dd>{t("tasks.plannerAttemptsValue", { used: run.used, max: run.maxAttempts })}</dd>
        <dt>{t("tasks.plannerContext")}</dt>
        <dd>
          {context.join(" · ")}
          {run.truncated && <div className="muted">{t("tasks.plannerTruncated")}</div>}
          {run.contextNote && <div className="muted">{run.contextNote}</div>}
          <div className="muted">{t("tasks.plannerUntrusted")}</div>
        </dd>
        {run.feedback && run.state !== "SUCCEEDED" && (
          <>
            <dt>{t("tasks.plannerRefused")}</dt>
            <dd>
              <code className="planner-feedback">{run.feedback}</code>
            </dd>
          </>
        )}
        {run.reason && (
          <>
            <dt>{t("tasks.reason")}</dt>
            <dd>{run.reason}</dd>
          </>
        )}
      </dl>
      {run.hasSession && <p className="muted">{t("tasks.plannerSession")}</p>}
      {run.attempt > 0 && <PlannerAttempt taskId={taskId} run={run} />}
    </div>
  );
}

function PlannerAttempt({ taskId, run }: { taskId: string; run: PlanningRun }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [wsOpen, setWsOpen] = useState(false);
  const live = run.state === "RUNNING";
  return (
    <>
      <details className="attempt-details" onToggle={(e) => setWsOpen(e.currentTarget.open)}>
        <summary>{t("tasks.plannerWorkspace", { n: run.attempt })}</summary>
        {wsOpen && <AttemptWorkspace taskId={taskId} stepId={run.stepId} fence={run.attempt} number={run.attempt} live={live} />}
      </details>
      <details className="attempt-details" onToggle={(e) => setOpen(e.currentTarget.open)}>
        <summary>{t("tasks.plannerLog", { n: run.attempt })}</summary>
        {open && <AttemptLog taskId={taskId} stepId={run.stepId} fence={run.attempt} number={run.attempt} live={live} />}
      </details>
    </>
  );
}
