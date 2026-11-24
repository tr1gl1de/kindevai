import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import { useState } from "react";
import type { Approval } from "../../api/types";
import { EmptyState, ErrorState, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useApprovals, type ApprovalFilter } from "./api";
import { RiskBadge, StateBadge } from "./badges";

const filters: ApprovalFilter[] = ["pending", "decided", "all"];

/** Approval Center list: the Owner sees every project; a Project Admin sees the projects they administer. */
export function ApprovalsPage() {
  const { t } = useTranslation();
  const [filter, setFilter] = useState<ApprovalFilter>("pending");
  const list = useApprovals(filter);

  return (
    <>
      <PageHeading>{t("approvals.title")}</PageHeading>
      <p className="muted">{t("approvals.intro")}</p>
      <fieldset className="filter-group">
        <legend className="muted">{t("approvals.filterLabel")}</legend>
        {filters.map((f) => (
          <button key={f} type="button" className="button" aria-pressed={f === filter} onClick={() => setFilter(f)}>
            {t(`approvals.filter_${f}`)}
          </button>
        ))}
      </fieldset>
      {list.isPending ? (
        <Loading />
      ) : list.isError ? (
        <ErrorState message={errorMessage(t, list.error)} onRetry={() => void list.refetch()} />
      ) : list.data.length === 0 ? (
        <EmptyState title={t(filter === "pending" ? "approvals.emptyPending" : "approvals.emptyOther")} />
      ) : (
        <ApprovalTable items={list.data} />
      )}
    </>
  );
}

function ApprovalTable({ items }: { items: Approval[] }) {
  const { t, i18n } = useTranslation();
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th scope="col">{t("approvals.colTask")}</th>
            <th scope="col">{t("approvals.colProject")}</th>
            <th scope="col">{t("approvals.colRequest")}</th>
            <th scope="col">{t("approvals.colRisk")}</th>
            <th scope="col">{t("approvals.colRevision")}</th>
            <th scope="col">{t("approvals.colRequested")}</th>
            <th scope="col">{t("approvals.colState")}</th>
          </tr>
        </thead>
        <tbody>
          {items.map((a) => (
            <tr key={a.id}>
              <td>
                <Link to={`/approvals/${encodeURIComponent(a.id)}`}>{a.taskTitle}</Link>
              </td>
              <td>{a.projectName}</td>
              <td>
                {t(`approvals.scope_${a.scope}`)}
                {a.reason === "scope_exceeded" && <div className="muted">{t("approvals.reason_scope_exceeded")}</div>}
              </td>
              <td>
                <RiskBadge risk={a.risk} />
              </td>
              <td>{a.planRevision}</td>
              <td>{formatDateTime(a.requestedAt, i18n.language)}</td>
              <td>
                <StateBadge state={a.state} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
