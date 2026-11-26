import { useTranslation } from "react-i18next";
import type { RunnerStatus } from "../../api/types";
import { Badge, type Tone } from "../../ui/Badge";

const TONE: Record<RunnerStatus, Tone> = { pending: "warning", approved: "success", disabled: "neutral", revoked: "neutral" };

/** Enrollment status as text (never colour alone); a pending runner awaits the Owner's approval. */
export function StatusBadge({ status }: { status: RunnerStatus }) {
  const { t } = useTranslation();
  return <Badge tone={TONE[status]}>{t(`runners.status_${status}`)}</Badge>;
}
