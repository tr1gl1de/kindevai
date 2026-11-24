import { useTranslation } from "react-i18next";
import type { RunnerStatus } from "../../api/types";

/** Status as text (never colour alone); the class only adds emphasis. */
export function StatusBadge({ status }: { status: RunnerStatus }) {
  const { t } = useTranslation();
  return <span className={`badge runner-${status}`}>{t(`runners.status_${status}`)}</span>;
}
