import { useId, useState } from "react";
import { useTranslation } from "react-i18next";
import type { Alert, AlertAnalysisTask } from "../../api/types";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { FormAlert } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useNotify } from "../../ui/notify";
import { useAnalysisPreview, useSuggestAnalysis } from "./api";

/** Requested or created analysis task of an alert: a link to the YouTrack issue once it exists. */
export function AnalysisStatus({ task }: { task: AlertAnalysisTask }) {
  const { t } = useTranslation();
  if (task.issueId && task.issueUrl && /^https?:\/\//i.test(task.issueUrl)) {
    return (
      <a href={task.issueUrl} target="_blank" rel="noopener noreferrer">
        {t("alerts.analysisIssue", { issue: task.issueId })}
      </a>
    );
  }
  if (task.failure) {
    return <span className="muted">{t("alerts.analysisFailed", { project: task.youtrackProject, code: task.failure })}</span>;
  }
  return <span className="muted">{t("alerts.analysisPending", { project: task.youtrackProject })}</span>;
}

/**
 * Alert First (KIN-61): shows the issue drafted from the alert (secrets masked by the server) and creates it in
 * YouTrack only when the user confirms. A platform alert needs a target project bound to YouTrack.
 */
export function AnalysisDialog({ alert, onClose }: { alert: Alert; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const lang = (i18n.resolvedLanguage ?? i18n.language).startsWith("ru") ? "ru" : "en";
  const preview = useAnalysisPreview(alert.id, lang);
  const suggest = useSuggestAnalysis();
  const notify = useNotify();
  const selectId = useId();
  const [picked, setTarget] = useState<string>();
  const [error, setError] = useState<string>();
  const targets = preview.data?.targets ?? [];

  // The first target is preselected until the user picks one.
  const target = targets.some((x) => x.projectId === picked) ? picked : targets[0]?.projectId;

  const ready = preview.isSuccess && preview.data.configured && targets.length > 0 && target !== undefined;

  async function confirm() {
    setError(undefined);
    try {
      await suggest.mutateAsync({
        id: alert.id,
        language: lang,
        projectId: target,
      });
      notify(t("alerts.analysisDone"));
      onClose();
    } catch (err) {
      setError(errorMessage(t, err));
    }
  }

  let body;
  if (preview.isPending) {
    body = <p className="muted">{t("alerts.analysisLoading")}</p>;
  } else if (preview.isError) {
    body = <FormAlert message={errorMessage(t, preview.error)} />;
  } else {
    const d = preview.data;
    body = (
      <>
        {!d.configured ? (
          <FormAlert message={t("alerts.analysisNotConfigured")} />
        ) : targets.length === 0 ? (
          <FormAlert message={t("alerts.analysisNoTarget")} />
        ) : (
          <p>
            <label htmlFor={selectId}>{t("alerts.analysisTarget")}</label>{" "}
            <select id={selectId} value={target ?? ""} onChange={(e) => setTarget(e.target.value)}>
              {targets.map((x) => (
                <option key={x.projectId} value={x.projectId}>
                  {t("alerts.analysisTargetOption", {
                    project: x.projectName,
                    youtrack: x.youtrackProject,
                  })}
                </option>
              ))}
            </select>
          </p>
        )}
        <h3>{t("alerts.analysisSummary")}</h3>
        <p className="draft">{d.summary}</p>
        <h3>{t("alerts.analysisDescription")}</h3>
        <pre className="draft">{d.description}</pre>
        <FormAlert message={error} />
      </>
    );
  }

  return (
    <ConfirmDialog
      title={t("alerts.analysisTitle")}
      text={t("alerts.analysisText")}
      confirmLabel={t("alerts.analysisConfirm")}
      cancelLabel={t("alerts.analysisCancel")}
      tone="primary"
      busy={suggest.isPending}
      confirmDisabled={!ready}
      onConfirm={() => void confirm()}
      onCancel={onClose}
    >
      {body}
    </ConfirmDialog>
  );
}
