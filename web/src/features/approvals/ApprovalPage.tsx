import { useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Link, useParams } from "react-router";
import { isApiError } from "../../api/client";
import type { Approval, ApprovalDetail, PlanChange, PlanRevision } from "../../api/types";
import { TextAreaField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import { useApproval, useDecide } from "./api";
import { RiskBadge, StateBadge } from "./badges";

/** One approval: why it is needed, the plan revision it binds to, what changed, and the Owner's decision. */
export function ApprovalPage() {
  const { t } = useTranslation();
  const { id = "" } = useParams();
  const q = useApproval(id);

  if (q.isPending) return <Loading />;
  if (isApiError(q.error, "not_found")) {
    return (
      <>
        <PageHeading>{t("approvals.notFound")}</PageHeading>
        <p>{t("approvals.notFoundText")}</p>
        <Link to="/approvals">{t("approvals.back")}</Link>
      </>
    );
  }
  if (q.isError) return <ErrorState message={errorMessage(t, q.error)} onRetry={() => void q.refetch()} />;
  return <ApprovalCard d={q.data} />;
}

const shortHash = (h: string) => h.slice(0, 12);

function ApprovalCard({ d }: { d: ApprovalDetail }) {
  const { t, i18n } = useTranslation();
  const a = d.approval;
  const others = d.related.filter((r) => r.id !== a.id);
  const raised = a.proposedRisk && a.proposedRisk !== a.risk;
  const waitsForVerification = a.scope === "integrate_dev" && a.state === "pending" && d.task.state !== "VERIFYING";

  return (
    <>
      <p className="breadcrumbs">
        <Link to="/approvals">{t("approvals.back")}</Link>
      </p>
      <div className="page-head">
        <PageHeading>{a.taskTitle}</PageHeading>
        <StateBadge state={a.state} />
      </div>

      <section aria-labelledby="approval-details" className="page-section">
        <h2 id="approval-details">{t("approvals.details")}</h2>
        <dl className="details">
          <dt>{t("approvals.project")}</dt>
          <dd>{a.projectName}</dd>
          <dt>{t("approvals.request")}</dt>
          <dd>{t(`approvals.scope_${a.scope}`)}</dd>
          <dt>{t("approvals.reason")}</dt>
          <dd>{t(`approvals.reason_${a.reason}`)}</dd>
          <dt>{t("approvals.taskState")}</dt>
          <dd>
            <code>{d.task.state}</code>
          </dd>
          <dt>{t("approvals.revision")}</dt>
          <dd>
            <code>{t("approvals.revisionValue", { number: a.planRevision, hash: shortHash(a.planHash) })}</code>
          </dd>
          <dt>{t("approvals.risk")}</dt>
          <dd>
            <RiskBadge risk={a.risk} />{" "}
            {raised ? t("approvals.riskRaised", { from: a.proposedRisk ?? "", to: a.risk }) : t("approvals.riskAsProposed")}
          </dd>
          <dt>{t("approvals.requested")}</dt>
          <dd>{formatDateTime(a.requestedAt, i18n.language)}</dd>
          {a.state !== "pending" && a.decidedAt && (
            <>
              <dt>{t("approvals.decision")}</dt>
              <dd>
                {a.approver
                  ? t("approvals.decidedBy", {
                      state: t(`approvals.state_${a.state}`),
                      approver: a.approver === "policy" ? t("approvals.policyApprover") : a.approver,
                      at: formatDateTime(a.decidedAt, i18n.language),
                    })
                  : `${t(`approvals.state_${a.state}`)} · ${formatDateTime(a.decidedAt, i18n.language)}`}
              </dd>
            </>
          )}
          {a.comment && (
            <>
              <dt>{t("approvals.comment")}</dt>
              <dd>{a.comment}</dd>
            </>
          )}
        </dl>
      </section>

      {a.reason === "scope_exceeded" && (
        <section aria-labelledby="approval-scope" className="card warn page-section">
          <h2 id="approval-scope">{t("approvals.scopeTitle")}</h2>
          <p>{t("approvals.scopeText", { step: a.stepKey ?? "" })}</p>
          <ul className="paths">
            {a.paths.map((p) => (
              <li key={p}>
                <code>{p}</code>
              </li>
            ))}
          </ul>
        </section>
      )}

      {a.findings.length > 0 && (
        <section aria-labelledby="approval-findings" className="page-section">
          <h2 id="approval-findings">{t("approvals.findingsTitle")}</h2>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">{t("approvals.colRule")}</th>
                  <th scope="col">{t("approvals.colRisk")}</th>
                  <th scope="col">{t("approvals.colPattern")}</th>
                  <th scope="col">{t("approvals.colPath")}</th>
                </tr>
              </thead>
              <tbody>
                {a.findings.map((f) => (
                  <tr key={`${f.category}|${f.repository}|${f.path}`}>
                    <td>{t(`approvals.category_${f.category}`)}</td>
                    <td>
                      <RiskBadge risk={f.risk} />
                    </td>
                    <td>
                      <code>{f.pattern}</code>
                    </td>
                    <td>
                      <code>{`${f.repository}:${f.path}`}</code>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      <Decision d={d} waitsForVerification={waitsForVerification} />

      {d.previous && <Diff previous={d.previous} changes={d.changes} />}
      <Plan revision={d.revision} />

      {others.length > 0 && (
        <section aria-labelledby="approval-related" className="page-section">
          <h2 id="approval-related">{t("approvals.relatedTitle")}</h2>
          <ul className="project-list">
            {others.map((r) => (
              <Related key={r.id} a={r} />
            ))}
          </ul>
        </section>
      )}
    </>
  );
}

function Related({ a }: { a: Approval }) {
  const { t } = useTranslation();
  return (
    <li>
      <Link className="project-link" to={`/approvals/${encodeURIComponent(a.id)}`}>
        {t(`approvals.scope_${a.scope}`)}
      </Link>
      <span>{t("approvals.revisionValue", { number: a.planRevision, hash: shortHash(a.planHash) })}</span>
      <StateBadge state={a.state} />
    </li>
  );
}

function Decision({ d, waitsForVerification }: { d: ApprovalDetail; waitsForVerification: boolean }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const a = d.approval;
  const decide = useDecide(a.id);
  const [comment, setComment] = useState("");
  const [fieldError, setFieldError] = useState<string>();
  const [error, setError] = useState<string>();
  const ref = useRef<HTMLTextAreaElement>(null);

  if (a.state !== "pending") return null;
  // Merging into dev is decided after verification: until then there is nothing to approve.
  if (waitsForVerification) return <p className="warning page-section">{t("approvals.integrateWait")}</p>;
  if (!d.canDecide) return <p className="muted page-section">{t("approvals.ownerOnly")}</p>;

  async function submit(approve: boolean, ev?: FormEvent) {
    ev?.preventDefault();
    setError(undefined);
    if (!approve && comment.trim() === "") {
      setFieldError(t("validation.comment_required"));
      setError(t("form.summary", { count: 1 }));
      ref.current?.focus();
      return;
    }
    setFieldError(undefined);
    try {
      await decide.mutateAsync({ approve, planRevision: a.planRevision, planHash: a.planHash, comment });
      notify(t(approve ? "approvals.approved" : "approvals.rejected"));
    } catch (err) {
      if (isApiError(err, "validation") && err.field === "comment") {
        setFieldError(validationMessage(t, err.detail) ?? t("errors.unknown"));
        setError(t("form.summary", { count: 1 }));
        ref.current?.focus();
        return;
      }
      setError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate className="card form-card page-section" aria-labelledby="decide-title" onSubmit={(e) => void submit(true, e)}>
      <h2 id="decide-title">{t("approvals.decideTitle")}</h2>
      <FormAlert message={error} />
      <TextAreaField
        ref={ref}
        label={t("approvals.comment")}
        hint={t("approvals.commentHint")}
        error={fieldError}
        rows={3}
        maxLength={2000}
        value={comment}
        onChange={(e) => {
          setComment(e.target.value);
          if (fieldError && e.target.value.trim()) setFieldError(undefined);
        }}
      />
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={decide.isPending}>
          {decide.isPending ? t("form.submitting") : t("approvals.approve", { number: a.planRevision })}
        </button>
        <button type="button" className="button danger" disabled={decide.isPending} onClick={() => void submit(false)}>
          {t("approvals.reject")}
        </button>
      </div>
    </form>
  );
}

function changeField(t: ReturnType<typeof useTranslation>["t"], c: PlanChange) {
  const field = t(`approvals.field_${c.field}` as "approvals.field_summary");
  return c.stepKey ? `${c.stepKey} · ${field}` : field;
}

function Diff({ previous, changes }: { previous: PlanRevision; changes: PlanChange[] }) {
  const { t } = useTranslation();
  const title = t("approvals.diffTitle", { number: previous.number });
  return (
    <section aria-label={title} className="page-section">
      <h2>{title}</h2>
      {changes.length === 0 ? (
        <p className="muted">{t("approvals.diffEmpty")}</p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">{t("approvals.colChange")}</th>
                <th scope="col">{t("approvals.colField")}</th>
                <th scope="col">{t("approvals.before")}</th>
                <th scope="col">{t("approvals.after")}</th>
              </tr>
            </thead>
            <tbody>
              {changes.map((c) => (
                <tr key={`${c.kind}|${c.stepKey ?? ""}|${c.field}`}>
                  <td>{t(`approvals.change_${c.kind}`)}</td>
                  <td>{changeField(t, c)}</td>
                  <td>{c.before ? <del>{c.before}</del> : "—"}</td>
                  <td>{c.after ? <ins>{c.after}</ins> : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function Plan({ revision: r }: { revision: PlanRevision }) {
  const { t } = useTranslation();
  return (
    <section aria-labelledby="approval-plan" className="page-section">
      <h2 id="approval-plan">{t("approvals.planTitle", { number: r.number })}</h2>
      <p>{r.plan.summary}</p>
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
                </td>
                <td>
                  <code>{s.repository}</code>
                </td>
                <td>
                  {s.scope.length ? (
                    <ul className="paths">
                      {s.scope.map((p) => (
                        <li key={p}>
                          <code>{p}</code>
                        </li>
                      ))}
                    </ul>
                  ) : (
                    "—"
                  )}
                </td>
                <td>{s.dependsOn.join(", ") || "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
