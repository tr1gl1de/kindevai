import { useTranslation } from "react-i18next";
import type { AttemptState, StepState, TaskState, TaskTime, WorkStatus } from "../../api/types";
import { formatDuration } from "../../ui/format";

/** States are always text, never colour alone. */
export function TaskStateBadge({ state }: { state: TaskState }) {
  const { t } = useTranslation();
  return <span className={`badge task-state task-${state}`}>{t(`tasks.state_${state}`)}</span>;
}

export function StepStateBadge({ state }: { state: StepState }) {
  const { t } = useTranslation();
  return <span className={`badge task-state step-${state}`}>{t(`tasks.step_${state}`)}</span>;
}

export function AttemptStateBadge({ state }: { state: AttemptState }) {
  const { t } = useTranslation();
  return <span className={`badge task-state attempt-${state}`}>{t(`tasks.attempt_${state}`)}</span>;
}

/** Agent time with its status: running time says so, and an incomplete record is never shown as a plain total. */
export function AgentTimeValue({ ms, status }: { ms: number; status: WorkStatus }) {
  const { t, i18n } = useTranslation();
  if (status === "none") return <span className="muted">{t("tasks.work_none")}</span>;
  return (
    <span className="agent-time">
      {formatDuration(ms, i18n.language)}
      {status === "running" && <span className="muted"> ({t("tasks.running")})</span>}
      {status === "incomplete" && <span className="badge time-incomplete">{t("tasks.incomplete")}</span>}
    </span>
  );
}

export function CalendarValue({ time }: { time: TaskTime }) {
  const { t, i18n } = useTranslation();
  if (!time.started) return <span className="muted">{t("tasks.notStarted")}</span>;
  return (
    <span>
      {formatDuration(time.calendarMs, i18n.language)}
      {time.running && <span className="muted"> ({t("tasks.running")})</span>}
    </span>
  );
}
