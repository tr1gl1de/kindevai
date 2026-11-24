import { useTranslation } from "react-i18next";
import type { ApprovalState, RiskLevel } from "../../api/types";

/** Risk level as text (never colour alone). */
export function RiskBadge({ risk }: { risk: RiskLevel }) {
  return <span className={`badge risk-${risk}`}>{risk}</span>;
}

export function StateBadge({ state }: { state: ApprovalState }) {
  const { t } = useTranslation();
  return <span className={`badge approval-${state}`}>{t(`approvals.state_${state}`)}</span>;
}
