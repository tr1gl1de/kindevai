import type { TFunction } from "i18next";
import { useId, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link, useParams, useSearchParams } from "react-router";
import { isApiError } from "../../api/client";
import type { PlanChange, PlanRevision, PlanStep, RevisionDiff } from "../../api/types";
import { SelectField } from "../../ui/Field";
import { ErrorState, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { RiskBadge } from "../approvals/badges";
import { revisionPath, useRevisionDiff } from "./api";
import "./tasks.css";

const shortHash = (h: string) => h.slice(0, 12);

/**
 * Any plan revision of a task with what changed since the previous one, or since another revision chosen with
 * ?base= (KIN-97). The server computes the diff from the canonical plans; this page only lays it out.
 */
export function RevisionPage() {
  const { t } = useTranslation();
  const { id = "", n: nParam = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const n = Number.parseInt(nParam, 10);
  const base = Number.parseInt(params.get("base") ?? "", 10) || 0;
  const valid = Number.isInteger(n) && n > 0 && String(n) === nParam;
  const q = useRevisionDiff(id, valid ? n : 0, base);

  if (!valid || isApiError(q.error, "not_found")) {
    return (
      <>
        <PageHeading>{t("planRevision.notFound")}</PageHeading>
        <p>{t("planRevision.notFoundText")}</p>
        <Link to={`/tasks/${encodeURIComponent(id)}`}>{t("planRevision.backToTask")}</Link>
      </>
    );
  }
  if (q.isPending) return <Loading />;
  if (q.isError) return <ErrorState message={errorMessage(t, q.error)} onRetry={() => void q.refetch()} />;
  return (
    <RevisionView
      d={q.data}
      onBase={(m) => setParams(m === n - 1 ? {} : { base: String(m) }, { replace: true })}
    />
  );
}

function RevisionView({ d, onBase }: { d: RevisionDiff; onBase: (m: number) => void }) {
  const { t, i18n } = useTranslation();
  const r = d.revision;
  const latest = Math.max(d.task.planRevision, r.number);
  const others = Array.from({ length: latest }, (_, i) => i + 1).filter((m) => m !== r.number);
  return (
    <>
      <p className="breadcrumbs">
        <Link to={`/tasks/${encodeURIComponent(d.task.id)}`}>{d.task.title}</Link>
      </p>
      <PageHeading>{t("planRevision.title", { n: r.number })}</PageHeading>

      <nav aria-label={t("planRevision.navLabel")} className="revision-nav">
        {r.number > 1 && (
          <Link className="button" to={revisionPath(d.task.id, r.number - 1)}>
            <span aria-hidden="true">← </span>
            {t("planRevision.revisionN", { n: r.number - 1 })}
          </Link>
        )}
        {r.number < d.task.planRevision && (
          <Link className="button" to={revisionPath(d.task.id, r.number + 1)}>
            {t("planRevision.revisionN", { n: r.number + 1 })}
            <span aria-hidden="true"> →</span>
          </Link>
        )}
      </nav>

      <dl className="details">
        <dt>{t("planRevision.status")}</dt>
        <dd>
          {r.number === d.task.planRevision
            ? t("planRevision.current")
            : t("planRevision.replaced", { n: d.task.planRevision })}
        </dd>
        <dt>{t("planRevision.hash")}</dt>
        <dd>
          <code>{shortHash(r.hash)}</code>
        </dd>
        <dt>{t("planRevision.risk")}</dt>
        <dd>
          <RiskBadge risk={r.risk} />
        </dd>
        <dt>{t("planRevision.createdBy")}</dt>
        <dd>
          {r.createdBy} · {formatDateTime(r.createdAt, i18n.language)}
        </dd>
      </dl>

      {others.length > 0 && (
        <div className="revision-compare">
          <SelectField
            label={t("planRevision.compareWith")}
            value={d.previous ? String(d.previous.number) : ""}
            onChange={(e) => onBase(Number(e.target.value))}
          >
            {!d.previous && <option value="">{t("planRevision.choose")}</option>}
            {others.map((m) => (
              <option key={m} value={String(m)}>
                {t("planRevision.revisionN", { n: m })}
              </option>
            ))}
          </SelectField>
        </div>
      )}

      {d.previous ? (
        <Changes d={d} previous={d.previous} />
      ) : (
        <p className="muted page-section">{t("planRevision.first")}</p>
      )}
      <Steps revision={r} />
    </>
  );
}

const listFields = new Set(["tests", "contracts", "migrations", "docs", "scope", "dependsOn", "capabilities"]);

function fieldLabel(t: TFunction, field: string) {
  return t(`approvals.field_${field}` as "approvals.field_summary");
}

/** One changed field: removed and added items for a list, the old and new value otherwise; never colour alone. */
function ChangeValue({ c }: { c: PlanChange }) {
  const { t } = useTranslation();
  if (listFields.has(c.field)) {
    return (
      <ul className="diff-items">
        {c.removed.map((x) => (
          <li key={`-${x}`}>
            <span className="diff-sign">{t("planRevision.removed")}</span> <del>{x}</del>
          </li>
        ))}
        {c.added.map((x) => (
          <li key={`+${x}`}>
            <span className="diff-sign">{t("planRevision.added")}</span> <ins>{x}</ins>
          </li>
        ))}
      </ul>
    );
  }
  return (
    <ul className="diff-items">
      <li>
        <span className="diff-sign">{t("planRevision.was")}</span> {c.before ? <del>{c.before}</del> : "—"}
      </li>
      <li>
        <span className="diff-sign">{t("planRevision.now")}</span> {c.after ? <ins>{c.after}</ins> : "—"}
      </li>
    </ul>
  );
}

function ChangeList({ changes }: { changes: PlanChange[] }) {
  const { t } = useTranslation();
  return (
    <dl className="details diff-fields">
      {changes.map((c) => (
        <div key={c.field} className="diff-field">
          <dt>{fieldLabel(t, c.field)}</dt>
          <dd>
            <ChangeValue c={c} />
          </dd>
        </div>
      ))}
    </dl>
  );
}

function Group({ title, children }: { title: string; children: ReactNode }) {
  const id = useId();
  return (
    <section aria-labelledby={id} className="diff-group">
      <h3 id={id}>{title}</h3>
      {children}
    </section>
  );
}

function StepSummary({ s }: { s: PlanStep }) {
  const { t } = useTranslation();
  return (
    <li className="card diff-step">
      <p>
        <strong>{s.title}</strong> <code>{s.key}</code>
      </p>
      <dl className="details">
        <dt>{t("approvals.colRepository")}</dt>
        <dd>
          <code>{s.repository}</code>
        </dd>
        <dt>{t("approvals.colScope")}</dt>
        <dd>
          <Paths items={s.scope} />
        </dd>
        {s.dependsOn.length > 0 && (
          <>
            <dt>{t("approvals.colDepends")}</dt>
            <dd>{s.dependsOn.join(", ")}</dd>
          </>
        )}
      </dl>
    </li>
  );
}

function Paths({ items }: { items: string[] }) {
  if (items.length === 0) return <>—</>;
  return (
    <ul className="paths">
      {items.map((p) => (
        <li key={p}>
          <code>{p}</code>
        </li>
      ))}
    </ul>
  );
}

function Changes({ d, previous }: { d: RevisionDiff; previous: PlanRevision }) {
  const { t } = useTranslation();
  const titleId = useId();
  const title =
    previous.number < d.revision.number
      ? t("planRevision.changesSince", { n: previous.number })
      : t("planRevision.changesFrom", { n: previous.number });
  const plan = d.changes.filter((c) => !c.stepKey);
  const step = (steps: PlanStep[], key?: string) => steps.find((s) => s.key === key);
  const added = d.changes.filter((c) => c.kind === "added").flatMap((c) => step(d.revision.plan.steps, c.stepKey) ?? []);
  const removed = d.changes.filter((c) => c.kind === "removed").flatMap((c) => step(previous.plan.steps, c.stepKey) ?? []);
  const modified = new Map<string, PlanChange[]>();
  for (const c of d.changes) {
    if (c.kind === "modified" && c.stepKey) modified.set(c.stepKey, [...(modified.get(c.stepKey) ?? []), c]);
  }
  return (
    <section aria-labelledby={titleId} className="page-section">
      <h2 id={titleId}>{title}</h2>
      {d.changes.length === 0 && <p className="muted">{t("approvals.diffEmpty")}</p>}
      {plan.length > 0 && (
        <Group title={t("planRevision.groupPlan")}>
          <ChangeList changes={plan} />
        </Group>
      )}
      {added.length > 0 && (
        <Group title={t("planRevision.stepsAdded", { count: added.length })}>
          <ul className="diff-steps">
            {added.map((s) => (
              <StepSummary key={s.key} s={s} />
            ))}
          </ul>
        </Group>
      )}
      {removed.length > 0 && (
        <Group title={t("planRevision.stepsRemoved", { count: removed.length })}>
          <ul className="diff-steps">
            {removed.map((s) => (
              <StepSummary key={s.key} s={s} />
            ))}
          </ul>
        </Group>
      )}
      {modified.size > 0 && (
        <Group title={t("planRevision.stepsChanged", { count: modified.size })}>
          {[...modified].map(([key, cs]) => (
            <ChangedStep key={key} stepKey={key} title={step(d.revision.plan.steps, key)?.title ?? key} changes={cs} />
          ))}
        </Group>
      )}
    </section>
  );
}

function ChangedStep({ stepKey, title, changes }: { stepKey: string; title: string; changes: PlanChange[] }) {
  const id = useId();
  return (
    <article aria-labelledby={id} className="card diff-step">
      <h4 id={id}>
        {title} <code>{stepKey}</code>
      </h4>
      <ChangeList changes={changes} />
    </article>
  );
}

function Steps({ revision: r }: { revision: PlanRevision }) {
  const { t } = useTranslation();
  const titleId = useId();
  const impact = (["contracts", "migrations", "tests", "docs"] as const).filter((k) => r.plan[k].length > 0);
  return (
    <section aria-labelledby={titleId} className="page-section">
      <h2 id={titleId}>{t("planRevision.stepsTitle", { n: r.number })}</h2>
      <p className="plan-summary">{r.plan.summary}</p>
      {impact.length > 0 && (
        <dl className="details">
          {impact.map((k) => (
            <div key={k} className="diff-field">
              <dt>{t(`tasks.impact_${k}`)}</dt>
              <dd>
                <Paths items={r.plan[k]} />
              </dd>
            </div>
          ))}
        </dl>
      )}
      {r.plan.steps.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">{t("approvals.colStep")}</th>
                <th scope="col">{t("approvals.colRepository")}</th>
                <th scope="col">{t("approvals.colScope")}</th>
                <th scope="col">{t("approvals.colDepends")}</th>
              </tr>
            </thead>
            <tbody>
              {r.plan.steps.map((s) => (
                <tr key={s.key}>
                  <td>
                    {s.title} <code>{s.key}</code>
                    {s.role === "reviewer" && <> · {t("tasks.role_reviewer")}</>}
                  </td>
                  <td>
                    <code>{s.repository}</code>
                  </td>
                  <td>
                    <Paths items={s.scope} />
                  </td>
                  <td>{s.dependsOn.join(", ") || "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
