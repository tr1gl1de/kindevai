import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import type { StorageForecast, StorageMachine, StorageVolume } from "../../api/types";
import { ErrorState } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatBytes, formatDateTime } from "../../ui/format";
import { useStorageOverview } from "./api";
import { AvailabilityBadge } from "./AvailabilityBadge";
import { OutputStorageSummary } from "./OutputStorage";
import "./runners.css";

/** Dashboard card (KIN-20, KB KIN-A-26): disk space of the Control Plane and every runner with a fill-up forecast. */
export function StorageOverviewCard() {
  const { t } = useTranslation();
  const q = useStorageOverview(true);
  const machines = q.data?.machines ?? [];
  return (
    <section className="card storage-card" aria-labelledby="ov-storage">
      <h2 id="ov-storage">{t("storage.title")}</h2>
      {q.isError ? (
        <ErrorState message={errorMessage(t, q.error)} onRetry={() => void q.refetch()} />
      ) : q.isPending ? (
        <p aria-busy="true">{t("app.loading")}</p>
      ) : (
        <>
          {!machines.some((m) => m.volumes.length > 0) ? (
            <p>{t("storage.empty")}</p>
          ) : (
            <ul className="machine-list">
              {machines.map((m) => (
                <MachineRow key={m.id} m={m} />
              ))}
            </ul>
          )}
          {q.data.runnerOutput && <OutputStorageSummary o={q.data.runnerOutput} />}
          <p>
            <Link to="/infrastructure/storage">{t("storage.openDetails")}</Link>
          </p>
        </>
      )}
    </section>
  );
}

function MachineRow({ m }: { m: StorageMachine }) {
  const { t } = useTranslation();
  const runner = m.kind === "runner";
  return (
    <li className="machine">
      <div className="machine-head">
        <h3>{runner ? <Link to={`/runners/${encodeURIComponent(m.id)}`}>{m.name}</Link> : t("storage.controlPlane")}</h3>
        {runner && <AvailabilityBadge availability={m.availability} />}
      </div>
      {runner && !m.diskHeavyAllowed && <p className="warning">{t("storage.diskHeavyPaused")}</p>}
      {m.volumes.length === 0 ? (
        <p className="muted">{t("storage.noReport")}</p>
      ) : (
        m.volumes.map((v) => <VolumeRow key={v.role} v={v} />)
      )}
    </li>
  );
}

function VolumeRow({ v }: { v: StorageVolume }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const role = t(`storage.role_${v.role}`);
  const used = Math.round(v.usedPercent);
  const f = trend(v);
  const when = (d: number | null): string => (d === 0 ? t("storage.reached") : t("storage.inDays", { count: d ?? 0 }));
  return (
    <div className={`volume level-${v.level}${v.stale ? " stale" : ""}`}>
      <p className="volume-head">
        <span className="volume-role">{role}</span>{" "}
        <code className="muted" title={v.path}>
          {v.mountOrVolumeId}
        </code>
      </p>
      <meter min={0} max={100} low={75} high={85} optimum={0} value={used} aria-label={t("storage.meterLabel", { role, percent: used })} />
      <p>
        <span className={`badge level-badge level-${v.level}`}>{t(`storage.level_${v.level}`)}</span>{" "}
        {t("storage.used", { percent: used })} · {t("storage.free", { free: formatBytes(v.availableBytes, lng), total: formatBytes(v.totalBytes, lng) })}
      </p>
      <p className="muted">
        {f.state === "insufficient_data"
          ? t("storage.forecastInsufficient")
          : f.state === "not_needed"
            ? t("storage.forecastNotNeeded")
            : t("storage.forecast", { at85: when(f.daysTo85), at90: when(f.daysTo90) })}
      </p>
      <p className={v.stale ? "warning" : "muted"}>
        {t(v.stale ? "storage.stale" : "storage.reported", { date: formatDateTime(v.lastReportedAt, lng) })}
      </p>
    </div>
  );
}

/** The 7-day trend, or the 30-day one when the last week has too few points. */
function trend(v: StorageVolume): StorageForecast {
  return v.forecast7d.state === "insufficient_data" ? v.forecast30d : v.forecast7d;
}
