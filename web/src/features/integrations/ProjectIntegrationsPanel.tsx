import { useEffect, useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import type { Project, ProjectRepository } from "../../api/types";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { TextField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useFieldErrors } from "../../ui/form";
import { useNotify } from "../../ui/notify";
import { useAddRepository, useBindYouTrackProject, useProjectIntegrations, useRemoveRepository, useUpdateRepository } from "./api";
import { fieldError } from "./IntegrationsPage";

/** Owner-only part of the project page: YouTrack project link and the repository registry. */
export function ProjectIntegrationsPanel({ project }: { project: Project }) {
  const { t } = useTranslation();
  const data = useProjectIntegrations(project.id);
  return (
    <section aria-labelledby="project-integrations" className="section">
      <h2 id="project-integrations">{t("integrations.title")}</h2>
      {data.isPending && <Loading />}
      {data.isError && <ErrorState message={errorMessage(t, data.error)} onRetry={() => void data.refetch()} />}
      {data.isSuccess && (
        <>
          <YouTrackBinding key={data.data.youtrackProject} project={project} current={data.data.youtrackProject} />
          <Repositories project={project} repositories={data.data.repositories} />
          <AddRepository project={project} />
        </>
      )}
    </section>
  );
}

function YouTrackBinding({ project, current }: { project: Project; current: string }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const bind = useBindYouTrackProject(project.id);
  const { errors, show, clear, register } = useFieldErrors(["shortName"] as const);
  const [shortName, setShortName] = useState(current);
  const [formError, setFormError] = useState<string>();

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    try {
      const pi = await bind.mutateAsync(shortName.trim());
      notify(
        pi.youtrackProject
          ? t("integrations.bound", { project: project.name, shortName: pi.youtrackProject })
          : t("integrations.unbound", { project: project.name }),
      );
    } catch (err) {
      if (isApiError(err, "short_name_taken")) show({ shortName: t("integrations.shortNameTaken") });
      else if (isApiError(err, "validation") && err.field === "shortName") show({ shortName: fieldError(t, err.detail) });
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("integrations.youtrackLinkForm")} className="card form-card inline-form">
      <FormAlert message={formError} />
      <TextField
        ref={register("shortName")}
        label={t("integrations.youtrackProject")}
        hint={t("integrations.youtrackProjectHint")}
        error={errors.shortName}
        autoCapitalize="characters"
        autoComplete="off"
        spellCheck={false}
        value={shortName}
        onChange={(e) => {
          setShortName(e.target.value);
          clear("shortName");
        }}
      />
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={bind.isPending}>
          {bind.isPending ? t("form.submitting") : t("integrations.saveYoutrackProject")}
        </button>
      </div>
    </form>
  );
}

function Repositories({ project, repositories }: { project: Project; repositories: ProjectRepository[] }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const remove = useRemoveRepository(project.id);
  const [confirm, setConfirm] = useState<ProjectRepository | null>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [error, setError] = useState<string>();
  const heading = useRef<HTMLHeadingElement>(null);

  async function doRemove(r: ProjectRepository) {
    setError(undefined);
    try {
      await remove.mutateAsync(r.id);
      notify(t("integrations.repoRemoved", { repo: r.fullName }));
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setConfirm(null);
      queueMicrotask(() => heading.current?.focus());
    }
  }

  return (
    <>
      <h3 ref={heading} tabIndex={-1}>
        {t("integrations.repositories")}
      </h3>
      <p className="muted">{t("integrations.repositoriesHint")}</p>
      <FormAlert message={error} />
      {repositories.length === 0 ? (
        <p className="muted">{t("integrations.noRepositories")}</p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">{t("integrations.repository")}</th>
                <th scope="col">{t("integrations.integrationBranch")}</th>
                <th scope="col">{t("integrations.protectedBranches")}</th>
                <th scope="col">
                  <span className="visually-hidden">{t("members.actions")}</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {repositories.map((r) => (
                <tr key={r.id}>
                  <td>{r.fullName}</td>
                  {editing === r.id ? (
                    <td colSpan={3}>
                      <BranchEditor project={project} repo={r} onDone={() => setEditing(null)} />
                    </td>
                  ) : (
                    <>
                      <td>{r.integrationBranch}</td>
                      <td>{r.protectedBranches.join(", ")}</td>
                      <td className="row-actions">
                        <button
                          type="button"
                          className="button subtle"
                          aria-label={t("integrations.changeBranchLabel", { repo: r.fullName })}
                          onClick={() => setEditing(r.id)}
                        >
                          {t("integrations.changeBranch")}
                        </button>
                        <button
                          type="button"
                          className="button subtle"
                          aria-label={t("integrations.removeRepoLabel", { repo: r.fullName })}
                          onClick={() => setConfirm(r)}
                        >
                          {t("members.remove")}
                        </button>
                      </td>
                    </>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {confirm && (
        <ConfirmDialog
          title={t("integrations.removeRepoTitle", { repo: confirm.fullName, project: project.name })}
          text={t("integrations.removeRepoText")}
          confirmLabel={t("members.remove")}
          cancelLabel={t("users.cancel")}
          busy={remove.isPending}
          onConfirm={() => void doRemove(confirm)}
          onCancel={() => setConfirm(null)}
        />
      )}
    </>
  );
}

function BranchEditor({ project, repo, onDone }: { project: Project; repo: ProjectRepository; onDone: () => void }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const update = useUpdateRepository(project.id);
  const { errors, show, clear, register } = useFieldErrors(["integrationBranch"] as const);
  const [branch, setBranch] = useState(repo.integrationBranch);
  const [formError, setFormError] = useState<string>();
  const input = useRef<HTMLInputElement | null>(null);
  // The editor replaces the button that opened it: keep keyboard focus in place.
  useEffect(() => input.current?.focus(), []);

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    if (!branch.trim()) {
      show({ integrationBranch: t("form.required") });
      return;
    }
    try {
      const r = await update.mutateAsync({ repoId: repo.id, integrationBranch: branch.trim() });
      notify(t("integrations.branchChanged", { repo: r.fullName, branch: r.integrationBranch }));
      onDone();
    } catch (err) {
      if (isApiError(err, "validation") && err.field === "integrationBranch") show({ integrationBranch: fieldError(t, err.detail) });
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("integrations.changeBranchLabel", { repo: repo.fullName })} className="inline-form">
      <FormAlert message={formError} />
      <TextField
        ref={(el) => {
          register("integrationBranch")(el);
          input.current = el;
        }}
        label={t("integrations.integrationBranch")}
        error={errors.integrationBranch}
        autoCapitalize="none"
        autoComplete="off"
        spellCheck={false}
        value={branch}
        onChange={(e) => {
          setBranch(e.target.value);
          clear("integrationBranch");
        }}
      />
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={update.isPending}>
          {update.isPending ? t("form.submitting") : t("integrations.saveBranch")}
        </button>
        <button type="button" className="button subtle" onClick={onDone}>
          {t("users.cancel")}
        </button>
      </div>
    </form>
  );
}

const repoFields = ["owner", "name", "integrationBranch"] as const;

function AddRepository({ project }: { project: Project }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const add = useAddRepository(project.id);
  const { errors, show, clear, register, count } = useFieldErrors(repoFields);
  const [values, setValues] = useState({ owner: "", name: "", integrationBranch: "dev" });
  const [formError, setFormError] = useState<string>();

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const missing: Partial<Record<(typeof repoFields)[number], string>> = {};
    if (!values.owner.trim()) missing.owner = t("form.required");
    if (!values.name.trim()) missing.name = t("form.required");
    if (Object.keys(missing).length) {
      show(missing);
      return;
    }
    try {
      const r = await add.mutateAsync({ owner: values.owner.trim(), name: values.name.trim(), integrationBranch: values.integrationBranch.trim() });
      notify(t("integrations.repoAdded", { repo: r.fullName }));
      setValues({ owner: "", name: "", integrationBranch: "dev" });
    } catch (err) {
      const f = isApiError(err) ? err.field : undefined;
      if (isApiError(err, "repository_exists")) show({ name: t("integrations.repoExists") });
      else if (isApiError(err, "validation") && (f === "owner" || f === "name" || f === "integrationBranch")) show({ [f]: fieldError(t, err.detail) });
      else setFormError(errorMessage(t, err));
    }
  }

  const field = (k: (typeof repoFields)[number]) => ({
    ref: register(k),
    error: errors[k],
    autoCapitalize: "none",
    autoComplete: "off",
    spellCheck: false,
    value: values[k],
    onChange: (e: React.ChangeEvent<HTMLInputElement>) => {
      setValues((v) => ({ ...v, [k]: e.target.value }));
      clear(k);
    },
  });

  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("integrations.addRepository")} className="card form-card inline-form">
      <FormAlert message={count ? t("form.summary", { count }) : formError} />
      <TextField label={t("integrations.owner")} hint={t("integrations.ownerHint")} {...field("owner")} />
      <TextField label={t("integrations.repository")} {...field("name")} />
      <TextField label={t("integrations.integrationBranch")} hint={t("integrations.integrationBranchHint")} {...field("integrationBranch")} />
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={add.isPending}>
          {add.isPending ? t("form.submitting") : t("integrations.addRepository")}
        </button>
      </div>
    </form>
  );
}
