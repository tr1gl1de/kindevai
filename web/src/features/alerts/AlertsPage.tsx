import { useCallback, useState } from "react";
import { useTranslation } from "react-i18next";
import type { Alert, AlertAction } from "../../api/types";
import {
  EmptyState,
  ErrorState,
  FormAlert,
  Loading,
  PageHeading,
} from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import { AnalysisDialog, AnalysisStatus } from "./AnalysisDialog";
import { useAlertAction, useAlerts, type AlertFilter } from "./api";

const filters: AlertFilter[] = ["active", "all"];

/**
 * Alert Center: anomalies the platform found. The Owner sees platform alerts (runners, storage) and every project;
 * other users see their projects' alerts. Alert First: nothing acts on an alert automatically.
 */
export function AlertsPage() {
  const { t } = useTranslation();
  const [filter, setFilter] = useState<AlertFilter>("active");
  const list = useAlerts(filter);
  return (
    <>
      <PageHeading>{t("alerts.title")}</PageHeading>
      <p className="muted">{t("alerts.intro")}</p>
      <fieldset className="filter-group">
        <legend className="muted">{t("alerts.filterLabel")}</legend>
        {filters.map((f) => (
          <button
            key={f}
            type="button"
            className="button"
            aria-pressed={f === filter}
            onClick={() => setFilter(f)}
          >
            {t(`alerts.filter_${f}`)}
          </button>
        ))}
      </fieldset>
      {list.isPending ? (
        <Loading />
      ) : list.isError ? (
        <ErrorState
          message={errorMessage(t, list.error)}
          onRetry={() => void list.refetch()}
        />
      ) : list.data.length === 0 ? (
        <EmptyState
          title={t(
            filter === "active" ? "alerts.emptyActive" : "alerts.emptyAll",
          )}
        />
      ) : (
        <AlertTable items={list.data} />
      )}
    </>
  );
}

/** Human description of an alert, from its kind and the fields its monitor recorded. */
export function useAlertText() {
  const { t } = useTranslation();
  return (a: Alert) => {
    switch (a.kind) {
      case "runner_unreachable":
        return t("alerts.kind_runner_unreachable", { subject: a.subject });
      case "storage_level":
        return t("alerts.kind_storage_level", {
          subject: a.subject,
          usedPercent: a.details.usedPercent ?? "?",
        });
      case "egress_check_failed":
        return t("alerts.kind_egress_check_failed", {
          subject: a.subject,
          code: a.details.code ?? "?",
        });
      case "ci_failed":
        return t("alerts.kind_ci_failed", {
          subject: a.subject,
          conclusion: a.details.conclusion ?? "?",
        });
      case "integration_circuit_open":
        return t("alerts.kind_integration_circuit_open", {
          subject: a.subject,
        });
      case "task_blocked":
        return t("alerts.kind_task_blocked", {
          subject: a.subject,
          reason: a.details.reason ?? "?",
        });
      case "runner_output_full":
        return t("alerts.kind_runner_output_full");
      case "cli_held":
        return t("alerts.kind_cli_held", {
          agent: a.details.agent ?? "?",
          runnerId: a.details.runnerId ?? "?",
          cause: t(`agents.cause_${a.details.cause ?? "rate_limit"}`, {
            defaultValue: a.details.cause ?? "?",
          }),
        });
      default:
        return t("alerts.kind_other", { kind: a.kind, subject: a.subject });
    }
  };
}

const actions: AlertAction[] = ["acknowledge", "resolve", "dismiss"];

function AlertTable({ items }: { items: Alert[] }) {
  const { t, i18n } = useTranslation();
  const text = useAlertText();
  const act = useAlertAction();
  const notify = useNotify();
  const [error, setError] = useState<string>();
  const [analysis, setAnalysis] = useState<Alert>();
  const closeAnalysis = useCallback(() => setAnalysis(undefined), []);

  async function run(a: Alert, action: AlertAction) {
    setError(undefined);
    try {
      await act.mutateAsync({ id: a.id, action });
      notify(t(`alerts.done_${action}`));
    } catch (err) {
      setError(errorMessage(t, err));
    }
  }

  return (
    <>
      <FormAlert message={error} />
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th scope="col">{t("alerts.colSeverity")}</th>
              <th scope="col">{t("alerts.colAlert")}</th>
              <th scope="col">{t("alerts.colProject")}</th>
              <th scope="col">{t("alerts.colSeen")}</th>
              <th scope="col">{t("alerts.colState")}</th>
              <th scope="col">{t("alerts.colActions")}</th>
            </tr>
          </thead>
          <tbody>
            {items.map((a) => (
              <tr key={a.id} id={`alert-${a.id}`}>
                <td>
                  <span className={`badge level-badge level-${a.severity}`}>
                    {t(`alerts.severity_${a.severity}`)}
                  </span>
                </td>
                <td>
                  {text(a)}
                  {a.occurrences > 1 && (
                    <div className="muted">
                      {t("alerts.occurrences", { count: a.occurrences })}
                    </div>
                  )}
                </td>
                <td>{a.projectName ?? t("alerts.platform")}</td>
                <td>{formatDateTime(a.lastSeenAt, i18n.language)}</td>
                <td>
                  <span className="badge">{t(`alerts.state_${a.state}`)}</span>
                </td>
                <td>
                  {a.canManage || a.canSuggestAnalysis || a.analysisTask ? (
                    <div className="actions">
                      {a.canManage &&
                        actions
                          .filter(
                            (x) => x !== "acknowledge" || a.state === "OPEN",
                          )
                          .map((x) => (
                            <button
                              key={x}
                              type="button"
                              className="button subtle"
                              disabled={act.isPending}
                              aria-label={t(`alerts.action_${x}_label`, {
                                alert: text(a),
                              })}
                              onClick={() => void run(a, x)}
                            >
                              {t(`alerts.action_${x}`)}
                            </button>
                          ))}
                      {a.analysisTask ? (
                        <AnalysisStatus task={a.analysisTask} />
                      ) : (
                        a.canSuggestAnalysis && (
                          <button
                            type="button"
                            className="button subtle"
                            aria-label={t("alerts.action_analysis_label", {
                              alert: text(a),
                            })}
                            onClick={() => setAnalysis(a)}
                          >
                            {t("alerts.action_analysis")}
                          </button>
                        )
                      )}
                    </div>
                  ) : (
                    <span className="muted">—</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {analysis && <AnalysisDialog alert={analysis} onClose={closeAnalysis} />}
    </>
  );
}
