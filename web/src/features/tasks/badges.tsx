import { useTranslation } from "react-i18next";
import type { AttemptState, StepState, TaskState, TaskTime, WorkStatus } from "../../api/types";
import { Badge, type Tone } from "../../ui/Badge";
import { formatDuration } from "../../ui/format";

// Brand System components.md: queued → neutral, planning/running → info, awaiting approval → warning,
// succeeded → success, failed → danger, cancelled → neutral. BLOCKED, WAITING and UNKNOWN need attention → warning.
const TASK_TONE: Record<TaskState, Tone> = {
  CREATED: "neutral",
  ANALYZING: "info",
  READY: "neutral",
  AWAITING_APPROVAL: "warning",
  RUNNING: "info",
  VERIFYING: "info",
  COMPLETED: "success",
  BLOCKED: "warning",
  FAILED: "danger",
  CANCELLED: "neutral",
};
const STEP_TONE: Record<StepState, Tone> = {
  PENDING: "neutral",
  READY: "neutral",
  CLAIMED: "info",
  RUNNING: "info",
  SUCCEEDED: "success",
  WAITING: "warning",
  FAILED: "danger",
  CANCELLED: "neutral",
};
const ATTEMPT_TONE: Record<AttemptState, Tone> = {
  RUNNING: "info",
  SUCCEEDED: "success",
  FAILED: "danger",
  CANCELLED: "neutral",
  UNKNOWN: "warning",
};

/** States are always text, never colour alone. */
export function TaskStateBadge({ state }: { state: TaskState }) {
  const { t } = useTranslation();
  return <Badge tone={TASK_TONE[state]}>{t(`tasks.state_${state}`)}</Badge>;
}

export function StepStateBadge({ state }: { state: StepState }) {
  const { t } = useTranslation();
  return <Badge tone={STEP_TONE[state]}>{t(`tasks.step_${state}`)}</Badge>;
}

export function AttemptStateBadge({ state }: { state: AttemptState }) {
  const { t } = useTranslation();
  return <Badge tone={ATTEMPT_TONE[state]}>{t(`tasks.attempt_${state}`)}</Badge>;
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
