import { useState } from "react";
import { useTranslation } from "react-i18next";
import { ErrorState } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatBytes } from "../../ui/format";
import { useAttemptLog, type LogEntry } from "./api";

interface Props {
  taskId: string;
  stepId: string;
  fence: number;
  number: number;
  live: boolean;
}

/**
 * An attempt's output and lifecycle events, read in bounded pages and kept in a bounded buffer. Output is the
 * runner's (untrusted) text: it is rendered as text inside <pre>, never as markup.
 */
export function AttemptLog(props: Props) {
  const [run, setRun] = useState(0); // a new key reads the log again from the start
  return <LogView key={run} {...props} onRetry={() => setRun((n) => n + 1)} />;
}

function LogView({ taskId, stepId, fence, number, live, onRetry }: Props & { onRetry: () => void }) {
  const { t } = useTranslation();
  const log = useAttemptLog(taskId, stepId, fence, live);
  if (log.error) return <ErrorState message={errorMessage(t, log.error)} onRetry={onRetry} />;
  return (
    <div className="attempt-log" aria-busy={log.loading || undefined}>
      {log.trimmed && <p className="muted">{t("tasks.logTrimmed")}</p>}
      {log.loading && log.entries.length === 0 ? (
        <p className="muted">{t("tasks.logLoading")}</p>
      ) : log.entries.length === 0 ? (
        <p className="muted">{t("tasks.logEmpty")}</p>
      ) : (
        // oxlint-disable-next-line jsx-a11y/no-noninteractive-tabindex -- a scrollable log must be reachable by keyboard (WCAG 2.1.1)
        <pre className="log" tabIndex={0} aria-label={t("tasks.logLabel", { n: number })}>
          {log.entries.map((e) => (
            <LogLine key={e.seq} e={e} />
          ))}
        </pre>
      )}
      {live && <p className="muted">{t("tasks.logLive")}</p>}
    </div>
  );
}

function LogLine({ e }: { e: LogEntry }) {
  const { t, i18n } = useTranslation();
  if (e.kind === "execution.output" && !e.omitted) {
    return <span className={e.stream === "stderr" ? "log-stderr" : undefined}>{e.text}</span>;
  }
  let text: string;
  switch (e.kind) {
    case "execution.accepted":
      text = t("tasks.event_accepted");
      break;
    case "execution.started":
      text = t("tasks.event_started");
      break;
    case "execution.completed":
      text = t("tasks.event_completed");
      break;
    case "execution.failed":
      text = t("tasks.event_failed", { reason: e.text });
      break;
    case "execution.output.dropped":
      text = e.omitted ? t("tasks.event_omitted", { reason: e.text }) : t("tasks.event_dropped", { bytes: formatBytes(e.bytes ?? 0, i18n.language) });
      break;
    default:
      text = e.omitted ? t("tasks.event_omitted", { reason: e.text }) : e.kind;
  }
  return <span className="log-event">{`▸ ${text}\n`}</span>;
}
