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
import {
  TASK_STATES,
  useProjectYouTrack,
  useRunIssue,
  useSaveAutoPlan,
  useSaveStateMap,
  useSaveTriggerTag,
  type IntakeEntry,
  type ProjectYouTrack,
  type TaskStateName,
} from "./intakeApi";

const knownReasons = ["unknown_user", "not_allowed", "tag_not_allowed", "issue_not_found", "other_project", "invalid_issue", "unavailable"];

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
          <AutoPlan project={project} on={data.data.binding.autoPlan} />
          <StateMap key={JSON.stringify(data.data.binding.states ?? {})} project={project} states={data.data.binding.states ?? {}} />
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

/** KIN-85: whether intake tasks go to the AI planner at once; the Owner and the project admin switch it. */
function AutoPlan({ project, on }: { project: Project; on: boolean }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const save = useSaveAutoPlan(project.id);
  const [formError, setFormError] = useState<string>();
  if (project.myRole !== "owner" && project.myRole !== "project_admin") {
    return <p className="muted">{on ? t("intake.autoPlanOn") : t("intake.autoPlanOff")}</p>;
  }

  async function toggle(next: boolean) {
    setFormError(undefined);
    try {
      await save.mutateAsync(next);
      notify(next ? t("intake.autoPlanEnabled") : t("intake.autoPlanDisabled"));
    } catch (err) {
      setFormError(errorMessage(t, err));
    }
  }

  return (
    <div className="card form-card inline-form">
      <FormAlert message={formError} />
      <label className="check">
        <input type="checkbox" checked={on} disabled={save.isPending} onChange={(e) => void toggle(e.target.checked)} />
        <span>{t("intake.autoPlan")}</span>
      </label>
      <p className="muted">{t("intake.autoPlanHint")}</p>
    </div>
  );
}

/** Mirrors the server's rule (KIN-38): a value becomes the YouTrack command "State {v}". */
const MAX_STATE_VALUE = 80;
function validStateValue(v: string): boolean {
  // eslint-disable-next-line no-control-regex
  return v.length <= MAX_STATE_VALUE && !/[{}\u0000-\u001f\u007f-\u009f]/.test(v);
}

/**
 * KIN-107: which YouTrack state a task's issue is moved to when the task enters a state. The Owner and the project
 * admin edit it; an empty field changes nothing (comments only, the default). Members read the mapped states.
 */
function StateMap({ project, states }: { project: Project; states: Record<string, string> }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const save = useSaveStateMap(project.id);
  const { errors, show, clear, register } = useFieldErrors(TASK_STATES);
  const [values, setValues] = useState<Record<string, string>>(() => ({ ...states }));
  const [formError, setFormError] = useState<string>();
  const label = (s: TaskStateName) => t(`tasks.state_${s}`);

  if (project.myRole !== "owner" && project.myRole !== "project_admin") {
    const mapped = TASK_STATES.filter((s) => states[s]);
    return (
      <section aria-labelledby="youtrack-states" className="card form-card inline-form">
        <h3 id="youtrack-states">{t("intake.statesTitle")}</h3>
        {mapped.length === 0 ? (
          <p className="muted">{t("intake.statesNone")}</p>
        ) : (
          <ul>
            {mapped.map((s) => (
              <li key={s}>{t("intake.stateMapped", { task: label(s), youtrack: states[s] ?? "" })}</li>
            ))}
          </ul>
        )}
      </section>
    );
  }

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const out: Record<string, string> = {};
    const bad: Partial<Record<TaskStateName, string>> = {};
    for (const s of TASK_STATES) {
      const v = (values[s] ?? "").trim();
      if (!v) continue;
      if (!validStateValue(v)) bad[s] = t("intake.stateValueError");
      out[s] = v;
    }
    if (Object.keys(bad).length > 0) {
      show(bad);
      return;
    }
    try {
      await save.mutateAsync(out);
      notify(t("intake.statesSaved"));
    } catch (err) {
      if (isApiError(err, "validation")) setFormError(t("intake.stateValueError"));
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate onSubmit={onSubmit} aria-labelledby="youtrack-states" className="card form-card">
      <h3 id="youtrack-states">{t("intake.statesTitle")}</h3>
      <p className="muted" id="youtrack-states-hint">
        {t("intake.statesHint")}
      </p>
      <FormAlert message={formError} />
      <div className="state-map">
        {TASK_STATES.map((s) => (
          <TextField
            key={s}
            ref={register(s)}
            label={label(s)}
            error={errors[s]}
            autoComplete="off"
            spellCheck={false}
            maxLength={MAX_STATE_VALUE * 2}
            value={values[s] ?? ""}
            onChange={(e) => {
              setValues((v) => ({ ...v, [s]: e.target.value }));
              clear(s);
            }}
          />
        ))}
      </div>
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={save.isPending}>
          {save.isPending ? t("form.submitting") : t("intake.saveStates")}
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
                  <td>
                    {/* Since KIN-50 the tag's applier, not the reporter, would be the initiator: no reporter fallback for tag rejections. */}
                    {e.initiator ?? (e.reporter && e.reason !== "tag_not_allowed" ? t("intake.reporterLabel", { login: e.reporter }) : "")}
                  </td>
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
