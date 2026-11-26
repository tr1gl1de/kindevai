import { useTranslation } from "react-i18next";
import type { RunnerAvailability } from "../../api/types";
import { Badge, type Tone } from "../../ui/Badge";
import "./runners.css";

const TONE: Record<RunnerAvailability, Tone> = { never_connected: "neutral", online: "success", unreachable: "warning" };

/** Liveness from heartbeats as text (never colour alone), shown apart from job status. */
export function AvailabilityBadge({ availability }: { availability: RunnerAvailability }) {
  const { t } = useTranslation();
  return (
    <Badge tone={TONE[availability]} className={`availability-${availability}`}>
      {t(`runners.availability_${availability}`)}
    </Badge>
  );
}
