import { useTranslation } from "react-i18next";
import type { RunnerAvailability } from "../../api/types";
import "./runners.css";

/** Liveness from heartbeats as text (never colour alone). */
export function AvailabilityBadge({ availability }: { availability: RunnerAvailability }) {
  const { t } = useTranslation();
  return <span className={`badge availability-${availability}`}>{t(`runners.availability_${availability}`)}</span>;
}
