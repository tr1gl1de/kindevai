import { useMemo, useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useSearchParams } from "react-router";
import type { AuditEvent, AuditFilter, Project } from "../../api/types";
import { SelectField, TextField } from "../../ui/Field";
import { EmptyState, ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useProjects } from "../projects/api";
import { useAuditEvents, useAuditKinds } from "./api";
import { isKnownKind, kindKey } from "./kinds";

/** Filter fields as kept in the URL (dates as YYYY-MM-DD in the viewer's time zone). */
interface Draft {
  actor: string;
  project: string;
  kind: string;
  from: string;
  to: string;
}

const FIELDS = ["actor", "project", "kind", "from", "to"] as const;

function readDraft(params: URLSearchParams): Draft {
  const d = { actor: "", project: "", kind: "", from: "", to: "" };
  for (const f of FIELDS) d[f] = params.get(f) ?? "";
  return d;
}

const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

/** Start of the local day as RFC 3339; `nextDay` gives the exclusive end of an inclusive "to" date. */
function dayStart(date: string, nextDay = false): string | undefined {
  if (!DATE_RE.test(date)) return undefined;
  const [y, m, d] = date.split("-").map(Number) as [number, number, number];
  const t = new Date(y, m - 1, d + (nextDay ? 1 : 0));
  return Number.isNaN(t.getTime()) ? undefined : t.toISOString();
}

function toFilter(d: Draft): AuditFilter {
  return {
    actor: d.actor.trim() || undefined,
    project: d.project || undefined,
    kind: d.kind || undefined,
    from: dayStart(d.from),
    to: dayStart(d.to, true),
  };
}

/** Owner-only, read-only view of the append-only audit log with filters and "load more" pagination. */
export function AuditPage() {
  const { t } = useTranslation();
  const [params, setParams] = useSearchParams();
  const applied = useMemo(() => readDraft(params), [params]);
  const filter = useMemo(() => toFilter(applied), [applied]);
  const events = useAuditEvents(filter);
  const projects = useProjects();
  const filtered = FIELDS.some((f) => applied[f] !== "");

  const projectNames = useMemo(() => new Map((projects.data ?? []).map((p) => [p.id, p.name])), [projects.data]);
  const rows = events.data?.pages.flatMap((p) => p.events) ?? [];

  return (
    <>
      <PageHeading>{t("audit.title")}</PageHeading>
      <p className="muted">{t("audit.intro")}</p>
      <Filters
        initial={applied}
        projects={projects.data ?? []}
        onApply={(d) => {
          const next = new URLSearchParams();
          for (const f of FIELDS) if (d[f].trim()) next.set(f, d[f].trim());
          setParams(next);
        }}
        onReset={filtered ? () => setParams(new URLSearchParams()) : undefined}
      />
      {events.isPending && <Loading />}
      {events.isError && <ErrorState message={errorMessage(t, events.error)} onRetry={() => void events.refetch()} />}
      {events.isSuccess && rows.length === 0 && (
        <EmptyState title={t("audit.emptyTitle")}>
          <p>{filtered ? t("audit.empty") : t("audit.emptyAll")}</p>
        </EmptyState>
      )}
      {rows.length > 0 && (
        <>
          <div className="table-wrap">
            <table aria-label={t("audit.title")}>
              <thead>
                <tr>
                  <th scope="col">{t("audit.time")}</th>
                  <th scope="col">{t("audit.actor")}</th>
                  <th scope="col">{t("audit.kind")}</th>
                  <th scope="col">{t("audit.project")}</th>
                  <th scope="col">{t("audit.details")}</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((e) => (
                  <EventRow key={e.id} event={e} projectName={e.projectId ? projectNames.get(e.projectId) : undefined} />
                ))}
              </tbody>
            </table>
          </div>
          <div className="list-footer">
            <p className="muted">
              {t("audit.shown", { count: rows.length })}
            </p>
            {events.hasNextPage && (
              <button type="button" className="button" disabled={events.isFetchingNextPage} onClick={() => void events.fetchNextPage()}>
                {events.isFetchingNextPage ? t("audit.loadingMore") : t("audit.loadMore")}
              </button>
            )}
          </div>
          {events.isFetchNextPageError && <ErrorState message={errorMessage(t, events.error)} onRetry={() => void events.fetchNextPage()} />}
        </>
      )}
    </>
  );
}

function Filters({
  initial,
  projects,
  onApply,
  onReset,
}: {
  initial: Draft;
  projects: Project[];
  onApply: (d: Draft) => void;
  onReset?: () => void;
}) {
  const { t } = useTranslation();
  const kinds = useAuditKinds();
  const [draft, setDraft] = useState(initial);
  const [toError, setToError] = useState<string>();
  // The URL is the applied filter: Back/Forward, a shared link or the nav link replace what the form shows.
  const [shown, setShown] = useState(initial);
  if (shown !== initial) {
    setShown(initial);
    setDraft(initial);
    setToError(undefined);
  }
  const toRef = useRef<HTMLInputElement>(null);
  const set = (f: keyof Draft) => (e: { target: { value: string } }) => {
    setDraft((d) => ({ ...d, [f]: e.target.value }));
    if (f === "from" || f === "to") setToError(undefined);
  };

  function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    if (draft.from && draft.to && draft.to < draft.from) {
      setToError(t("audit.toBeforeFrom"));
      toRef.current?.focus();
      return;
    }
    onApply(draft);
  }

  // A kind from the URL that is not (yet) in the list stays selectable.
  const kindOptions = [...new Set([...(kinds.data ?? []), ...(draft.kind ? [draft.kind] : [])])].sort();
  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("audit.filters")} className="card form-card filters">
      <FormAlert message={toError ? t("form.summary", { count: 1 }) : undefined} />
      <TextField
        label={t("audit.actor")}
        hint={t("audit.actorHint")}
        autoCapitalize="none"
        autoComplete="off"
        spellCheck={false}
        value={draft.actor}
        onChange={set("actor")}
      />
      <SelectField label={t("audit.project")} value={draft.project} onChange={set("project")}>
        <option value="">{t("audit.allProjects")}</option>
        {projects.map((p) => (
          <option key={p.id} value={p.id}>
            {p.name}
          </option>
        ))}
      </SelectField>
      <SelectField label={t("audit.kind")} value={draft.kind} onChange={set("kind")}>
        <option value="">{t("audit.allKinds")}</option>
        {kindOptions.map((k) => (
          <option key={k} value={k}>
            {kindLabel(t, k)}
          </option>
        ))}
      </SelectField>
      <TextField label={t("audit.from")} type="date" value={draft.from} max={draft.to || undefined} onChange={set("from")} />
      <TextField ref={toRef} label={t("audit.to")} type="date" value={draft.to} min={draft.from || undefined} error={toError} onChange={set("to")} />
      <div className="form-actions">
        <button type="submit" className="button primary">
          {t("audit.apply")}
        </button>
        {onReset && (
          <button
            type="button"
            className="button subtle"
            onClick={onReset}
          >
            {t("audit.reset")}
          </button>
        )}
      </div>
    </form>
  );
}

type T = ReturnType<typeof useTranslation>["t"];

/** Translated name of a kind; a kind without a label yet (added by a later task) shows its identifier. */
function kindLabel(t: T, kind: string): string {
  if (!isKnownKind(kind)) return kind;
  return t(`audit.kinds.${kindKey(kind)}` as "audit.kinds.user_created");
}

function actorLabel(t: T, e: AuditEvent): string {
  switch (e.actor.type) {
    case "system":
      // A failed sign-in has no authenticated actor.
      return e.actor.id === "anonymous" ? t("audit.actor_anonymous") : t("audit.actor_system");
    case "runner":
      return t("audit.actor_runner", { label: e.actor.label || e.actor.id });
    default:
      return e.actor.label || e.actor.id;
  }
}

function EventRow({ event: e, projectName }: { event: AuditEvent; projectName?: string }) {
  const { t, i18n } = useTranslation();
  const details = Object.entries(e.details ?? {}).sort(([a], [b]) => a.localeCompare(b));
  return (
    <tr>
      <td>
        <time dateTime={e.at}>{formatDateTime(e.at, i18n.language)}</time>
      </td>
      <td>{actorLabel(t, e)}</td>
      <td>
        {kindLabel(t, e.kind)}
        {isKnownKind(e.kind) && (
          <>
            {" "}
            <code className="muted">{e.kind}</code>
          </>
        )}
      </td>
      <td>{e.projectId ? (projectName ?? t("audit.deletedProject")) : t("audit.platform")}</td>
      <td>
        {e.subject && <code>{e.subject}</code>}
        {details.length > 0 && (
          <dl className="audit-details">
            {details.map(([k, v]) => (
              <div key={k}>
                <dt>{k}</dt>
                <dd>{v}</dd>
              </div>
            ))}
          </dl>
        )}
      </td>
    </tr>
  );
}
