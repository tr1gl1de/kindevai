import { useEffect, useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import { isApiError } from "../../api/client";
import type { Invitation, ProjectRole } from "../../api/types";
import { useProjects } from "../projects/api";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { CopyButton } from "../../ui/CopyButton";
import { SelectField } from "../../ui/Field";
import { EmptyState, ErrorState, FormAlert, Loading } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useFieldErrors } from "../../ui/form";
import { useNotify } from "../../ui/notify";
import { invitationLink, useCreateInvitation, useInvitations, useRevokeInvitation } from "./invitationsApi";

const ttlOptions = [24, 72, 168] as const;

/** Owner-only: one-time invitation links that let someone create their own account with a project role. */
export function InvitationsSection() {
  const { t, i18n } = useTranslation();
  const notify = useNotify();
  const invitations = useInvitations();
  const revoke = useRevokeInvitation();
  const [link, setLink] = useState<{ url: string; project: string; expiresAt: string } | null>(null);
  const [confirm, setConfirm] = useState<Invitation | null>(null);
  const [actionError, setActionError] = useState<string>();

  async function runRevoke(inv: Invitation) {
    setActionError(undefined);
    try {
      await revoke.mutateAsync(inv.id);
      notify(t("invitations.revokedDone", { project: projectName(inv) }));
    } catch (err) {
      setActionError(errorMessage(t, err));
    } finally {
      setConfirm(null);
    }
  }

  const projectName = (inv: Invitation) => inv.projectName || t("invitations.deletedProject");

  return (
    <section aria-labelledby="invitations-heading" className="page-section">
      <h2 id="invitations-heading">{t("invitations.title")}</h2>
      <p className="muted">{t("invitations.intro")}</p>
      {link && <InvitationLink key={link.url} {...link} onDone={() => setLink(null)} />}
      <NewInvitation blocked={link !== null} onCreated={setLink} />
      <FormAlert message={actionError} />
      {invitations.isPending && <Loading />}
      {invitations.isError && <ErrorState message={errorMessage(t, invitations.error)} onRetry={() => void invitations.refetch()} />}
      {invitations.data?.length === 0 && <EmptyState title={t("invitations.empty")} mood="none" />}
      {invitations.data && invitations.data.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">{t("invitations.project")}</th>
                <th scope="col">{t("invitations.role")}</th>
                <th scope="col">{t("invitations.state")}</th>
                <th scope="col">{t("invitations.expires")}</th>
                <th scope="col">{t("invitations.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {invitations.data.map((inv) => (
                <tr key={inv.id}>
                  <td>{projectName(inv)}</td>
                  <td>{t(`projects.role_${inv.role}`)}</td>
                  <td>{t(`invitations.state_${inv.state}`)}</td>
                  <td>{formatDateTime(inv.expiresAt, i18n.language)}</td>
                  <td className="row-actions">
                    {inv.state === "pending" && (
                      <button
                        type="button"
                        className="button subtle"
                        aria-label={t("invitations.revokeLabel", { project: projectName(inv) })}
                        onClick={() => setConfirm(inv)}
                      >
                        {t("invitations.revoke")}
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {confirm && (
        <ConfirmDialog
          title={t("invitations.revokeTitle", { project: projectName(confirm) })}
          text={t("invitations.revokeText")}
          confirmLabel={t("invitations.revoke")}
          cancelLabel={t("users.cancel")}
          busy={revoke.isPending}
          onConfirm={() => void runRevoke(confirm)}
          onCancel={() => setConfirm(null)}
        />
      )}
    </section>
  );
}

function NewInvitation({ blocked, onCreated }: { blocked: boolean; onCreated: (l: { url: string; project: string; expiresAt: string }) => void }) {
  const { t } = useTranslation();
  const projects = useProjects();
  const create = useCreateInvitation();
  const { errors, show, clear, register } = useFieldErrors(["projectId", "role"] as const);
  const [projectId, setProjectId] = useState("");
  const [role, setRole] = useState<ProjectRole>("member");
  const [ttl, setTtl] = useState<number>(72);
  const [formError, setFormError] = useState<string>();

  if (projects.isPending) return <Loading />;
  if (projects.isError) return <ErrorState message={errorMessage(t, projects.error)} onRetry={() => void projects.refetch()} />;
  if (projects.data.length === 0) {
    return (
      <EmptyState title={t("invitations.noProjects")} mood="none">
        <Link className="button" to="/projects">
          {t("invitations.toProjects")}
        </Link>
      </EmptyState>
    );
  }

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    if (!projectId) {
      show({ projectId: t("invitations.chooseProject") });
      return;
    }
    try {
      const r = await create.mutateAsync({ projectId, role, ttlHours: ttl });
      create.reset(); // drop the response (with the token) from the mutation
      onCreated({ url: invitationLink(r.token), project: r.invitation.projectName, expiresAt: r.invitation.expiresAt });
    } catch (err) {
      if (isApiError(err, "validation") && err.field === "projectId") show({ projectId: validationMessage(t, err.detail) ?? t("errors.unknown") });
      else if (isApiError(err, "validation")) show({ role: validationMessage(t, err.detail) ?? t("errors.unknown") });
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("invitations.new")} className="card form-card inline-form">
      <FormAlert message={formError} />
      <SelectField
        ref={register("projectId")}
        label={t("invitations.project")}
        error={errors.projectId}
        value={projectId}
        onChange={(e) => {
          setProjectId(e.target.value);
          clear("projectId");
        }}
      >
        <option value="">{t("invitations.chooseProject")}</option>
        {projects.data.map((p) => (
          <option key={p.id} value={p.id}>
            {p.name}
          </option>
        ))}
      </SelectField>
      <SelectField ref={register("role")} label={t("invitations.role")} error={errors.role} value={role} onChange={(e) => setRole(e.target.value as ProjectRole)}>
        <option value="member">{t("projects.role_member")}</option>
        <option value="project_admin">{t("projects.role_project_admin")}</option>
      </SelectField>
      <SelectField label={t("invitations.validFor")} value={ttl} onChange={(e) => setTtl(Number(e.target.value))}>
        {ttlOptions.map((h) => (
          <option key={h} value={h}>
            {t("invitations.ttl", { count: h / 24 })}
          </option>
        ))}
      </SelectField>
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={create.isPending || blocked}>
          {create.isPending ? t("form.submitting") : t("invitations.create")}
        </button>
      </div>
    </form>
  );
}

/** One-time display of the link; the token cannot be shown again. */
function InvitationLink({ url, project, expiresAt, onDone }: { url: string; project: string; expiresAt: string; onDone: () => void }) {
  const { t, i18n } = useTranslation();
  const title = t("invitations.linkTitle", { project });
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => heading.current?.focus(), []);
  return (
    <section className="card secret" aria-label={title}>
      <h3 ref={heading} tabIndex={-1}>
        {title}
      </h3>
      <p>{t("invitations.linkText", { date: formatDateTime(expiresAt, i18n.language) })}</p>
      <p>
        <code className="secret-value link">{url}</code>
      </p>
      <div className="form-actions">
        <CopyButton text={url} label={t("invitations.copyLink")} failedHint={t("invitations.copyFailed")} />
        <button type="button" className="button primary" onClick={onDone}>
          {t("users.done")}
        </button>
      </div>
    </section>
  );
}
