import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import type { Project } from "../../api/types";
import { TextField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useFieldErrors } from "../../ui/form";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import { fieldError } from "./IntegrationsPage";
import { StateText } from "./IntakeSettingsSection";
import { useProjectYouTrack, useRunIssue, useSaveTriggerTag, type IntakeEntry, type ProjectYouTrack } from "./intakeApi";

const knownReasons = ["unknown_user", "not_allowed", "issue_not_found", "other_project", "invalid_issue", "unavailable"];

/** The project's YouTrack intake for its members: status, "run by issue ID" and why recent issues did (not) become tasks. */
export function ProjectYouTrackPanel({ project, isOwner }: { project: Project; isOwner: boolean }) {
  const { t, i18n } = useTranslation();
  const data = useProjectYouTrack(project.id);
  return (
    <section aria-labelledby="project-youtrack" className="section">
      <h2 id="project-youtrack">{t("intake.projectTitle")}</h2>
      {data.isPending && <Loading />}
      {data.isError && <ErrorState message={errorMessage(t, data.error)} onRetry={() => void data.refetch()} />}
      {data.isSuccess && !data.data.connected && <p className="muted">{t("intake.notConnected")}</p>}
      {data.isSuccess && data.data.connected && !data.data.binding && <p className="muted">{t("intake.notBound")}</p>}
      {data.isSuccess && data.data.connected && data.data.binding && (
        <>
          <p>{t("intake.boundTo", { shortName: data.data.binding.shortName, tag: data.data.binding.triggerTag })}</p>
          <p aria-live="polite">
            {t("intake.statusTitle")}: <StateText state={data.data.status} />{" "}
            <span className="muted">
              {data.data.lastPolledAt
                ? t("intake.lastPolled", { date: formatDateTime(data.data.lastPolledAt, i18n.language) })
                : t("intake.neverPolled")}
            </span>
          </p>
          {isOwner && <TriggerTag key={data.data.binding.triggerTag} project={project} current={data.data.binding.triggerTag} />}
          <RunIssue project={project} shortName={data.data.binding.shortName} />
          <Recent data={data.data} />
        </>
      )}
    </section>
  );
}

function TriggerTag({ project, current }: { project: Project; current: string }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const save = useSaveTriggerTag(project.id);
  const { errors, show, clear, register } = useFieldErrors(["triggerTag"] as const);
  const [tag, setTag] = useState(current);
  const [formError, setFormError] = useState<string>();

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    try {
      await save.mutateAsync(tag.trim());
      notify(t("intake.tagSaved"));
    } catch (err) {
      if (isApiError(err, "validation")) show({ triggerTag: fieldError(t, err.detail) });
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("intake.triggerTitle")} className="card form-card inline-form">
      <FormAlert message={formError} />
      <TextField
        ref={register("triggerTag")}
        label={t("intake.triggerTag")}
        hint={t("intake.triggerTagHint")}
        error={errors.triggerTag}
        autoComplete="off"
        spellCheck={false}
        value={tag}
        onChange={(e) => {
          setTag(e.target.value);
          clear("triggerTag");
        }}
      />
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={save.isPending}>
          {save.isPending ? t("form.submitting") : t("intake.saveTag")}
        </button>
      </div>
    </form>
  );
}

function RunIssue({ project, shortName }: { project: Project; shortName: string }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const run = useRunIssue(project.id);
  const { errors, show, clear, register } = useFieldErrors(["issueId"] as const);
  const [issueId, setIssueId] = useState("");
  const [formError, setFormError] = useState<string>();

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    if (!issueId.trim()) {
      show({ issueId: t("form.required") });
      return;
    }
    try {
      const e = await run.mutateAsync(issueId.trim());
      notify(t("intake.runQueued", { issueId: e.issueId }));
      setIssueId("");
    } catch (err) {
      if (isApiError(err, "validation")) show({ issueId: fieldError(t, err.detail) });
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("intake.runTitle")} className="card form-card inline-form">
      <h3>{t("intake.runTitle")}</h3>
      <p className="muted">{t("intake.runHint")}</p>
      <FormAlert message={formError} />
      <TextField
        ref={register("issueId")}
        label={t("intake.issueId")}
        error={errors.issueId}
        placeholder={`${shortName}-1`}
        autoCapitalize="characters"
        autoComplete="off"
        spellCheck={false}
        value={issueId}
        onChange={(e) => {
          setIssueId(e.target.value);
          clear("issueId");
        }}
      />
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={run.isPending}>
          {run.isPending ? t("form.submitting") : t("intake.run")}
        </button>
      </div>
    </form>
  );
}

function Outcome({ entry }: { entry: IntakeEntry }) {
  const { t } = useTranslation();
  const label = t(`intake.outcome_${entry.outcome}`);
  if (entry.outcome !== "rejected") return <>{label}</>;
  const reason = entry.reason && knownReasons.includes(entry.reason) ? entry.reason : "other";
  return (
    <>
      <strong>{label}.</strong> {t(`intake.reason_${reason}` as "intake.reason_other")}
    </>
  );
}

function Recent({ data }: { data: ProjectYouTrack }) {
  const { t, i18n } = useTranslation();
  return (
    <>
      <h3>{t("intake.recentTitle")}</h3>
      {data.recent.length === 0 ? (
        <p className="muted">{t("intake.noRecent")}</p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">{t("intake.issue")}</th>
                <th scope="col">{t("intake.outcome")}</th>
                <th scope="col">{t("intake.initiator")}</th>
                <th scope="col">{t("intake.when")}</th>
              </tr>
            </thead>
            <tbody>
              {data.recent.map((e) => (
                <tr key={e.issueId}>
                  <td>{e.issueId}</td>
                  <td>
                    <Outcome entry={e} />
                  </td>
                  <td>{e.initiator ?? (e.reporter ? t("intake.reporterLabel", { login: e.reporter }) : "")}</td>
                  <td>
                    {formatDateTime(e.processedAt, i18n.language)}{" "}
                    <span className="muted">({t(`intake.channel_${e.channel}`)})</span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}
