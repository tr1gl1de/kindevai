import { useTranslation } from "react-i18next";
import type { ApprovalState, RiskLevel } from "../../api/types";
import { Badge, type Tone } from "../../ui/Badge";

const TONE: Record<ApprovalState, Tone> = { pending: "warning", approved: "success", rejected: "danger", superseded: "neutral" };

/** Risk level as text (never colour alone). */
export function RiskBadge({ risk }: { risk: RiskLevel }) {
  return <span className={`badge risk-${risk}`}>{risk}</span>;
}

export function StateBadge({ state }: { state: ApprovalState }) {
  const { t } = useTranslation();
  return (
    <Badge tone={TONE[state]} className={`approval-${state}`}>
      {t(`approvals.state_${state}`)}
    </Badge>
  );
}
