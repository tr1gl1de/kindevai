import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import type { SpaceConsumer, StorageForecast, StorageMachine, StorageMachineDetail, StorageVolume } from "../../api/types";
import { EmptyState, ErrorState, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatBytes, formatDateTime } from "../../ui/format";
import { useStorageMachine, useStorageOverview } from "./api";
import { AvailabilityBadge } from "./AvailabilityBadge";
import { HistoryChart } from "./HistoryChart";

type Days = 7 | 30;

/** Infrastructure → Storage (KIN-55, KB KIN-A-26): per machine the fill history, all volumes, forecasts, consumers. */
export function StoragePage() {
  const { t } = useTranslation();
  const q = useStorageOverview(true);
  const [days, setDays] = useState<Days>(7);
  const machines = q.data?.machines ?? [];
  return (
    <>
      <PageHeading>{t("storagePage.title")}</PageHeading>
      <p className="muted">{t("storagePage.intro")}</p>
      {q.isError ? (
        <ErrorState message={errorMessage(t, q.error)} onRetry={() => void q.refetch()} />
      ) : q.isPending ? (
        <Loading />
      ) : !machines.some((m) => m.volumes.length > 0) ? (
        <EmptyState title={t("storagePage.emptyTitle")}>
          <p>{t("storage.empty")}</p>
          <Link to="/runners">{t("storagePage.openRunners")}</Link>
        </EmptyState>
      ) : (
        <>
          <fieldset className="segmented">
            <legend>{t("storagePage.window")}</legend>
            {([7, 30] as const).map((d) => (
              <label key={d}>
                <input type="radio" name="storage-days" value={d} checked={days === d} onChange={() => setDays(d)} />
                {t("storagePage.days", { count: d })}
              </label>
            ))}
          </fieldset>
          {machines.map((m) => (
            <MachineSection key={m.id} m={m} days={days} />
          ))}
        </>
      )}
    </>
  );
}

function MachineSection({ m, days }: { m: StorageMachine; days: Days }) {
  const { t } = useTranslation();
  const runner = m.kind === "runner";
  const name = runner ? m.name : t("storage.controlPlane");
  const headingId = `storage-${m.id}`;
  const q = useStorageMachine(m.id, days);
  return (
    <section className="card storage-machine" id={`machine-${m.id}`} aria-labelledby={headingId}>
      <div className="machine-head">
        <h2 id={headingId}>{name}</h2>
        {runner && <AvailabilityBadge availability={m.availability} />}
        {runner && <Link to={`/runners/${encodeURIComponent(m.id)}`}>{t("storagePage.runnerDetails")}</Link>}
      </div>
      {runner && !m.diskHeavyAllowed && <p className="warning">{t("storage.diskHeavyPaused")}</p>}
      {q.isError ? (
        <ErrorState message={errorMessage(t, q.error)} onRetry={() => void q.refetch()} />
      ) : !q.data ? (
        <Loading />
      ) : (
        <div aria-busy={q.isPlaceholderData}>
          {q.data.volumes.length === 0 ? (
            <p className="muted">{t("storage.noReport")}</p>
          ) : (
            q.data.volumes.map((v) => <VolumeDetail key={v.role} v={v} d={q.data} />)
          )}
          {runner && <Consumers c={q.data.consumers} />}
        </div>
      )}
    </section>
  );
}

function VolumeDetail({ v, d }: { v: StorageVolume; d: StorageMachineDetail }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const role = t(`storage.role_${v.role}`);
  const used = Math.round(v.usedPercent);
  const samples = d.history[v.role] ?? [];
  return (
    <section className={`volume-detail level-${v.level}${v.stale ? " stale" : ""}`} aria-label={role}>
      <h3>
        {role} <code className="muted">{v.mountOrVolumeId}</code>
      </h3>
      <meter min={0} max={100} low={75} high={85} optimum={0} value={used} aria-label={t("storage.meterLabel", { role, percent: used })} />
      <p>
        <span className={`badge level-badge level-${v.level}`}>{t(`storage.level_${v.level}`)}</span>{" "}
        <span>{t("storage.used", { percent: used })}</span> ·{" "}
        {t("storage.free", { free: formatBytes(v.availableBytes, lng), total: formatBytes(v.totalBytes, lng) })}
      </p>
      <p className="muted small">
        {t("storagePage.path")}: <code>{v.path}</code>
      </p>
      <p className={v.stale ? "warning" : "muted"}>
        {t(v.stale ? "storage.stale" : "storage.reported", { date: formatDateTime(v.lastReportedAt, lng) })}
      </p>
      <ForecastTable v={v} />
      {samples.length === 0 ? (
        <p className="muted">{t("storagePage.noHistory")}</p>
      ) : (
        <HistoryChart samples={samples} days={d.days} role={role} />
      )}
    </section>
  );
}

function ForecastTable({ v }: { v: StorageVolume }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const when = (n: number | null) => (n === 0 ? t("storage.reached") : t("storage.inDays", { count: n ?? 0 }));
  const row = (label: string, f: StorageForecast) => (
    <tr key={label}>
      <th scope="row">{label}</th>
      {f.state === "ok" ? (
        <>
          <td>{t("storagePage.growth", { size: `${f.growthBytesPerDay < 0 ? "−" : "+"}${formatBytes(Math.abs(f.growthBytesPerDay), lng)}` })}</td>
          <td>{when(f.daysTo85)}</td>
          <td>{when(f.daysTo90)}</td>
        </>
      ) : (
        <td colSpan={3}>{t(f.state === "not_needed" ? "storage.forecastNotNeeded" : "storage.forecastInsufficient")}</td>
      )}
    </tr>
  );
  return (
    <div className="table-wrap">
      <table className="compact">
        <caption>{t("storagePage.forecastTitle")}</caption>
        <thead>
          <tr>
            <th scope="col">{t("storagePage.colWindow")}</th>
            <th scope="col">{t("storagePage.colGrowth")}</th>
            <th scope="col">85%</th>
            <th scope="col">90%</th>
          </tr>
        </thead>
        <tbody>
          {row(t("storagePage.window7"), v.forecast7d)}
          {row(t("storagePage.window30"), v.forecast30d)}
        </tbody>
      </table>
    </div>
  );
}

function Consumers({ c }: { c: StorageMachineDetail["consumers"] }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  if (!c) {
    return (
      <div className="consumers">
        <h3>{t("storagePage.consumersTitle")}</h3>
        <p className="muted">
          {t("storagePage.consumersNone")} <code>ai-runner run --measure-space</code>
        </p>
      </div>
    );
  }
  const items = [...c.items].sort((a, b) => b.bytes - a.bytes);
  const size = (x: SpaceConsumer) =>
    x.partial ? t("storagePage.atLeast", { size: formatBytes(x.bytes, lng) }) : formatBytes(x.bytes, lng);
  const num = new Intl.NumberFormat(lng);
  return (
    <div className="consumers">
      <div className="table-wrap">
        <table className="compact">
          <caption>
            <h3>{t("storagePage.consumersTitle")}</h3>
            <span className="muted small">{t("storagePage.consumersMeasured", { date: formatDateTime(c.measuredAt, lng) })}</span>
          </caption>
          <thead>
            <tr>
              <th scope="col">{t("storagePage.colCategory")}</th>
              <th scope="col">{t("storagePage.colSize")}</th>
              <th scope="col">{t("storagePage.colEntries")}</th>
            </tr>
          </thead>
          <tbody>
            {items.map((x) => (
              <tr key={x.category}>
                <th scope="row">{t(`storagePage.consumer_${x.category}`)}</th>
                <td>{size(x)}</td>
                <td>{x.category === "container_cache" ? "—" : num.format(x.entries)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {items.some((x) => x.partial) && <p className="muted small">{t("storagePage.partialNote")}</p>}
    </div>
  );
}
