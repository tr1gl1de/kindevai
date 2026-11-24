import { useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Link, useParams } from "react-router";
import { isApiError } from "../../api/client";
import type { Me, Project, ProjectRole } from "../../api/types";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { SelectField, TextField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { useFieldErrors } from "../../ui/form";
import { useNotify } from "../../ui/notify";
import { useMembers, useProject, useRemoveMember, useSetMember } from "./api";
import { ProjectIntegrationsPanel } from "../integrations/ProjectIntegrationsPanel";
import { ProjectYouTrackPanel } from "../integrations/ProjectYouTrackPanel";
import { RiskPolicyPanel } from "../approvals/RiskPolicyPanel";
import { NotFoundPage } from "./NotFoundPage";
import { RoleLabel } from "./RoleLabel";

export function ProjectPage({ me }: { me: Me }) {
  const { t } = useTranslation();
  const { id = "" } = useParams();
  const project = useProject(id);

  if (project.isPending) return <Loading />;
  if (isApiError(project.error, "not_found")) return <NotFoundPage />;
  if (project.isError) return <ErrorState message={errorMessage(t, project.error)} onRetry={() => void project.refetch()} />;
  const p = project.data;
  return (
    <>
      <p className="breadcrumbs">
        <Link to="/projects">{t("projects.back")}</Link>
      </p>
      <div className="page-head">
        <PageHeading>{p.name}</PageHeading>
        <RoleLabel role={p.myRole} />
      </div>
      <p className="muted">{p.slug}</p>
      <Members project={p} canManage={me.role === "owner"} />
      <ProjectYouTrackPanel project={p} isOwner={me.role === "owner"} />
      {me.role === "owner" && <ProjectIntegrationsPanel project={p} />}
      {(me.role === "owner" || p.myRole === "project_admin") && (
        <RiskPolicyPanel projectId={p.id} canEdit={me.role === "owner"} />
      )}
    </>
  );
}

function Members({ project, canManage }: { project: Project; canManage: boolean }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const members = useMembers(project.id, true);
  const remove = useRemoveMember(project.id);
  const [confirm, setConfirm] = useState<string | null>(null);
  const [removeError, setRemoveError] = useState<string>();
  const heading = useRef<HTMLHeadingElement>(null);

  async function doRemove(login: string) {
    setRemoveError(undefined);
    try {
      await remove.mutateAsync(login);
      notify(t("members.removed", { login, project: project.name }));
    } catch (err) {
      if (isApiError(err, "membership_not_found")) {
        // Someone else removed them already: the outcome the user wanted.
        await members.refetch();
        notify(t("members.removed", { login, project: project.name }));
      } else {
        setRemoveError(errorMessage(t, err));
      }
    } finally {
      setConfirm(null);
      // The row (and its button) is gone; keep keyboard focus in the section.
      queueMicrotask(() => heading.current?.focus());
    }
  }

  return (
    <section aria-labelledby="members-title" className="section">
      <h2 id="members-title" ref={heading} tabIndex={-1}>
        {t("members.title")}
      </h2>
      <FormAlert message={removeError} />
      {members.isPending && <Loading />}
      {members.isError && <ErrorState message={errorMessage(t, members.error)} onRetry={() => void members.refetch()} />}
      {members.data?.length === 0 && <p className="muted">{t("members.empty")}</p>}
      {members.data && members.data.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">{t("members.login")}</th>
                <th scope="col">{t("members.role")}</th>
                {canManage && <th scope="col">{t("members.actions")}</th>}
              </tr>
            </thead>
            <tbody>
              {members.data.map((m) => (
                <tr key={m.userId}>
                  <td>{m.login}</td>
                  <td>
                    <RoleLabel role={m.role} />
                  </td>
                  {canManage && (
                    <td>
                      <button
                        type="button"
                        className="button subtle"
                        aria-label={t("members.removeLabel", { login: m.login })}
                        onClick={() => setConfirm(m.login)}
                      >
                        {t("members.remove")}
                      </button>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {canManage && <AddMember project={project} />}
      {confirm && (
        <ConfirmDialog
          title={t("members.confirmTitle", { login: confirm, project: project.name })}
          text={t("members.confirmText")}
          confirmLabel={t("members.remove")}
          cancelLabel={t("members.cancel")}
          busy={remove.isPending}
          onConfirm={() => void doRemove(confirm)}
          onCancel={() => setConfirm(null)}
        />
      )}
    </section>
  );
}

const fields = ["login", "role"] as const;

function AddMember({ project }: { project: Project }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const setMember = useSetMember(project.id);
  const { errors, show, clear, register } = useFieldErrors(fields);
  const [login, setLogin] = useState("");
  const [role, setRole] = useState<ProjectRole>("member");
  const [formError, setFormError] = useState<string>();

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    if (!login.trim()) {
      show({ login: t("form.required") });
      return;
    }
    try {
      const m = await setMember.mutateAsync({ login: login.trim().toLowerCase(), role });
      notify(t("members.added", { login: m.login, project: project.name, role: t(`projects.role_${m.role}`) }));
      setLogin("");
    } catch (err) {
      if (isApiError(err, "user_not_found")) show({ login: t("validation.user_not_found") });
      else if (isApiError(err, "validation") && err.field === "login")
        show({ login: validationMessage(t, err.detail) ?? t("errors.unknown") });
      else if (isApiError(err, "validation") && err.field === "role") show({ role: t("validation.role_unknown") });
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("members.add")} className="card form-card inline-form">
      <FormAlert message={formError} />
      <TextField
        ref={register("login")}
        label={t("members.login")}
        error={errors.login}
        autoCapitalize="none"
        spellCheck={false}
        value={login}
        onChange={(e) => {
          setLogin(e.target.value);
          clear("login");
        }}
      />
      <SelectField
        ref={register("role")}
        label={t("members.role")}
        error={errors.role}
        value={role}
        onChange={(e) => setRole(e.target.value as ProjectRole)}
      >
        <option value="member">{t("projects.role_member")}</option>
        <option value="project_admin">{t("projects.role_project_admin")}</option>
      </SelectField>
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={setMember.isPending}>
          {setMember.isPending ? t("form.submitting") : t("members.add")}
        </button>
      </div>
    </form>
  );
}
