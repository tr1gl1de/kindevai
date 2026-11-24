import { useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Link, useParams } from "react-router";
import { isApiError } from "../../api/client";
import type { Runner } from "../../api/types";
import { useProjects } from "../projects/api";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { EmptyState, ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import { useRunner, useRunnerAction, type RunnerAction } from "./api";
import { AvailabilityBadge } from "./AvailabilityBadge";
import { StatusBadge } from "./StatusBadge";

/** Owner-only runner card: identity to verify, project permissions and lifecycle actions. */
export function RunnerPage() {
  const { t } = useTranslation();
  const { id = "" } = useParams();
  const runner = useRunner(id);

  if (runner.isPending) return <Loading />;
  if (isApiError(runner.error, "not_found")) {
    return (
      <>
        <PageHeading>{t("runners.notFound")}</PageHeading>
        <p>{t("runners.notFoundText")}</p>
        <Link to="/runners">{t("runners.back")}</Link>
      </>
    );
  }
  if (runner.isError) return <ErrorState message={errorMessage(t, runner.error)} onRetry={() => void runner.refetch()} />;
  return <RunnerCard runner={runner.data} />;
}

function RunnerCard({ runner: r }: { runner: Runner }) {
  const { t, i18n } = useTranslation();
  const notify = useNotify();
  const action = useRunnerAction(r.id);
  const [confirm, setConfirm] = useState<"disable" | "revoke" | null>(null);
  const [error, setError] = useState<string>();
  const date = (iso: string | null, empty: string) => (iso ? formatDateTime(iso, i18n.language) : empty);

  async function run(a: RunnerAction, done: string) {
    setError(undefined);
    try {
      await action.mutateAsync(a);
      notify(t(`runners.${done}` as "runners.approved", { name: r.hostname }));
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setConfirm(null);
    }
  }

  return (
    <>
      <p className="breadcrumbs">
        <Link to="/runners">{t("runners.back")}</Link>
      </p>
      <div className="page-head">
        <PageHeading>{r.hostname}</PageHeading>
        <StatusBadge status={r.status} />
        {r.status !== "pending" && r.status !== "revoked" && <AvailabilityBadge availability={r.availability} />}
      </div>
      {r.status === "approved" && r.availability === "unreachable" && <p className="warning">{t("runners.unreachableNotice")}</p>}
      {r.status === "revoked" && <p className="warning">{t("runners.revokedNotice")}</p>}
      {r.status === "disabled" && <p className="warning">{t("runners.disabledNotice")}</p>}
      <FormAlert message={error} />

      {r.status === "pending" && (
        <section className="card secret" aria-labelledby="verify-title">
          <h2 id="verify-title">{t("runners.verifyTitle")}</h2>
          <p>{t("runners.verifyText")}</p>
          <pre className="command">
            <code>ai-runner fingerprint</code>
          </pre>
          <p>
            <code className="secret-value fingerprint">{r.fingerprint}</code>
          </p>
        </section>
      )}

      <section aria-labelledby="details-title" className="page-section">
        <h2 id="details-title">{t("runners.details")}</h2>
        <dl className="details">
          <dt>{t("runners.fingerprint")}</dt>
          <dd>
            <code className="fingerprint">{r.fingerprint}</code>
          </dd>
          <dt>{t("runners.publicKey")}</dt>
          <dd>
            <code className="fingerprint">{r.publicKey}</code>
          </dd>
          <dt>{t("runners.platform")}</dt>
          <dd>{`${r.os}/${r.arch}`}</dd>
          <dt>{t("runners.version")}</dt>
          <dd>{r.version || t("runners.unknown")}</dd>
          <dt>{t("runners.enrolled")}</dt>
          <dd>{date(r.enrolledAt, "")}</dd>
          <dt>{t("runners.approvedAt")}</dt>
          <dd>{date(r.approvedAt, "—")}</dd>
          <dt>{t("runners.lastSeen")}</dt>
          <dd>{date(r.lastSeenAt, t("runners.never"))}</dd>
          <dt>{t("runners.lastHeartbeat")}</dt>
          <dd>{date(r.lastHeartbeatAt, t("runners.never"))}</dd>
        </dl>
      </section>

      {r.status !== "pending" && <CapabilitiesSection runner={r} />}

      {r.status !== "revoked" && (
        <ProjectsForm
          key={`${r.status}:${r.projects.map((p) => p.id).join(",")}`}
          runner={r}
          busy={action.isPending}
          onSubmit={(projectIds) =>
            r.status === "pending" ? run({ kind: "approve", projectIds }, "approved") : run({ kind: "projects", projectIds }, "projectsSaved")
          }
        />
      )}

      {r.status !== "revoked" && (
        <div className="form-actions page-section">
          {r.status === "approved" && (
            <button type="button" className="button" onClick={() => setConfirm("disable")}>
              {t("runners.disable")}
            </button>
          )}
          {r.status === "disabled" && (
            <button type="button" className="button" disabled={action.isPending} onClick={() => void run({ kind: "enable" }, "enabled")}>
              {t("runners.enable")}
            </button>
          )}
          <button type="button" className="button danger" onClick={() => setConfirm("revoke")}>
            {t("runners.revoke")}
          </button>
        </div>
      )}

      {confirm && (
        <ConfirmDialog
          title={t(confirm === "revoke" ? "runners.revokeTitle" : "runners.disableTitle", { name: r.hostname })}
          text={t(confirm === "revoke" ? "runners.revokeText" : "runners.disableText")}
          confirmLabel={t(confirm === "revoke" ? "runners.revoke" : "runners.disable")}
          cancelLabel={t("users.cancel")}
          busy={action.isPending}
          onConfirm={() => void run({ kind: confirm }, confirm === "revoke" ? "revoked" : "disabled")}
          onCancel={() => setConfirm(null)}
        />
      )}
    </>
  );
}

/** Explicit project list: approval of a pending runner, or a change of an approved/disabled one. */
function ProjectsForm({ runner, busy, onSubmit }: { runner: Runner; busy: boolean; onSubmit: (ids: string[]) => Promise<void> }) {
  const { t } = useTranslation();
  const projects = useProjects();
  const [chosen, setChosen] = useState<Set<string>>(() => new Set(runner.projects.map((p) => p.id)));
  const [error, setError] = useState<string>();
  const first = useRef<HTMLInputElement>(null);
  const pending = runner.status === "pending";

  if (projects.isPending) return <Loading />;
  if (projects.isError) return <ErrorState message={errorMessage(t, projects.error)} onRetry={() => void projects.refetch()} />;
  if (projects.data.length === 0) {
    return (
      <EmptyState title={t("runners.noProjectsYet")}>
        <Link className="button" to="/projects">
          {t("runners.toProjects")}
        </Link>
      </EmptyState>
    );
  }

  async function submit(ev: FormEvent) {
    ev.preventDefault();
    if (chosen.size === 0) {
      setError(t("runners.chooseProject"));
      first.current?.focus();
      return;
    }
    setError(undefined);
    try {
      await onSubmit([...chosen]);
    } catch (err) {
      setError(isApiError(err, "validation") ? (validationMessage(t, err.detail) ?? t("errors.unknown")) : errorMessage(t, err));
    }
  }

  const toggle = (id: string) => {
    setChosen((s) => {
      const n = new Set(s);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });
    setError(undefined);
  };

  return (
    <form noValidate onSubmit={(e) => void submit(e)} className="card form-card page-section" aria-labelledby="projects-legend">
      <fieldset aria-invalid={error ? true : undefined} aria-describedby="projects-hint projects-error">
        <legend id="projects-legend">{t("runners.projectsLegend")}</legend>
        <p id="projects-hint" className="field-hint">
          {t("runners.projectsHint")}
        </p>
        {projects.data.map((p, i) => (
          <label key={p.id} className="check">
            <input ref={i === 0 ? first : undefined} type="checkbox" checked={chosen.has(p.id)} onChange={() => toggle(p.id)} />
            <span>{p.name}</span>
          </label>
        ))}
        <p id="projects-error" className="field-error" role="alert" hidden={!error}>
          {error}
        </p>
      </fieldset>
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={busy}>
          {busy ? t("form.submitting") : pending ? t("runners.approve") : t("runners.saveProjects")}
        </button>
      </div>
    </form>
  );
}

/** What the runner reported about itself (KIN-20): self-reported and informational, it grants nothing. */
function CapabilitiesSection({ runner }: { runner: Runner }) {
  const { t, i18n } = useTranslation();
  const c = runner.capabilities;
  return (
    <section aria-labelledby="caps-title" className="page-section">
      <h2 id="caps-title">{t("runners.capabilities")}</h2>
      {!c ? (
        <p className="muted">{t("runners.capabilitiesNone")}</p>
      ) : (
        <>
          <p className="muted">{t("runners.capabilitiesHint", { date: formatDateTime(c.reportedAt, i18n.language) })}</p>
          <dl className="details">
            <dt>{t("runners.platform")}</dt>
            <dd>{`${c.os}/${c.arch}`}</dd>
            <dt>{t("runners.wsl")}</dt>
            <dd>{t(c.wsl ? "runners.yes" : "runners.no")}</dd>
            <dt>{t("runners.strictEgress")}</dt>
            <dd>{t(c.strictEgress ? "runners.supported" : "runners.notSupported")}</dd>
            {KNOWN_TOOLS.map((name) => {
              const tool = c.tools.find((x) => x.name === name);
              return (
                <div key={name} className="details-row">
                  <dt>
                    <code>{name}</code>
                  </dt>
                  <dd>{tool ? tool.version || t("runners.versionUnknown") : <span className="muted">{t("runners.toolMissing")}</span>}</dd>
                </div>
              );
            })}
          </dl>
        </>
      )}
    </section>
  );
}

const KNOWN_TOOLS = ["claude", "codex", "git", "docker"];
