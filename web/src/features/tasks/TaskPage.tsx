import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Link, useParams } from "react-router";
import { isApiError } from "../../api/client";
import type { Attempt, MockScenario, Step, TaskDetail } from "../../api/types";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { SelectField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import { RiskBadge, StateBadge } from "../approvals/badges";
import { useProjects } from "../projects/api";
import { AttemptLog } from "./AttemptLog";
import { isTerminal, useCancelTask, useMockRun, useRetryTask, useTask, useTaskApprovals } from "./api";
import { AgentTimeValue, AttemptStateBadge, CalendarValue, StepStateBadge, TaskStateBadge } from "./badges";
import "./tasks.css";

/** One task: its plan revision, the steps of its latest execution with attempts, logs and agent time, actions. */
export function TaskPage({ isOwner }: { isOwner: boolean }) {
  const { t } = useTranslation();
  const { id = "" } = useParams();
  const q = useTask(id);

  if (q.isPending) return <Loading />;
  if (isApiError(q.error, "not_found")) {
    return (
      <>
        <p className="breadcrumbs">
          <Link to="/tasks">{t("tasks.back")}</Link>
        </p>
        <PageHeading>{t("tasks.notFound")}</PageHeading>
        <p>{t("tasks.notFoundText")}</p>
      </>
    );
  }
  if (q.isError) return <ErrorState message={errorMessage(t, q.error)} onRetry={() => void q.refetch()} />;
  return <TaskCard d={q.data} isOwner={isOwner} />;
}

const shortHash = (h: string) => h.slice(0, 12);

function TaskCard({ d, isOwner }: { d: TaskDetail; isOwner: boolean }) {
  const { t, i18n } = useTranslation();
  const task = d.task;
  const projects = useProjects();
  const projectName = projects.data?.find((p) => p.id === task.projectId)?.name ?? task.projectId;

  return (
    <>
      <p className="breadcrumbs">
        <Link to={`/tasks?projectId=${encodeURIComponent(task.projectId)}`}>{t("tasks.back")}</Link>
      </p>
      <div className="page-head">
        <PageHeading>{task.title}</PageHeading>
        <TaskStateBadge state={task.state} />
      </div>
      {task.blockReason && <p className="card warn">{t(`tasks.block_${task.blockReason}`)}{task.reason ? ` — ${task.reason}` : ""}</p>}

      <section aria-labelledby="task-details" className="section">
        <h2 id="task-details">{t("tasks.details")}</h2>
        <dl className="details">
          <dt>{t("tasks.project")}</dt>
          <dd>{projectName}</dd>
          {task.youtrackIssue && (
            <>
              <dt>{t("tasks.youtrack")}</dt>
              <dd>
                <code>{task.youtrackIssue}</code>
              </dd>
            </>
          )}
          <dt>{t("tasks.createdAt")}</dt>
          <dd>{formatDateTime(task.createdAt, i18n.language)}</dd>
          <dt>{t("tasks.updatedAt")}</dt>
          <dd>{formatDateTime(task.updatedAt, i18n.language)}</dd>
          {task.reason && !task.blockReason && (
            <>
              <dt>{t("tasks.reason")}</dt>
              <dd>{task.reason}</dd>
            </>
          )}
        </dl>
      </section>

      <TimeSection d={d} />

      <section aria-labelledby="task-plan" className="section">
        <h2 id="task-plan">{t("tasks.planTitle")}</h2>
        {d.revision ? (
          <dl className="details">
            <dt>{t("tasks.revision")}</dt>
            <dd>
              <code>{t("tasks.revisionValue", { number: d.revision.number, hash: shortHash(d.revision.hash) })}</code>
            </dd>
            <dt>{t("tasks.risk")}</dt>
            <dd>
              <RiskBadge risk={d.revision.risk} />
            </dd>
            <dt>{t("tasks.summary")}</dt>
            <dd>{d.revision.summary}</dd>
            <dt>{t("tasks.plannedBy")}</dt>
            <dd>
              {d.revision.createdBy} · {formatDateTime(d.revision.createdAt, i18n.language)}
            </dd>
          </dl>
        ) : (
          <p className="muted">{t("tasks.noPlan")}</p>
        )}
        {/* KIN-44: the plan discussion chat (Owner / project admin) mounts here, under the revision it discusses. */}
      </section>

      {d.canManage && <Actions d={d} />}

      <StepsSection d={d} isOwner={isOwner} />

      {d.canManage && <TaskApprovals taskId={task.id} />}
    </>
  );
}

function TimeSection({ d }: { d: TaskDetail }) {
  const { t } = useTranslation();
  return (
    <section aria-labelledby="task-time" className="section">
      <h2 id="task-time">{t("tasks.timeTitle")}</h2>
      <dl className="details">
        <dt>{t("tasks.agentTime")}</dt>
        <dd>
          <AgentTimeValue ms={d.time.agentMs} status={d.time.agentStatus} />
          <div className="muted">{t("tasks.totalHint")}</div>
        </dd>
        <dt>{t("tasks.calendar")}</dt>
        <dd>
          <CalendarValue time={d.time} />
          <div className="muted">{t("tasks.calendarHint")}</div>
        </dd>
      </dl>
      {d.time.agentStatus === "incomplete" && <p className="card warn">{t("tasks.incompleteHint")}</p>}
    </section>
  );
}

const scenarios: MockScenario[] = ["success", "fail", "flaky", "crash", "hang", "gate"];

function Actions({ d }: { d: TaskDetail }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const task = d.task;
  const mock = useMockRun(task.id);
  const cancel = useCancelTask(task.id);
  const retry = useRetryTask(task.id);
  const [scenario, setScenario] = useState<MockScenario>("success");
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState<string>();
  const fail = (e: unknown) => setError(errorMessage(t, e));
  const canPlan = task.state === "CREATED";
  const canRetry = task.state === "FAILED" && task.planRevision > 0;
  const canCancel = !isTerminal(task.state);
  if (!canPlan && !canRetry && !canCancel) return null;

  const plan = (e: FormEvent) => {
    e.preventDefault();
    setError(undefined);
    mock.mutate(scenario, { onSuccess: () => notify(t("tasks.mockRunDone")), onError: fail });
  };

  return (
    <section aria-labelledby="task-actions" className="section">
      <h2 id="task-actions">{t("tasks.actionsTitle")}</h2>
      <FormAlert message={error} />
      {canPlan && (
        <form onSubmit={plan} className="card form-card">
          <SelectField label={t("tasks.scenario")} hint={t("tasks.scenarioHint")} value={scenario}
            onChange={(e) => setScenario(e.target.value as MockScenario)}>
            {scenarios.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </SelectField>
          <div>
            <button type="submit" className="button primary" disabled={mock.isPending}>
              {mock.isPending ? t("tasks.mockRunning") : t("tasks.mockRun")}
            </button>
          </div>
        </form>
      )}
      <div className="task-actions">
        {canRetry && (
          <div>
            <button type="button" className="button primary" disabled={retry.isPending}
              onClick={() => {
                setError(undefined);
                retry.mutate(undefined, { onSuccess: () => notify(t("tasks.retryDone")), onError: fail });
              }}>
              {retry.isPending ? t("tasks.retrying") : t("tasks.retry")}
            </button>
            <p className="muted">{t("tasks.retryHint")}</p>
          </div>
        )}
        {canCancel && (
          <button type="button" className="button danger" onClick={() => setConfirming(true)}>
            {t("tasks.cancel")}
          </button>
        )}
      </div>
      {confirming && (
        <ConfirmDialog
          title={t("tasks.cancelTitle", { title: task.title })}
          text={t("tasks.cancelText")}
          confirmLabel={t("tasks.cancel")}
          cancelLabel={t("tasks.cancelKeep")}
          busy={cancel.isPending}
          onCancel={() => setConfirming(false)}
          onConfirm={() => {
            setError(undefined);
            cancel.mutate(t("tasks.cancelReason"), {
              onSuccess: () => notify(t("tasks.cancelDone")),
              onError: fail,
              onSettled: () => setConfirming(false),
            });
          }}
        />
      )}
    </section>
  );
}

/** Steps grouped into stages by dependency depth: a stage's steps can run in parallel once earlier stages succeeded. */
export function stages(steps: Step[]): Step[][] {
  const byKey = new Map(steps.map((s) => [s.key, s]));
  const depth = new Map<string, number>();
  const level = (s: Step, seen: Set<string>): number => {
    const known = depth.get(s.key);
    if (known !== undefined) return known;
    if (seen.has(s.key)) return 0; // a cycle cannot pass plan validation; never loop on bad data
    seen.add(s.key);
    const deps = s.dependsOn.map((k) => byKey.get(k)).filter((d): d is Step => d !== undefined);
    const n = deps.length === 0 ? 0 : 1 + Math.max(...deps.map((d) => level(d, seen)));
    depth.set(s.key, n);
    return n;
  };
  const out: Step[][] = [];
  for (const s of steps) (out[level(s, new Set())] ??= []).push(s);
  return out.filter(Boolean);
}

function StepsSection({ d, isOwner }: { d: TaskDetail; isOwner: boolean }) {
  const { t } = useTranslation();
  const e = d.execution;
  return (
    <section aria-labelledby="task-steps" className="section">
      <h2 id="task-steps">{t("tasks.stepsTitle")}</h2>
      {!e ? (
        <p className="muted">{t("tasks.noExecution")}</p>
      ) : (
        <>
          <p>
            {t("tasks.execution")}: {t("tasks.executionValue", { state: e.state, revision: e.planRevision })} ·{" "}
            {t("tasks.agentTime")}: <AgentTimeValue ms={e.agentTime.ms} status={e.agentTime.status} />
          </p>
          {e.reason && <p className="muted">{e.reason}</p>}
          <ol className="stages">
            {stages(e.steps).map((stage, i) => (
              <li key={i}>
                <h3>{t("tasks.stage", { n: i + 1 })}</h3>
                <ul className="card-list">
                  {stage.map((s) => (
                    <StepCard key={s.id} taskId={d.task.id} s={s} isOwner={isOwner} />
                  ))}
                </ul>
              </li>
            ))}
          </ol>
        </>
      )}
    </section>
  );
}

function RunnerRef({ id, isOwner }: { id: string; isOwner: boolean }) {
  const short = id.slice(0, 10);
  return isOwner ? <Link to={`/runners/${encodeURIComponent(id)}`}>{short}</Link> : <code>{short}</code>;
}

function StepCard({ taskId, s, isOwner }: { taskId: string; s: Step; isOwner: boolean }) {
  const { t } = useTranslation();
  return (
    <li className="card step-card">
      <div className="page-head">
        <h4>
          {s.title} <code className="muted">{s.key}</code>
        </h4>
        <StepStateBadge state={s.state} />
      </div>
      <p className="muted">
        {t("tasks.repository")}: <code>{s.repository}</code>
        {s.dependsOn.length > 0 && <> · {t("tasks.dependsOn", { keys: s.dependsOn.join(", ") })}</>}
        {s.waitReason && <> · {t(`tasks.block_${s.waitReason}`)}</>}
      </p>
      {s.reason && <p>{s.reason}</p>}
      <p>
        {t("tasks.agentTime")}: <AgentTimeValue ms={s.agentTime.ms} status={s.agentTime.status} />
      </p>
      {s.attempts.length === 0 ? (
        <p className="muted">{t("tasks.noAttempts")}</p>
      ) : (
        <Attempts taskId={taskId} s={s} isOwner={isOwner} />
      )}
    </li>
  );
}

function Attempts({ taskId, s, isOwner }: { taskId: string; s: Step; isOwner: boolean }) {
  const { t, i18n } = useTranslation();
  return (
    <>
      <div className="table-wrap">
        <table>
          <caption className="visually-hidden">{t("tasks.attemptsCaption", { step: s.key })}</caption>
          <thead>
            <tr>
              <th scope="col">{t("tasks.colAttempt")}</th>
              <th scope="col">{t("tasks.colRunner")}</th>
              <th scope="col">{t("tasks.colAgent")}</th>
              <th scope="col">{t("tasks.colState")}</th>
              <th scope="col">{t("tasks.colStarted")}</th>
              <th scope="col">{t("tasks.colWork")}</th>
              <th scope="col">{t("tasks.colError")}</th>
            </tr>
          </thead>
          <tbody>
            {s.attempts.map((a) => (
              <tr key={a.number}>
                <td>{a.number}</td>
                <td>
                  <RunnerRef id={a.runnerId} isOwner={isOwner} />
                </td>
                <td>{a.agent ?? "—"}</td>
                <td>
                  <AttemptStateBadge state={a.state} />
                </td>
                <td>{formatDateTime(a.workStartedAt ?? a.startedAt, i18n.language)}</td>
                <td>
                  <AgentTimeValue ms={a.workMs} status={a.workStatus} />
                </td>
                <td>{a.error ?? ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {s.attempts.map((a) => (
        <AttemptDetails key={a.number} taskId={taskId} stepId={s.id} a={a} />
      ))}
    </>
  );
}

/** The log loads only when opened, so a long task page does not poll every attempt. */
function AttemptDetails({ taskId, stepId, a }: { taskId: string; stepId: string; a: Attempt }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  return (
    <details className="attempt-details" onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary>{t("tasks.logShow", { n: a.number })}</summary>
      {open && (
        <AttemptLog taskId={taskId} stepId={stepId} fence={a.fence} number={a.number}
          live={a.state === "RUNNING" || a.state === "UNKNOWN"} />
      )}
    </details>
  );
}

function TaskApprovals({ taskId }: { taskId: string }) {
  const { t } = useTranslation();
  const q = useTaskApprovals(taskId, true);
  return (
    <section aria-labelledby="task-approvals" className="section">
      <h2 id="task-approvals">{t("tasks.approvalsTitle")}</h2>
      {q.isPending ? (
        <Loading />
      ) : q.isError ? (
        <ErrorState message={errorMessage(t, q.error)} onRetry={() => void q.refetch()} />
      ) : q.data.length === 0 ? (
        <p className="muted">{t("tasks.approvalsEmpty")}</p>
      ) : (
        <ul className="project-list">
          {q.data.map((a) => (
            <li key={a.id}>
              <Link to={`/approvals/${encodeURIComponent(a.id)}`}>
                {t("tasks.approvalRow", { scope: t(`approvals.scope_${a.scope}`), revision: a.planRevision })}
              </Link>
              <StateBadge state={a.state} />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
