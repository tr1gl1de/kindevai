import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import type { TFunction } from "i18next";
import { isApiError } from "../../api/client";
import type { ConnectionCheck, YouTrackConnection } from "../../api/types";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { TextField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { useFieldErrors } from "../../ui/form";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import { useProjects } from "../projects/api";
import { IntakeSettingsSection } from "./IntakeSettingsSection";
import { useCheckYouTrack, useDisconnectYouTrack, useIntegrations, useSaveYouTrack } from "./api";
import { GitHubSettings } from "./GitHubSettings";

/** Message for a field-level validation code from the integrations API. */
export function fieldError(t: TFunction, code: string | undefined): string {
  if (code === "required") return t("form.required");
  return validationMessage(t, code) ?? t("errors.unknown");
}

/** Owner-only: the installation's YouTrack connection and where projects are linked. */
export function IntegrationsPage() {
  const { t } = useTranslation();
  const integrations = useIntegrations();
  return (
    <>
      <PageHeading>{t("integrations.title")}</PageHeading>
      <p className="muted">{t("integrations.intro")}</p>
      <section aria-labelledby="youtrack-title" className="section">
        <h2 id="youtrack-title">{t("integrations.youtrack")}</h2>
        {integrations.isPending && <Loading />}
        {integrations.isError && <ErrorState message={errorMessage(t, integrations.error)} onRetry={() => void integrations.refetch()} />}
        {integrations.isSuccess && (
          <YouTrackSettings key={integrations.data.youtrack?.updatedAt ?? "none"} connection={integrations.data.youtrack} />
        )}
      </section>
      {integrations.data?.youtrack && <IntakeSettingsSection />}
      <section aria-labelledby="github-title" className="section">
        <h2 id="github-title">{t("github.title")}</h2>
        {integrations.isSuccess && <GitHubSettings key={integrations.data.github?.updatedAt ?? "none"} app={integrations.data.github ?? null} />}
      </section>
      <ProjectLinks />
    </>
  );
}

function CheckResult({ check }: { check: ConnectionCheck }) {
  const { t, i18n } = useTranslation();
  let text: string;
  switch (check.status) {
    case "pending":
      text = t("integrations.checkPending");
      break;
    case "ok":
      text = t("integrations.checkOk", { login: check.detail ?? "" });
      break;
    case "failed": {
      const code = check.code ?? "unknown";
      const known = ["unauthorized", "not_found", "unreachable", "redirected", "unexpected_status", "bad_response", "bad_url", "timeout", "not_configured"];
      text = t(`integrations.check_${known.includes(code) ? code : "unknown"}` as "integrations.check_unknown");
      if (check.detail) text += ` (${check.detail})`;
      break;
    }
    default:
      text = t("integrations.checkNone");
  }
  return (
    <p className="check-result" aria-live="polite" aria-busy={check.status === "pending"}>
      <strong>{t(`integrations.checkLabel_${check.status}`)}</strong> {text}
      {check.checkedAt && check.status !== "pending" && (
        <span className="muted"> {t("integrations.checkedAt", { date: formatDateTime(check.checkedAt, i18n.language) })}</span>
      )}
    </p>
  );
}

const ytFields = ["baseUrl", "token"] as const;

function YouTrackSettings({ connection }: { connection: YouTrackConnection | null }) {
  const { t, i18n } = useTranslation();
  const notify = useNotify();
  const save = useSaveYouTrack();
  const check = useCheckYouTrack();
  const disconnect = useDisconnectYouTrack();
  const { errors, show, clear, register, count } = useFieldErrors(ytFields);
  const [baseUrl, setBaseUrl] = useState(connection?.baseUrl ?? "");
  const [token, setToken] = useState("");
  const [formError, setFormError] = useState<string>();
  const [confirm, setConfirm] = useState(false);

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const missing: Partial<Record<(typeof ytFields)[number], string>> = {};
    if (!baseUrl.trim()) missing.baseUrl = t("form.required");
    if (!connection && !token.trim()) missing.token = t("form.required");
    if (Object.keys(missing).length) {
      show(missing);
      return;
    }
    try {
      await save.mutateAsync({ baseUrl: baseUrl.trim(), token: token.trim() });
      setToken("");
      save.reset(); // drop the request (with the token) from the mutation cache
      notify(t("integrations.saved"));
    } catch (err) {
      save.reset();
      if (isApiError(err, "validation") && (err.field === "baseUrl" || err.field === "token")) show({ [err.field]: fieldError(t, err.detail) });
      else setFormError(errorMessage(t, err));
    }
  }

  async function runCheck() {
    setFormError(undefined);
    try {
      await check.mutateAsync();
    } catch (err) {
      setFormError(errorMessage(t, err));
    }
  }

  async function doDisconnect() {
    setFormError(undefined);
    try {
      await disconnect.mutateAsync();
      notify(t("integrations.disconnected"));
    } catch (err) {
      setFormError(errorMessage(t, err));
    } finally {
      setConfirm(false);
    }
  }

  const tokenHint = connection?.token.set
    ? t("integrations.tokenSet", { date: connection.token.updatedAt ? formatDateTime(connection.token.updatedAt, i18n.language) : "" })
    : t("integrations.tokenHint");

  return (
    <>
      {connection ? (
        <>
          <p>{t("integrations.connectedTo", { url: connection.baseUrl })}</p>
          <CheckResult check={connection.check} />
        </>
      ) : (
        <p>
          <strong>{t("integrations.notConnected")}</strong>
        </p>
      )}
      <form noValidate onSubmit={onSubmit} aria-label={t("integrations.youtrackForm")} className="card form-card">
        <FormAlert message={count ? t("form.summary", { count }) : formError} />
        <TextField
          ref={register("baseUrl")}
          label={t("integrations.baseUrl")}
          hint={t("integrations.baseUrlHint")}
          error={errors.baseUrl}
          type="url"
          inputMode="url"
          autoComplete="url"
          autoCapitalize="none"
          spellCheck={false}
          value={baseUrl}
          onChange={(e) => {
            setBaseUrl(e.target.value);
            clear("baseUrl");
          }}
        />
        <TextField
          ref={register("token")}
          label={t("integrations.token")}
          hint={tokenHint}
          keepHint
          error={errors.token}
          type="password"
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          value={token}
          onChange={(e) => {
            setToken(e.target.value);
            clear("token");
          }}
        />
        <div className="form-actions">
          <button type="submit" className="button primary" disabled={save.isPending}>
            {save.isPending ? t("form.submitting") : t("integrations.save")}
          </button>
          {connection && (
            <>
              <button
                type="button"
                className="button"
                onClick={() => void runCheck()}
                disabled={check.isPending || connection.check.status === "pending"}
              >
                {t("integrations.check")}
              </button>
              <button type="button" className="button subtle" onClick={() => setConfirm(true)}>
                {t("integrations.disconnect")}
              </button>
            </>
          )}
        </div>
      </form>
      {confirm && (
        <ConfirmDialog
          title={t("integrations.disconnectTitle")}
          text={t("integrations.disconnectText")}
          confirmLabel={t("integrations.disconnect")}
          cancelLabel={t("users.cancel")}
          busy={disconnect.isPending}
          onConfirm={() => void doDisconnect()}
          onCancel={() => setConfirm(false)}
        />
      )}
    </>
  );
}

/** Project bindings live on each project's page; list the projects to get there. */
function ProjectLinks() {
  const { t } = useTranslation();
  const projects = useProjects();
  return (
    <section aria-labelledby="integration-projects" className="section">
      <h2 id="integration-projects">{t("integrations.projectsTitle")}</h2>
      <p className="muted">{t("integrations.projectsHint")}</p>
      {projects.isPending && <Loading />}
      {projects.isError && <ErrorState message={errorMessage(t, projects.error)} onRetry={() => void projects.refetch()} />}
      {projects.data?.length === 0 && (
        <p>
          <Link to="/projects">{t("integrations.noProjects")}</Link>
        </p>
      )}
      {projects.data && projects.data.length > 0 && (
        <ul className="link-list">
          {projects.data.map((p) => (
            <li key={p.id}>
              <Link to={`/projects/${encodeURIComponent(p.id)}`}>{p.name}</Link>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
