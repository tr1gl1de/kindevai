import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import { isApiError } from "../../api/client";
import type { Me } from "../../api/types";
import { TextField } from "../../ui/Field";
import { EmptyState, ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { useFieldErrors, type FieldErrors } from "../../ui/form";
import { useNotify } from "../../ui/notify";
import { slugify, useCreateProject, useProjects } from "./api";
import { RoleLabel } from "./RoleLabel";

export function ProjectsPage({ me }: { me: Me }) {
  const { t } = useTranslation();
  const projects = useProjects();
  const [creating, setCreating] = useState(false);
  const isOwner = me.role === "owner";

  return (
    <>
      <div className="page-head">
        <PageHeading>{t("projects.title")}</PageHeading>
        {isOwner && !creating && (
          <button type="button" className="button primary" onClick={() => setCreating(true)}>
            {t("projects.new")}
          </button>
        )}
      </div>
      {creating && <NewProjectForm onDone={() => setCreating(false)} />}
      {projects.isPending && <Loading />}
      {projects.isError && <ErrorState message={errorMessage(t, projects.error)} onRetry={() => void projects.refetch()} />}
      {projects.data?.length === 0 && !creating && (
        <EmptyState title={t("projects.emptyTitle")}>
          <p>{isOwner ? t("projects.emptyOwner") : t("projects.emptyMember")}</p>
        </EmptyState>
      )}
      {projects.data && projects.data.length > 0 && (
        <ul className="project-list">
          {projects.data.map((p) => (
            <li key={p.id} className="card">
              <Link to={`/projects/${encodeURIComponent(p.id)}`} className="project-link">
                {p.name}
              </Link>
              <span className="muted">{p.slug}</span>
              <RoleLabel role={p.myRole} />
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

const fields = ["name", "slug"] as const;
type F = (typeof fields)[number];

function NewProjectForm({ onDone }: { onDone: () => void }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const create = useCreateProject();
  const { errors, show, clear, register, count } = useFieldErrors(fields);
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [slugEdited, setSlugEdited] = useState(false);
  const [formError, setFormError] = useState<string>();

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const e: FieldErrors<F> = {};
    if (!name.trim()) e.name = t("validation.name_format");
    if (!/^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])$/.test(slug)) e.slug = t("validation.slug_format");
    if (Object.keys(e).length) {
      show(e);
      return;
    }
    try {
      const p = await create.mutateAsync({ name: name.trim(), slug });
      notify(t("projects.created", { name: p.name }));
      onDone();
    } catch (err) {
      if (isApiError(err, "slug_taken")) show({ slug: t("validation.slug_taken") });
      else if (isApiError(err, "validation") && (err.field === "name" || err.field === "slug"))
        show({ [err.field]: validationMessage(t, err.detail) ?? t("errors.unknown") });
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("projects.new")} className="card form-card">
      <FormAlert message={formError ?? (count ? t("form.summary", { count }) : undefined)} />
      <TextField
        ref={register("name")}
        label={t("projects.name")}
        error={errors.name}
        autoFocus
        value={name}
        onChange={(e) => {
          setName(e.target.value);
          clear("name");
          if (!slugEdited) setSlug(slugify(e.target.value));
        }}
      />
      <TextField
        ref={register("slug")}
        label={t("projects.slug")}
        hint={t("projects.slugHint")}
        error={errors.slug}
        autoCapitalize="none"
        spellCheck={false}
        value={slug}
        onChange={(e) => {
          setSlug(e.target.value.toLowerCase());
          setSlugEdited(true);
          clear("slug");
        }}
      />
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={create.isPending}>
          {create.isPending ? t("form.submitting") : t("projects.create")}
        </button>
        <button type="button" className="button" onClick={onDone}>
          {t("projects.cancel")}
        </button>
      </div>
    </form>
  );
}
